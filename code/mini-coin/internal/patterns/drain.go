package patterns

import (
	"context"
	"time"
)

// PageResult 表示一页的处理结果。
type PageResult struct {
	Rows int
}

// DrainPages 逐页处理数据，支持上下文取消和批间等待。
//
// next 每次读取并处理下一批；返回 Rows == 0 表示处理完成。
// 每批之间等待 delay，避免持续给数据库和从库复制造成压力。
func DrainPages(
	ctx context.Context,
	delay time.Duration,
	next func(context.Context) (PageResult, error),
) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		page, err := next(ctx)
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}
		if page.Rows == 0 {
			return nil
		}
		if delay <= 0 {
			continue
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
