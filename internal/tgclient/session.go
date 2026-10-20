package tgclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/session"
	"gorm.io/gorm"

	"tgchannel/internal/db"
)

// ErrSessionGone 表示账号已删或 session 解不开（MASTER_KEY 变了），不该再重试连接。
var ErrSessionGone = errors.New("账号已删除或 session 无法解密")

// dbSession 把 gotd 的 session 加密后存到 tg_accounts.session_enc。
type dbSession struct {
	db        *gorm.DB
	masterKey string
	accountID uint
}

func (s *dbSession) LoadSession(_ context.Context) ([]byte, error) {
	var acc db.TgAccount
	if err := s.db.Select("id, session_enc").Where("id = ?", s.accountID).Limit(1).Find(&acc).Error; err != nil {
		return nil, err
	}
	if acc.ID == 0 {
		return nil, ErrSessionGone
	}
	if len(acc.SessionEnc) == 0 {
		return nil, session.ErrNotFound
	}
	data, err := decrypt(s.masterKey, acc.SessionEnc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSessionGone, err)
	}
	return data, nil
}

func (s *dbSession) StoreSession(_ context.Context, data []byte) error {
	enc, err := encrypt(s.masterKey, data)
	if err != nil {
		return err
	}
	return s.db.Model(&db.TgAccount{}).Where("id = ?", s.accountID).Update("session_enc", enc).Error
}
