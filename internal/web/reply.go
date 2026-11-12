package web

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"tgchannel/internal/reply"
)

// 回复页：不走登录态，凭通知链接里的签名 token。token 放在 URL 的 # 后面，
// 浏览器不会把它发给服务器，所以页面 JS 读出来后用 POST 送到下面两个接口。

// uses 每个链接的已用次数和发出的回复，只在内存，链接过期或重启即清空（用户接受）。
type uses struct {
	mu   sync.Mutex
	m    map[string]entryUse
	last time.Time
}

type entryUse struct {
	n    int
	exp  time.Time
	sent []sentMsg
}

// sentMsg 从这个链接发出的一条回复，修改时只认这里的 id。
type sentMsg struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
	At   string `json:"at"`
}

func newUses() *uses { return &uses{m: map[string]entryUse{}} }

// get 返回已用次数和回复历史（副本）。
func (u *uses) get(key string) (int, []sentMsg) {
	u.mu.Lock()
	defer u.mu.Unlock()
	e := u.m[key]
	return e.n, append([]sentMsg{}, e.sent...)
}

// add 记一条发出去的回复。
func (u *uses) add(key string, m sentMsg) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if e, ok := u.m[key]; ok {
		e.sent = append(e.sent, m)
		u.m[key] = e
	}
}

// text 返回这个链接发出的某条回复的内容；不是它发的返回 false。
func (u *uses) text(key string, id int) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, m := range u.m[key].sent {
		if m.ID == id {
			return m.Text, true
		}
	}
	return "", false
}

// setText 修改成功后更新历史里的内容。
func (u *uses) setText(key string, id int, text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, m := range u.m[key].sent {
		if m.ID == id {
			u.m[key].sent[i].Text = text
		}
	}
}

// reserve 发送前原子预留一次，返回预留后的已用次数；满了返回 false。
// 过期项的清理每分钟做一次，不在每次预留时全表扫。
func (u *uses) reserve(key string, exp time.Time, max int) (int, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	if now.Sub(u.last) > time.Minute {
		for k, e := range u.m {
			if e.exp.Before(now) {
				delete(u.m, k)
			}
		}
		u.last = now
	}
	e := u.m[key]
	if e.n >= max {
		return e.n, false
	}
	e.n++
	e.exp = exp
	u.m[key] = e
	return e.n, true
}

// release 发送失败时把预留的一次还回去。
func (u *uses) release(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if e, ok := u.m[key]; ok && e.n > 0 {
		e.n--
		u.m[key] = e
	}
}

// bindJSON 解析失败自己回 400 / 413，不经 gin 的错误收集。
func bindJSON(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "请求太大"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		}
		return false
	}
	return true
}

func (s *Server) replyPage(c *gin.Context) {
	c.HTML(http.StatusOK, "reply.html", gin.H{})
}

type replyInfo struct {
	Peer      string    `json:"peer"`
	Sender    string    `json:"sender"`
	SenderID  int64     `json:"sender_id"`
	Text      string    `json:"text"`
	Remaining int       `json:"remaining"`
	ExpiresAt string    `json:"expires_at"`
	DeepLink  string    `json:"deep_link"`
	History   []sentMsg `json:"history"`
}

func (s *Server) parseToken(c *gin.Context, token string) (reply.Payload, bool) {
	p, err := reply.Parse(s.cfg.SessionSecret, token)
	switch {
	case errors.Is(err, reply.ErrExpired):
		c.JSON(http.StatusGone, gin.H{"error": "链接已过期"})
	case err != nil:
		c.JSON(http.StatusForbidden, gin.H{"error": "链接无效"})
	default:
		return p, true
	}
	return p, false
}

// replyOpen 页面打开时调用：验签并返回要显示的内容。
func (s *Server) replyOpen(c *gin.Context) {
	var in struct{ Token string }
	if !bindJSON(c, &in) {
		return
	}
	p, ok := s.parseToken(c, in.Token)
	if !ok {
		return
	}
	n, history := s.uses.get(reply.Key(in.Token))
	c.JSON(http.StatusOK, replyInfo{
		Peer: displayName("", p.PeerTitle), Sender: displayName(p.SenderUser, p.Sender), SenderID: p.SenderID, Text: p.Text,
		Remaining: reply.MaxUses - n,
		ExpiresAt: time.Unix(p.Exp, 0).Local().Format("01-02 15:04"),
		DeepLink:  reply.DeepLink(p.PeerKind, p.PeerID, p.MsgID),
		History:   history,
	})
}

// replyText 校验回复内容。
func replyText(c *gin.Context, raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if text == "" || len([]rune(text)) > 4000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "内容为空或太长"})
		return "", false
	}
	return text, true
}

// replySend 发送回复：成功才计次，满 3 次后不能再发。quote 为 true 时引用原消息，默认直接发。
func (s *Server) replySend(c *gin.Context) {
	var in struct {
		Token, Text string
		Quote       bool
	}
	if !bindJSON(c, &in) {
		return
	}
	p, ok := s.parseToken(c, in.Token)
	if !ok {
		return
	}
	text, ok := replyText(c, in.Text)
	if !ok {
		return
	}
	key := reply.Key(in.Token)
	n, ok := s.uses.reserve(key, time.Unix(p.Exp, 0), reply.MaxUses)
	if !ok {
		c.JSON(http.StatusGone, gin.H{"error": "这个链接的 3 次回复已用完"})
		return
	}
	id, err := s.tg.Reply(c.Request.Context(), p.Account, p.PeerKind, p.PeerID, p.AccessHash, p.MsgID, in.Quote, text)
	if err != nil {
		s.uses.release(key)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "发送失败：" + err.Error()})
		return
	}
	m := sentMsg{ID: id, Text: text, At: time.Now().Format("15:04:05")}
	if id != 0 { // 取不到 id 的不进历史，也就不能改
		s.uses.add(key, m)
	}
	c.JSON(http.StatusOK, gin.H{"remaining": reply.MaxUses - n, "sent": m})
}

// replyEdit 修改这个链接发出的回复，不占回复次数，和发送共用每账号 5 秒间隔。
func (s *Server) replyEdit(c *gin.Context) {
	var in struct {
		Token, Text string
		ID          int
	}
	if !bindJSON(c, &in) {
		return
	}
	p, ok := s.parseToken(c, in.Token)
	if !ok {
		return
	}
	text, ok := replyText(c, in.Text)
	if !ok {
		return
	}
	key := reply.Key(in.Token)
	old, ok := s.uses.text(key, in.ID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "只能修改从这个链接发出的回复"})
		return
	}
	if old != text { // 内容没变时 Telegram 会报 MESSAGE_NOT_MODIFIED
		if err := s.tg.Edit(c.Request.Context(), p.Account, p.PeerKind, p.PeerID, p.AccessHash, in.ID, text); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "修改失败：" + err.Error()})
			return
		}
		s.uses.setText(key, in.ID, text)
	}
	c.JSON(http.StatusOK, gin.H{"text": text})
}
