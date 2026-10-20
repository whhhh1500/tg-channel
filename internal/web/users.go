package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"tgchannel/internal/db"
)

// 用户管理，仅 admin。

type userRow struct {
	db.User
	Accounts int
}

func (s *Server) usersPage(c *gin.Context) {
	q := s.db.Table("users").
		Select("users.id, users.username, users.is_admin, users.created_at, (SELECT count(*) FROM tg_accounts WHERE tg_accounts.user_id = users.id) AS accounts")
	if after, _ := strconv.Atoi(c.Query("after")); after > 0 {
		q = q.Where("users.id > ?", after)
	}
	var rows []userRow
	q.Order("users.id").Limit(pageSize + 1).Scan(&rows)
	next := uint(0)
	if len(rows) > pageSize {
		rows = rows[:pageSize]
		next = rows[len(rows)-1].ID
	}
	s.render(c, "users.html", gin.H{"Users": rows, "Next": next, "Paged": c.Query("after") != ""})
}

func (s *Server) createUser(c *gin.Context) {
	name, pass := strings.TrimSpace(c.PostForm("username")), c.PostForm("password")
	if len(name) < 3 || len(name) > 64 {
		flash(c, "账号 3 到 64 位")
		s.redirect(c, "/admin/users")
		return
	}
	u, err := CreateUser(s.db, name, pass, c.PostForm("admin") == "on")
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		flash(c, "账号已存在")
	} else if err != nil {
		flash(c, err.Error())
	} else {
		s.push.SyncNtfyUser(*u, pass)
		flash(c, "已创建 "+name)
	}
	s.redirect(c, "/admin/users")
}

// resetPassword 改平台密码；ntfy 没有改密接口，删掉重建并重新授权 topic。
func (s *Server) resetPassword(c *gin.Context) {
	var u db.User
	if s.db.Where("id = ?", c.Param("id")).First(&u).Error != nil {
		c.String(http.StatusNotFound, "404")
		return
	}
	pass := c.PostForm("password")
	hash, err := hashPassword(pass)
	if err != nil {
		flash(c, err.Error())
		s.redirect(c, "/admin/users")
		return
	}
	// 同时把会话版本 +1：该用户所有已登录的浏览器立刻失效
	if err := s.db.Model(&u).Updates(map[string]any{"password_hash": hash, "session_ver": gorm.Expr("COALESCE(session_ver, 0) + 1")}).Error; err != nil {
		flash(c, "重置失败："+err.Error())
		s.redirect(c, "/admin/users")
		return
	}
	s.push.DeleteNtfyUser(c.Request.Context(), u.Username)
	s.push.SyncNtfyUser(u, pass)
	slog.Warn("audit: password reset", "by", s.currentUser(c).Username, "target", u.Username, "ip", c.ClientIP())
	flash(c, "已重置 "+u.Username+" 的密码，手机 ntfy 要重新登录")
	s.redirect(c, "/admin/users")
}

// deleteUser 连带注销其 TG 账号、删对话 / 渠道 / 消息、删 ntfy 用户。不能删自己。
func (s *Server) deleteUser(c *gin.Context) {
	var u db.User
	if s.db.Where("id = ?", c.Param("id")).First(&u).Error != nil {
		c.String(http.StatusNotFound, "404")
		return
	}
	if u.ID == s.currentUser(c).ID {
		flash(c, "不能删除自己")
		s.redirect(c, "/admin/users")
		return
	}
	s.tg.CancelJobs(u.ID)
	var accs []db.TgAccount
	s.db.Where("user_id = ?", u.ID).Find(&accs)
	// 注销是网络等待为主，最多 4 个并发，别让多账号用户的删除拖到超时
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, acc := range accs {
		wg.Add(1)
		sem <- struct{}{}
		go func(acc db.TgAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			s.tg.Logout(c.Request.Context(), acc.ID)
			s.deleteAccountData(acc)
		}(acc)
	}
	wg.Wait()
	s.db.Delete(&u)
	s.push.DeleteNtfyUser(c.Request.Context(), u.Username)
	slog.Warn("audit: user deleted", "by", s.currentUser(c).Username, "target", u.Username, "accounts", len(accs), "ip", c.ClientIP())
	flash(c, "已删除 "+u.Username)
	s.redirect(c, "/admin/users")
}
