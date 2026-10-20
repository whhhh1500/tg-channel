// Package config 从环境变量和 .env 读取配置。
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

type Config struct {
	Addr           string   `env:"ADDR" envDefault:":8080"`
	BasePath       string   `env:"BASE_PATH"`                                  // 挂在子路径下时填，如 /tg
	PublicURL      string   `env:"PUBLIC_URL"`                                 // 对外完整地址，如 https://example.com/tg；填了通知上才有"回复"按钮
	TrustedProxies []string `env:"TRUSTED_PROXIES" envDefault:"127.0.0.1,::1"` // 只信这些反代传来的 X-Forwarded-For，限速按真实来源 IP
	DataDir        string   `env:"DATA_DIR" envDefault:"./data"`
	Pprof          bool     `env:"PPROF" envDefault:"false"`
	LogLevel       string   `env:"LOG_LEVEL" envDefault:"info"` // debug 会打印每条收到的消息

	SessionSecret string `env:"SESSION_SECRET,required"`
	MasterKey     string `env:"MASTER_KEY,required"` // 加密 TG session 的主密钥
	AdminUser     string `env:"ADMIN_USER" envDefault:"admin"`
	AdminPassword string `env:"ADMIN_PASSWORD"` // 为空则不创建 admin

	TgAPIID   int    `env:"TG_API_ID"`
	TgAPIHash string `env:"TG_API_HASH"`

	NtfyURL      string `env:"NTFY_URL" envDefault:"https://ntfy.sh"`
	NtfyUser     string `env:"NTFY_USER"`
	NtfyPassword string `env:"NTFY_PASSWORD"`
	BarkURL      string `env:"BARK_URL" envDefault:"https://api.day.app"`

	RetentionDays int `env:"RETENTION_DAYS" envDefault:"7"` // 只保留元数据，正文不落库
}

func Load() (*Config, error) {
	_ = godotenv.Load() // 没有 .env 也没关系
	var c Config
	if err := env.Parse(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}
