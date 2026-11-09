package worker

import (
	"context"
	"sync"
)

// ProcessAll 用于演示: 这版正确处理了循环变量捕获。
// 训练时会故意改错成 go func() { consume(id) }() 观察 bug。
func ProcessAll(ctx context.Context, ids []int, workers int) []int {
	jobs := make(chan int)
	out := make(chan int)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				out <- id * 2
			}
		}()
	}
	go func() {
		defer close(out)
		wg.Wait()
	}()
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
	}()

	var got []int
	for v := range out {
		got = append(got, v)
	}
	return got
}
