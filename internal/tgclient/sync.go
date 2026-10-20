package tgclient

import (
	"context"
	"errors"
	"fmt"
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
