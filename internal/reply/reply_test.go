package reply

import (
	"strings"
	"testing"
	"time"
)

func TestSignParse(t *testing.T) {
	p := Payload{Account: 1, PeerKind: "channel", PeerID: 42, AccessHash: 7, MsgID: 9, Exp: time.Now().Add(time.Hour).Unix(),
		PeerTitle: "群", Sender: "Bob", SenderUser: "bob", SenderID: 5, Text: strings.Repeat("长", 1500)}
	tok, err := Sign("s", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) > 1200 {
		t.Fatalf("token 太长: %d", len(tok))
	}
	got, err := Parse("s", tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account != 1 || got.PeerID != 42 || got.MsgID != 9 || got.Sender != "Bob" || !strings.HasSuffix(got.Text, "…") || len([]rune(got.Text)) != PreviewRunes+1 {
		t.Fatalf("parsed = %+v", got)
	}
	if _, err := Parse("other", tok); err != ErrBadToken {
		t.Fatalf("错密钥应拒绝: %v", err)
	}
	if _, err := Parse("s", tok[:len(tok)-2]+"xx"); err != ErrBadToken {
		t.Fatalf("改签名应拒绝: %v", err)
	}
	p.Exp = time.Now().Add(-time.Minute).Unix()
	tok, _ = Sign("s", p)
	if _, err := Parse("s", tok); err != ErrExpired {
		t.Fatalf("过期应拒绝: %v", err)
	}
	if Key(tok) == "" || Key(tok) == tok {
		t.Fatal("Key 应为签名部分")
	}
	// 不可压缩的 4 字节生僻字：1000 字会超长，应自动降到更短的预览
	rnd := make([]rune, 1000)
	for i := range rnd {
		rnd[i] = rune(0x20000 + (i*7919)%40000)
	}
	p.Exp = time.Now().Add(time.Hour).Unix()
	p.Text = string(rnd)
	tok, _ = Sign("s", p)
	got, _ = Parse("s", tok)
	if len(tok) > MaxTokenLen || len([]rune(got.Text)) > 501 {
		t.Fatalf("随机文本 token=%d 字符，预览 %d 字", len(tok), len([]rune(got.Text)))
	}
	if DeepLink("user", 5, 0) != "tg://user?id=5" || DeepLink("channel", 42, 9) != "https://t.me/c/42/9" || DeepLink("chat", 1, 1) != "" {
		t.Fatal("deep link")
	}
}
