package tgclient

import (
	"context"
	"testing"

	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"

	"tgchannel/internal/db"
)

// 复现线上 session_enc 为 NULL：建号时 Assign 写入，之后 StoreSession 回写。
func TestSessionRoundTrip(t *testing.T) {
	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	key := "master"
	enc, _ := encrypt(key, []byte(`{"Version":1,"Data":{"AuthKey":"x"}}`))

	self := &tg.User{ID: 42, FirstName: "n", Username: "u"}
	acc, err := saveAccount(g, 1, self, enc)
	if err != nil {
		t.Fatal(err)
	}
	again, err := saveAccount(g, 1, self, enc)
	if err != nil || again.ID != acc.ID {
		t.Fatalf("重复扫码应更新同一条: %v id=%d/%d", err, again.ID, acc.ID)
	}
	var n int
	g.Raw("select length(session_enc) from tg_accounts where id = ?", acc.ID).Scan(&n)
	if n == 0 {
		t.Fatalf("建号后 session_enc 为空（Assign 没写进去）")
	}

	st := &dbSession{db: g, masterKey: key, accountID: acc.ID}
	got, err := st.LoadSession(context.Background())
	if err != nil || len(got) == 0 {
		t.Fatalf("load: %v len=%d", err, len(got))
	}
	if err := st.StoreSession(context.Background(), []byte(`{"Version":1,"Data":{"AuthKey":"y"}}`)); err != nil {
		t.Fatal(err)
	}
	g.Raw("select length(session_enc) from tg_accounts where id = ?", acc.ID).Scan(&n)
	if n == 0 {
		t.Fatalf("StoreSession 后 session_enc 为空")
	}
	got, _ = st.LoadSession(context.Background())
	if string(got) != `{"Version":1,"Data":{"AuthKey":"y"}}` {
		t.Fatalf("回读不一致: %s", got)
	}
}

func TestStateStorage(t *testing.T) {
	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	ctx := context.Background()
	st := &dbState{db: g, accountID: 7}
	if _, found, _ := st.GetState(ctx, 0); found {
		t.Fatal("空库不该有状态")
	}
	_ = st.SetState(ctx, 0, updates.State{Pts: 1, Qts: 2, Date: 3, Seq: 4})
	_ = st.SetPts(ctx, 0, 10)
	_ = st.SetDateSeq(ctx, 0, 30, 40)
	s, found, _ := st.GetState(ctx, 0)
	if !found || s != (updates.State{Pts: 10, Qts: 2, Date: 30, Seq: 40}) {
		t.Fatalf("state = %+v found=%v", s, found)
	}
	_ = st.SetChannelPts(ctx, 0, 100, 5)
	_ = st.SetChannelPts(ctx, 0, 100, 6)
	_ = st.SetChannelPts(ctx, 0, 200, 1)
	if pts, found, _ := st.GetChannelPts(ctx, 0, 100); !found || pts != 6 {
		t.Fatalf("channel pts = %d found=%v", pts, found)
	}
	n := 0
	_ = st.ForEachChannels(ctx, 0, func(context.Context, int64, int) error { n++; return nil })
	if n != 2 {
		t.Fatalf("channels = %d", n)
	}
	other := &dbState{db: g, accountID: 8}
	if _, found, _ := other.GetState(ctx, 0); found {
		t.Fatal("状态不该跨账号")
	}
}

func TestChannelAccessHash(t *testing.T) {
	g, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := g.DB(); sqlDB.Close() })
	ctx := context.Background()
	st := &dbState{db: g, accountID: 1}
	g.Create(&db.TgPeer{AccountID: 1, Kind: "channel", PeerID: 500, AccessHash: 777, Title: "群"})
	if h, ok, _ := st.GetChannelAccessHash(ctx, 0, 500); !ok || h != 777 {
		t.Fatalf("应从 tg_peers 读到 hash: %d %v", h, ok)
	}
	if _, ok, _ := st.GetChannelAccessHash(ctx, 0, 501); ok {
		t.Fatal("未知频道不该有 hash")
	}
	_ = st.SetChannelAccessHash(ctx, 0, 501, 888)
	var p db.TgPeer
	g.Where("account_id = 1 AND kind = 'channel' AND peer_id = 501").First(&p)
	if p.AccessHash != 888 || p.Monitored {
		t.Fatalf("新频道应被插入且默认不监控: %+v", p)
	}
	_ = st.SetChannelAccessHash(ctx, 0, 500, 999)
	var p2 db.TgPeer // 不能复用 p：GORM 的 First 会把已填的主键当查询条件
	g.Where("account_id = 1 AND kind = 'channel' AND peer_id = 500").First(&p2)
	if p2.AccessHash != 999 || p2.Title != "群" {
		t.Fatalf("已有频道应只更新 hash: %+v", p2)
	}
}
