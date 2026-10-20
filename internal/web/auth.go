package web

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"tgchannel/internal/db"
)

func (s *Server) loginPage(c *gin.Context)    { s.render(c, "login.html", nil) }
func (s *Server) registerPage(c *gin.Context) { s.render(c, "register.html", nil) }

func (s *Server) login(c *gin.Context) {
	name, pass := strings.TrimSpace(c.PostForm("username")), c.PostForm("password")
	if len(name) > 64 {
		name = name[:64]
	}
	// 先按来源 IP 总量限（换用户名也逃不掉），再按 IP+账号限，防枚举和 bcrypt 耗 CPU
	if !s.limit.allow("login-ip:"+c.ClientIP(), 60, time.Minute) || !s.limit.allow("login:"+c.ClientIP()+":"+name, 10, time.Minute) {
		flash(c, "尝试太频繁，一分钟后再试")
		s.redirect(c, "/login")
		return
	}
	var u db.User
	err := s.db.Where("username = ?", name).First(&u).Error
	if err != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pass)) != nil {
		flash(c, "账号或密码错误")
		s.redirect(c, "/login")
		return
	}
	s.push.SyncNtfyUser(u, pass)
	s.setLogin(c, &u)
	s.redirect(c, "/accounts")
}

func (s *Server) register(c *gin.Context) {
	name, pass := strings.TrimSpace(c.PostForm("username")), c.PostForm("password")
	if !s.limit.allow("register:"+c.ClientIP(), 5, time.Minute) {
		flash(c, "注册太频繁，一分钟后再试")
		s.redirect(c, "/register")
		return
	}
	if len(name) < 3 || len(name) > 64 {
		flash(c, "账号 3 到 64 位")
		s.redirect(c, "/register")
		return
	}
	u, err := CreateUser(s.db, name, pass, false)
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		flash(c, "账号已存在")
		s.redirect(c, "/register")
		return
	}
	if err != nil {
		flash(c, err.Error())
		s.redirect(c, "/register")
		return
	}
	s.push.SyncNtfyUser(*u, pass)
	s.setLogin(c, u)
	s.redirect(c, "/accounts")
}

func (s *Server) logout(c *gin.Context) {
	sess := sessions.Default(c)
	sess.Clear()
	_ = sess.Save()
	s.redirect(c, "/login")
}

func (s *Server) setLogin(c *gin.Context, u *db.User) {
	sess := sessions.Default(c)
	sess.Set("uid", u.ID)
	sess.Set("sv", u.SessionVer)
	_ = sess.Save()
}

// hashPassword 校验长度（bcrypt 上限 72 字节）并生成哈希。
func hashPassword(pass string) (string, error) {
	if len(pass) < 6 || len(pass) > 72 {
		return "", errors.New("密码 6 到 72 个字节")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CreateUser 建用户；admin 种子也走这里。
func CreateUser(g *gorm.DB, name, pass string, admin bool) (*db.User, error) {
	hash, err := hashPassword(pass)
	if err != nil {
		return nil, err
	}
	u := &db.User{Username: name, PasswordHash: hash, IsAdmin: admin}
	return u, g.Create(u).Error
}
