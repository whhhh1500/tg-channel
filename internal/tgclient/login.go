package tgclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"gorm.io/gorm"
	"rsc.io/qr"

	"tgchannel/internal/db"
)

// LoginJob 一次扫码登录流程，页面轮询它的状态。
type LoginJob struct {
	ID     string
	UserID uint

	mu        sync.Mutex
	State     string // qr / password / checking / syncing / done / error
	Msg       string
	QR        []byte // PNG
	QRVer     int
	AccountID uint
	password  chan string
	cancel    context.CancelFunc
}

func (j *LoginJob) set(state, msg string) {
	j.mu.Lock()
	j.State, j.Msg = state, msg
	j.mu.Unlock()
}

// JobView 是 LoginJob 的只读快照，供页面渲染。
type JobView struct {
	ID        string
	State     string
	Msg       string
	QR        []byte
	QRVer     int
	AccountID uint
}

func (j *LoginJob) Snapshot() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JobView{ID: j.ID, State: j.State, Msg: j.Msg, QR: j.QR, QRVer: j.QRVer, AccountID: j.AccountID}
}

// SubmitPassword 提交两步验证密码。
func (j *LoginJob) SubmitPassword(pwd string) {
	select {
	case j.password <- pwd:
		j.set("checking", "")
	default:
	}
}

func (m *Manager) Job(id string) *LoginJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// CancelJobs 取消某用户所有未完成的扫码任务（删用户时用）。
func (m *Manager) CancelJobs(userID uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, j := range m.jobs {
		if j.UserID == userID {
			j.cancel()
			delete(m.jobs, id)
		}
	}
}

// StartLogin 发起扫码登录，5 分钟内不完成自动取消。
// 登录成功后这条连接直接转为账号的常驻连接：同一把 auth key 绝不能同时开两个连接，
// 否则 Telegram 会以 AUTH_KEY_DUPLICATED 吊销它。
func (m *Manager) StartLogin(userID uint) (*LoginJob, error) {
	if m.cfg.TgAPIID == 0 || m.cfg.TgAPIHash == "" {
		return nil, errors.New("服务端未配置 TG_API_ID / TG_API_HASH")
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	job := &LoginJob{ID: hex.EncodeToString(b), UserID: userID, State: "qr", password: make(chan string, 1)}
	ctx, cancel := context.WithCancel(m.ctx)
	job.cancel = cancel

	// 同一用户只保留一个进行中的任务：检查和登记在同一个临界区里，并发点两次也只起一个
	m.mu.Lock()
	for _, j := range m.jobs {
		if j.UserID == userID {
			if st := j.Snapshot().State; st != "done" && st != "error" {
				m.mu.Unlock()
				cancel()
				return j, nil
			}
		}
	}
	m.jobs[job.ID] = job
	m.mu.Unlock()
	time.AfterFunc(10*time.Minute, func() {
		m.mu.Lock()
		delete(m.jobs, job.ID)
		m.mu.Unlock()
	})

	time.AfterFunc(5*time.Minute, func() { // 只有还没登录成功才超时
		if s := job.Snapshot().State; s != "done" && s != "syncing" {
			cancel()
		}
	})

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		if err := m.runLogin(ctx, job); err != nil && job.Snapshot().State != "done" && job.Snapshot().State != "syncing" {
			if ctx.Err() != nil {
				err = errors.New("二维码已过期，请重新添加")
			}
			job.set("error", err.Error())
		}
	}()
	return job, nil
}

// switchStorage 登录阶段存内存，拿到账号 ID 后切到数据库，gotd 之后的 session 更新直接落库。
type switchStorage struct {
	inner atomic.Pointer[session.Storage]
}

func (s *switchStorage) use(st session.Storage) { s.inner.Store(&st) }
func (s *switchStorage) LoadSession(ctx context.Context) ([]byte, error) {
	return (*s.inner.Load()).LoadSession(ctx)
}
func (s *switchStorage) StoreSession(ctx context.Context, data []byte) error {
	return (*s.inner.Load()).StoreSession(ctx, data)
}

// switchHandler 登录阶段直接用 dispatcher，登录后换成带补洞的 updates 管理器。
type switchHandler struct {
	inner atomic.Pointer[telegram.UpdateHandler]
}

func (s *switchHandler) use(h telegram.UpdateHandler) { s.inner.Store(&h) }
func (s *switchHandler) Handle(ctx context.Context, u tg.UpdatesClass) error {
	return (*s.inner.Load()).Handle(ctx, u)
}

func (m *Manager) runLogin(ctx context.Context, job *LoginJob) error {
	d := tg.NewUpdateDispatcher()
	loggedIn := qrlogin.OnLoginToken(d)
	storage := &switchStorage{}
	storage.use(&session.StorageMemory{})
	handler := &switchHandler{}
	handler.use(d)
	client := m.newClient(storage, handler)

	return client.Run(ctx, func(ctx context.Context) error {
		_, err := client.QR().Auth(ctx, loggedIn, func(_ context.Context, t qrlogin.Token) error {
			code, err := qr.Encode(t.URL(), qr.M)
			if err != nil {
				return err
			}
			code.Scale = 10 // rsc.io/qr 的 Image() 不认 Scale，PNG() 才认
			job.mu.Lock()
			job.QR, job.QRVer = code.PNG(), job.QRVer+1
			job.mu.Unlock()
			return nil
		})
		// 开了两步验证：gotd 可能给语义错误，也可能直接透出 RPC 错误码
		if errors.Is(err, auth.ErrPasswordAuthNeeded) || tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			err = askPassword(ctx, job, client)
		}
		if err != nil {
			return err
		}

		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		var owner db.User
		if m.db.Where("id = ?", job.UserID).Limit(1).Find(&owner); owner.ID == 0 {
			return errors.New("用户已被删除")
		}
		// 同一 TG 账号重新扫码：先停掉旧连接和它的重连定时器，免得它用旧 key 覆盖刚写入的 session
		var old db.TgAccount
		m.db.Where("user_id = ? AND tg_user_id = ?", job.UserID, self.ID).Limit(1).Find(&old)
		if old.ID != 0 && !m.stopAndWait(old.ID, 15*time.Second) {
			return errors.New("旧连接还没退出，请稍后重试")
		}
		data, err := storage.LoadSession(ctx)
		if err != nil {
			return err
		}
		enc, err := encrypt(m.cfg.MasterKey, data)
		if err != nil {
			return err
		}
		acc, err := saveAccount(m.db, job.UserID, self, enc)
		if err != nil {
			return err
		}
		storage.use(&dbSession{db: m.db, masterKey: m.cfg.MasterKey, accountID: acc.ID})

		// 立刻登记为常驻连接：从这一刻起 Start 不会再为这个账号另开连接
		m.listen(acc.ID, d)
		st := &dbState{db: m.db, accountID: acc.ID}
		gaps := updates.New(updates.Config{Handler: d, Storage: st, AccessHasher: st})
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		r := &running{client: client, cancel: cancel, ready: make(chan struct{})}
		close(r.ready)
		m.register(acc.ID, r)
		defer m.unregister(acc.ID, r)
		job.mu.Lock()
		job.AccountID = acc.ID
		job.mu.Unlock()
		job.set("syncing", "") // 已登录，超时不再取消

		// 同步对话失败不影响登录，用户可在页面上点"刷新对话"
		if _, err := syncPeers(ctx, m.db, acc.ID, client.API()); err != nil {
			slog.Warn("sync peers after login", "account", acc.ID, "err", err)
		}
		job.set("done", "")
		if m.OnAccountAdded != nil {
			m.OnAccountAdded(acc)
		}
		handler.use(gaps)

		err = gaps.Run(ctx, client.API(), self.ID, updates.AuthOptions{})
		m.afterRun(acc.ID, r, err, ctx)
		return err
	})
}

// saveAccount 同一用户重复扫同一个 TG 账号时更新原记录，否则新建。
// 不用 FirstOrCreate+Assign：它新建时不会写 []byte 字段。
func saveAccount(g *gorm.DB, userID uint, self *tg.User, sessionEnc []byte) (db.TgAccount, error) {
	acc := db.TgAccount{UserID: userID, TgUserID: self.ID}
	g.Where(acc).Limit(1).Find(&acc)
	acc.Username, acc.FirstName = self.Username, userName(self)
	acc.SessionEnc, acc.Status, acc.LastError = sessionEnc, "online", ""
	return acc, g.Save(&acc).Error
}

// askPassword 等页面提交两步验证密码，错了可以重试。
func askPassword(ctx context.Context, job *LoginJob, client *telegram.Client) error {
	job.set("password", "")
	for {
		var pwd string
		select {
		case pwd = <-job.password:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := client.Auth().Password(ctx, pwd)
		if errors.Is(err, auth.ErrPasswordInvalid) {
			job.set("password", "密码错误，请重试")
			continue
		}
		return err
	}
}
