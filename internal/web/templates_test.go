package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"tgchannel/internal/db"
	"tgchannel/internal/tgclient"
)

// 每个页面都真正执行一遍，防止引用了没加载的模板导致空白页。
func TestAllTemplatesRender(t *testing.T) {
	r := loadTemplates(func(s string) string { return "/tg" + s }, "v1")
	now := time.Now()
	user := &db.User{Username: "bob", IsAdmin: true}
	acc := db.TgAccount{ID: 1, FirstName: "Bob"}
	cases := map[string]gin.H{
		"login.html":     {},
		"register.html":  {},
		"accounts.html":  {"User": user, "Accounts": []db.TgAccount{acc}},
		"account.html":   {"User": user, "Account": &acc, "Ntfy": &db.Channel{ID: 1, Target: "t", Enabled: true}, "Bark": (*db.Channel)(nil), "NtfyURL": "http://n", "BarkURL": "http://b"},
		"peers.html":     {"User": user, "Peers": []peerRow{{TgPeer: db.TgPeer{Title: "群"}, Account: "Bob"}}},
		"messages.html":  {"User": user, "Messages": []messageRow{{Message: db.Message{Text: "hi", SenderUser: "tom"}, PeerUser: "grp", PushState: "sent", PushAt: &now}}},
		"providers.html": {"User": user, "Providers": []db.Provider{{Kind: "ntfy"}}},
		"users.html":     {"User": user, "Users": []userRow{{User: db.User{ID: 1, Username: "bob", IsAdmin: true}, Accounts: 2}, {User: db.User{ID: 2, Username: "tom"}}}},
		"qr.html":        {"User": user, "Job": tgclient.JobView{ID: "j", State: "qr"}},
		"reply.html":     {},
		"qr_status":      {"Job": tgclient.JobView{ID: "j", State: "password", Msg: "密码错误"}},
		"qr_checking":    {"Job": tgclient.JobView{ID: "j", State: "checking"}},
	}
	for name, data := range cases {
		rec := httptest.NewRecorder()
		if name == "qr_checking" {
			name = "qr_status"
		}
		if err := r.Instance(name, data).Render(rec); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if rec.Body.Len() < 50 || (strings.HasSuffix(name, ".html") && !strings.Contains(rec.Body.String(), "</html>")) {
			t.Errorf("%s: 输出不完整: %q", name, rec.Body.String())
		}
	}
}

func TestNameAndClip(t *testing.T) {
	h := string(nameHTML("bob", "名字<x>", 42))
	if !strings.HasPrefix(h, "@bob 名字&lt;x&gt; <span") || !strings.Contains(h, `data-copy="42"`) {
		t.Fatalf("nameHTML = %s", h)
	}
	if h := string(nameHTML("", "只有名字", 0)); h != "只有名字" {
		t.Fatalf("nameHTML no id = %s", h)
	}
	if h := string(clipHTML("一二三四五六七八")); !strings.Contains(h, "一二三四五六…") || !strings.Contains(h, `hidden>一二三四五六七八<`) {
		t.Fatalf("clipHTML long = %s", h)
	}
	if h := string(clipHTML("短")); strings.Contains(h, "clip") {
		t.Fatalf("clipHTML short = %s", h)
	}
	if h := string(clipHTML("")); !strings.Contains(h, "不留存") {
		t.Fatalf("clipHTML empty = %s", h)
	}
}

func TestNameAndTime(t *testing.T) {
	if got := displayName("bob", "一二三四五六七八九十一二三四五六七八"); got != "@bob 一二三四五六七八九十一二三四五六…" {
		t.Fatalf("displayName = %q", got)
	}
	if got := displayName("", "短名"); got != "短名" {
		t.Fatalf("displayName no username = %q", got)
	}
	base := time.Date(2026, 10, 19, 21, 0, 34, 0, time.Local)
	cases := map[string]time.Time{
		"35.121":             base.Add(1121 * time.Millisecond),
		"01:35.121":          base.Add(61121 * time.Millisecond),
		"10-19 22:00:34.000": base.Add(time.Hour),
	}
	for want, tm := range cases {
		if got := relTime(tm, base); got != want {
			t.Errorf("relTime(%v) = %q, want %q", tm, got, want)
		}
		tp := tm
		if got := relTime(&tp, base); got != want {
			t.Errorf("relTime(ptr) = %q, want %q", got, want)
		}
	}
	if relTime((*time.Time)(nil), base) != "" {
		t.Error("nil 应为空")
	}
	if got := fmtTime(base); got != "10-19 21:00:34.000" {
		t.Errorf("fmtTime = %q", got)
	}
}
