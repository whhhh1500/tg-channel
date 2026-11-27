package db

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// 旧库只有 repeat_from / repeat_n：升级后按旧设置换算成每个等级的遍数，新账号用默认值。
func TestMigrateRepeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	old, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE tg_accounts (id integer PRIMARY KEY, user_id integer, repeat_from integer NOT NULL DEFAULT 5, repeat_n integer NOT NULL DEFAULT 3)`,
		`INSERT INTO tg_accounts (id, user_id, repeat_from, repeat_n) VALUES (1, 1, 4, 2), (2, 1, 5, 3), (3, 1, 2, 1)`,
	} {
		if err := old.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	oldDB, _ := old.DB()
	oldDB.Close()

	g, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint]string{1: "11121", 2: "11113", 3: "11111"}
	var accs []TgAccount
	g.Order("id").Find(&accs)
	for _, a := range accs {
		if a.Repeats != want[a.ID] {
			t.Errorf("账号 %d 换算成 %q，应为 %q", a.ID, a.Repeats, want[a.ID])
		}
	}
	// 再次打开不能重算（用户之后改的设置要保留）
	g.Model(&TgAccount{}).Where("id = 1").Update("repeats", "21111")
	sqlDB, _ := g.DB()
	sqlDB.Close()
	g, err = OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var a TgAccount
	g.First(&a, 1)
	if a.Repeats != "21111" {
		t.Errorf("重启后被重算了: %q", a.Repeats)
	}
	n := TgAccount{UserID: 2}
	g.Create(&n)
	g.First(&n, n.ID)
	if n.Repeats != "11113" {
		t.Errorf("新账号默认应为紧急 3 遍: %q", n.Repeats)
	}
	sqlDB, _ = g.DB()
	sqlDB.Close()
}
