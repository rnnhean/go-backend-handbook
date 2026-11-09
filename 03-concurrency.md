# 第 3 章 并发：goroutine、channel、context 与数据竞争

> 目标：能写出正确的并发代码，知道什么时候用 channel、什么时候用锁，能用 `-race` 和 pprof 找到并发 bug。
>
> JS 的 `async/await` 是单线程事件循环：两个 `await` 之间不会真的同时改内存。Go 的 goroutine 可以跑在不同 CPU 核上，**共享变量是真的同时被读写**。这是本章最重要的区别。

---

## 3.1 进程、线程、协程

| 概念 | 是什么 | 隔离性 | 切换成本 |
|---|---|---|---|
| 进程 | 运行中的程序 | 独立地址空间 | 高 |
| 线程 | OS 调度单元 | 共享进程内存 | 中 |
| goroutine | Go 运行时调度的轻量协程 | 共享进程内存 | 低 |

一个 Go 进程启动时有一个主 goroutine。`go f()` 创建新 goroutine，由 Go 调度器映射到 OS 线程上。

goroutine 初始栈约 2KB，可增长；阻塞时让出线程。所以开几万个 goroutine 很正常，但不代表可以无限开——每个 goroutine 都占内存，泄漏会拖垮进程。

```go
func main() {
    go func() {
        fmt.Println("goroutine")
    }()
    fmt.Println("main")
    time.Sleep(100 * time.Millisecond) // 等 goroutine 输出
}
```

输出顺序不保证：可能是 `main` 先，也可能 `goroutine` 先。**不要用 Sleep 等待 goroutine**，生产代码用 `sync.WaitGroup` 或 channel。

---

## 3.2 第一个并发程序：`sync.WaitGroup`

```go
func main() {
    var wg sync.WaitGroup

    for i := 1; i <= 3; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            fmt.Println("worker", id)
        }(i)
    }

    wg.Wait()
    fmt.Println("done")
}
```

可能的输出：

```
worker 2
worker 1
worker 3
done
```

顺序不保证，但 `done` 一定最后。

### 循环变量陷阱

Go 1.22 之前：

```go
for i := 0; i < 3; i++ {
    go func() { fmt.Println(i) }()
}
```

可能全部打印 3，因为闭包共享同一个 `i`。修复：

```go
go func(i int) { fmt.Println(i) }(i)
```

Go 1.22 起每次迭代有独立变量，但仍建议显式传参，意图更清晰。

---

## 3.3 数据竞争：`go test -race`

```go
var counter int

func main() {
    var wg sync.WaitGroup
    for i := 0; i < 1000; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            counter++ // 数据竞争
        }()
    }
    wg.Wait()
    fmt.Println(counter) // 通常小于 1000
}
```

`counter++` 不是原子操作：读、加、写三步可能交错。

### 用 mutex

互斥锁保护的是“共享状态的一个临界区”，不是整段业务。进入临界区前获取锁，离开时立即释放，锁内不要做网络调用、写日志或等待 channel；否则其他 goroutine 会被无关工作阻塞。`defer Unlock` 能避免提前 return 漏解锁，但也可能把锁持有到函数末尾，临界区够短时更直观。

```go
var (
    mu      sync.Mutex
    counter int
)

func inc() {
    mu.Lock()
    defer mu.Unlock()
    counter++
}
```

### 用 atomic

原子操作适合单个整数或指针的读改写，例如计数器、开关和快照引用。它保证这一次操作不可分割，但不自动保证多条原子操作组成的事务一致；需要同时更新余额和流水时，仍要靠锁、数据库事务或不可变快照。不要把复杂 map 的所有字段都改成一个巨大的 atomic 值。

```go
var counter atomic.Int64

func inc() {
    counter.Add(1)
}
```

### 用 race detector

```bash
go test -race ./...
go run -race .
```

输出会指出哪两个 goroutine 在哪个文件行发生读写冲突。

注意：`-race` 只检测**实际执行到**的路径。测试没覆盖的并发路径发现不了。

---

## 3.4 channel：goroutine 之间的通信

```go
ch := make(chan int) // 无缓冲

go func() {
    ch <- 42 // 阻塞，直到有人接收
}()

v := <-ch // 接收
fmt.Println(v)
```

### 无缓冲 vs 有缓冲

无缓冲 channel 是同步交接：发送方必须等到接收方准备好；有缓冲 channel 允许在容量内先放入消息，但容量满时仍会阻塞。选择取决于是否需要背压，而不是“哪个性能好”。有缓冲队列如果无限堆积，只是把过载延迟到内存耗尽，必须同时设置容量、超时和拒绝策略。

```go
unbuffered := make(chan int)      // 发送和接收必须同时准备好
buffered := make(chan int, 2)    // 缓冲 2 个，满了才阻塞
```

- 无缓冲：同步交接，适合“必须等对方收到”；
- 有缓冲：解耦生产/消费，适合 worker pool。

### 关闭 channel

channel 的所有权必须明确：通常由发送方关闭，接收方只读。接收方关闭会让发送方 panic；多个发送方时，任意一个直接 close 也可能让其他发送方 panic。常见做法是用 `context` 取消，或者由一个协调者等待所有发送方结束后再关闭。

```go
ch := make(chan int, 3)
go func() {
    defer close(ch)
    for i := 0; i < 3; i++ {
        ch <- i
    }
}()

for v := range ch {
    fmt.Println(v)
}
```

规则：

- **只有发送方 close**；
- 不要向已关闭的 channel 发送（panic）；
- 从已关闭的 channel 接收会立即返回零值，`v, ok` 的 `ok=false`；
- 不要 close 一个还有发送者的 channel。

### 常见错误：往无人接收的 channel 发送

这个例子会触发运行时死锁检测，因为所有 goroutine 都阻塞且没有任何退出路径。真实服务里更常见的是“临时无人接收但进程还有其他 goroutine”，它不会立刻报死锁，而是静默泄漏。发送端应使用 `select` 同时等待接收和 `ctx.Done()`，并为 channel 明确所有权。

```go
ch := make(chan int)
ch <- 1 // fatal error: all goroutines are asleep - deadlock!
```

---

## 3.5 select：多路等待与超时

```go
select {
case v := <-ch:
    fmt.Println("got", v)
case <-time.After(500 * time.Millisecond):
    fmt.Println("timeout")
case <-ctx.Done():
    fmt.Println("canceled:", ctx.Err())
}
```

`select` 同时等多个 channel，哪个先就绪走哪个。多个同时就绪时随机选一个。

### 非阻塞发送/接收

```go
select {
case ch <- v:
    // 发送成功
default:
    // 缓冲满/无人接收，走这里
}
```

适合“不要阻塞主流程”的场景，如上报指标。

---

## 3.6 context：取消与超时

```go
ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
defer cancel()

select {
case <-ctx.Done():
    fmt.Println(ctx.Err()) // context deadline exceeded
case <-time.After(1 * time.Second):
    fmt.Println("done")
}
```

输出：`context deadline exceeded`。

### 在业务里传递

```go
func (s *Service) Recharge(ctx context.Context, req Request) error {
    if err := s.repo.Save(ctx, req); err != nil {
        return err
    }
    return nil
}
```

数据库/HTTP/gRPC 调用都用带 ctx 的版本：

```go
db.QueryContext(ctx, ...)
http.NewRequestWithContext(ctx, ...)
```

### context 的规则

`context` 用于传递取消、超时和少量请求级元数据，不用于传业务参数。它应该作为函数的第一个参数，并沿调用链继续传下去；不要把 `context.Background()` 藏在深层函数里，那会切断上层取消。

- 每个可能阻塞的调用都应接收 ctx；
- `cancel` 必须调用（`defer cancel()`），否则泄漏定时器；
- `context.WithValue` 只放请求级元数据（trace_id、uid），不要放业务参数；
- 不要把 context 存进结构体；
- 不要用 `context.Background()` 在业务函数里新建，要从上层传。

---

## 3.7 worker pool：限制并发

不要为每个任务开一个 goroutine 然后无限制地打数据库：

```go
// 危险：100 万任务 = 100 万 goroutine + 100 万 DB 请求
for _, id := range ids {
    go process(id)
}
```

正确：固定 worker 数。

```go
func ProcessAll(ctx context.Context, ids []int64, workers int) error {
    jobs := make(chan int64)
    errCh := make(chan error, workers)
    var wg sync.WaitGroup

    for i := 0; i < workers; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for id := range jobs {
                if err := process(ctx, id); err != nil {
                    select {
                    case errCh <- err:
                    default:
                    }
                }
            }
        }()
    }

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

    wg.Wait()
    close(errCh)

    for err := range errCh {
        return err
    }
    return nil
}
```

要点：

- worker 数固定，保护下游；
- `ctx.Done()` 让任务可以取消；
- 错误通过 channel 收集，不让 worker 阻塞；
- 发送方 close jobs。

---

## 3.8 errgroup：收集错误并取消其余任务

```go
import "golang.org/x/sync/errgroup"

func loadAll(ctx context.Context, ids []string) error {
    g, ctx := errgroup.WithContext(ctx)

    for _, id := range ids {
        id := id
        g.Go(func() error {
            return loadOne(ctx, id)
        })
    }

    return g.Wait()
}
```

任意一个任务返回错误，`ctx` 被取消，其他任务应尽快退出。适合并行请求多个下游、批量加载。

---

## 3.9 并发下的正确性：不漏、不重

### 不漏

“不漏”指每个启动的 goroutine 都有明确退出条件。常见退出路径包括任务完成、channel 关闭、context 取消和下游返回错误；无限制接收的任务要设置并发上限，否则故障时反而会创建更多协程拖垮进程。

- 任务失败要能被感知：errgroup / errCh；
- 不要 `go func(){ _ = do() }()` 然后丢掉错误；
- 进程退出前要等待在途任务（`WaitGroup` / 优雅退出）。

### 不重

“不重”指同一个任务不能被多个消费者同时处理，或重复执行要安全。需要基于唯一键、数据库条件更新或消息幂等，而不是仅靠本地 mutex；本地锁只能保护单个进程。

- 消费消息：业务唯一键 + 唯一索引；
- 定时任务：分布式锁或选主；
- 重试：幂等键 + 状态机。

### 并发充值为什么难

```sql
-- 错：读-改-写
SELECT balance FROM account WHERE uid = ?; -- 两个请求都读到 100
UPDATE account SET balance = 200 WHERE uid = ?; -- 都写 200，丢了 100
```

正确：让数据库做原子更新。

```sql
UPDATE account SET balance = balance + ?
WHERE uid = ? AND status = 1;
```

或乐观锁：

```sql
UPDATE account SET balance = balance + ?, version = version + 1
WHERE uid = ? AND version = ?;
```

或事务里 `SELECT ... FOR UPDATE`。

再加幂等：`recharge_order.req_id` 唯一索引。

---

## 3.10 sync 原语

### Mutex / RWMutex

```go
var (
    mu   sync.RWMutex
    data map[string]string
)

func Get(k string) string {
    mu.RLock()
    defer mu.RUnlock()
    return data[k]
}

func Set(k, v string) {
    mu.Lock()
    defer mu.Unlock()
    data[k] = v
}
```

读多写少用 RWMutex；临界区尽量短，不要在锁里做网络调用。

### Once

`sync.Once` 保证初始化函数最多执行一次，即使多个 goroutine 同时调用。它适合加载全局只读配置、注册一次性资源，不适合需要按 key 初始化或可能失败的长期状态；Once 内部 panic 后，后续调用也不会重新执行，因此初始化函数必须自己把错误处理清楚。

```go
var once sync.Once

func initConfig() {
    once.Do(func() {
        // 只执行一次
    })
}
```

### sync.Map

适合读多写少、key 生命周期短的场景；大多数业务用 `map + RWMutex` 更直观。

### atomic

```go
var n atomic.Int64
n.Add(1)
fmt.Println(n.Load())
```

只适合简单计数/标志，不要用 atomic 保护复杂结构。

---

## 3.11 常见的并发 bug

| 现象 | 原因 | 排查 |
|---|---|---|
| 结果偶尔少 | 共享变量未加锁 | `-race` |
| panic: send on closed channel | 多方 close | 只让发送方 close |
| 死锁 | 锁顺序不一致/无人接收 | goroutine profile |
| goroutine 泄漏 | 永远阻塞 | pprof goroutine |
| 内存上涨 | goroutine 泄漏/大对象 | heap profile |
| CPU 100% | 忙等/自旋 | CPU profile |
| 请求卡死 | 无超时 | ctx + trace |

goroutine 泄漏例子：

```go
func leak() {
    ch := make(chan int)
    go func() {
        ch <- 1 // 永远没人接收
    }()
}
```

排查：

```bash
go tool pprof http://127.0.0.1:6060/debug/pprof/goroutine
```

---

## 3.12 并发不是越多越快

并发提升吞吐的前提是下游能承接、CPU 有并行空间、任务确实在等待。对数据库连接、远程 API 和磁盘做无限并发，只会把等待放大成超时、连接池耗尽和重试风暴。正确做法是先测出单实例容量，再给并发设置上限、队列和背压。

- goroutine 太多会占内存、增加调度开销；
- 下游连接池有限，开太多只会排队；
- 数据库 CPU 可能先被打满；
- 用 worker pool/信号量限制并发；
- 用压测找最佳并发，不要拍脑袋。

---

## 习题

### 基础题

1. 进程、线程、goroutine 的区别？
2. 为什么 `counter++` 在并发下会丢更新？
3. 无缓冲 channel 和有缓冲 channel 的区别？
4. 为什么只能由发送方 close channel？
5. `select` 里加 `case <-ctx.Done()` 有什么作用？
6. `context.WithValue` 为什么不适合传业务参数？
7. `sync.Mutex` 和 `atomic` 各自适合什么？
8. goroutine 泄漏是什么？举两个例子。

### 动手题

9. 用 `WaitGroup` 启动 10 个 goroutine，每个打印自己的编号，等全部完成后输出 done。
10. 用 worker pool 处理 1000 个任务，10 个 worker，收集错误。
11. 写一个带超时的函数：500ms 内没完成就返回 `context.DeadlineExceeded`。
12. 用 mutex 和 atomic 分别实现并发安全计数器，并用 `-race` 验证。

### 综合题

13. 两个请求并发给同一账户充值 100，如何保证不丢更新、不重复加钱？写出 SQL、事务和幂等设计。

---

## 参考答案

<details>
<summary>展开</summary>

1. 进程有独立地址空间；线程共享进程内存，由操作系统调度；goroutine 由 Go runtime 调度，初始栈小、创建和切换成本低，但数量仍受内存和下游容量限制。

2. `counter++` 不是原子操作，包含读取旧值、加一、写回。两个 goroutine 可能都读到 1，再都写 2，最终少加一次。

3. 无缓冲 channel 要求发送和接收同步交接；有缓冲 channel 在容量内可先放入，满了仍阻塞。缓冲不是无限队列，容量必须有上限和背压。

4. 接收方不知道是否还有其他发送者；接收方 close 会让发送方发送到关闭 channel 而 panic。多个发送者时应由协调者等待全部发送完成后 close，或使用 context 取消。

5. `select` 中等待 `ctx.Done()` 可以让请求取消或超时后退出阻塞，避免 goroutine 永久卡住和泄漏。

6. `context.WithValue` 没有类型安全，键和值容易冲突，调用链上隐式依赖难追踪；它只适合 trace_id、uid 等请求级元数据，业务参数应显式传参。

7. Mutex 适合保护复杂结构或多个字段的一致更新；atomic 适合单个整数/指针的原子读改写。atomic 不能自动让多条操作组成一个事务。

8. goroutine 泄漏是 goroutine 永远阻塞无法退出。例如向无人接收的 channel 发送、select 没有 ctx 退出分支、HTTP 调用没有超时、worker 等待一个永远不会关闭的 channel。用 goroutine profile 观察数量持续上涨。

9.

```go
var wg sync.WaitGroup
for i := 1; i <= 10; i++ {
	wg.Add(1)
	go func(id int) {
		defer wg.Done()
		fmt.Println(id)
	}(i)
}
wg.Wait()
fmt.Println("done")
```

输出顺序不固定，这是并发调度决定的；`done` 必须在全部 goroutine 完成后输出。用 `go run -race .` 验证没有数据竞争。

10.

```go
func ProcessAll(ctx context.Context, ids []int64, workers int) error {
	if workers <= 0 { return errors.New("workers must be positive") }
	jobs := make(chan int64)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if err := process(ctx, id); err != nil {
					select {
					case errCh <- fmt.Errorf("process %d: %w", id, err):
					default:
					}
				}
			}
		}()
	}

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

	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}
```

测试 1000 个任务、10 个 worker，断言全部成功；取消 context 后断言函数能及时返回且不留下阻塞 worker。

11.

```go
func WithTimeout500ms(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

接受 `context.DeadlineExceeded`，并让内部函数继续使用传入 ctx；如果它不响应取消，goroutine 仍可能泄漏，所以真实下游必须使用带 ctx 的 DB/HTTP 调用。

12.

```go
type MutexCounter struct {
	mu sync.Mutex
	n  int64
}
func (c *MutexCounter) Add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

type AtomicCounter struct{ n atomic.Int64 }
func (c *AtomicCounter) Add() { c.n.Add(1) }
```

让 1000 个 goroutine 各加 1，两个计数器最终都应为 1000；执行 `go test -race ./... -count=1`。失败时先看 race 输出的读写栈，不要把 `-race` 没过误判成机器问题。

13. 在一个事务里按固定顺序完成：

```sql
BEGIN;
INSERT INTO recharge_order(req_id, uid, amount_cent, channel, status)
VALUES (?, ?, ?, ?, 0);
-- 若 req_id 唯一键冲突：读取原订单，比较 uid/amount_cent/channel；
-- 参数一致返回原结果，不一致 ROLLBACK 并返回 REQ_ID_CONFLICT。
SELECT id, balance, status FROM account WHERE uid = ? FOR UPDATE;
UPDATE account
   SET balance = balance + ?, version = version + 1
 WHERE uid = ? AND status = 1;
INSERT INTO recharge_flow(uid, order_id, delta, balance_after) VALUES (?, ?, ?, ?);
UPDATE recharge_order SET status = 1, balance_after = ? WHERE req_id = ?;
COMMIT;
```

不丢更新靠 `FOR UPDATE` 或条件更新；不重复加钱靠 `req_id` 唯一索引和同事务订单状态。失败场景：事务超时结果未知时按 req_id 查询；唯一键冲突不能直接返回成功，必须比较参数；任何 SQL 失败都回滚，不能让订单和余额处于不一致状态。

</details>
