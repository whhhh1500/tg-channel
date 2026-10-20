package db

import "time"

type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;size:64"`
	PasswordHash string
	IsAdmin      bool
	SessionVer   int `gorm:"not null;default:0"` // 改密码时 +1，旧 cookie 全部失效
	CreatedAt    time.Time
}

// TgAccount 一个已扫码登录的 Telegram 账号。
type TgAccount struct {
	ID         uint `gorm:"primaryKey"`
	UserID     uint `gorm:"index"`
	TgUserID   int64
	Username   string
	FirstName  string
	SessionEnc []byte // gotd session，MASTER_KEY 加密
	Status     string `gorm:"size:16"` // online / offline / error
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// TgPeer 账号下的一个对话（用户 / 群 / 频道）。
type TgPeer struct {
	ID         uint   `gorm:"primaryKey"`
	AccountID  uint   `gorm:"uniqueIndex:idx_peer"`
	Kind       string `gorm:"uniqueIndex:idx_peer;size:8"` // user / chat / channel
	PeerID     int64  `gorm:"uniqueIndex:idx_peer"`
	AccessHash int64
	Username   string // @用户名，群可能没有
	Title      string
	Monitored  bool
	UpdatedAt  time.Time
}

// Channel 账号的一个推送渠道。
type Channel struct {
	ID        uint   `gorm:"primaryKey"`
	AccountID uint   `gorm:"index"`
	Kind      string `gorm:"size:16"` // ntfy / bark
	Server    string // 服务地址，空则用平台默认
	Target    string // ntfy 的 topic 或 bark 的 device key
	Enabled   bool
	CreatedAt time.Time
}

type Message struct {
	ID         uint `gorm:"primaryKey"`
	AccountID  uint `gorm:"index:idx_msg_acc_recv"`
	PeerID     uint `gorm:"index"`
	TgMsgID    int
	Sender     string // 发送者显示名
	SenderUser string // 发送者 @用户名
	SenderID   int64  // 发送者 TG 用户 id
	Text       string
	SentAt     time.Time
	ReceivedAt time.Time `gorm:"index:idx_msg_acc_recv"`
}

// PushLog 既是待推送队列（status=pending）也是推送记录。
type PushLog struct {
	ID        uint `gorm:"primaryKey"`
	MessageID uint `gorm:"index"`
	ChannelID uint
	Status    string    `gorm:"size:8;index:idx_push_due"` // pending / sent / failed
	NextAt    time.Time `gorm:"index:idx_push_due"`
	Attempts  int
	SentAt    *time.Time
	Error     string
}

// TgState / TgChannelState 是 gotd updates 的 pts 状态，按账号持久化，用于断线补洞。
type TgState struct {
	AccountID           uint `gorm:"primaryKey"`
	Pts, Qts, Date, Seq int
}

type TgChannelState struct {
	AccountID uint  `gorm:"primaryKey;autoIncrement:false"`
	ChannelID int64 `gorm:"primaryKey;autoIncrement:false"`
	Pts       int
}

// Provider 平台级渠道服务地址，仅 admin 管理。
type Provider struct {
	ID      uint   `gorm:"primaryKey"`
	Kind    string `gorm:"uniqueIndex;size:16"`
	BaseURL string
}
