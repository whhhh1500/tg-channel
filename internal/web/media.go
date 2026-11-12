package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"tgchannel/internal/reply"
	"tgchannel/internal/tgclient"
)

// 媒体预览：回复页点"预览"时，先用 token 换一张 10 分钟的票据，
// 浏览器再拿票据地址直接当 <img>/<video>/<audio>/<iframe> 的 src，边下边显示，支持拖动进度（Range）。
// 文件从 Telegram 按需拉取、直接转给浏览器，服务端不落盘、不缓存整份。

const ticketTTL = 10 * time.Minute

type ticket struct {
	account uint
	media   tgclient.Media
	exp     time.Time
}

type tickets struct {
	mu   sync.Mutex
	m    map[string]ticket
	busy chan struct{} // 同时下载数上限，控制内存（每路约 2MB）和账号连接压力
}

func newTickets() *tickets { return &tickets{m: map[string]ticket{}, busy: make(chan struct{}, 4)} }

func (t *tickets) add(account uint, m tgclient.Media) string {
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range t.m { // 票据很少，插入时顺手清过期的
		if v.exp.Before(now) {
			delete(t.m, k)
		}
	}
	t.m[id] = ticket{account, m, now.Add(ticketTTL)}
	return id
}

func (t *tickets) get(id string) (ticket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[id]
	return v, ok && v.exp.After(time.Now())
}

// replyMedia 校验 token，重新取一次消息拿最新的文件引用，返回预览地址。
func (s *Server) replyMedia(c *gin.Context) {
	var in struct{ Token string }
	if !bindJSON(c, &in) {
		return
	}
	p, ok := s.parseToken(c, in.Token)
	if !ok {
		return
	}
	if !s.limit.allow("media:"+reply.Key(in.Token), 6, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "点太快了，稍后再试"})
		return
	}
	m, err := s.tg.Media(c.Request.Context(), p.Account, p.PeerKind, p.PeerID, p.AccessHash, p.MsgID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "读取失败：" + err.Error()})
		return
	}
	if m.Preview == "" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "这个文件不能预览"})
		return
	}
	id := s.files.add(p.Account, m)
	c.JSON(http.StatusOK, gin.H{"url": s.url("/reply/file/" + id), "type": m.Preview, "name": m.Name})
}

// replyFile 按票据把文件转给浏览器，支持单段 Range。
func (s *Server) replyFile(c *gin.Context) {
	t, ok := s.files.get(c.Param("id"))
	if !ok {
		c.String(http.StatusGone, "预览已过期，回到页面重新点预览")
		return
	}
	select {
	case s.files.busy <- struct{}{}:
		defer func() { <-s.files.busy }()
	default:
		c.String(http.StatusServiceUnavailable, "预览繁忙，稍后再试")
		return
	}
	m := t.media
	from, to, partial := int64(0), m.Size-1, false
	if r := c.GetHeader("Range"); r != "" && m.Size > 0 {
		var ok bool
		if from, to, ok = parseRange(r, m.Size); !ok {
			c.Header("Content-Range", fmt.Sprintf("bytes */%d", m.Size))
			c.Status(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		partial = true
	}
	h := c.Writer.Header()
	h.Set("Content-Type", m.Mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=600")
	h.Set("X-Accel-Buffering", "no") // 让 Nginx 不缓冲，边收边发、不写临时文件
	if m.Size > 0 {
		h.Set("Accept-Ranges", "bytes")
		h.Set("Content-Length", strconv.FormatInt(to-from+1, 10))
	}
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, m.Size))
		c.Status(http.StatusPartialContent)
	} else {
		c.Status(http.StatusOK)
	}
	if c.Request.Method == http.MethodHead {
		return
	}
	if m.Size <= 0 {
		to = 1<<62 - 1 // 不知道大小就拉到文件尾
	}
	// 服务的写超时是 60 秒，慢网络下 10MB 视频不够，单独放宽
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(5 * time.Minute))
	err := s.tg.Download(c.Request.Context(), t.account, m.Loc, from, to, c.Writer)
	if err == nil || c.Request.Context().Err() != nil { // 浏览器自己断开（拖进度、关页面）不算错
		return
	}
	slog.Warn("preview download", "account", t.account, "err", err)
	if !c.Writer.Written() { // 还没开始传就失败（如账号掉线）：换成明确的错误，别让浏览器等一个空的长度
		h.Del("Content-Length")
		h.Del("Content-Range")
		c.String(http.StatusServiceUnavailable, "预览失败："+err.Error())
	}
}

// parseRange 解析单段 "bytes=a-b" / "bytes=a-" / "bytes=-n"，多段不支持。
func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	var from, to int64
	var err error
	switch {
	case a == "": // 最后 n 字节
		n, e := strconv.ParseInt(b, 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, false
		}
		from, to = max(size-n, 0), size-1
	default:
		if from, err = strconv.ParseInt(a, 10, 64); err != nil {
			return 0, 0, false
		}
		to = size - 1
		if b != "" {
			if to, err = strconv.ParseInt(b, 10, 64); err != nil {
				return 0, 0, false
			}
			to = min(to, size-1)
		}
	}
	if from < 0 || from > to || from >= size {
		return 0, 0, false
	}
	return from, to, true
}
