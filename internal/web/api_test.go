package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/push"
	"tgchannel/internal/tgclient"
)

// 对外接口全流程：建主题拿 key → 调接口 → 各种拒绝 → 重置 / 停用 / 删除 → 归属隔离。
func TestAPITopics(t *testing.T) {
	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	cfg := &config.Config{BasePath: "/tg", SessionSecret: "x", PublicURL: "https://h/tg"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(New(cfg, g, tgclient.NewManager(ctx, cfg, g), push.New(cfg, g)))
	defer srv.Close()

	client := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	read := func(resp *http.Response, err error) (int, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	bob := client()
	read(bob.PostForm(srv.URL+"/tg/register", url.Values{"username": {"bob"}, "password": {"bob123456"}}))
	var u db.User
	g.Where("username = ?", "bob").First(&u)

	// 没有 TG 账号：提示先加账号，建不了
	_, body := read(bob.Get(srv.URL + "/tg/topics"))
	if !strings.Contains(body, "先在") || !strings.Contains(body, `class="on">对外接口`) {
		t.Fatalf("无账号页面: %s", body)
	}
	acc := db.TgAccount{UserID: u.ID, FirstName: "Bob"}
	g.Create(&acc)
	ch := db.Channel{AccountID: acc.ID, Kind: "ntfy", Target: "t", Enabled: true}
	g.Create(&ch)

	create := func(c *http.Client, code string) (int, string) {
		return read(c.PostForm(srv.URL+"/tg/topics", url.Values{"name": {"调研问卷"}, "code": {code}, "icon": {"📋"}, "note": {"给问卷系统"}}))
	}
	keyRe := regexp.MustCompile(`tgk_[A-Za-z0-9_-]{43}`)
	code, body := create(bob, "survey")
	key := keyRe.FindString(body)
	if code != 200 || key == "" || !strings.Contains(body, "https://h/tg/api/v1/push/survey") {
		t.Fatalf("创建主题 %d: %s", code, body)
	}
	var topic db.ApiTopic
	g.Where("code = ?", "survey").First(&topic)
	if topic.AccountID != acc.ID || topic.KeyHash == key || topic.KeyHash != keyHash(key) || !topic.Enabled {
		t.Fatalf("主题 = %+v", topic)
	}
	// 同名标识、非法标识不能建；列表只显示 key 前缀
	if code, _ := create(bob, "survey"); code != 302 {
		t.Fatalf("重复标识应拒绝: %d", code)
	}
	if code, _ := create(bob, "坏 标识"); code != 302 {
		t.Fatalf("非法标识应拒绝: %d", code)
	}
	var n int64
	g.Model(&db.ApiTopic{}).Count(&n)
	_, body = read(bob.Get(srv.URL + "/tg/topics"))
	if n != 1 || strings.Contains(body, key) || !strings.Contains(body, topic.KeyPrefix+"…") {
		t.Fatalf("列表 n=%d", n)
	}

	call := func(path, auth, ctype, payload string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/tg/api/v1/push/"+path, strings.NewReader(payload))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		code, b := read(http.DefaultClient.Do(req))
		var m map[string]any
		_ = json.Unmarshal([]byte(b), &m)
		return code, m
	}
	form := "application/x-www-form-urlencoded"
	if c, m := call("survey", key, form, "有新的问卷"); c != 202 || m["ok"] != true || m["id"] == nil {
		t.Fatalf("纯文本调用 = %d %v", c, m)
	}
	if c, m := call("survey", key, "application/json", `{"title":"新问卷","message":"张三","priority":5,"tags":["a"],"click":"https://e.com"}`); c != 202 {
		t.Fatalf("JSON 调用 = %d %v", c, m)
	}
	for _, bad := range []string{`{"message":""}`, `{"message":"x","priority":6}`, `{"message":"x","click":"javascript:alert(1)"}`, `{"message":"x","tags":["a","b","c","d"]}`, `{bad`} {
		if c, m := call("survey", key, "application/json", bad); c != 400 || m["error"] == "" {
			t.Fatalf("%s = %d %v", bad, c, m)
		}
	}
	if c, _ := call("survey", key, form, strings.Repeat("x", 70<<10)); c != 413 {
		t.Fatalf("超大请求 = %d", c)
	}
	g.First(&topic, topic.ID)
	if topic.Calls != 2 || topic.LastAt == nil {
		t.Fatalf("调用计数 = %d", topic.Calls)
	}
	// 消息记录里显示主题和调用方
	_, body = read(bob.Get(srv.URL + "/tg/messages"))
	if !strings.Contains(body, "📋 [调研问卷]") || !strings.Contains(body, "接口 127.0.0.1") {
		t.Fatalf("消息记录没显示接口消息")
	}

	// key 错 / 标识不对应 / 没带 key 都是 401
	if c, _ := call("survey", "tgk_wrong", form, "x"); c != 401 {
		t.Fatalf("错 key = %d", c)
	}
	if c, _ := call("other", key, form, "x"); c != 401 {
		t.Fatalf("标识不对应 = %d", c)
	}
	if c, _ := call("survey", "", form, "x"); c != 401 {
		t.Fatalf("没带 key = %d", c)
	}

	// 渠道全停：409；停用主题：403
	g.Model(&ch).Update("enabled", false)
	if c, _ := call("survey", key, form, "x"); c != 409 {
		t.Fatalf("渠道停用 = %d", c)
	}
	g.Model(&ch).Update("enabled", true)
	read(bob.Post(srv.URL+"/tg/topics/"+itoa(topic.ID)+"/toggle", "", nil))
	if c, _ := call("survey", key, form, "x"); c != 403 {
		t.Fatalf("主题停用 = %d", c)
	}
	read(bob.Post(srv.URL+"/tg/topics/"+itoa(topic.ID)+"/toggle", "", nil))

	// 编辑不影响 key 和标识
	read(bob.PostForm(srv.URL+"/tg/topics/"+itoa(topic.ID), url.Values{"name": {"问卷"}, "icon": {""}, "code": {"hack"}}))
	g.First(&topic, topic.ID)
	if topic.Name != "问卷" || topic.Icon != "" || topic.Code != "survey" || topic.KeyHash != keyHash(key) {
		t.Fatalf("编辑后 = %+v", topic)
	}

	// 别人管不了，也看不到
	tom := client()
	read(tom.PostForm(srv.URL+"/tg/register", url.Values{"username": {"tom"}, "password": {"tom123456"}}))
	var tu db.User
	g.Where("username = ?", "tom").First(&tu)
	g.Create(&db.TgAccount{UserID: tu.ID, FirstName: "Tom"})
	for _, p := range []string{"/toggle", "/key", "/delete", ""} {
		if c, _ := read(tom.PostForm(srv.URL+"/tg/topics/"+itoa(topic.ID)+p, url.Values{"name": {"x"}})); c != 404 {
			t.Fatalf("别人操作 %s = %d", p, c)
		}
	}
	if _, body := read(tom.Get(srv.URL + "/tg/topics")); strings.Contains(body, topic.KeyPrefix) || !strings.Contains(body, "还没有主题") {
		t.Fatal("别人的主题不该出现")
	}

	// 重置 key：旧的立即失效，新的能用
	_, body = read(bob.Post(srv.URL+"/tg/topics/"+itoa(topic.ID)+"/key", "", nil))
	key2 := keyRe.FindString(body)
	if key2 == "" || key2 == key {
		t.Fatal("重置应给新 key")
	}
	if c, _ := call("survey", key, form, "x"); c != 401 {
		t.Fatalf("旧 key = %d", c)
	}
	if c, _ := call("survey", key2, form, "x"); c != 202 {
		t.Fatalf("新 key = %d", c)
	}

	// 每分钟 60 次（前面已用 3 次）
	last := 0
	for i := 0; i < 60; i++ {
		last, _ = call("survey", key2, form, "x")
	}
	if last != 429 {
		t.Fatalf("超频应 429: %d", last)
	}

	// 删除后 key 失效
	read(bob.Post(srv.URL+"/tg/topics/"+itoa(topic.ID)+"/delete", "", nil))
	if c, _ := call("survey", key2, form, "x"); c != 401 {
		t.Fatalf("删除后 = %d", c)
	}

	// 同一 IP key 错满 10 次后，连对的 key 也先拒绝
	_, body = create(bob, "survey2")
	key3 := keyRe.FindString(body)
	locked := false
	for i := 0; i < apiFailMax && !locked; i++ {
		c, _ := call("survey2", "tgk_wrong", form, "x")
		locked = c == 429
	}
	if c, m := call("survey2", key3, form, "x"); c != 429 {
		t.Fatalf("错太多次应锁定: %d %v", c, m)
	}
}
