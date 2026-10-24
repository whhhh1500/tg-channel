package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"tgchannel/internal/db"
)

// SyncPeers 用在线客户端拉对话列表写入 tg_peers，保留已有的监控开关。
func (m *Manager) SyncPeers(ctx context.Context, accountID uint) (int, error) {
	r := m.get(accountID)
	if r == nil {
		return 0, errors.New("账号不在线")
	}
	select {
	case <-r.ready:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(15 * time.Second):
		return 0, errors.New("账号连接中，稍后再试")
	}
	return syncPeers(ctx, m.db, accountID, r.client.API())
}

// syncPeers 拉完全部对话后在一个事务里写入：原子，且只有一次 fsync。
func syncPeers(ctx context.Context, g *gorm.DB, accountID uint, api *tg.Client) (int, error) {
	it := query.NewQuery(api).Dialogs().GetDialogs().BatchSize(100).Iter()
	var peers []db.TgPeer
	for it.Next(ctx) {
		e := it.Value()
		p := db.TgPeer{AccountID: accountID}
		switch v := e.Peer.(type) {
		case *tg.InputPeerUser:
			p.Kind, p.PeerID, p.AccessHash = "user", v.UserID, v.AccessHash
			if u, ok := e.Entities.User(v.UserID); ok {
				p.Title, p.Username = userName(u), u.Username
			}
		case *tg.InputPeerChat:
			p.Kind, p.PeerID = "chat", v.ChatID
			if c, ok := e.Entities.Chat(v.ChatID); ok {
				p.Title = c.Title
			}
		case *tg.InputPeerChannel:
			p.Kind, p.PeerID, p.AccessHash = "channel", v.ChannelID, v.AccessHash
			if c, ok := e.Entities.Channel(v.ChannelID); ok {
				p.Title, p.Username = c.Title, c.Username
			}
		default:
			continue
		}
		if p.Title == "" {
			p.Title = fmt.Sprintf("%s %d", p.Kind, p.PeerID)
		}
		p.UpdatedAt = time.Now()
		peers = append(peers, p)
	}
	if err := it.Err(); err != nil {
		return 0, err
	}
	if len(peers) == 0 {
		return 0, nil
	}
	// 同一对话在列表里可能出现两次（置顶 / 文件夹），按复合键去重
	type key struct {
		kind string
		id   int64
	}
	seen := map[key]bool{}
	uniq := peers[:0]
	for _, p := range peers {
		k := key{p.Kind, p.PeerID}
		if !seen[k] {
			seen[k] = true
			uniq = append(uniq, p)
		}
	}
	peers = uniq
	// 每 200 条一条 upsert，GORM 把各批包在同一事务里：冲突只更新标题、用户名、access hash，监控开关保留
	err := g.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}, {Name: "kind"}, {Name: "peer_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"access_hash", "title", "username", "updated_at"}),
	}).CreateInBatches(peers, 200).Error
	return len(peers), err
}

// ensurePeer 收到陌生对话的消息时补进 tg_peers（默认不监控），新会话不用手动同步就出现在列表里；
// 已有的占位标题（如 "channel 123"）顺手补成真名。已知对话只多一次主键索引查询。
func (m *Manager) ensurePeer(accountID uint, e tg.Entities, peer tg.PeerClass) {
	p := db.TgPeer{AccountID: accountID}
	switch v := peer.(type) {
	case *tg.PeerUser:
		p.Kind, p.PeerID = "user", v.UserID
		if u := e.Users[v.UserID]; u != nil {
			p.Title, p.Username = userName(u), u.Username
			if !u.Min {
				p.AccessHash = u.AccessHash
			}
		}
	case *tg.PeerChat:
		p.Kind, p.PeerID = "chat", v.ChatID
		if c := e.Chats[v.ChatID]; c != nil {
			p.Title = c.Title
		}
	case *tg.PeerChannel:
		p.Kind, p.PeerID = "channel", v.ChannelID
		if c := e.Channels[v.ChannelID]; c != nil {
			p.Title, p.Username = c.Title, c.Username
			if !c.Min {
				p.AccessHash = c.AccessHash
			}
		}
	default:
		return
	}
	placeholder := fmt.Sprintf("%s %d", p.Kind, p.PeerID)
	var old db.TgPeer
	if err := m.db.Select("id", "title").Where("account_id = ? AND kind = ? AND peer_id = ?", accountID, p.Kind, p.PeerID).Limit(1).Find(&old).Error; err != nil {
		slog.Warn("lookup peer", "account", accountID, "err", err)
		return
	}
	if old.ID != 0 {
		if old.Title == placeholder && p.Title != "" {
			m.db.Model(&db.TgPeer{}).Where("id = ?", old.ID).Updates(map[string]any{"title": p.Title, "username": p.Username})
		}
		return
	}
	if p.Title == "" {
		p.Title = placeholder
	}
	p.UpdatedAt = time.Now()
	// 和同步并发时以先写入的为准
	if err := m.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&p).Error; err != nil {
		slog.Warn("add peer", "account", accountID, "err", err)
		return
	}
	slog.Info("new peer", "account", accountID, "kind", p.Kind)
}
