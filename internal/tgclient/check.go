package tgclient

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"gorm.io/gorm"

	"tgchannel/internal/config"
)

// readOnlySession 只读取不回写，诊断时不能覆盖库里的 session。
type readOnlySession struct{ inner *dbSession }

func (r readOnlySession) LoadSession(ctx context.Context) ([]byte, error) {
	return r.inner.LoadSession(ctx)
}
func (readOnlySession) StoreSession(context.Context, []byte) error { return nil }

// CheckSession 用库里存的 session 连一次 Telegram，返回诊断信息。供 cmd/tgcheck 使用。
func CheckSession(ctx context.Context, cfg *config.Config, g *gorm.DB, accountID uint) (string, error) {
	st := readOnlySession{&dbSession{db: g, masterKey: cfg.MasterKey, accountID: accountID}}
	raw, err := st.LoadSession(ctx)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}
	var sd struct {
		Version int
		Data    session.Data
	}
	_ = json.Unmarshal(raw, &sd)
	info := fmt.Sprintf("session bytes=%d version=%d dc=%d addr=%s keyid=%x keylen=%d salt=%d\n",
		len(raw), sd.Version, sd.Data.DC, sd.Data.Addr, sd.Data.AuthKeyID, len(sd.Data.AuthKey), sd.Data.Salt)

	m := &Manager{cfg: cfg}
	client := m.newClient(st, tg.NewUpdateDispatcher())
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		info += fmt.Sprintf("authorized=%v\n", status.Authorized)
		if !status.Authorized {
			return nil
		}
		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("self: %w", err)
		}
		info += fmt.Sprintf("self id=%d name=%s @%s\n", self.ID, userName(self), self.Username)
		return nil
	})
	return info, err
}
