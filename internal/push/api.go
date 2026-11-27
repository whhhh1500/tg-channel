package push

import (
	"errors"
	"strings"
	"time"

	"tgchannel/internal/db"
)

// APIMessage 对外接口收到的一条消息，走 AccountID 那个 TG 账号已启用的渠道。
type APIMessage struct {
	AccountID, TopicID uint
	From               string // 调用方 IP，记在消息记录的"发送者"
	Title, Text, Click string
	Priority           int // 0 表示用 ntfy 默认
	Tags               []string
}

// apiExtra 接口消息除正文外的内容，和正文一样只在内存。
type apiExtra struct {
	title, click string
	priority     int
	tags         []string
}

var (
	ErrNoChannel = errors.New("推送渠道都已停用")
	ErrBusy      = errors.New("推送队列已满，稍后再试")
)

// maxQueued 内存里待推正文的上限：推送服务长时间挂掉时，接口调用不能无限堆内存。TG 消息不受限。
const maxQueued = 20000

// HandleAPI 接口消息和 TG 消息进同一个推送队列：失败按同样的退避重试，消息记录里能看到状态。
func (s *Service) HandleAPI(m APIMessage) (uint, error) {
	var chs []db.Channel
	if err := s.db.Where("account_id = ? AND enabled = ?", m.AccountID, true).Find(&chs).Error; err != nil {
		return 0, err
	}
	if len(chs) == 0 {
		return 0, ErrNoChannel
	}
	s.tmu.Lock()
	n := len(s.texts)
	s.tmu.Unlock()
	if n >= maxQueued {
		return 0, ErrBusy
	}
	var acc db.TgAccount
	s.db.Select("priority", "repeats").Where("id = ?", m.AccountID).Limit(1).Find(&acc)
	prio := m.Priority // 调用方没给就用账号默认等级
	if prio == 0 {
		prio = acc.Priority
	}
	now := time.Now()
	msg := db.Message{AccountID: m.AccountID, TopicID: m.TopicID, Sender: m.From, SentAt: now, ReceivedAt: now}
	id, err := s.enqueue(msg, chs, body{text: m.Text, times: repeatTimes(prio, acc.Repeats),
		api: &apiExtra{title: m.Title, click: m.Click, priority: prio, tags: m.Tags}})
	if err != nil {
		return 0, err
	}
	s.kick()
	return id, nil
}

// apiTitle 标题 "图标 [主题名] 调用方标题"；ntfy 附加字段：主题标识作为第一个标签，方便在 App 里筛选。
func apiTitle(t db.ApiTopic, x *apiExtra) (string, map[string]any) {
	name := t.Name
	if name == "" {
		name = "接口"
	}
	title := strings.TrimSpace(t.Icon + " [" + name + "]")
	if x.title != "" {
		title += " " + x.title
	}
	extra := map[string]any{}
	var tags []string
	if t.Code != "" {
		tags = append(tags, t.Code)
	}
	if tags = append(tags, x.tags...); len(tags) > 0 {
		extra["tags"] = tags
	}
	if x.priority > 0 {
		extra["priority"] = x.priority
	}
	if x.click != "" {
		extra["click"] = x.click
	}
	return title, extra
}
