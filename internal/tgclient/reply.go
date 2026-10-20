package tgclient

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
)

const replyInterval = 5 * time.Second // 每账号发送间隔，防风控

// Reply 用账号的常驻连接回复一条消息。只做"回复"，不主动发起会话。
func (m *Manager) Reply(ctx context.Context, accountID uint, kind string, peerID, accessHash int64, msgID int, text string) error {
	r := m.get(accountID)
	if r == nil {
		return errors.New("账号不在线")
	}
	select {
	case <-r.ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("账号连接中，稍后再试")
	}

	m.mu.Lock()
	if wait := replyInterval - time.Since(m.lastReply[accountID]); wait > 0 {
		m.mu.Unlock()
		return errors.New("发太快了，隔几秒再试")
	}
	m.lastReply[accountID] = time.Now()
	m.mu.Unlock()

	var peer tg.InputPeerClass
	switch kind {
	case "user":
		peer = &tg.InputPeerUser{UserID: peerID, AccessHash: accessHash}
	case "chat":
		peer = &tg.InputPeerChat{ChatID: peerID}
	case "channel":
		peer = &tg.InputPeerChannel{ChannelID: peerID, AccessHash: accessHash}
	default:
		return errors.New("未知对话类型")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := message.NewSender(r.client.API()).To(peer).Reply(msgID).Text(ctx, text)
	return err
}
