package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"tgchannel/internal/db"
	"tgchannel/internal/push"
)

// 对外接口：用户在"通知设置 → 对外接口"建主题，每个主题一个 key；
// 外部系统 POST /api/v1/push/<主题标识> 带 key，消息进推送队列，走主题绑定的 TG 账号的渠道。

const (
	maxTopics     = 20 // 每个用户最多几个主题
	apiRate       = 60 // 每个主题每分钟最多调用次数
	apiFailMax    = 10 // 同一 IP 在 apiFailWindow 内 key 错这么多次就先拒绝，防猜 key
	apiFailWindow = 10 * time.Minute
	maxAPIText    = 4000
	maxAPITitle   = 100
)

var codeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// newKey 32 字节随机数，库里只存 sha256。
func newKey() (key, hash, prefix string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	key = "tgk_" + base64.RawURLEncoding.EncodeToString(b)
	return key, keyHash(key), key[:12]
}

func keyHash(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

type apiIn struct {
	Title    string   `json:"title"`
	Message  string   `json:"message"`
	Priority int      `json:"priority"`
	Click    string   `json:"click"`
	Tags     []string `json:"tags"`
}

// apiPush POST /api/v1/push/:code，请求头 Authorization: Bearer <key>。
func (s *Server) apiPush(c *gin.Context) {
	fail := func(code int, msg string) { c.JSON(code, gin.H{"ok": false, "error": msg}) }
	ip := c.ClientIP()
	if s.limit.full("apifail:"+ip, apiFailMax, apiFailWindow) {
		c.Header("Retry-After", strconv.Itoa(int(apiFailWindow.Seconds())))
		fail(http.StatusTooManyRequests, "key 错误次数太多，10 分钟后再试")
		return
	}
	var t db.ApiTopic
	if key, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer "); ok && strings.TrimSpace(key) != "" {
		s.db.Where("key_hash = ?", keyHash(strings.TrimSpace(key))).Limit(1).Find(&t)
	}
	if t.ID == 0 || t.Code != c.Param("code") {
		s.limit.allow("apifail:"+ip, apiFailMax, apiFailWindow)
		fail(http.StatusUnauthorized, "key 无效或与主题标识不匹配")
		return
	}
	if !t.Enabled {
		fail(http.StatusForbidden, "主题已停用")
		return
	}
	if !s.limit.allow(fmt.Sprintf("api:%d", t.ID), apiRate, time.Minute) {
		c.Header("Retry-After", "60")
		fail(http.StatusTooManyRequests, fmt.Sprintf("调用太快，每分钟最多 %d 次", apiRate))
		return
	}
	in, err := readAPIBody(c)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(http.StatusRequestEntityTooLarge, "请求体超过 64KB")
		} else {
			fail(http.StatusBadRequest, err.Error())
		}
		return
	}
	id, err := s.push.HandleAPI(push.APIMessage{AccountID: t.AccountID, TopicID: t.ID, From: ip,
		Title: in.Title, Text: in.Message, Click: in.Click, Priority: in.Priority, Tags: in.Tags})
	switch {
	case errors.Is(err, push.ErrNoChannel):
		fail(http.StatusConflict, "推送渠道都已停用，到「我的 TG」里启用 ntfy 或 Bark")
		return
	case errors.Is(err, push.ErrBusy):
		c.Header("Retry-After", "60")
		fail(http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		fail(http.StatusInternalServerError, "保存失败，稍后重试")
		return
	}
	s.db.Model(&db.ApiTopic{}).Where("id = ?", t.ID).Updates(map[string]any{"calls": gorm.Expr("calls + 1"), "last_at": time.Now()})
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "id": id})
}

// readAPIBody JSON 按字段读；其他类型（curl -d 默认是表单类型）整个请求体当正文。
func readAPIBody(c *gin.Context) (apiIn, error) {
	var in apiIn
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return in, err
	}
	if c.ContentType() == "application/json" {
		if err := json.Unmarshal(raw, &in); err != nil {
			return in, errors.New("JSON 格式错误，字段见接口文档")
		}
	} else {
		in.Message = string(raw)
	}
	in.Title = strings.Join(strings.Fields(in.Title), " ") // 标题不留换行
	in.Message = strings.TrimSpace(in.Message)
	switch {
	case in.Message == "":
		return in, errors.New("message 不能为空")
	case !utf8.ValidString(in.Message) || !utf8.ValidString(in.Title):
		return in, errors.New("内容必须是 UTF-8 编码")
	case utf8.RuneCountInString(in.Message) > maxAPIText:
		return in, fmt.Errorf("message 最多 %d 字", maxAPIText)
	case utf8.RuneCountInString(in.Title) > maxAPITitle:
		return in, fmt.Errorf("title 最多 %d 字", maxAPITitle)
	case in.Priority < 0 || in.Priority > 5:
		return in, errors.New("priority 只能是 1-5")
	case len(in.Tags) > 3:
		return in, errors.New("tags 最多 3 个")
	}
	if in.Click != "" {
		u, err := url.Parse(in.Click)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(in.Click) > 1000 {
			return in, errors.New("click 必须是 http 或 https 网址")
		}
	}
	for i, tag := range in.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || utf8.RuneCountInString(tag) > 20 || strings.Contains(tag, ",") {
			return in, errors.New("tags 每个 1-20 字，不能含逗号")
		}
		in.Tags[i] = tag
	}
	return in, nil
}

// ---- 主题管理（需登录，只能管自己的） ----

type topicRow struct {
	db.ApiTopic
	Account string
	Last    string
}

// apiBase 接口地址前缀，文档和示例里用。
func (s *Server) apiBase(c *gin.Context) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/") + "/api/v1/push/"
	}
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host + s.base + "/api/v1/push/"
}

func (s *Server) topicsPage(c *gin.Context) {
	uid := s.currentUser(c).ID
	var accounts []db.TgAccount
	s.db.Where("user_id = ?", uid).Order("id").Find(&accounts)
	var rows []topicRow
	s.db.Table("api_topics").Select("api_topics.*, tg_accounts.first_name AS account").
		Joins("LEFT JOIN tg_accounts ON tg_accounts.id = api_topics.account_id").
		Where("api_topics.user_id = ?", uid).Order("api_topics.id").Scan(&rows)
	for i := range rows {
		if rows[i].LastAt != nil {
			rows[i].Last = fmtSec(*rows[i].LastAt)
		}
	}
	var edit *db.ApiTopic
	if id := c.Query("edit"); id != "" {
		var t db.ApiTopic
		if s.db.Where("id = ? AND user_id = ?", id, uid).Limit(1).Find(&t); t.ID != 0 {
			edit = &t
		}
	}
	s.render(c, "topics.html", gin.H{"Nav": "peers", "Topics": rows, "Accounts": accounts, "Edit": edit, "APIBase": s.apiBase(c), "Max": maxTopics})
}

// topicForm 读名称、图标、备注、推送账号，校验后写进 t。返回给用户看的错误。
func (s *Server) topicForm(c *gin.Context, t *db.ApiTopic) string {
	t.Name = strings.TrimSpace(c.PostForm("name"))
	t.Icon = strings.TrimSpace(c.PostForm("icon"))
	t.Note = strings.TrimSpace(c.PostForm("note"))
	switch n := utf8.RuneCountInString(t.Name); {
	case n == 0 || n > 20:
		return "名称 1-20 个字"
	case utf8.RuneCountInString(t.Icon) > 8:
		return "图标最多 8 个字符"
	case utf8.RuneCountInString(t.Note) > 100:
		return "备注最多 100 字"
	}
	var accounts []db.TgAccount
	s.db.Where("user_id = ?", t.UserID).Order("id").Find(&accounts)
	want, _ := strconv.Atoi(c.PostForm("account"))
	if want == 0 && len(accounts) == 1 {
		want = int(accounts[0].ID)
	}
	for _, a := range accounts {
		if int(a.ID) == want {
			t.AccountID = a.ID
			return ""
		}
	}
	if len(accounts) == 0 {
		return "先添加 TG 账号"
	}
	return "请选择推送到哪个 TG 账号"
}

func (s *Server) createTopic(c *gin.Context) {
	uid := s.currentUser(c).ID
	t := db.ApiTopic{UserID: uid, Enabled: true, Code: strings.TrimSpace(c.PostForm("code"))}
	msg := s.topicForm(c, &t)
	if msg == "" && !codeRe.MatchString(t.Code) {
		msg = "主题标识只能用字母、数字、- 和 _，最多 32 位"
	}
	if msg == "" {
		var n, dup int64
		s.db.Model(&db.ApiTopic{}).Where("user_id = ?", uid).Count(&n)
		s.db.Model(&db.ApiTopic{}).Where("user_id = ? AND code = ?", uid, t.Code).Count(&dup)
		switch {
		case n >= maxTopics:
			msg = fmt.Sprintf("最多 %d 个主题", maxTopics)
		case dup > 0:
			msg = "主题标识 " + t.Code + " 已经用过了"
		}
	}
	var key string
	if msg == "" {
		key, t.KeyHash, t.KeyPrefix = newKey()
		if err := s.db.Create(&t).Error; err != nil {
			msg = "保存失败：" + err.Error()
		}
	}
	if msg != "" {
		flash(c, msg)
		s.redirect(c, "/topics")
		return
	}
	s.renderKey(c, t, key, "主题已创建")
}

func (s *Server) ownedTopic(c *gin.Context) *db.ApiTopic {
	var t db.ApiTopic
	if s.db.Where("id = ? AND user_id = ?", c.Param("id"), s.currentUser(c).ID).Limit(1).Find(&t); t.ID == 0 {
		c.String(http.StatusNotFound, "404")
		return nil
	}
	return &t
}

// updateTopic 改名称、图标、备注、推送账号；主题标识和 key 不变，调用方不受影响。
func (s *Server) updateTopic(c *gin.Context) {
	t := s.ownedTopic(c)
	if t == nil {
		return
	}
	if msg := s.topicForm(c, t); msg != "" {
		flash(c, msg)
		s.redirect(c, fmt.Sprintf("/topics?edit=%d", t.ID))
		return
	}
	s.db.Model(t).Updates(map[string]any{"name": t.Name, "icon": t.Icon, "note": t.Note, "account_id": t.AccountID})
	flash(c, "已保存")
	s.redirect(c, "/topics")
}

func (s *Server) toggleTopic(c *gin.Context) {
	if s.db.Exec("UPDATE api_topics SET enabled = NOT enabled WHERE id = ? AND user_id = ?", c.Param("id"), s.currentUser(c).ID).RowsAffected == 0 {
		c.String(http.StatusNotFound, "404")
		return
	}
	c.Status(http.StatusNoContent)
}

// resetTopicKey 换新 key，旧 key 立即失效。
func (s *Server) resetTopicKey(c *gin.Context) {
	t := s.ownedTopic(c)
	if t == nil {
		return
	}
	key, hash, prefix := newKey()
	s.db.Model(t).Updates(map[string]any{"key_hash": hash, "key_prefix": prefix})
	s.renderKey(c, *t, key, "key 已重置，旧 key 已失效")
}

func (s *Server) deleteTopic(c *gin.Context) {
	if s.db.Where("id = ? AND user_id = ?", c.Param("id"), s.currentUser(c).ID).Delete(&db.ApiTopic{}).RowsAffected == 0 {
		c.String(http.StatusNotFound, "404")
		return
	}
	flash(c, "已删除，旧 key 已失效")
	s.redirect(c, "/topics")
}

// renderKey key 原文只在这一页出现：不进 cookie、不进库，也不让浏览器缓存。
func (s *Server) renderKey(c *gin.Context, t db.ApiTopic, key, title string) {
	c.Header("Cache-Control", "no-store")
	s.render(c, "topic_key.html", gin.H{"Nav": "peers", "Title": title, "Topic": t, "Key": key, "URL": s.apiBase(c) + t.Code})
}
