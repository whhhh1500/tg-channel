package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/push"
	"tgchannel/internal/tgclient"
)

func TestPages(t *testing.T) {
	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	cfg := &config.Config{BasePath: "/tg", SessionSecret: "x", NtfyURL: "http://ntfy"}
	g.Create(&db.Provider{Kind: "ntfy", BaseURL: "http://ntfy"})
	g.Create(&db.Provider{Kind: "bark", BaseURL: "http://bark"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(New(cfg, g, tgclient.NewManager(ctx, cfg, g), push.New(cfg, g)))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(p string) (int, string) {
		resp, err := c.Get(srv.URL + "/tg" + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 64<<10)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, sb.String()
	}
	post := func(p string, form url.Values) int {
		resp, err := c.PostForm(srv.URL+"/tg"+p, form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	want := func(cond bool, msg string) {
		t.Helper()
		if !cond {
			t.Fatal(msg)
		}
	}

	code, _ := get("/accounts")
	want(code == 302, "未登录应跳转")
	post("/register", url.Values{"username": {"bob"}, "password": {"bob123456"}})
	var cnt0 int64
	code, body := get("/accounts")
	want(code == 200 && strings.Contains(body, "退出 bob"), "注册后应已登录")
	for i := 0; i < 11; i++ {
		post("/login", url.Values{"username": {"nobody"}, "password": {"x"}})
	}
	code, body = get("/login")
	want(strings.Contains(body, "太频繁"), "登录应被限速")
	want(!strings.Contains(body, "渠道管理"), "普通用户不应看到渠道管理")
	code, _ = get("/admin/providers")
	want(code == 404, "普通用户访问 admin 应 404")

	// 伪造一个已登录账号
	var u db.User
	g.Where("username = ?", "bob").First(&u)
	acc := db.TgAccount{UserID: u.ID, FirstName: "Bob TG", Status: "online"}
	g.Create(&acc)
	peer := db.TgPeer{AccountID: acc.ID, Kind: "chat", PeerID: 1, Title: "群A"}
	g.Create(&peer)
	other := db.TgAccount{UserID: 999}
	g.Create(&other)
	g.Create(&db.TgPeer{AccountID: other.ID, Kind: "chat", PeerID: 2, Title: "别人的群"})

	code, body = get("/accounts")
	want(strings.Contains(body, "Bob TG"), "账号列表应显示账号")
	code, body = get("/peers")
	want(strings.Contains(body, "群A") && !strings.Contains(body, "别人的群"), "对话列表只显示自己的")

	want(post("/peers/"+itoa(peer.ID)+"/toggle", nil) == 204, "切换监控")
	g.First(&peer, peer.ID)
	want(peer.Monitored, "监控应已打开")
	var otherPeer db.TgPeer
	g.Where("account_id = ?", other.ID).First(&otherPeer)
	want(post("/peers/"+itoa(otherPeer.ID)+"/toggle", nil) == 404, "不能切换别人的对话")

	code, body = get("/accounts/" + itoa(acc.ID))
	want(code == 200 && strings.Contains(body, "ntfy") && strings.Contains(body, "Bark"), "账号详情页")
	post("/accounts/"+itoa(acc.ID)+"/ntfy", url.Values{"topic": {"bad topic!"}})
	post("/accounts/"+itoa(acc.ID)+"/ntfy", url.Values{"topic": {"my-topic_1"}})
	post("/accounts/"+itoa(acc.ID)+"/bark", url.Values{"key": {"key1"}})
	var chs []db.Channel
	g.Where("account_id = ?", acc.ID).Order("kind").Find(&chs)
	want(len(chs) == 2 && chs[0].Kind == "bark" && chs[0].Target == "key1" && chs[1].Target == "my-topic_1", "应有 bark 和 ntfy 两个渠道")
	post("/accounts/"+itoa(acc.ID)+"/ntfy", url.Values{"topic": {"renamed"}})
	g.Where("account_id = ? AND kind = 'ntfy'", acc.ID).Find(&chs)
	want(len(chs) == 1 && chs[0].Target == "renamed", "改 topic 应更新同一条")
	g.Create(&db.Channel{AccountID: other.ID, Kind: "ntfy", Target: "taken-by-other"})
	post("/accounts/"+itoa(acc.ID)+"/ntfy", url.Values{"topic": {"taken-by-other"}})
	g.Where("account_id = ? AND kind = 'ntfy'", acc.ID).Find(&chs)
	want(chs[0].Target == "renamed", "别人用过的 topic 不能抢")
	want(post("/channels/"+itoa(chs[0].ID)+"/toggle", nil) == 204, "切换渠道")
	g.First(&chs[0], chs[0].ID)
	want(!chs[0].Enabled, "渠道应已停用")

	msg := db.Message{AccountID: acc.ID, PeerID: peer.ID, Sender: "Tom", Text: "hi there", SentAt: time.Now(), ReceivedAt: time.Now()}
	g.Create(&msg)
	now := time.Now()
	g.Create(&db.PushLog{MessageID: msg.ID, ChannelID: chs[0].ID, Status: "sent", SentAt: &now})
	code, body = get("/messages")
	want(code == 200 && strings.Contains(body, "hi there") && strings.Contains(body, "群A"), "消息页应显示消息和对话名")
	want(!strings.Contains(body, "排队中") && !strings.Contains(body, "无渠道"), "推送状态应为已发送时间")

	post("/accounts/"+itoa(acc.ID)+"/bark", url.Values{"key": {""}})
	g.Where("account_id = ? AND kind = 'bark'", acc.ID).Find(&chs)
	want(len(chs) == 0, "留空应删除 bark 渠道")

	// 游标分页：60 个对话、60 条消息
	for i := 0; i < 59; i++ {
		g.Create(&db.TgPeer{AccountID: acc.ID, Kind: "chat", PeerID: int64(100 + i), Title: fmt.Sprintf("群%02d", i)})
	}
	code, body = get("/peers?account=" + itoa(other.ID))
	want(strings.Count(body, "<tr>") == 0 && strings.Contains(body, "还没有对话"), "筛别人的账号应为空")
	code, body = get("/peers?account=" + itoa(acc.ID))
	want(strings.Contains(body, "selected") && strings.Count(body, "<tr>") == 1+pageSize, "按账号筛选应有 50 行")
	code, body = get("/peers")
	want(strings.Count(body, "<tr>") == 1+pageSize && strings.Contains(body, "after="), "对话第一页应 50 行且有下一页")
	nextRe := regexp.MustCompile(`after=([0-9]+-[0-9]+)`)
	code, body = get("/peers?after=" + nextRe.FindStringSubmatch(body)[1])
	want(strings.Count(body, "<tr>") == 1+10 && !strings.Contains(body, "after="), "对话第二页应 10 行且无下一页")
	for i := 0; i < 59; i++ {
		g.Create(&db.Message{AccountID: acc.ID, PeerID: peer.ID, Text: fmt.Sprintf("m%02d", i), SentAt: time.Now(), ReceivedAt: time.Now()})
	}
	code, body = get("/messages")
	want(strings.Count(body, "<tr>") == 1+pageSize && strings.Contains(body, "m58") && !strings.Contains(body, "hi there"), "消息第一页应是最新 50 条")
	beforeRe := regexp.MustCompile(`before=([0-9]+)`)
	code, body = get("/messages?before=" + beforeRe.FindStringSubmatch(body)[1])
	want(strings.Count(body, "<tr>") == 1+10 && strings.Contains(body, "hi there") && !strings.Contains(body, "before="), "消息第二页应是最早 10 条")

	// 用户管理：普通用户 404，admin 可建 / 重置 / 删
	code, _ = get("/admin/users")
	want(code == 404, "普通用户不能进用户管理")
	// 渠道管理：地址能改；id 参数不能注入
	g.Model(&db.User{}).Where("id = ?", u.ID).Update("is_admin", true)
	post("/admin/providers", url.Values{"kind": {"bark"}, "base_url": {"http://b2"}})
	var prov db.Provider
	g.Where("kind = 'bark'").First(&prov)
	want(prov.BaseURL == "http://b2", "渠道地址应已更新")
	post("/admin/providers", url.Values{"kind": {"evil"}, "base_url": {"http://x"}})
	g.Model(&db.Provider{}).Where("kind = 'evil'").Count(&cnt0)
	want(cnt0 == 0, "kind 只能是 ntfy/bark")
	want(post("/admin/users/1%20OR%201%3D1/delete", nil) == 404, "id 带 SQL 片段应 404")
	g.Model(&db.User{}).Where("id = ?", u.ID).Update("is_admin", false)
	g.Model(&db.User{}).Where("id = ?", u.ID).Update("is_admin", true)
	code, body = get("/admin/users")
	want(code == 200 && strings.Contains(body, "bob"), "admin 能看用户列表")
	post("/admin/users", url.Values{"username": {"tom"}, "password": {"tom123456"}})
	var tom db.User
	want(g.Where("username = ?", "tom").First(&tom).Error == nil && !tom.IsAdmin, "应建出普通用户 tom")
	post("/admin/users/"+itoa(tom.ID)+"/password", url.Values{"password": {"newpass123"}})
	g.First(&tom, tom.ID)
	want(bcrypt.CompareHashAndPassword([]byte(tom.PasswordHash), []byte("newpass123")) == nil && tom.SessionVer == 1, "密码应已重置且会话版本 +1")
	post("/admin/users/"+itoa(tom.ID)+"/password", url.Values{"password": {strings.Repeat("x", 80)}})
	g.First(&tom, tom.ID)
	want(bcrypt.CompareHashAndPassword([]byte(tom.PasswordHash), []byte("newpass123")) == nil, "超长密码应被拒绝，旧哈希不变")
	// 改自己的密码后，当前 cookie 应失效
	post("/admin/users/"+itoa(u.ID)+"/password", url.Values{"password": {"bob999999"}})
	kicked, _ := c.Get(srv.URL + "/tg/accounts")
	kicked.Body.Close()
	want(kicked.StatusCode == 302 && strings.HasSuffix(kicked.Header.Get("Location"), "/tg/login"), "改密后旧会话应被踢回登录页而不是 /logout")
	post("/login", url.Values{"username": {"bob"}, "password": {"bob999999"}})
	code, _ = get("/admin/users")
	want(code == 200, "新密码应能登录")
	// 管理员也不能碰别人的账号、对话、消息
	code, _ = get("/accounts/" + itoa(other.ID))
	want(code == 404, "管理员访问他人账号应 404")
	want(post("/accounts/"+itoa(other.ID)+"/sync", nil) == 404, "管理员刷新他人对话应 404")
	want(post("/accounts/"+itoa(other.ID)+"/ntfy", url.Values{"topic": {"steal"}}) == 404, "管理员改他人 topic 应 404")
	want(post("/peers/"+itoa(otherPeer.ID)+"/toggle", nil) == 404, "管理员切换他人对话应 404")
	g.Create(&db.Message{AccountID: other.ID, PeerID: otherPeer.ID, Text: "secret-of-other", SentAt: time.Now(), ReceivedAt: time.Now()})
	code, body = get("/messages")
	want(code == 200 && !strings.Contains(body, "secret-of-other") && !strings.Contains(body, "别人的群"), "管理员消息页不应出现他人消息")
	code, body = get("/peers")
	want(!strings.Contains(body, "别人的群"), "管理员对话页不应出现他人对话")
	// 用户分页
	for i := 0; i < 55; i++ {
		g.Create(&db.User{Username: fmt.Sprintf("u%02d", i), PasswordHash: "x"})
	}
	code, body = get("/admin/users")
	want(strings.Count(body, "<tr>") == 1+pageSize && strings.Contains(body, "after="), "用户第一页 50 行")
	afterRe := regexp.MustCompile(`after=([0-9]+)`)
	code, body = get("/admin/users?after=" + afterRe.FindStringSubmatch(body)[1])
	want(strings.Count(body, "<tr>") > 1 && !strings.Contains(body, "after="), "用户第二页无下一页")
	g.Where("username LIKE 'u__'").Delete(&db.User{})
	tomAcc := db.TgAccount{UserID: tom.ID, FirstName: "tom tg"}
	g.Create(&tomAcc)
	g.Create(&db.Message{AccountID: tomAcc.ID, Text: "x", SentAt: time.Now(), ReceivedAt: time.Now()})
	post("/admin/users/"+itoa(tom.ID)+"/delete", nil)
	g.Model(&db.Message{}).Where("account_id = ?", tomAcc.ID).Count(&cnt0)
	want(cnt0 == 0, "删用户应连消息一起删")
	var cnt int64
	g.Model(&db.User{}).Where("id = ?", tom.ID).Count(&cnt)
	want(cnt == 0, "tom 应已删除")
	g.Model(&db.TgAccount{}).Where("user_id = ?", tom.ID).Count(&cnt)
	want(cnt == 0, "tom 的 TG 账号应已删除")
	post("/admin/users/"+itoa(u.ID)+"/delete", nil)
	g.Model(&db.User{}).Where("id = ?", u.ID).Count(&cnt)
	want(cnt == 1, "不能删自己")
	g.Model(&db.User{}).Where("id = ?", u.ID).Update("is_admin", false)

	get("/accounts") // 消费掉上一步的闪现提示
	post("/accounts/new", nil)
	code, body = get("/accounts")
	want(strings.Contains(body, "TG_API_ID"), "未配置 API ID 应提示")
}

func itoa(n uint) string { return strconv.FormatUint(uint64(n), 10) }
