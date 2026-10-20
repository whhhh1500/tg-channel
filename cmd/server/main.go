// 入口：加载配置、打开数据库、种子 admin、拉起 TG 账号和推送 worker、启动 HTTP。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/push"
	"tgchannel/internal/tgclient"
	"tgchannel/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	g, err := db.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	if err := seed(cfg, g); err != nil {
		return err
	}
	if cfg.Pprof {
		go func() { slog.Info("pprof on 127.0.0.1:6060"); _ = http.ListenAndServe("127.0.0.1:6060", nil) }()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	pusher := push.New(cfg, g)
	tgm := tgclient.NewManager(ctx, cfg, g)
	tgm.OnMessage = pusher.HandleMessage
	tgm.OnAccountAdded = pusher.SetupAccount
	tgm.StartAll()
	wg.Add(2)
	go func() { defer wg.Done(); pusher.Run(ctx) }()
	go func() { defer wg.Done(); pusher.RunCleanup(ctx) }()
	go func() { // 周期打点，方便看泄漏
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
				slog.Info("stats", "goroutines", runtime.NumGoroutine(), "tg_online", tgm.Online())
			}
		}
	}()

	srv := &http.Server{
		Addr: cfg.Addr, Handler: web.New(cfg, g, tgm, pusher),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("listening", "addr", cfg.Addr, "base", cfg.BasePath)
	err = srv.ListenAndServe()
	stop() // 端口被占等启动失败时也要先取消后台任务，否则下面会一直等
	tgm.Close()
	wg.Wait() // 等 HTTP 排空和推送 worker 退出，别把进行中的推送记成失败
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// seed 首次启动创建 admin 和平台默认渠道地址。
func seed(cfg *config.Config, g *gorm.DB) error {
	if cfg.AdminPassword != "" {
		var n int64
		g.Model(&db.User{}).Where("username = ?", cfg.AdminUser).Count(&n)
		if n == 0 {
			if _, err := web.CreateUser(g, cfg.AdminUser, cfg.AdminPassword, true); err != nil {
				return err
			}
			slog.Info("admin created", "user", cfg.AdminUser)
		}
	}
	for kind, url := range map[string]string{"ntfy": cfg.NtfyURL, "bark": cfg.BarkURL} {
		p := db.Provider{Kind: kind, BaseURL: url}
		g.Where("kind = ?", kind).FirstOrCreate(&p)
	}
	return nil
}
