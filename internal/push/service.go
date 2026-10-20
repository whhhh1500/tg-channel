// Package push 把监听到的消息落库，并通过 outbox 推送到各渠道。
package push

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nikoksr/notify/service/bark"
	"gorm.io/gorm"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/reply"
	"tgchannel/internal/tgclient"
)

const (
	maxAttempts   = 8
	batch         = 50
	workers       = 8
	pushBodyBytes = 3600 // ntfy 单条 4096 字节上限，留余量
)

type Service struct {
	cfg  *config.Config
	db   *gorm.DB
	ntfy *ntfyClient
	wake chan struct{}

	// 正文只在内存里排队，按 push_log id 存，推完即删；库里永远不落正文。
	tmu   sync.Mutex
	texts map[uint]string

	// 固定 worker 池；inflight 记录已派发未完成的任务，drain 不会重复派发
	jobs     chan db.PushLog
	imu      sync.Mutex
	inflight map[uint]bool
}

func New(cfg *config.Config, g *gorm.DB) *Service {
	s := &Service{cfg: cfg, db: g, wake: make(chan struct{}, 1), texts: map[uint]string{}, jobs: make(chan db.PushLog, workers*2), inflight: map[uint]bool{}}
	if cfg.NtfyUser != "" {
		s.ntfy = newNtfy(cfg.NtfyURL, cfg.NtfyUser, cfg.NtfyPassword)
	}
	return s
}

// SyncNtfyUser 注册和每次登录时调用：ntfy 里没有同名用户就建（密码同平台），
// 并把该用户所有账号的 topic 授只读。后台异步执行，ntfy 挂了也不拖慢登录。
func (s *Service) SyncNtfyUser(u db.User, password string) {
	if s.ntfy == nil {
		return
	}
	go s.syncNtfyUser(u, password)
}

func (s *Service) syncNtfyUser(u db.User, password string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.ntfy.createUser(ctx, u.Username, password); err != nil {
		slog.Warn("ntfy create user", "user", u.Username, "err", err)
		return
	}
	var topics []string
	s.db.Model(&db.Channel{}).Where("kind = 'ntfy' AND account_id IN (SELECT id FROM tg_accounts WHERE user_id = ?)", u.ID).Pluck("target", &topics)
	for _, t := range topics {
		if err := s.ntfy.grantRead(ctx, u.Username, t); err != nil {
			slog.Warn("ntfy grant", "user", u.Username, "topic", t, "err", err)
		}
	}
}

// DeleteNtfyUser 删除平台用户时同步删 ntfy 用户。
func (s *Service) DeleteNtfyUser(ctx context.Context, username string) {
	if s.ntfy == nil {
		return
	}
	if err := s.ntfy.deleteUser(ctx, username); err != nil {
		slog.Warn("ntfy delete user", "user", username, "err", err)
	}
}

// SetupAccount 新 TG 账号自动配一个随机 topic 的 ntfy 渠道，并给用户只读权限。
func (s *Service) SetupAccount(acc db.TgAccount) {
	var n int64
	s.db.Model(&db.Channel{}).Where("account_id = ? AND kind = 'ntfy'", acc.ID).Count(&n)
	if n > 0 {
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	ch := db.Channel{AccountID: acc.ID, Kind: "ntfy", Target: "tg-" + hex.EncodeToString(b), Enabled: true}
	if err := s.db.Create(&ch).Error; err != nil {
		slog.Warn("create ntfy channel", "err", err)
		return
	}
	s.GrantRead(acc.UserID, ch.Target)
}

// RevokeRead 回收用户对 topic 的授权（改 topic、删账号时用）。
func (s *Service) RevokeRead(userID uint, topic string) {
	if s.ntfy == nil || topic == "" {
		return
	}
	var u db.User
	if err := s.db.First(&u, userID).Error; err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.ntfy.revoke(ctx, u.Username, topic); err != nil {
		slog.Warn("ntfy revoke", "user", u.Username, "topic", topic, "err", err)
	}
}

// GrantRead 给账号所属用户授 topic 只读。
func (s *Service) GrantRead(userID uint, topic string) {
	if s.ntfy == nil {
		return
	}
	var u db.User
	if err := s.db.First(&u, userID).Error; err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.ntfy.grantRead(ctx, u.Username, topic); err != nil {
		slog.Warn("ntfy grant", "user", u.Username, "topic", topic, "err", err)
	}
}

// HandleMessage 命中监控的对话才落库并排队推送。
func (s *Service) HandleMessage(in tgclient.Incoming) {
	var peer db.TgPeer
	if err := s.db.Where("account_id = ? AND kind = ? AND peer_id = ? AND monitored = ?", in.AccountID, in.PeerKind, in.PeerID, true).Limit(1).Find(&peer).Error; err != nil {
		slog.Error("lookup peer", "account", in.AccountID, "err", err) // 查库失败不等于未监控
		return
	}
	if peer.ID == 0 {
		slog.Debug("message ignored: peer not monitored", "account", in.AccountID, "kind", in.PeerKind, "peer", in.PeerID)
		return
	}
	var chs []db.Channel
	s.db.Where("account_id = ? AND enabled = ?", in.AccountID, true).Find(&chs)

	// 库里只存元数据，正文进内存队列。
	// 每次尝试都用全新对象：GORM 会把回滚掉的自增 ID 写回结构体，复用会变成主键冲突；
	// 失败的尝试放进内存的正文当场清掉。
	err := retry(3, func() error {
		msg := db.Message{AccountID: in.AccountID, PeerID: peer.ID, TgMsgID: in.MsgID, Sender: in.Sender, SenderUser: in.SenderUser, SenderID: in.SenderID, SentAt: in.SentAt, ReceivedAt: time.Now()}
		var logs []db.PushLog
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&msg).Error; err != nil {
				return err
			}
			for _, ch := range chs {
				l := db.PushLog{MessageID: msg.ID, ChannelID: ch.ID, Status: "pending", NextAt: time.Now()}
				if err := tx.Create(&l).Error; err != nil {
					return err
				}
				logs = append(logs, l)
			}
			// 提交前就把正文放进内存，worker 看到 pending 时一定能取到
			s.tmu.Lock()
			for _, l := range logs {
				s.texts[l.ID] = in.Text
			}
			s.tmu.Unlock()
			return nil
		})
		if err != nil {
			for _, l := range logs {
				s.forget(l.ID)
			}
		}
		return err
	})
	if err != nil {
		slog.Error("save message", "err", err)
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run 推送 worker 池：定时或被唤醒后把到期的 pending 任务派给固定数量的 worker。
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	s.startWorkers(ctx, &wg)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// drain 把到期且未在处理中的任务派发出去；队列满时阻塞，慢任务只占自己那个 worker。
func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		var logs []db.PushLog
		s.db.Where("status = 'pending' AND next_at <= ?", time.Now()).Order("id").Limit(batch).Find(&logs)
		sent := 0
		for _, l := range logs {
			s.imu.Lock()
			dup := s.inflight[l.ID]
			if !dup {
				s.inflight[l.ID] = true
			}
			s.imu.Unlock()
			if dup {
				continue
			}
			select {
			case s.jobs <- l:
				sent++
			case <-ctx.Done():
				return
			}
		}
		if sent == 0 || len(logs) < batch {
			return
		}
	}
}

func (s *Service) startWorkers(ctx context.Context, wg *sync.WaitGroup) {
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case l := <-s.jobs:
					if s.deliver(ctx, l) {
						s.imu.Lock()
						delete(s.inflight, l.ID)
						s.imu.Unlock()
					}
					// 返回 false 表示状态没能写进库：保留 inflight，不再派发，免得突破退避 / 次数上限
				}
			}
		}()
	}
}

// drainSync 测试用：起临时 worker，派发并等全部处理完。
func (s *Service) drainSync(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	s.startWorkers(ctx, &wg)
	for {
		s.drain(ctx)
		s.imu.Lock()
		n := len(s.inflight)
		s.imu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	wg.Wait()
}

// deliver 处理一条推送任务。返回 false 表示任务状态没能写进库，调用方要保留 inflight。
func (s *Service) deliver(ctx context.Context, l db.PushLog) bool {
	// drain 的查询是快照，派发时任务可能已被上一个 worker 推完或改了退避：按库里当前状态复核
	var cur db.PushLog
	if s.db.First(&cur, l.ID).Error != nil || cur.Status != "pending" || cur.NextAt.After(time.Now()) {
		return true
	}
	l = cur

	var msg db.Message
	var ch db.Channel
	var peer db.TgPeer
	if s.db.First(&msg, l.MessageID).Error != nil || s.db.First(&ch, l.ChannelID).Error != nil {
		s.forget(l.ID)
		return s.setLog(&l, map[string]any{"status": "failed", "error": "消息或渠道已删除"}) == nil
	}
	if !ch.Enabled { // 退避期间用户关了渠道
		s.forget(l.ID)
		return s.setLog(&l, map[string]any{"status": "failed", "error": "渠道已停用"}) == nil
	}
	s.db.First(&peer, msg.PeerID)

	s.tmu.Lock()
	text, ok := s.texts[l.ID]
	s.tmu.Unlock()
	if !ok { // 正文只在内存，进程重启过就没了
		return s.setLog(&l, map[string]any{"status": "failed", "error": "进程重启，正文已丢弃"}) == nil
	}
	title := peer.Title
	body := text
	if msg.Sender != "" && msg.Sender != peer.Title {
		body = msg.Sender + ": " + body
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var err error
	switch ch.Kind {
	case "bark":
		err = bark.NewWithServers(ch.Target, s.serverFor(ch)).Send(ctx, title, fitBytes(body, pushBodyBytes))
	case "ntfy":
		err = s.ntfyOrAnon().publish(ctx, s.serverFor(ch), ch.Target, title, fitBytes(body, pushBodyBytes), "", s.replyActions(msg, peer, text))
	default:
		err = fmt.Errorf("未知渠道类型 %s", ch.Kind)
	}

	now := time.Now()
	if err == nil {
		// 已经发出去了，正文立即释放；状态写不进库就保留 inflight，绝不能重复发
		s.forget(l.ID)
		if err := s.setLog(&l, map[string]any{"status": "sent", "sent_at": now, "error": "", "attempts": l.Attempts + 1}); err != nil {
			slog.Error("mark sent", "log", l.ID, "err", err)
			return false
		}
		return true
	}
	if errors.Is(err, context.Canceled) { // 进程在退出，下次启动原样重发，不算失败
		return true
	}
	attempts := l.Attempts + 1
	upd := map[string]any{"attempts": attempts, "error": err.Error()}
	if attempts >= maxAttempts {
		upd["status"] = "failed"
		s.forget(l.ID)
	} else {
		upd["next_at"] = now.Add(min(30*time.Second<<attempts, time.Hour))
	}
	if err := s.setLog(&l, upd); err != nil {
		slog.Error("update push log", "log", l.ID, "err", err)
		return false
	}
	return true
}

// setLog 更新推送记录，SQLite 偶发失败重试。
func (s *Service) setLog(l *db.PushLog, upd map[string]any) error {
	return retry(3, func() error { return s.db.Model(l).Updates(upd).Error })
}

// retry 对 SQLite 偶发的 busy / 写失败重试几次。
func retry(n int, f func() error) error {
	var err error
	for i := 0; i < n; i++ {
		if err = f(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 50 * time.Millisecond)
	}
	return err
}

func (s *Service) forget(logID uint) {
	s.tmu.Lock()
	delete(s.texts, logID)
	s.tmu.Unlock()
}

// ForgetPending 删账号 / 删渠道前调用：把还没推完的任务正文从内存清掉，不然永远留着。
func (s *Service) ForgetPending(where string, args ...any) {
	var ids []uint
	s.db.Model(&db.PushLog{}).Where("status = 'pending'").Where(where, args...).Pluck("id", &ids)
	s.tmu.Lock()
	for _, id := range ids {
		delete(s.texts, id)
	}
	s.tmu.Unlock()
}

// serverFor 渠道的服务器地址，空则用平台默认。
func (s *Service) serverFor(ch db.Channel) string {
	if ch.Server != "" {
		return ch.Server
	}
	var p db.Provider
	s.db.Where("kind = ?", ch.Kind).First(&p)
	return p.BaseURL
}

func (s *Service) ntfyOrAnon() *ntfyClient {
	if s.ntfy != nil {
		return s.ntfy
	}
	return newNtfy("", "", "")
}

// replyActions 通知按钮：配置了 PUBLIC_URL 才有"回复"，有深链才有"打开 Telegram"。
// 点通知本身只展开，不跳转（用户要求）。
func (s *Service) replyActions(msg db.Message, peer db.TgPeer, text string) []action {
	link := reply.DeepLink(peer.Kind, peer.PeerID, msg.TgMsgID)
	var actions []action
	if s.cfg.PublicURL != "" {
		tok, err := reply.Sign(s.cfg.SessionSecret, reply.Payload{
			Account: msg.AccountID, PeerKind: peer.Kind, PeerID: peer.PeerID, AccessHash: peer.AccessHash, MsgID: msg.TgMsgID,
			Exp: time.Now().Add(reply.TTL).Unix(), PeerTitle: peer.Title,
			Sender: msg.Sender, SenderUser: msg.SenderUser, SenderID: msg.SenderID, Text: text,
		})
		if err == nil {
			actions = append(actions, action{Action: "view", Label: "回复", URL: strings.TrimRight(s.cfg.PublicURL, "/") + "/reply#" + tok})
		}
	}
	if link != "" {
		actions = append(actions, action{Action: "view", Label: "打开 Telegram", URL: link})
	}
	return actions
}

// fitBytes 推送正文按字节上限截断（ntfy 单条 4096 字节，APNs 类似），按字符边界切。
func fitBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - 3 // 给省略号留 3 字节
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
