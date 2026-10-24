// Package web 提供 HTTP 路由、页面渲染和登录态。
package web

import (
	"embed"
	"fmt"
	"hash/crc32"
	"html"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
	"gorm.io/gorm"

	"tgchannel/internal/config"
	"tgchannel/internal/db"
	"tgchannel/internal/push"
	"tgchannel/internal/tgclient"
)

//go:embed templates static
var assets embed.FS

type Server struct {
	cfg     *config.Config
	db      *gorm.DB
	tg      *tgclient.Manager
	push    *push.Service
	base    string     // BASE_PATH，如 /tg；空表示根
	uses    *uses      // 回复链接已用次数
	limit   *limiter   // 登录 / 注册限速
	topicMu sync.Mutex // topic 切换和 ntfy 授权回收 / 授予整体串行，避免乱序留下旧授权
}

func New(cfg *config.Config, g *gorm.DB, tg *tgclient.Manager, p *push.Service) *gin.Engine {
	s := &Server{cfg: cfg, db: g, tg: tg, push: p, base: strings.TrimRight(cfg.BasePath, "/"), uses: newUses(), limit: newLimiter()}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	_ = r.SetTrustedProxies(cfg.TrustedProxies) // 不信任任意来源的 X-Forwarded-For
	store := cookie.NewStore([]byte(cfg.SessionSecret))
	store.Options(sessions.Options{Path: s.base + "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	r.Use(gin.Recovery(), sessions.Sessions("sess", store), logErrors)
	r.HTMLRender = loadTemplates(s.url, assetVersion())

	root := r.Group(s.base)
	static, _ := fs.Sub(assets, "static")
	root.Group("/static", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=604800, immutable") // 链接带版本号，可长期缓存
	}).StaticFS("/", http.FS(static))

	root.GET("/login", s.loginPage)
	root.POST("/login", s.login)
	root.GET("/register", s.registerPage)
	root.POST("/register", s.register)
	root.POST("/logout", s.logout)
	root.GET("/reply", s.replyPage) // 回复页：凭通知链接里的 token，不需要登录
	limit := func(c *gin.Context) { c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10) }
	root.POST("/reply/open", limit, s.replyOpen)
	root.POST("/reply/send", limit, s.replySend)

	auth := root.Group("/", s.requireLogin)
	auth.GET("/", func(c *gin.Context) { s.redirect(c, "/accounts") })
	auth.GET("/accounts", s.accountsPage)
	auth.POST("/accounts/new", s.startLogin)
	auth.GET("/accounts/new/:job", s.loginJobPage)
	auth.GET("/accounts/new/:job/status", s.loginJobStatus)
	auth.GET("/accounts/new/:job/qr.png", s.loginJobQR)
	auth.POST("/accounts/new/:job/password", s.loginJobPassword)
	auth.GET("/accounts/:id", s.accountPage)
	auth.POST("/accounts/:id/sync", s.syncAccount)
	auth.POST("/accounts/:id/delete", s.deleteAccount)
	auth.POST("/accounts/:id/ntfy", s.saveNtfy)
	auth.POST("/accounts/:id/bark", s.saveBark)
	auth.POST("/channels/:id/toggle", s.toggleChannel)
	auth.GET("/peers", s.peersPage)
	auth.POST("/peers/:id/toggle", s.togglePeer("monitored"))
	auth.POST("/peers/:id/mute", s.togglePeer("muted"))
	auth.POST("/accounts/:id/monitor-all", s.toggleMonitorAll)
	auth.GET("/messages", s.messagesPage)

	admin := auth.Group("/admin", s.requireAdmin)
	admin.GET("/providers", s.providersPage)
	admin.POST("/providers", s.saveProvider)
	admin.GET("/users", s.usersPage)
	admin.POST("/users", s.createUser)
	admin.POST("/users/:id/password", s.resetPassword)
	admin.POST("/users/:id/delete", s.deleteUser)
	return r
}

// logErrors 把 gin 内部错误（如模板渲染失败）打到日志。
func logErrors(c *gin.Context) {
	c.Next()
	for _, e := range c.Errors {
		if e.IsType(gin.ErrorTypeBind) { // 客户端的坏请求不值得记 ERROR，匿名接口会被刷日志
			continue
		}
		slog.Error("http", "method", c.Request.Method, "path", c.Request.URL.Path, "err", e.Err)
	}
}

// assetVersion 用静态文件内容的哈希做版本号，文件变了链接就变。
func assetVersion() string {
	h := crc32.NewIEEE()
	_ = fs.WalkDir(assets, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := assets.ReadFile(p)
			h.Write(b)
		}
		return nil
	})
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

// displayName 显示为 "@用户名 名字"，名字超过 16 个字截断加省略号。
func displayName(username, title string) string {
	if r := []rune(title); len(r) > 16 {
		title = string(r[:16]) + "…"
	}
	if username != "" {
		return "@" + username + " " + title
	}
	return title
}

// nameHTML "@用户名 名字" 原样显示；id 非 0 时附在后面：小一号、带色、点击复制。
func nameHTML(username, title string, id int64) template.HTML {
	h := html.EscapeString(displayName(username, title))
	if id != 0 {
		h += fmt.Sprintf(` <span class="nm" data-copy="%d" title="点击复制">%d</span>`, id, id)
	}
	return template.HTML(h)
}

// clipHTML 内容默认只显示前 6 个字，点击展开全文；正文推完即清，空的显示说明。
func clipHTML(text string) template.HTML {
	if text == "" {
		return `<small style="color:#888">不留存</small>`
	}
	r := []rune(text)
	if len(r) <= 6 {
		return template.HTML(`<span class="full">` + html.EscapeString(text) + `</span>`)
	}
	return template.HTML(fmt.Sprintf(`<span class="clip" title="点击展开"><span class="short">%s…</span><span class="full" hidden>%s</span></span>`,
		html.EscapeString(string(r[:6])), html.EscapeString(text)))
}

// fmtTime 完整时间到毫秒。
func fmtTime(t time.Time) string { return t.Local().Format("01-02 15:04:05.000") }

// fmtSec 到秒：Telegram 的消息时间本身只有秒级精度，显示毫秒会误导。
func fmtSec(t time.Time) string { return t.Local().Format("01-02 15:04:05") }

// relTime 相对基准时间只显示变化的部分：同一分钟只显示 秒.毫秒，同一小时显示 分:秒.毫秒。
// v 可以是 time.Time 或 *time.Time（nil 返回空）。
func relTime(v any, base time.Time) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return ""
		}
		t = *x
	default:
		return ""
	}
	a, b := t.Local(), base.Local()
	switch {
	case a.Format("01-02 15:04") == b.Format("01-02 15:04"):
		return a.Format("05.000")
	case a.Format("01-02 15") == b.Format("01-02 15"):
		return a.Format("04:05.000")
	}
	return fmtTime(a)
}

// url 给站内路径加上 BASE_PATH。
func (s *Server) url(p string) string { return s.base + p }

func (s *Server) redirect(c *gin.Context, p string) { c.Redirect(http.StatusFound, s.url(p)) }

// loadTemplates 基础模板 = layout + 所有 partials；每个页面在其上克隆一份，避免 define 冲突。
// partials 也可单独按名字渲染（htmx 局部刷新）。
func loadTemplates(url func(string) string, ver string) render.HTMLRender {
	funcs := template.FuncMap{
		"url":   url,
		"asset": func(p string) string { return url(p) + "?v=" + ver },
		"name":  nameHTML,
		"clip":  clipHTML,
		"ts":    fmtTime,
		"tsec":  fmtSec,
		"rel":   relTime,
	}
	r := pageRender{pages: map[string]entry{}}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials/*.html"))
	pages, _ := fs.Glob(assets, "templates/pages/*.html")
	for _, p := range pages {
		t := template.Must(template.Must(base.Clone()).ParseFS(assets, p))
		r.pages[path.Base(p)] = entry{t, "layout"}
	}
	partials, _ := fs.Glob(assets, "templates/partials/*.html")
	for _, p := range partials {
		name := strings.TrimSuffix(path.Base(p), ".html")
		r.pages[name] = entry{base, name}
	}
	return r
}

type entry struct {
	t    *template.Template
	name string
}

type pageRender struct{ pages map[string]entry }

func (p pageRender) Instance(name string, data any) render.Render {
	e := p.pages[name]
	return render.HTML{Template: e.t, Name: e.name, Data: data}
}

// render 统一注入当前用户和闪现消息。
func (s *Server) render(c *gin.Context, page string, data gin.H) {
	if data == nil {
		data = gin.H{}
	}
	sess := sessions.Default(c)
	if f := sess.Flashes(); len(f) > 0 {
		data["Flash"] = f[len(f)-1] // 只显示最新一条
		_ = sess.Save()
	}
	if u, ok := c.Get("user"); ok {
		data["User"] = u
	}
	// 当前导航项：去掉 BASE_PATH 后的前一两段，如 accounts、admin/users
	p := strings.TrimPrefix(c.Request.URL.Path, s.base)
	segs := strings.SplitN(strings.Trim(p, "/"), "/", 3)
	nav := segs[0]
	if nav == "admin" && len(segs) > 1 {
		nav += "/" + segs[1]
	}
	data["Nav"] = nav
	c.HTML(http.StatusOK, page, data)
}

func flash(c *gin.Context, msg string) {
	sess := sessions.Default(c)
	sess.AddFlash(msg)
	_ = sess.Save()
}

func (s *Server) currentUser(c *gin.Context) *db.User {
	if u, ok := c.Get("user"); ok {
		return u.(*db.User)
	}
	return nil
}

func (s *Server) requireLogin(c *gin.Context) {
	sess := sessions.Default(c)
	id, _ := sess.Get("uid").(uint)
	ver, _ := sess.Get("sv").(int)
	if id == 0 {
		s.redirect(c, "/login")
		c.Abort()
		return
	}
	var u db.User
	if err := s.db.First(&u, id).Error; err != nil || u.SessionVer != ver {
		sess.Clear() // 用户已删或密码已重置：清掉 cookie 回登录页
		_ = sess.Save()
		s.redirect(c, "/login")
		c.Abort()
		return
	}
	c.Set("user", &u)
}

func (s *Server) requireAdmin(c *gin.Context) {
	if !s.currentUser(c).IsAdmin {
		c.String(http.StatusNotFound, "404") // 对普通用户不暴露该页存在
		c.Abort()
	}
}

// ownedAccount 取当前用户名下的账号，不是自己的返回 nil。
func (s *Server) ownedAccount(c *gin.Context, id string) *db.TgAccount {
	var acc db.TgAccount
	if err := s.db.Where("id = ? AND user_id = ?", id, s.currentUser(c).ID).First(&acc).Error; err != nil {
		c.String(http.StatusNotFound, "404")
		return nil
	}
	return &acc
}
