package push

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/tgclient"
)

// 接口消息进同一个队列，用账号的 ntfy 渠道推出：标题带 [主题名]，标签首个是主题标识，没有回复按钮。
func TestAPIPush(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
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
	ch := db.Channel{AccountID: acc.ID, Kind: "ntfy", Target: "tg-abc", Enabled: true}
	g.Create(&ch)
	topic := db.ApiTopic{UserID: 1, AccountID: acc.ID, Code: "survey", Name: "调研问卷", Icon: "📋", KeyHash: "h", Enabled: true}
	g.Create(&topic)
	s := New(&config.Config{NtfyURL: srv.URL, PublicURL: "https://x/tg", SessionSecret: "sec"}, g)

	id, err := s.HandleAPI(APIMessage{AccountID: acc.ID, TopicID: topic.ID, From: "1.2.3.4", Title: "新问卷", Text: "张三提交了", Priority: 4, Click: "https://e.com/1", Tags: []string{"满意度"}})
	if err != nil || id == 0 {
		t.Fatalf("HandleAPI = %d %v", id, err)
	}
	s.drainSync(context.Background())
	if got["topic"] != "tg-abc" || got["title"] != "📋 [调研问卷] 新问卷" || got["message"] != "张三提交了" {
		t.Fatalf("推送内容 = %v", got)
	}
	if got["priority"] != float64(4) || got["click"] != "https://e.com/1" || got["actions"] != nil {
		t.Fatalf("附加字段 = %v", got)
	}
	if tags, _ := got["tags"].([]any); len(tags) != 2 || tags[0] != "survey" || tags[1] != "满意度" {
		t.Fatalf("tags = %v", got["tags"])
	}
	var msg db.Message
	g.First(&msg, id)
	var l db.PushLog
	g.Where("message_id = ?", id).First(&l)
	if msg.TopicID != topic.ID || msg.PeerID != 0 || msg.Sender != "1.2.3.4" || msg.Text != "" || l.Status != "sent" {
		t.Fatalf("记录 = %+v %+v", msg, l)
	}
	if len(s.texts) != 0 {
		t.Fatal("推完正文应从内存释放")
	}

	// 不带标题、主题已删：标题退回 [接口]，只有主题标识以外的字段都不带
	g.Delete(&topic)
	s.HandleAPI(APIMessage{AccountID: acc.ID, TopicID: topic.ID, Text: "只有正文"})
	s.drainSync(context.Background())
	if got["title"] != "[接口]" || got["message"] != "只有正文" || got["priority"] != float64(3) || got["tags"] != nil {
		t.Fatalf("最简推送 = %v", got)
	}

	// 渠道都停用：拒绝，不落库
	g.Model(&ch).Update("enabled", false)
	var before int64
	g.Model(&db.Message{}).Count(&before)
	if _, err := s.HandleAPI(APIMessage{AccountID: acc.ID, TopicID: 1, Text: "x"}); !errors.Is(err, ErrNoChannel) {
		t.Fatalf("渠道停用应拒绝: %v", err)
	}
	var after int64
	g.Model(&db.Message{}).Count(&after)
	if after != before {
		t.Fatal("被拒绝的调用不该落库")
	}
}

// 通知等级：对话自己设了用对话的，没设（0）跟随账号默认；推到 ntfy 的 priority。
func TestPeerPriority(t *testing.T) {
	urgentGap = 10 * time.Millisecond
	var got map[string]any
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		hits++
		_ = json.NewDecoder(r.Body).Decode(&got)
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
	g.Create(&db.TgPeer{AccountID: acc.ID, Kind: "user", PeerID: 7, Title: "Bob", Monitored: true})
	g.Create(&db.TgPeer{AccountID: acc.ID, Kind: "user", PeerID: 8, Title: "Boss", Monitored: true, Priority: 5})
	g.Create(&db.Channel{AccountID: acc.ID, Kind: "ntfy", Target: "t", Enabled: true})
	s := New(&config.Config{NtfyURL: srv.URL}, g)
	send := func(peer int64) any {
		hits = 0
		s.HandleMessage(tgclient.Incoming{AccountID: acc.ID, PeerKind: "user", PeerID: peer, Text: "hi", SentAt: time.Now()})
		s.drainSync(context.Background())
		return got["priority"]
	}
	if p := send(7); p != float64(3) || hits != 1 {
		t.Fatalf("新账号默认应为普通 3，只发一遍: %v %d", p, hits)
	}
	if p := send(8); p != float64(5) || hits != 3 {
		t.Fatalf("对话设了紧急，应连发三遍: %v %d", p, hits)
	}
	g.Model(&acc).Update("priority", 2)
	if p := send(7); p != float64(2) {
		t.Fatalf("跟随账号默认 2: %v", p)
	}
	if p := send(8); p != float64(5) {
		t.Fatalf("对话自己的优先于账号默认: %v", p)
	}
	var sent int64
	g.Model(&db.PushLog{}).Where("status = 'sent'").Count(&sent)
	if sent != 4 {
		t.Fatalf("补发不另记推送记录: %d", sent)
	}

	// 接口消息按同一账号的连发设置：priority=5 默认连发三遍
	topic := db.ApiTopic{UserID: 1, AccountID: acc.ID, Code: "c", Name: "n", KeyHash: "k", Enabled: true}
	g.Create(&topic)
	api := func(prio int) int {
		hits = 0
		s.HandleAPI(APIMessage{AccountID: acc.ID, TopicID: topic.ID, Text: "x", Priority: prio})
		s.drainSync(context.Background())
		return hits
	}
	if n := api(5); n != 3 {
		t.Fatalf("接口紧急消息应发三遍: %d", n)
	}
	// 改成"高及以上发 2 遍"：高 2 遍，普通 1 遍，不传等级用账号默认（2 最低→1 遍）
	g.Model(&acc).Updates(map[string]any{"repeat_from": 4, "repeat_n": 2})
	if n := api(4); n != 2 {
		t.Fatalf("高及以上应发两遍: %d", n)
	}
	if n := api(3); n != 1 {
		t.Fatalf("普通只发一遍: %d", n)
	}
	if n := api(0); n != 1 || got["priority"] != float64(2) {
		t.Fatalf("不传等级用账号默认: %d %v", n, got["priority"])
	}
	if p := send(8); p != float64(5) || hits != 2 {
		t.Fatalf("TG 消息也按新设置连发: %d", hits)
	}
	// 1 遍 = 不连发
	g.Model(&acc).Update("repeat_n", 1)
	if p := send(8); p != float64(5) || hits != 1 {
		t.Fatalf("设成 1 遍应不连发: %d", hits)
	}
}

func TestRepeatTimes(t *testing.T) {
	for _, c := range [][4]int{{5, 5, 3, 3}, {4, 5, 3, 1}, {5, 4, 2, 2}, {3, 0, 3, 1}, {5, 5, 1, 1}, {5, 5, 9, maxRepeat}} {
		if got := repeatTimes(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("repeatTimes%v = %d", c[:3], got)
		}
	}
}
