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
	if m["remaining"].(float64) != 3 || len(m["history"].([]any)) != 0 {
		t.Fatalf("失败不应计次也不进历史: %v", m)
	}
	// 引用开关是可选参数
	if code, m := post("/reply/send", map[string]any{"token": tok, "text": "ok", "quote": true}); code != 503 {
		t.Fatalf("带引用发送（不在线）应 503: %d %v", code, m)
	}
	// 媒体预览：账号不在线时取不到，票据地址乱填应过期
	if code, m := post("/reply/media", map[string]string{"token": tok}); code != 503 {
		t.Fatalf("不在线预览应 503: %d %v", code, m)
	}
	if resp, _ := http.Get(srv.URL + "/tg/reply/file/0123456789abcdef0123456789abcdef"); resp.StatusCode != 410 {
		t.Fatalf("无效票据应 410: %d", resp.StatusCode)
	}
	pm := p
	pm.Media, pm.Preview, pm.Text = "[图片 1280×960 12K]", "image", "配文"
	mtok, _ := reply.Sign("sec", pm)
	if _, m := post("/reply/open", map[string]string{"token": mtok}); m["media"] != "[图片 1280×960 12K]" || m["preview"] != "image" || m["text"] != "配文" {
		t.Fatalf("open 应带媒体说明: %v", m)
	}
	// 只能改这个链接发出的回复
	if code, m := post("/reply/edit", map[string]any{"token": tok, "id": 123, "text": "改"}); code != 404 {
		t.Fatalf("改不是本链接发出的消息应 404: %d %v", code, m)
	}
	if code, m := post("/reply/edit", map[string]any{"token": tok, "id": 123, "text": " "}); code != 400 {
		t.Fatalf("改成空内容应 400: %d %v", code, m)
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

	// 回复历史：按链接隔离，修改只改自己的
	u.add("k", sentMsg{ID: 7, Text: "第一条", At: "10:00:00"})
	u.add("other", sentMsg{ID: 8, Text: "没预留过的链接不记"})
	if n, h := u.get("k"); n != 3 || len(h) != 1 || h[0].Text != "第一条" {
		t.Fatalf("history = %d %v", n, h)
	}
	if _, ok := u.text("k", 8); ok {
		t.Fatal("别的链接的消息不能改")
	}
	u.setText("k", 7, "改过")
	if txt, ok := u.text("k", 7); !ok || txt != "改过" {
		t.Fatalf("修改后 = %q %v", txt, ok)
	}
	_, h := u.get("k")
	h[0].Text = "外部改副本"
	if txt, _ := u.text("k", 7); txt != "改过" {
		t.Fatal("get 应返回副本")
	}
}
