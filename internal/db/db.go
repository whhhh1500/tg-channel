// Package db 打开 SQLite 并迁移表结构。
package db

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func Open(dataDir string) (*gorm.DB, error) {
	return OpenFile(filepath.Join(dataDir, "app.db"))
}

func OpenFile(path string) (*gorm.DB, error) {
	// secure_delete：删除 / 覆盖的内容立即清零，不在空闲页或 WAL 里残留；
	// journal_size_limit：checkpoint 后 WAL 文件截回 4MB 以内；
	// synchronous=NORMAL：WAL 下仍保证一致性，只是断电可能丢最后几次提交，而 gotd 的 pts 丢了会自动补回，
	// 换来的是每条 Telegram 更新不再各 fsync 一次。
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)&_pragma=journal_size_limit(4194304)&_pragma=synchronous(NORMAL)"
	lg := logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{LogLevel: logger.Error, IgnoreRecordNotFoundError: true})
	g, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: lg, TranslateError: true})
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB, err := g.DB()
	if err != nil {
		return nil, err
	}
	// SQLite 只有一个写者，单连接最省事也不会 "database is locked"。
	sqlDB.SetMaxOpenConns(1)

	if err := g.AutoMigrate(&User{}, &TgAccount{}, &TgPeer{}, &Channel{}, &Message{}, &PushLog{}, &Provider{}, &TgState{}, &TgChannelState{}); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// 旧库升级：新加的列对已有行是 NULL，回填成 0
	g.Exec("UPDATE users SET session_ver = 0 WHERE session_ver IS NULL")
	// ntfy topic 全服务唯一（Bark 的 device key 同一用户多个账号可以共用，不加）。
	// 已有重复数据会建不出来，直接启动失败，让人先处理重复。
	if err := g.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_ntfy_topic ON channels(target) WHERE kind = 'ntfy'").Error; err != nil {
		return nil, fmt.Errorf("ntfy topic 唯一索引: %w（请先清理 channels 里重复的 topic）", err)
	}
	return g, nil
}
