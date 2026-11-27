package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"tgchannel/internal/db"
)

type peerRow struct {
	db.TgPeer
	Account string
}

const pageSize = 50

// peersPage 顶部按账号筛选；游标分页按 (账号 id, 对话 id) 排序，游标是上一页最后一行的 "账号id-对话id"。
func (s *Server) peersPage(c *gin.Context) {
	var accounts []db.TgAccount
	s.db.Where("user_id = ?", s.currentUser(c).ID).Order("id").Find(&accounts)
	q := s.db.Table("tg_peers").Select("tg_peers.*, tg_accounts.first_name AS account").
		Joins("JOIN tg_accounts ON tg_accounts.id = tg_peers.account_id AND tg_accounts.user_id = ?", s.currentUser(c).ID)
	selected, _ := strconv.Atoi(c.Query("account"))
	if selected == 0 && len(accounts) == 1 { // 只有一个账号时直接选中，才能显示"监听所有人"
		selected = int(accounts[0].ID)
	}
	var current *db.TgAccount
	for i := range accounts {
		if int(accounts[i].ID) == selected {
			current = &accounts[i]
		}
	}
	if selected > 0 {
		q = q.Where("tg_peers.account_id = ?", selected)
	}
	var accID, peerID int
	if n, _ := fmt.Sscanf(c.Query("after"), "%d-%d", &accID, &peerID); n == 2 {
		q = q.Where("(tg_peers.account_id, tg_peers.id) > (?, ?)", accID, peerID)
	}
	var peers []peerRow
	q.Order("tg_peers.account_id, tg_peers.id").Limit(pageSize + 1).Scan(&peers)
	next := ""
	if len(peers) > pageSize {
		peers = peers[:pageSize]
		last := peers[len(peers)-1]
		next = fmt.Sprintf("%d-%d", last.AccountID, last.ID)
	}
	var strategy []levelRepeat
	show := 5 // 发送策略下拉默认显示最高的那个连发等级，都不连发就显示紧急
	if current != nil {
		for _, l := range levels {
			n := repeatOf(current.Repeats, l.V)
			strategy = append(strategy, levelRepeat{l.V, l.L, n})
			if n > 1 {
				show = l.V
			}
		}
	}
	s.render(c, "peers.html", gin.H{"Levels": levels, "Strategy": strategy, "Show": show, "Repeats": []int{1, 2, 3, 4, 5}, "Peers": peers, "Next": next, "Paged": c.Query("after") != "", "Accounts": accounts, "Selected": uint(selected), "Current": current})
}

// togglePeer 切换对话的 monitored（监控转发）或 muted（永不监听），列名只来自路由，不来自请求。
func (s *Server) togglePeer(col string) gin.HandlerFunc {
	sql := `UPDATE tg_peers SET ` + col + ` = NOT ` + col + ` WHERE id = ? AND account_id IN (SELECT id FROM tg_accounts WHERE user_id = ?)`
	return func(c *gin.Context) {
		if s.db.Exec(sql, c.Param("id"), s.currentUser(c).ID).RowsAffected == 0 {
			c.String(http.StatusNotFound, "404")
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// levels 通知等级，对应 ntfy priority 1-5。
var levels = []struct {
	V int
	L string
}{{1, "最低"}, {2, "低"}, {3, "普通"}, {4, "高"}, {5, "紧急"}}

type levelRepeat struct {
	V int
	L string
	N int
}

var repeatsRe = regexp.MustCompile(`^[1-5]{5}$`)

// repeatOf 发送策略里等级 v 发几遍，不合法按 1。
func repeatOf(repeats string, v int) int {
	if repeatsRe.MatchString(repeats) && v >= 1 && v <= 5 {
		return int(repeats[v-1] - '0')
	}
	return 1
}

// levelName 等级的中文名，不在 1-5 返回空。
func levelName(v int) string {
	for _, l := range levels {
		if l.V == v {
			return l.L
		}
	}
	return ""
}

// setPeerPriority 对话的通知等级：0 跟随账号默认，1-5 固定。下拉框改动即保存（htmx）。
func (s *Server) setPeerPriority(c *gin.Context) {
	p, err := strconv.Atoi(c.PostForm("priority"))
	if err != nil || p < 0 || p > 5 {
		c.String(http.StatusBadRequest, "等级只能是 0-5")
		return
	}
	if s.db.Exec(`UPDATE tg_peers SET priority = ? WHERE id = ? AND account_id IN (SELECT id FROM tg_accounts WHERE user_id = ?)`,
		p, c.Param("id"), s.currentUser(c).ID).RowsAffected == 0 {
		c.String(http.StatusNotFound, "404")
		return
	}
	c.Status(http.StatusNoContent)
}

// setAccountNotify 账号的默认通知等级和连发设置，回到该账号的通知设置页（行里"跟随默认"的文字要跟着变）。
// 只改表单里带了且合法的字段。
func (s *Server) setAccountNotify(c *gin.Context) {
	acc := s.ownedAccount(c, c.Param("id"))
	if acc == nil {
		return
	}
	upd := map[string]any{}
	if p, err := strconv.Atoi(c.PostForm("priority")); err == nil && levelName(p) != "" {
		upd["priority"] = p
	}
	if r := c.PostForm("repeats"); repeatsRe.MatchString(r) {
		upd["repeats"] = r
	}
	if len(upd) > 0 {
		s.db.Model(acc).Updates(upd)
	}
	s.redirect(c, fmt.Sprintf("/peers?account=%d", acc.ID))
}

// testStrategy "测一次"：按页面上当前选的发送策略给账号的渠道发一条测试通知，返回一行结果给页面显示。
// 每个账号每分钟最多 3 次，防止连点把手机刷屏。
func (s *Server) testStrategy(c *gin.Context) {
	acc := s.ownedAccount(c, c.Param("id"))
	if acc == nil {
		return
	}
	if !s.limit.allow(fmt.Sprintf("notifytest:%d", acc.ID), 3, time.Minute) {
		c.String(http.StatusOK, "测得太频繁，过一分钟再试")
		return
	}
	// 用下拉框里当前选的值（可能还没保存），不合法的沿用已保存的
	test := *acc
	if r := c.PostForm("repeats"); repeatsRe.MatchString(r) {
		test.Repeats = r
	}
	lv, _ := strconv.Atoi(c.PostForm("level"))
	if levelName(lv) == "" {
		lv = 5
	}
	n, err := s.push.TestStrategy(c.Request.Context(), test, lv, levelName(lv))
	switch {
	case n == 0 && err != nil:
		c.String(http.StatusOK, "发送失败："+err.Error())
	case err != nil:
		c.String(http.StatusOK, "部分渠道失败："+err.Error())
	default:
		c.String(http.StatusOK, "已发出，留意手机")
	}
}

// toggleMonitorAll 切换账号的"监听所有人"，回到该账号的通知设置页。
func (s *Server) toggleMonitorAll(c *gin.Context) {
	acc := s.ownedAccount(c, c.Param("id"))
	if acc == nil {
		return
	}
	s.db.Model(acc).Update("monitor_all", !acc.MonitorAll)
	s.redirect(c, fmt.Sprintf("/peers?account=%d", acc.ID))
}

type messageRow struct {
	db.Message
	Peer      string
	PeerUser  string
	Topic     string // 接口消息的主题名和图标
	TopicIcon string
	PushState string // 最近一次推送状态
	PushAt    *time.Time
	PushError string
}

// messagesPage 游标分页：按 id 倒序，游标是上一页最后一条的 id。
func (s *Server) messagesPage(c *gin.Context) {
	q := s.db.Table("messages")
	if before, _ := strconv.Atoi(c.Query("before")); before > 0 {
		q = q.Where("messages.id < ?", before)
	}
	var rows []messageRow
	q.
		Select(`messages.*, tg_peers.title AS peer, tg_peers.username AS peer_user, api_topics.name AS topic, api_topics.icon AS topic_icon,
			pl.status AS push_state, pl.sent_at AS push_at, pl.error AS push_error`).
		Joins("JOIN tg_accounts ON tg_accounts.id = messages.account_id AND tg_accounts.user_id = ?", s.currentUser(c).ID).
		Joins("LEFT JOIN tg_peers ON tg_peers.id = messages.peer_id").
		Joins("LEFT JOIN api_topics ON api_topics.id = messages.topic_id").
		// 每条消息只取最新一条推送记录
		Joins("LEFT JOIN push_logs pl ON pl.id = (SELECT id FROM push_logs WHERE message_id = messages.id ORDER BY id DESC LIMIT 1)").
		Order("messages.id DESC").Limit(pageSize + 1).Scan(&rows)
	next := uint(0)
	if len(rows) > pageSize {
		rows = rows[:pageSize]
		next = rows[len(rows)-1].ID
	}
	s.render(c, "messages.html", gin.H{"Messages": rows, "Next": next, "Paged": c.Query("before") != "", "Retention": s.cfg.RetentionDays})
}

func (s *Server) providersPage(c *gin.Context) {
	var ps []db.Provider
	s.db.Order("kind").Find(&ps)
	s.render(c, "providers.html", gin.H{"Providers": ps})
}

func (s *Server) saveProvider(c *gin.Context) {
	p := db.Provider{
		Kind:    strings.TrimSpace(c.PostForm("kind")),
		BaseURL: strings.TrimRight(strings.TrimSpace(c.PostForm("base_url")), "/"),
	}
	if (p.Kind != "ntfy" && p.Kind != "bark") || p.BaseURL == "" {
		flash(c, "类型只能是 ntfy 或 bark，地址不能为空")
	} else {
		s.db.Where("kind = ?", p.Kind).Assign(map[string]any{"base_url": p.BaseURL}).FirstOrCreate(&p)
		flash(c, "已保存")
	}
	s.redirect(c, "/admin/providers")
}
