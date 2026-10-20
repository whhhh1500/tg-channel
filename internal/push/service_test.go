package push

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/tgclient"
)

type hit struct {
	path, title, body string
	actions           int
}

func TestHandleAndDeliver(t *testing.T) {
	var mu sync.Mutex
	var hits []hit
	fail := true // 第一次 bark 请求失败，验证退避
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/push" && fail {
			fail = false
			http.Error(w, "boom", 500)
			return
		}
		if r.URL.Path == "/" { // ntfy JSON 发布
			var m struct {
				Topic, Title, Message, Click string
				Actions                      []map[string]any
			}
			_ = json.Unmarshal(b, &m)
			if m.Click != "" {
				t.Errorf("通知不应带点击跳转: %s", m.Click)
			}
			hits = append(hits, hit{"/" + m.Topic, m.Title, m.Message, len(m.Actions)})
		} else {
			hits = append(hits, hit{r.URL.Path, "", string(b), 0})
		}
		w.Write([]byte(`{"code":200}`))
	}))
	defer srv.Close()

	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	g.Create(&db.Provider{Kind: "ntfy", BaseURL: srv.URL})
	g.Create(&db.Provider{Kind: "bark", BaseURL: srv.URL})
	acc := db.TgAccount{UserID: 1}
	g.Create(&acc)
	g.Create(&db.TgPeer{AccountID: acc.ID, Kind: "chat", PeerID: 42, Title: "测试群", Monitored: true})
	g.Create(&db.TgPeer{AccountID: acc.ID, Kind: "chat", PeerID: 43, Title: "未监控", Monitored: false})
	g.Create(&db.Channel{AccountID: acc.ID, Kind: "ntfy", Target: "topic1", Enabled: true})
	g.Create(&db.Channel{AccountID: acc.ID, Kind: "bark", Target: "devkey", Enabled: true})
	g.Create(&db.Channel{AccountID: acc.ID, Kind: "ntfy", Target: "off", Enabled: false})

	s := New(&config.Config{NtfyURL: srv.URL, PublicURL: "https://x/tg", SessionSecret: "sec"}, g)
	s.HandleMessage(tgclient.Incoming{AccountID: acc.ID, PeerKind: "chat", PeerID: 43, Text: "ignored", SentAt: time.Now()})
	s.HandleMessage(tgclient.Incoming{AccountID: acc.ID, PeerKind: "chat", PeerID: 42, Sender: "Bob", Text: "hello", SentAt: time.Now()})

	var n int64
	g.Model(&db.Message{}).Count(&n)
	if n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
	var stored db.Message
	g.First(&stored)
	if stored.Text != "" {
		t.Fatalf("正文不该落库: %q", stored.Text)
	}
	g.Model(&db.PushLog{}).Where("status = 'pending'").Count(&n)
	if n != 2 {
		t.Fatalf("pending push logs = %d, want 2", n)
	}

	s.drainSync(context.Background())
	mu.Lock()
	got := len(hits)
	mu.Unlock()
	if got != 1 {
		t.Fatalf("after first drain hits = %d, want 1 (ntfy ok, bark failed once)", got)
	}
	if hits[0].path != "/topic1" || hits[0].title != "测试群" || hits[0].body != "Bob: hello" || hits[0].actions != 1 {
		t.Fatalf("ntfy hit = %+v（chat 类型无深链，只有回复按钮）", hits[0])
	}

	var failed db.PushLog
	g.Where("status = 'pending' AND attempts = 1").First(&failed)
	if failed.ID == 0 || !failed.NextAt.After(time.Now()) {
		t.Fatalf("bark log should be pending with backoff: %+v", failed)
	}
	g.Model(&failed).Update("next_at", time.Now().Add(-time.Second)) // 模拟到期
	s.drainSync(context.Background())
	g.Model(&db.PushLog{}).Where("status = 'sent'").Count(&n)
	if n != 2 {
		t.Fatalf("sent = %d, want 2", n)
	}
	if len(s.texts) != 0 {
		t.Fatalf("推完内存正文应释放: %d", len(s.texts))
	}
	// 删渠道 / 删账号前清理 pending 正文
	s.HandleMessage(tgclient.Incoming{AccountID: acc.ID, PeerKind: "chat", PeerID: 42, Text: "pending", SentAt: time.Now()})
	if len(s.texts) != 2 {
		t.Fatalf("应有 2 条待推正文: %d", len(s.texts))
	}
	s.ForgetPending("message_id IN (SELECT id FROM messages WHERE account_id = ?)", acc.ID)
	if len(s.texts) != 0 {
		t.Fatalf("ForgetPending 后应为空: %d", len(s.texts))
	}
	g.Where("status = 'pending'").Delete(&db.PushLog{})
	// 退避期间渠道被停用：到期后不再发送
	g.Model(&db.Channel{}).Where("kind = 'bark'").Update("enabled", false)
	s.HandleMessage(tgclient.Incoming{AccountID: acc.ID, PeerKind: "chat", PeerID: 42, Text: "late", SentAt: time.Now()})
	var late db.PushLog
	g.Order("id desc").First(&late)
	g.Model(&db.Channel{}).Where("id = ?", late.ChannelID).Update("enabled", false)
	s.drainSync(context.Background())
	g.First(&late, late.ID)
	if late.Status != "failed" || !strings.Contains(late.Error, "停用") {
		t.Fatalf("停用渠道的任务应标失败: %+v", late)
	}
	g.Model(&db.Channel{}).Where("id = ?", late.ChannelID).Update("enabled", true)
	// 模拟重启：库里有 pending 任务但内存没有正文
	g.Create(&db.PushLog{MessageID: 1, ChannelID: 1, Status: "pending", NextAt: time.Now()})
	s2 := New(&config.Config{NtfyURL: srv.URL}, g)
	s2.drainSync(context.Background())
	var orphan db.PushLog
	g.Order("id desc").First(&orphan)
	if orphan.Status != "failed" || !strings.Contains(orphan.Error, "重启") {
		t.Fatalf("重启残留任务应标失败: %+v", orphan)
	}

	// 保留清理
	s.cfg.RetentionDays = 1
	g.Model(&db.Message{}).Where("1=1").Update("received_at", time.Now().AddDate(0, 0, -2))
	s.cleanupOnce()
	g.Model(&db.Message{}).Count(&n)
	if n != 0 {
		t.Fatalf("messages after cleanup = %d", n)
	}
	g.Model(&db.PushLog{}).Count(&n)
	if n != 0 {
		t.Fatalf("push logs after cleanup = %d", n)
	}
}

func TestFitBytes(t *testing.T) {
	if got := fitBytes("短", 100); got != "短" {
		t.Fatalf("短文本不该动: %q", got)
	}
	long := strings.Repeat("测", 2000) // 6000 字节
	got := fitBytes(long, 3600)
	if len(got) > 3600 || !strings.HasSuffix(got, "…") || len([]rune(got)) < 1190 {
		t.Fatalf("截断结果 %d 字节 %d 字", len(got), len([]rune(got)))
	}
}

func TestSecureDeleteAndVacuum(t *testing.T) {
	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	var sd, jsl int
	g.Raw("PRAGMA secure_delete").Scan(&sd)
	g.Raw("PRAGMA journal_size_limit").Scan(&jsl)
	if sd != 1 || jsl != 4194304 {
		t.Fatalf("secure_delete=%d journal_size_limit=%d", sd, jsl)
	}
	s := New(&config.Config{RetentionDays: 1}, g)
	s.cleanupOnce() // 含 checkpoint 和 VACUUM，不应报错
}
