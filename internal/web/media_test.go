package web

import (
	"testing"
	"time"

	"tgchannel/internal/tgclient"
)

func TestParseRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		h        string
		from, to int64
		ok       bool
	}{
		{"bytes=0-", 0, 999, true},
		{"bytes=0-1", 0, 1, true},
		{"bytes=500-", 500, 999, true},
		{"bytes=900-5000", 900, 999, true}, // 结尾超出按文件尾
		{"bytes=-100", 900, 999, true},     // 最后 100 字节
		{"bytes=-5000", 0, 999, true},
		{"bytes=1000-", 0, 0, false}, // 起点超出
		{"bytes=5-3", 0, 0, false},
		{"bytes=0-1,5-6", 0, 0, false}, // 多段不支持
		{"bytes=-0", 0, 0, false},
		{"items=0-1", 0, 0, false},
		{"bytes=a-", 0, 0, false},
	}
	for _, c := range cases {
		from, to, ok := parseRange(c.h, size)
		if ok != c.ok || (ok && (from != c.from || to != c.to)) {
			t.Errorf("%q = %d %d %v, want %d %d %v", c.h, from, to, ok, c.from, c.to, c.ok)
		}
	}
}

func TestTickets(t *testing.T) {
	ts := newTickets()
	id := ts.add(1, tgclient.Media{Preview: "image", Size: 10})
	if len(id) != 32 {
		t.Fatalf("票据应为 32 位随机串: %q", id)
	}
	if got, ok := ts.get(id); !ok || got.account != 1 || got.media.Size != 10 {
		t.Fatalf("get = %+v %v", got, ok)
	}
	if _, ok := ts.get("nope"); ok {
		t.Fatal("不存在的票据")
	}
	ts.mu.Lock()
	v := ts.m[id]
	v.exp = time.Now().Add(-time.Second)
	ts.m[id] = v
	ts.mu.Unlock()
	if _, ok := ts.get(id); ok {
		t.Fatal("过期票据不能用")
	}
	ts.add(2, tgclient.Media{})
	if _, still := ts.m[id]; still {
		t.Fatal("插入时应清掉过期票据")
	}
}
