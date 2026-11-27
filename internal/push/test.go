package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"tgchannel/internal/db"
)

// TestStrategy 按账号的发送策略发一条测试通知：等级取策略里选的等级，遍数按连发设置。
// 不进队列、不记消息记录。第一遍同步发，返回发成功的渠道数和各渠道错误，好让页面马上提示；
// 补发的几遍在后台按 urgentGap 间隔发完。level 是等级名，只用于通知正文。
func (s *Service) TestStrategy(ctx context.Context, acc db.TgAccount, level string) (int, error) {
	var chs []db.Channel
	if err := s.db.Where("account_id = ? AND enabled = ?", acc.ID, true).Find(&chs).Error; err != nil {
		return 0, err
	}
	if len(chs) == 0 {
		return 0, ErrNoChannel
	}
	prio, times := acc.RepeatFrom, repeatTimes(acc.RepeatFrom, acc.RepeatFrom, acc.RepeatN)
	title := "🔔 发送策略测试"
	body := fmt.Sprintf("等级「%s」，发 %d 遍，每遍隔 %d 秒。收到 %d 条说明设置生效。", level, times, int(urgentGap/time.Second), times)
	if times == 1 {
		body = fmt.Sprintf("等级「%s」，不连发，只发 1 遍。", level)
	}
	extra := map[string]any{"priority": prio}

	var ok []db.Channel
	var errs []error
	for _, ch := range chs {
		if err := s.sendTo(ctx, ch, title, body, nil, extra); err != nil {
			errs = append(errs, fmt.Errorf("%s：%w", ch.Kind, err))
			continue
		}
		ok = append(ok, ch)
	}
	if len(ok) > 0 && times > 1 {
		go func() { // 请求早已返回，用独立的 context
			for i := 1; i < times; i++ {
				time.Sleep(urgentGap)
				for _, ch := range ok {
					if err := s.sendTo(context.Background(), ch, title, body, nil, extra); err != nil {
						slog.Warn("test repeat", "account", acc.ID, "kind", ch.Kind, "err", err)
					}
				}
			}
		}()
	}
	return len(ok), errors.Join(errs...)
}
