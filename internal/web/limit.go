package web

import (
	"sync"
	"time"
)

// limiter 简单的滑动窗口限速：key 在 window 内最多 max 次。只在内存，进程重启归零。
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	last time.Time
}

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}} }

func (l *limiter) allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.last) > time.Minute { // 每分钟顺手清一遍过期 key
		for k, ts := range l.hits {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > window {
				delete(l.hits, k)
			}
		}
		l.last = now
	}
	ts := l.hits[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > window {
		i++
	}
	ts = ts[i:]
	if len(ts) >= max {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	return true
}
