package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"tgchannel/internal/db"
	"tgchannel/internal/tgclient"
)

func (s *Server) accountsPage(c *gin.Context) {
	var accounts []db.TgAccount
	s.db.Where("user_id = ?", s.currentUser(c).ID).Order("id").Find(&accounts)
	s.render(c, "accounts.html", gin.H{"Accounts": accounts})
}

// ---- 扫码登录 ----

func (s *Server) startLogin(c *gin.Context) {
	job, err := s.tg.StartLogin(s.currentUser(c).ID)
	if err != nil {
		flash(c, err.Error())
		s.redirect(c, "/accounts")
		return
	}
	s.redirect(c, "/accounts/new/"+job.ID)
}

func (s *Server) job(c *gin.Context) *tgclient.LoginJob {
	job := s.tg.Job(c.Param("job"))
	if job == nil || job.UserID != s.currentUser(c).ID {
		c.String(http.StatusNotFound, "404")
		return nil
	}
	return job
}

func (s *Server) loginJobPage(c *gin.Context) {
	if job := s.job(c); job != nil {
		s.render(c, "qr.html", gin.H{"Job": job.Snapshot()})
	}
}

// loginJobStatus 被页面每 2 秒轮询，返回片段；任务已过期则带回账号列表。
func (s *Server) loginJobStatus(c *gin.Context) {
	job := s.tg.Job(c.Param("job"))
	if job == nil || job.UserID != s.currentUser(c).ID {
		c.Header("HX-Redirect", s.url("/accounts"))
		c.Status(http.StatusOK)
		return
	}
	snap := job.Snapshot()
	if snap.State == "done" {
		c.Header("HX-Redirect", s.url(fmt.Sprintf("/accounts/%d", snap.AccountID)))
		c.Status(http.StatusOK)
		return
	}
	c.HTML(http.StatusOK, "qr_status", gin.H{"Job": snap})
}

func (s *Server) loginJobQR(c *gin.Context) {
	if job := s.job(c); job != nil {
		c.Header("Cache-Control", "private, max-age=600") // URL 带版本号，同一版本直接用缓存
		c.Data(http.StatusOK, "image/png", job.Snapshot().QR)
	}
}

func (s *Server) loginJobPassword(c *gin.Context) {
	if job := s.job(c); job != nil {
		job.SubmitPassword(c.PostForm("password"))
		c.HTML(http.StatusOK, "qr_status", gin.H{"Job": job.Snapshot()})
	}
}

// ---- 账号详情与渠道 ----

func (s *Server) accountPage(c *gin.Context) {
	acc := s.ownedAccount(c, c.Param("id"))
	if acc == nil {
		return
	}
	s.render(c, "account.html", gin.H{
		"Account": acc,
		"Ntfy":    s.channel(acc.ID, "ntfy"),
		"Bark":    s.channel(acc.ID, "bark"),
		"NtfyURL": s.providerURL("ntfy"),
		"BarkURL": s.providerURL("bark"),
	})
}

// channel 取账号某类渠道，每类最多一条；没有返回 nil。
func (s *Server) channel(accountID uint, kind string) *db.Channel {
	var ch db.Channel
	if s.db.Where("account_id = ? AND kind = ?", accountID, kind).First(&ch).Error != nil {
		return nil
	}
	return &ch
}

func (s *Server) providerURL(kind string) string {
	var p db.Provider
	s.db.Where("kind = ?", kind).First(&p)
	return p.BaseURL
}

var topicRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// saveNtfy 改 ntfy topic，并给用户授新 topic 的只读权限。
func (s *Server) saveNtfy(c *gin.Context) {
	acc := s.ownedAccount(c, c.Param("id"))
	if acc == nil {
		return
	}
	topic := strings.TrimSpace(c.PostForm("topic"))
	if !topicRe.MatchString(topic) {
		flash(c, "topic 只能用字母、数字、- 和 _，最长 64 位")
		s.redirect(c, fmt.Sprintf("/accounts/%d", acc.ID))
		return
	}
	// 唯一索引保证抢占原子：两个用户同时保存同一个 topic 只有一个能成功；
	// 整段（存库 + 回收旧授权 + 授新授权）串行，免得两个请求交错后旧 topic 的授权被重新加回
	s.topicMu.Lock()
	defer s.topicMu.Unlock()
	ch := db.Channel{AccountID: acc.ID, Kind: "ntfy"}
	s.db.Where(ch).Limit(1).Find(&ch)
	old := ch.Target
	ch.Target, ch.Enabled = topic, true
	err := s.db.Save(&ch).Error
	switch {
	case errors.Is(err, gorm.ErrDuplicatedKey):
		flash(c, "这个 topic 已被占用，换一个")
	case err != nil:
		flash(c, "保存失败："+err.Error())
	default:
		if old != "" && old != topic {
			s.push.RevokeRead(acc.UserID, old)
		}
		s.push.GrantRead(acc.UserID, topic)
		flash(c, "已保存，手机端订阅新 topic 即可")
	}
	s.redirect(c, fmt.Sprintf("/accounts/%d", acc.ID))
}

// saveBark 填 device key 即启用，留空即删除。
func (s *Server) saveBark(c *gin.Context) {
	acc := s.ownedAccount(c, c.Param("id"))
	if acc == nil {
		return
	}
	key := strings.TrimSpace(c.PostForm("key"))
	ch := db.Channel{AccountID: acc.ID, Kind: "bark"}
	if key == "" {
		if old := s.channel(acc.ID, "bark"); old != nil {
			s.push.ForgetPending("channel_id = ?", old.ID)
		}
		s.db.Where(ch).Delete(&db.Channel{})
		flash(c, "已关闭 Bark")
	} else {
		s.db.Where(ch).Assign(map[string]any{"target": key, "enabled": true}).FirstOrCreate(&ch)
		flash(c, "已保存")
	}
	s.redirect(c, fmt.Sprintf("/accounts/%d", acc.ID))
}

func (s *Server) syncAccount(c *gin.Context) {
	acc := s.ownedAccount(c, c.Param("id"))
	if acc == nil {
		return
	}
	n, err := s.tg.SyncPeers(c.Request.Context(), acc.ID)
	if err != nil {
		flash(c, "刷新失败："+err.Error())
	} else {
		flash(c, fmt.Sprintf("已同步 %d 个对话", n))
	}
	s.redirect(c, "/peers")
}

func (s *Server) deleteAccount(c *gin.Context) {
	acc := s.ownedAccount(c, c.Param("id"))
	if acc == nil {
		return
	}
	s.tg.Logout(c.Request.Context(), acc.ID)
	s.deleteAccountData(*acc)
	flash(c, "已删除")
	s.redirect(c, "/accounts")
}

// deleteAccountData 一个事务里删账号本身和它的对话、渠道、消息、推送记录、updates 状态，并回收 ntfy 授权。
func (s *Server) deleteAccountData(acc db.TgAccount) {
	if ch := s.channel(acc.ID, "ntfy"); ch != nil {
		s.push.RevokeRead(acc.UserID, ch.Target)
	}
	s.push.ForgetPending("message_id IN (SELECT id FROM messages WHERE account_id = ?)", acc.ID)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		id := acc.ID
		for _, q := range []string{
			"DELETE FROM push_logs WHERE message_id IN (SELECT id FROM messages WHERE account_id = ?)",
			"DELETE FROM messages WHERE account_id = ?",
			"DELETE FROM tg_peers WHERE account_id = ?",
			"DELETE FROM channels WHERE account_id = ?",
			"DELETE FROM tg_states WHERE account_id = ?",
			"DELETE FROM tg_channel_states WHERE account_id = ?",
			"DELETE FROM tg_accounts WHERE id = ?",
		} {
			if err := tx.Exec(q, id).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("delete account", "account", acc.ID, "err", err)
	}
}

func (s *Server) ownedChannel(c *gin.Context) *db.Channel {
	var ch db.Channel
	err := s.db.Joins("JOIN tg_accounts ON tg_accounts.id = channels.account_id AND tg_accounts.user_id = ?", s.currentUser(c).ID).
		Where("channels.id = ?", c.Param("id")).First(&ch).Error
	if err != nil {
		c.String(http.StatusNotFound, "404")
		return nil
	}
	return &ch
}

func (s *Server) toggleChannel(c *gin.Context) {
	if ch := s.ownedChannel(c); ch != nil {
		s.db.Model(ch).Update("enabled", !ch.Enabled)
		c.Status(http.StatusNoContent)
	}
}
