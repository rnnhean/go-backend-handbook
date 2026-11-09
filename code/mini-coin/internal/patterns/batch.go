// Package patterns 收录生产服务里反复出现的并发、生命周期与发布模式。
// 本包全部是教学用的虚构代码，不含任何真实业务标识。
package patterns

import (
	"context"
	"fmt"
	"sync"
)

// LoadFunc 定义单个 key 的加载函数。
type LoadFunc[T any] func(ctx context.Context, key string) (T, error)

// LoadBatch 并发加载多个 key，并保持结果与输入顺序一致。
//
//   - 最多 concurrency 个加载同时执行，用带缓冲 channel 当信号量；
//   - 每个 goroutine 只写自己的下标，不需要加锁；
//   - 所有任务结束后，按输入顺序返回第一个错误。
//
// 注意：函数会为每个 key 创建一个 goroutine。key 数量特别大时，
// 应改用固定 worker pool，而不是只靠信号量限制并发。
func LoadBatch[T any](
	ctx context.Context,
	keys []string,
	concurrency int,
	load LoadFunc[T],
) ([]T, error) {
	results := make([]T, len(keys))
	if len(keys) == 0 {
		return results, nil
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(keys) {
		concurrency = len(keys)
	}

	errs := make([]error, len(keys))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, key := range keys {
		i, key := i, key
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}

			if err := ctx.Err(); err != nil {
				errs[i] = err
				return
			}

			value, err := load(ctx, key)
			if err != nil {
				errs[i] = fmt.Errorf("load %q: %w", key, err)
				return
			}
			results[i] = value
		}()
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}
