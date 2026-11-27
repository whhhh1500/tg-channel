package push

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
)

func TestTestStrategy(t *testing.T) {
	urgentGap = 10 * time.Millisecond
	var mu sync.Mutex
	var bodies []map[string]any
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
	}))
	defer srv.Close()
	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	g.Create(&db.Provider{Kind: "ntfy", BaseURL: srv.URL})
	acc := db.TgAccount{UserID: 1}
	g.Create(&acc)
	s := New(&config.Config{NtfyURL: srv.URL}, g)
	g.First(&acc, acc.ID) // 取库里的默认值：紧急及以上发 3 遍

	if _, err := s.TestStrategy(context.Background(), acc, "紧急"); !errors.Is(err, ErrNoChannel) {
		t.Fatalf("没有渠道应报 ErrNoChannel: %v", err)
	}
	g.Create(&db.Channel{AccountID: acc.ID, Kind: "ntfy", Target: "t", Enabled: true})
	wait := func(want int) []map[string]any {
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			n := len(bodies)
			mu.Unlock()
			if n >= want || time.Now().After(deadline) {
				time.Sleep(50 * time.Millisecond) // 确认没有多发
				mu.Lock()
				defer mu.Unlock()
				got := bodies
				bodies = nil
				return got
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	n, err := s.TestStrategy(context.Background(), acc, "紧急")
	if n != 1 || err != nil {
		t.Fatalf("应发成功 1 个渠道: %d %v", n, err)
	}
	got := wait(3)
	if len(got) != 3 || got[0]["priority"] != float64(5) || !strings.Contains(got[0]["message"].(string), "发 3 遍") {
		t.Fatalf("默认策略应按紧急连发 3 遍: %v", got)
	}
	if _, ok := got[0]["actions"]; ok {
		t.Fatalf("测试通知不该带回复按钮: %v", got[0])
	}
	var cnt int64
	g.Model(&db.Message{}).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("测试不进消息记录: %d", cnt)
	}

	acc.RepeatFrom, acc.RepeatN = 4, 1
	s.TestStrategy(context.Background(), acc, "高")
	if got := wait(2); len(got) != 1 || got[0]["priority"] != float64(4) || !strings.Contains(got[0]["message"].(string), "不连发") {
		t.Fatalf("不连发只发 1 遍: %v", got)
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	acc.RepeatN = 3
	if n, err := s.TestStrategy(context.Background(), acc, "高"); n != 0 || err == nil || !strings.HasPrefix(err.Error(), "ntfy：") {
		t.Fatalf("渠道失败应返回错误且不补发: %d %v", n, err)
	}
}
