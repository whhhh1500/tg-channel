package push

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
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
	if got["title"] != "[接口]" || got["message"] != "只有正文" || got["priority"] != nil || got["tags"] != nil {
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
