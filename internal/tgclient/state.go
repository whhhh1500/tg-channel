package tgclient

import (
	"context"
	"fmt"

	"github.com/gotd/td/telegram/updates"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"tgchannel/internal/db"
)

// dbState 把 gotd 的 updates 状态（pts/qts/date/seq 和各频道 pts）按账号存库，
// 这样重连和重启后能 getDifference 补回断线期间的消息。
// 接口里的 userID 忽略，统一按平台账号 ID 存。
type dbState struct {
	db        *gorm.DB
	accountID uint
}

var _ updates.StateStorage = (*dbState)(nil)

func (s *dbState) GetState(_ context.Context, _ int64) (updates.State, bool, error) {
	var st db.TgState
	err := s.db.Where("account_id = ?", s.accountID).Limit(1).Find(&st).Error
	if err != nil || st.AccountID == 0 {
		return updates.State{}, false, err
	}
	return updates.State{Pts: st.Pts, Qts: st.Qts, Date: st.Date, Seq: st.Seq}, true, nil
}

func (s *dbState) SetState(_ context.Context, _ int64, st updates.State) error {
	row := db.TgState{AccountID: s.accountID, Pts: st.Pts, Qts: st.Qts, Date: st.Date, Seq: st.Seq}
	return s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func (s *dbState) set(cols map[string]any) error {
	return s.db.Model(&db.TgState{}).Where("account_id = ?", s.accountID).Updates(cols).Error
}

func (s *dbState) SetPts(_ context.Context, _ int64, pts int) error {
	return s.set(map[string]any{"pts": pts})
}
func (s *dbState) SetQts(_ context.Context, _ int64, qts int) error {
	return s.set(map[string]any{"qts": qts})
}
func (s *dbState) SetDate(_ context.Context, _ int64, date int) error {
	return s.set(map[string]any{"date": date})
}
func (s *dbState) SetSeq(_ context.Context, _ int64, seq int) error {
	return s.set(map[string]any{"seq": seq})
}
func (s *dbState) SetDateSeq(_ context.Context, _ int64, date, seq int) error {
	return s.set(map[string]any{"date": date, "seq": seq})
}

func (s *dbState) GetChannelPts(_ context.Context, _, channelID int64) (int, bool, error) {
	var row db.TgChannelState
	err := s.db.Where("account_id = ? AND channel_id = ?", s.accountID, channelID).Limit(1).Find(&row).Error
	if err != nil || row.AccountID == 0 {
		return 0, false, err
	}
	return row.Pts, true, nil
}

func (s *dbState) SetChannelPts(_ context.Context, _, channelID int64, pts int) error {
	row := db.TgChannelState{AccountID: s.accountID, ChannelID: channelID, Pts: pts}
	return s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

func (s *dbState) ForEachChannels(ctx context.Context, _ int64, f func(ctx context.Context, channelID int64, pts int) error) error {
	var rows []db.TgChannelState
	if err := s.db.Where("account_id = ?", s.accountID).Find(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		if err := f(ctx, r.ChannelID, r.Pts); err != nil {
			return err
		}
	}
	return nil
}

// ---- ChannelAccessHasher：频道 access hash 直接用 tg_peers 里同步到的，
// 否则 gotd 对未跟踪的频道拿不到 hash 会把更新丢掉。

var _ updates.ChannelAccessHasher = (*dbState)(nil)

func (s *dbState) GetChannelAccessHash(_ context.Context, _, channelID int64) (int64, bool, error) {
	var p db.TgPeer
	err := s.db.Where("account_id = ? AND kind = 'channel' AND peer_id = ?", s.accountID, channelID).Limit(1).Find(&p).Error
	if err != nil || p.ID == 0 || p.AccessHash == 0 {
		return 0, false, err
	}
	return p.AccessHash, true, nil
}

func (s *dbState) SetChannelAccessHash(_ context.Context, _, channelID, accessHash int64) error {
	// 只改 access_hash 这一列，不整行 Save，免得覆盖用户并发切换的 monitored
	res := s.db.Model(&db.TgPeer{}).Where("account_id = ? AND kind = 'channel' AND peer_id = ?", s.accountID, channelID).Update("access_hash", accessHash)
	if res.Error != nil || res.RowsAffected > 0 {
		return res.Error
	}
	// 新加入的群，先占位，刷新对话后补标题
	return s.db.Create(&db.TgPeer{AccountID: s.accountID, Kind: "channel", PeerID: channelID, AccessHash: accessHash, Title: fmt.Sprintf("channel %d", channelID)}).Error
}
