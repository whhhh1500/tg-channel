package push

import (
	"context"
	"log/slog"
	"time"

	"tgchannel/internal/db"
)

// RunCleanup 每天按保留天数分批删除旧消息和推送记录，避免长事务锁表。
func (s *Service) RunCleanup(ctx context.Context) {
	if s.cfg.RetentionDays <= 0 {
		return
	}
	for {
		s.cleanupOnce()
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

func (s *Service) cleanupOnce() {
	cutoff := time.Now().AddDate(0, 0, -s.cfg.RetentionDays)
	total := int64(0)
	for {
		var ids []uint
		s.db.Model(&db.Message{}).Where("received_at < ?", cutoff).Order("id").Limit(1000).Pluck("id", &ids)
		if len(ids) == 0 {
			break
		}
		s.db.Where("message_id IN ?", ids).Delete(&db.PushLog{})
		res := s.db.Where("id IN ?", ids).Delete(&db.Message{})
		total += res.RowsAffected
		if res.Error != nil {
			slog.Warn("cleanup", "err", res.Error)
			break
		}
	}
	if total > 0 {
		slog.Info("cleanup done", "deleted", total)
	}
	// 把 WAL 合并回主文件并截断；只有真删了东西或空闲页超过两成才 VACUUM，别白占唯一连接。
	s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	var free, pages int64
	s.db.Raw("PRAGMA freelist_count").Scan(&free)
	s.db.Raw("PRAGMA page_count").Scan(&pages)
	if total > 0 || (pages > 0 && free*5 > pages) {
		if err := s.db.Exec("VACUUM").Error; err != nil {
			slog.Warn("vacuum", "err", err)
		}
	}
}
