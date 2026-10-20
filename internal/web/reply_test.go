package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/push"
	"tgchannel/internal/reply"
	"tgchannel/internal/tgclient"
)

func TestReplyEndpoints(t *testing.T) {
	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	cfg := &config.Config{BasePath: "/tg", SessionSecret: "sec"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(New(cfg, g, tgclient.NewManager(ctx, cfg, g), push.New(cfg, g)))
	defer srv.Close()

	post := func(p string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/tg"+p, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	resp, _ := http.Get(srv.URL + "/tg/reply")
	if resp.StatusCode != 200 {
		t.Fatalf("回复页应不需登录: %d", resp.StatusCode)
	}
	if code, _ := post("/reply/open", map[string]string{"token": "junk"}); code != 403 {
		t.Fatalf("坏 token 应 403: %d", code)
	}
	if code, _ := post("/reply/open", map[string]string{"token": string(make([]byte, 200<<10))}); code != 413 {
		t.Fatalf("超大 body 应 413: %d", code)
	}
	p := reply.Payload{Account: 1, PeerKind: "chat", PeerID: 5, MsgID: 9, Exp: time.Now().Add(-time.Second).Unix(), PeerTitle: "群", Sender: "Bob", Text: "hi"}
	tok, _ := reply.Sign("sec", p)
	if code, _ := post("/reply/open", map[string]string{"token": tok}); code != 410 {
		t.Fatalf("过期应 410: %d", code)
	}
	p.Exp = time.Now().Add(time.Hour).Unix()
	tok, _ = reply.Sign("sec", p)
	code, m := post("/reply/open", map[string]string{"token": tok})
	if code != 200 || m["sender"] != "Bob" || m["peer"] != "群" || m["remaining"].(float64) != 3 {
		t.Fatalf("open = %d %v", code, m)
	}
	if code, m := post("/reply/send", map[string]string{"token": tok, "text": ""}); code != 400 {
		t.Fatalf("空内容应 400: %d %v", code, m)
	}
	// 账号不在线：发送失败，不计次
	code, m = post("/reply/send", map[string]string{"token": tok, "text": "ok"})
	if code != 503 {
		t.Fatalf("不在线应 503: %d %v", code, m)
	}
	_, m = post("/reply/open", map[string]string{"token": tok})
	if m["remaining"].(float64) != 3 {
		t.Fatalf("失败不应计次: %v", m)
	}

	// 次数预留 / 释放
	u := newUses()
	exp := time.Now().Add(time.Minute)
	for i := 1; i <= 3; i++ {
		if n, ok := u.reserve("k", exp, 3); !ok || n != i {
			t.Fatalf("第 %d 次预留 = %d %v", i, n, ok)
		}
	}
	if _, ok := u.reserve("k", exp, 3); ok {
		t.Fatal("第 4 次应被拒")
	}
	u.release("k")
	if _, ok := u.reserve("k", exp, 3); !ok {
		t.Fatal("释放后应能再预留")
	}
}
