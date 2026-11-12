package tgclient

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"

	"tgchannel/internal/db"
)

const replyInterval = 5 * time.Second // 每账号发送 / 修改间隔，防风控

// target 取账号的在线连接和对话的 InputPeer，并按每账号间隔限流。
func (m *Manager) target(ctx context.Context, accountID uint, kind string, peerID, accessHash int64) (*tg.Client, tg.InputPeerClass, error) {
	api, err := m.online(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	accessHash = m.peerHash(ctx, api, accountID, kind, peerID, accessHash)
	var peer tg.InputPeerClass
	switch kind {
	case "user":
		peer = &tg.InputPeerUser{UserID: peerID, AccessHash: accessHash}
	case "chat":
		peer = &tg.InputPeerChat{ChatID: peerID}
	case "channel":
		peer = &tg.InputPeerChannel{ChannelID: peerID, AccessHash: accessHash}
	default:
		return nil, nil, errors.New("未知对话类型")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.lastReply[accountID]) < replyInterval {
		return nil, nil, errors.New("发太快了，隔几秒再试")
	}
	m.lastReply[accountID] = time.Now()
	return api, peer, nil
}

// peerHash 推送时对话的 access hash 还是 0（第一次见到时对方实体是 min / 缺失），带着 0 发会 PEER_ID_INVALID。
// 先取库里最新的；库里也没有就同步一次对话列表（拉到的是完整 hash）再取。普通群不需要 hash。
func (m *Manager) peerHash(ctx context.Context, api *tg.Client, accountID uint, kind string, peerID, h int64) int64 {
	if h != 0 || kind == "chat" {
		return h
	}
	load := func() int64 {
		var p db.TgPeer
		m.db.Select("access_hash").Where("account_id = ? AND kind = ? AND peer_id = ?", accountID, kind, peerID).Limit(1).Find(&p)
		return p.AccessHash
	}
	if h = load(); h != 0 || api == nil {
		return h
	}
	if _, err := syncPeers(ctx, m.db, accountID, api); err != nil {
		slog.Warn("sync peers for access hash", "account", accountID, "err", err)
		return 0
	}
	return load()
}

// Reply 用账号的常驻连接在对话里发一条消息，返回新消息 id。只在已收到消息的对话里回，不主动发起会话。
// quote 为 true 时引用 msgID 那条消息，否则直接发。
func (m *Manager) Reply(ctx context.Context, accountID uint, kind string, peerID, accessHash int64, msgID int, quote bool, text string) (int, error) {
	api, peer, err := m.target(ctx, accountID, kind, peerID, accessHash)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	to := message.NewSender(api).To(peer)
	var upd tg.UpdatesClass
	if quote {
		upd, err = to.Reply(msgID).Text(ctx, text)
	} else {
		upd, err = to.Text(ctx, text)
	}
	if err != nil {
		return 0, err
	}
	return sentID(upd), nil
}

// Edit 修改自己发出的一条消息。
func (m *Manager) Edit(ctx context.Context, accountID uint, kind string, peerID, accessHash int64, msgID int, text string) error {
	api, peer, err := m.target(ctx, accountID, kind, peerID, accessHash)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{Peer: peer, ID: msgID, Message: text})
	return err
}

// sentID 从发送结果里取新消息 id：私聊 / 普通群一般是 UpdateShortSentMessage，超级群 / 频道带 UpdateMessageID。
func sentID(upd tg.UpdatesClass) int {
	var list []tg.UpdateClass
	switch u := upd.(type) {
	case *tg.UpdateShortSentMessage:
		return u.ID
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	}
	for _, x := range list {
		if v, ok := x.(*tg.UpdateMessageID); ok {
			return v.ID
		}
	}
	return 0
}
