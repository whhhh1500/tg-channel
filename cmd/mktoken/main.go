// mktoken 调试工具：按 .env 的 SESSION_SECRET 生成一个回复链接 token，用来本地打开 /reply 页面。
//
//	go run ./cmd/mktoken -account 1 -kind chat -peer 100 -msg 1 -title 测试群 -sender Bob -text "你好"
//
// 打开 http://127.0.0.1:8080/tg/reply#<输出的 token>
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"tgchannel/internal/config"
	"tgchannel/internal/reply"
)

func main() {
	p := reply.Payload{}
	flag.UintVar(&p.Account, "account", 0, "tg_accounts.id")
	flag.StringVar(&p.PeerKind, "kind", "chat", "对话类型 user / chat / channel")
	flag.Int64Var(&p.PeerID, "peer", 0, "对话的 Telegram id")
	flag.Int64Var(&p.AccessHash, "hash", 0, "对话的 access hash（user / channel 需要）")
	flag.IntVar(&p.MsgID, "msg", 0, "要回复的消息 id")
	flag.StringVar(&p.PeerTitle, "title", "", "对话名称")
	flag.StringVar(&p.Sender, "sender", "", "发送者名字")
	flag.StringVar(&p.SenderUser, "sender-user", "", "发送者 @用户名")
	flag.Int64Var(&p.SenderID, "sender-id", 0, "发送者 Telegram id")
	flag.StringVar(&p.Text, "text", "", "消息正文")
	ttl := flag.Duration("ttl", reply.TTL, "有效期")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	p.Exp = time.Now().Add(*ttl).Unix()
	tok, err := reply.Sign(cfg.SessionSecret, p)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
