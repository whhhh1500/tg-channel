// tgcheck 诊断工具：拿库里某个账号存的 session 连一次 Telegram，看是否还有效。
// 用法：MASTER_KEY=... TG_API_ID=... TG_API_HASH=... go run ./cmd/tgcheck -db data/app.db -account 1
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/tgclient"
)

func main() {
	dbPath := flag.String("db", "data/app.db", "SQLite 路径")
	account := flag.Uint("account", 1, "tg_accounts.id")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	g, err := db.OpenFile(*dbPath)
	if err != nil {
		fmt.Println("db:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := tgclient.CheckSession(ctx, cfg, g, uint(*account))
	fmt.Println(info)
	if err != nil {
		fmt.Println("ERROR:", err)
		os.Exit(1)
	}
}
