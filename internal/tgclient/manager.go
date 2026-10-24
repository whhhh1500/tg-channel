package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"gorm.io/gorm"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
)

// Incoming 监听到的一条新消息，交给上层决定是否转发。
type Incoming struct {
	AccountID  uint
	PeerKind   string
	PeerID     int64
	MsgID      int
	Sender     string
	SenderUser string
	SenderID   int64
	Text       string
	SentAt     time.Time
}

// Manager 管理所有在线账号，每个账号一条连接。
type Manager struct {
	ctx context.Context
	cfg *config.Config
	db  *gorm.DB

	OnMessage      func(Incoming)
	OnAccountAdded func(acc db.TgAccount)

	mu        sync.Mutex
	running   map[uint]*running
	jobs      map[string]*LoginJob
	timers    map[uint]*time.Timer // 断线后的重连定时器，重新扫码 / 删号时要取消
	lastReply map[uint]time.Time   // 回复限速，注销连接时清掉
	wg        sync.WaitGroup
}

type running struct {
	client *telegram.Client
	cancel context.CancelFunc
	ready  chan struct{}
	once   sync.Once
}

func NewManager(ctx context.Context, cfg *config.Config, g *gorm.DB) *Manager {
	return &Manager{ctx: ctx, cfg: cfg, db: g, running: map[uint]*running{}, jobs: map[string]*LoginJob{}, timers: map[uint]*time.Timer{}, lastReply: map[uint]time.Time{}}
}

func (m *Manager) newClient(storage session.Storage, handler telegram.UpdateHandler) *telegram.Client {
	return telegram.NewClient(m.cfg.TgAPIID, m.cfg.TgAPIHash, telegram.Options{
		SessionStorage: storage,
		UpdateHandler:  handler,
		Middlewares:    []telegram.Middleware{floodwait.NewSimpleWaiter()},
	})
}

// StartAll 启动时拉起所有已登录账号。
func (m *Manager) StartAll() {
	if m.cfg.TgAPIID == 0 || m.cfg.TgAPIHash == "" {
		slog.Error("TG_API_ID / TG_API_HASH 未配置，不启动任何账号")
		return
	}
	var accs []db.TgAccount
	m.db.Where("length(session_enc) > 0").Find(&accs)
	for _, a := range accs {
		m.Start(a.ID)
	}
}

// Start 用库里的 session 起一条常驻连接；已在线或账号已删则不动。
func (m *Manager) Start(accountID uint) {
	var n int64
	m.db.Model(&db.TgAccount{}).Where("id = ? AND length(session_enc) > 0", accountID).Count(&n)
	if n == 0 {
		return
	}
	m.mu.Lock()
	if t := m.timers[accountID]; t != nil {
		t.Stop()
		delete(m.timers, accountID)
	}
	if _, ok := m.running[accountID]; ok {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	d := tg.NewUpdateDispatcher()
	m.listen(accountID, d)
	st := &dbState{db: m.db, accountID: accountID}
	gaps := updates.New(updates.Config{Handler: d, Storage: st, AccessHasher: st})
	r := &running{cancel: cancel, ready: make(chan struct{})}
	r.client = m.newClient(&dbSession{db: m.db, masterKey: m.cfg.MasterKey, accountID: accountID}, gaps)
	m.running[accountID] = r
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.unregister(accountID, r)
		err := r.client.Run(ctx, func(ctx context.Context) error {
			self, err := r.client.Self(ctx)
			if err != nil {
				return err
			}
			m.setStatus(accountID, "online", "")
			r.once.Do(func() { close(r.ready) })
			return gaps.Run(ctx, r.client.API(), self.ID, updates.AuthOptions{})
		})
		m.afterRun(accountID, r, err, ctx)
	}()
}

// afterRun 连接结束后更新状态；非永久错误则稍后用库里的 session 重连。
// 被新连接顶替（重新扫码）的旧连接什么都不改。
func (m *Manager) afterRun(accountID uint, r *running, err error, ctx context.Context) {
	if m.get(accountID) != r {
		return
	}
	switch {
	case ctx.Err() != nil:
		m.setStatus(accountID, "offline", "")
	case errors.Is(err, ErrSessionGone):
		slog.Error("tg session gone", "account", accountID, "err", err)
		m.setStatus(accountID, "error", "session 无法读取，请删除后重新扫码")
	case auth.IsUnauthorized(err):
		slog.Error("tg session unauthorized", "account", accountID, "err", err)
		m.setStatus(accountID, "error", "登录已失效（"+shortErr(err)+"），请删除后重新扫码")
	default:
		slog.Warn("tg client stopped, retrying", "account", accountID, "err", err)
		m.setStatus(accountID, "error", "连接中断，15 秒后重连："+shortErr(err))
		m.mu.Lock()
		m.timers[accountID] = time.AfterFunc(15*time.Second, func() {
			if m.ctx.Err() == nil {
				m.Start(accountID)
			}
		})
		m.mu.Unlock()
	}
}

// stopAndWait 取消账号的重连定时器、停掉现有连接并等它退出登记（最多 wait）。
// 返回 false 表示旧连接没在时限内退出，调用方不应继续写该账号的 session。
func (m *Manager) stopAndWait(accountID uint, wait time.Duration) bool {
	m.mu.Lock()
	if t := m.timers[accountID]; t != nil {
		t.Stop()
		delete(m.timers, accountID)
	}
	r := m.running[accountID]
	m.mu.Unlock()
	if r == nil {
		return true
	}
	r.cancel()
	deadline := time.Now().Add(wait)
	for m.get(accountID) == r {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// register 把连接登记为账号的常驻连接；已有的先停掉，一个账号只能有一条连接。
func (m *Manager) register(accountID uint, r *running) {
	m.mu.Lock()
	old := m.running[accountID]
	m.running[accountID] = r
	m.mu.Unlock()
	if old != nil {
		old.cancel()
	}
}

// unregister 只在登记的还是自己时才摘掉，避免误删后来者。
func (m *Manager) unregister(accountID uint, r *running) {
	m.mu.Lock()
	if m.running[accountID] == r {
		delete(m.running, accountID)
		delete(m.lastReply, accountID)
	}
	m.mu.Unlock()
}

func (m *Manager) get(accountID uint) *running {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[accountID]
}

// Logout 在 Telegram 侧注销该 session，再停掉连接并等它退出（删号前用，避免收尾写回孤儿行）。
func (m *Manager) Logout(ctx context.Context, accountID uint) {
	if r := m.get(accountID); r != nil {
		select {
		case <-r.ready:
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err := r.client.API().AuthLogOut(ctx); err != nil {
				slog.Warn("tg logout", "account", accountID, "err", err)
			}
		default:
		}
	}
	if !m.stopAndWait(accountID, 5*time.Second) {
		slog.Warn("tg connection did not stop in time", "account", accountID)
	}
}

// Close 停掉全部连接并等待退出。
func (m *Manager) Close() {
	m.mu.Lock()
	for _, r := range m.running {
		r.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Online 当前在线账号数，用于日志。
func (m *Manager) Online() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.running)
}

func (m *Manager) setStatus(id uint, status, lastErr string) {
	m.db.Model(&db.TgAccount{}).Where("id = ?", id).Updates(map[string]any{"status": status, "last_error": lastErr})
}

// listen 注册新消息回调。
func (m *Manager) listen(accountID uint, d tg.UpdateDispatcher) {
	d.OnNewMessage(func(_ context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		m.handle(accountID, e, u.Message)
		return nil
	})
	d.OnNewChannelMessage(func(_ context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		m.handle(accountID, e, u.Message)
		return nil
	})
}

func (m *Manager) handle(accountID uint, e tg.Entities, raw tg.MessageClass) {
	if svc, ok := raw.(*tg.MessageService); ok { // 被拉进群、建群等
		m.ensurePeer(accountID, e, svc.PeerID)
		return
	}
	msg, ok := raw.(*tg.Message)
	if !ok {
		return
	}
	m.ensurePeer(accountID, e, msg.PeerID)
	if m.OnMessage == nil {
		return
	}
	slog.Debug("tg message", "account", accountID, "peer", fmt.Sprintf("%T", msg.PeerID), "id", msg.ID, "out", msg.Out, "len", len(msg.Message))
	if msg.Out { // 自己发出的不转发
		return
	}
	in := Incoming{AccountID: accountID, MsgID: msg.ID, Text: msg.Message, SentAt: time.Unix(int64(msg.Date), 0)}
	switch p := msg.PeerID.(type) {
	case *tg.PeerUser:
		in.PeerKind, in.PeerID = "user", p.UserID
	case *tg.PeerChat:
		in.PeerKind, in.PeerID = "chat", p.ChatID
	case *tg.PeerChannel:
		in.PeerKind, in.PeerID = "channel", p.ChannelID
	default:
		return
	}
	if in.Text == "" && msg.Media != nil {
		in.Text = "[媒体消息]"
	}
	var from *tg.User
	if f, ok := msg.FromID.(*tg.PeerUser); ok {
		from = e.Users[f.UserID]
	} else if in.PeerKind == "user" {
		from = e.Users[in.PeerID]
	}
	if from != nil {
		in.Sender, in.SenderUser, in.SenderID = userName(from), from.Username, from.ID
	}
	m.OnMessage(in)
}

// shortErr 只取 Telegram 错误码，不把 gotd 的堆栈展示给用户。
func shortErr(err error) string {
	if e, ok := tgerr.As(err); ok {
		return e.Type
	}
	s := err.Error()
	if i := strings.IndexByte(s, 10); i > 0 { // 第一行
		s = s[:i]
	}
	return s
}

func userName(u *tg.User) string {
	if u == nil {
		return ""
	}
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	return u.Username
}
