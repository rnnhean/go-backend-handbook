# 第 20 章 从生产代码提取可复用模式（脱敏版）

> 目标：把真实服务里反复出现的工程写法，提炼成可以直接搬进自己项目的模式。
> **本章所有代码都经过脱敏改写**：模块名、包路径、业务名词、接口、表名、域名、账号全部替换为虚构的 `mini-coin` 项目。它们保留的是结构与设计意图，不是任何真实业务实现。

---

## 20.1 为什么不能直接抄生产代码

`cp -r` 一段生产代码看着最快，实际把四种风险一起复制走了：

| 类别 | 真实代码里常见的痕迹 | 泄露后会发生什么 |
|---|---|---|
| 身份标识 | 组织名、团队名、服务名、完整模块路径、作者邮箱 | 别人能推断组织架构、服务边界和负责人 |
| 业务语义 | 产品名、表名、字段名、状态机、接口路径、运营规则 | 暴露未公开的业务设计和商业信息 |
| 基础设施 | 内网域名、IP、端口、VPC 名称、账号、证书、面板 ID | 攻击者获得可探测目标，凭据还可能直接被使用 |
| 运行数据 | 用户 ID、手机号、订单号、金额、真实日志、SQL 样本 | 隐私、合规和数据安全问题 |

更隐蔽的是**组合泄露**。单独一个 `pay_v3` 无所谓，但“模块路径 + 表名 + 错误码 + 域名 + 发布流水线名”拼在一起，足以还原一个内部系统。把密码字段替换成 `***` 并不能解决这个问题，因为结构、命名和调用关系还在。

所以本章采用的原则不是“删几行敏感词”，而是**迁移设计，不迁移资产**：保留并发控制、事务边界、错误处理、测试方法这些可复用知识；把业务对象、基础设施和真实数据全部换成一个自洽的虚构项目 `mini-coin`。改完的代码要能独立编译、独立测试，读者不需要原系统也能理解。

判断一段内容能不能公开，可以问四个问题：

1. 把这个名字放到搜索引擎里，会不会指向具体组织或服务？
2. 知道这个地址、账号或 ID 后，能不能访问任何真实资源？
3. 这段字段和状态机是否描述了未公开的业务规则？
4. 把上下文拼起来，是否仍能猜出原系统？

任一答案不确定，就不应发布；先改写成虚构版本，再重新回答。

---

## 20.2 脱敏方法论

脱敏要分“结构改写”和“内容替换”两层。只做后者，代码仍会带着原系统的骨架；只做前者，又可能把示例改得无法运行。推荐按下面的顺序做：

1. **确定保留的知识点**：例如只保留“事务内写订单 + 加余额 + 写流水”。
2. **重建最小项目**：新建模块 `example.com/minicoin`，不要在原仓库里直接全局搜索替换。
3. **重命名领域对象**：账户 → `account`，充值订单 → `recharge_order`，用户 → `uid`，渠道 → `channel`。
4. **重写接口形状**：路径、字段、错误码全部重新设计；只保留方法职责和边界。
5. **替换基础设施**：公网示例统一使用 `example.com`，本地地址使用 `127.0.0.1`，中间件使用 Docker 本地实例。
6. **构造假数据**：ID 用 `u-1001`、`req-demo-1`，金额和状态手工设计，不复制任何真实样本。
7. **删除不可复用内容**：真实日志、工单号、监控面板 ID、告警群、内部文档链接、CI 变量、证书路径。
8. **执行自动扫描**：先扫已知敏感词和格式，再人工通读上下文。
9. **运行验证**：格式、静态检查、单测、集成测试全部通过，确认改写没有把示例改坏。
10. **二次审查**：让不熟悉原项目的人只看公开版，问他能否理解、是否会误以为这是真实系统。

### 扫描命令与结果分类

下面是**作者发布前的脱敏检查**，不是应用运行时逻辑，也不应写进业务代码：

```bash
# 1. 已知组织名、内部平台名和内部域名。把左侧示例词换成本次项目的真实禁词表。
grep -rInE '公司名|内部平台名|内部域名|corp\.example|intranet' .

# 2. 私有 IP、回环以外的内网地址和常见内部端口
grep -rInE '\b(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)[0-9.]+' .

# 3. 凭证和会话材料
grep -rInE '(password|passwd|secret|token|authorization|cookie)[[:space:]]*[:=]' .

# 4. 可疑长串。命中不等于泄露，必须人工判断
grep -rInE '[A-Za-z0-9_-]{32,}' .
```

扫描结果分三类处理：

| 分类 | 例子 | 处理 |
|---|---|---|
| 真泄露 | 真实内网域名、账号、Cookie、用户数据 | 必须删除并轮换已暴露凭据，不能只改文本 |
| 教学占位符 | `example.com`、`127.0.0.1`、`REPLACE_ME` | 保留，但在上下文中明确标注“示例” |
| 误报 | Git commit hash、测试用固定随机串、长英文句子 | 记录判断依据；必要时缩短或换成明显假值 |

结束标准不是“grep 没输出”，而是：自动扫描无未处理命中；人工通读没有真实组织、业务、地址、数据；示例项目能独立运行；测试全部通过。只要还有一项没做到，就不能发布。

---

## 20.3 模式一：服务启动与优雅退出

一个生产级 Go 服务启动通常按这个顺序：

```text
解析配置 → 初始化日志 → 初始化存储 → 初始化 tracer
→ 初始化下游客户端 → 初始化业务服务
→ 监听端口 → 注册拦截器 → 启动 HTTP 辅助端口
→ 等待信号 → 优雅关闭
```

脱敏后的骨架：

```go
package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"example.com/minicoin/internal/config"
	"example.com/minicoin/internal/logging"
	"example.com/minicoin/internal/service"
)

func main() {
	configPath := flag.String("c", "config/dev.json", "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		panic(err)
	}
	if err := logging.Init(cfg.Log); err != nil {
		panic(err)
	}

	svc, err := service.New(cfg)
	if err != nil {
		panic(err)
	}
	defer svc.Close()

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		panic(err)
	}

	grpcServer := newGRPCServer(cfg, svc.GRPCAddress())
	httpServer := newAuxHTTPServer(cfg)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Error("aux http server stopped", "err", err)
		}
	}()

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			logging.Error("grpc server stopped", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 先停止接收新请求，再关闭辅助 HTTP 服务。
	grpcServer.GracefulStop()
	_ = httpServer.Shutdown(shutdownCtx)
	svc.Close()
}
```

要记住的三件事：

1. **初始化顺序有依赖**：日志要在最前面，业务服务最后；
2. **每个资源都要有 `Close`**，用 `defer` 兜底；
3. **优雅退出要有超时**：先停止接收新请求，再等在途请求结束，最后强杀。

> **JS 类比**：像 `process.on('SIGTERM')` 里先 `server.close()` 再 `process.exit()`。区别是 Go 里连接、协程、后台任务都要自己收尾。

**怎么验证。** 启动一个故意 sleep 3 秒的请求，随后向进程发送 `SIGTERM`。正确结果是进程停止接收新连接，但旧请求在超时窗口内返回，进程最后以 0 退出。再发第二次 `SIGTERM`，应当立即终止，避免发布系统等满整个超时。

**错误案例。** 在 goroutine 里直接 `panic` 或调用 `os.Exit`；没有监听 `Shutdown` 的返回错误；`GracefulStop` 和 `Shutdown` 都没有超时。它们会让请求写到一半，或者让进程永久卡住。

**边界。** 优雅退出只解决“进程收到的信号”，不保证数据库提交成功。已接收但还没完成的请求要靠上层超时、重试和幂等兜底。

---

## 20.4 模式二：拦截器链

gRPC 的拦截器类似 Express 中间件，负责横切关注点：

```go
server := grpc.NewServer(
	grpc.ChainUnaryInterceptor(
		recoveryInterceptor,   // panic 兜底
		metricsInterceptor,    // 指标
		tracingInterceptor,    // 链路追踪
		loggingInterceptor,    // 访问日志
	),
)
```

顺序很重要：

```text
先 recovery（最外层兜底）
再 metrics（统计耗时）
再 tracing（建立 span）
最后 logging（记录业务字段）
```

**为什么 recovery 放最外层？** 内层中间件 panic 时，外层才能接住。

一个最小 recovery 拦截器：

```go
func recoveryInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			logging.Error("panic in handler",
				"method", info.FullMethod,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}
```

**它包不住子协程。** handler 里 `go func(){ ... }()` 的 panic 必须自己 recover，见 20.8。

**怎么验证。** 在测试 handler 中让 `panic("boom")` 触发，断言客户端收到 `codes.Internal`，指标中的 panic counter 增加，但测试进程仍然继续执行。再用一个正常的 handler 验证响应头、日志和 trace 没有被 recovery 拦截器弄丢。

**错误案例。** 把 recovery 放在 metrics 内层，结果 panic 发生时 metrics 还没被调用；或者只 recover 主 goroutine，却让后台任务的 panic 杀掉进程。

**边界。** 拦截器只适用于经过 gRPC server 的调用。定时任务、MQ 消费循环、自己启动的 goroutine 都要单独加恢复和退出逻辑。

---

## 20.5 模式三：并发批量加载（保持顺序 + 限制并发）

需求：一次查 30 个账户的汇总，串行太慢，全部并发又怕打爆下游。

错误写法：

```go
for _, key := range keys {
	go load(key)          // 并发无上限
}
```

正确写法在 `code/mini-coin/internal/patterns/batch.go`：

```go
func LoadBatch[T any](
	ctx context.Context,
	keys []string,
	concurrency int,
	load LoadFunc[T],
) ([]T, error) {
	results := make([]T, len(keys))
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

			value, err := load(ctx, key)
			if err != nil {
				errs[i] = err
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
```

四个关键点：

| 设计 | 作用 |
|---|---|
| 预分配 `results` | 每个协程写自己的下标，**不需要锁** |
| 带缓冲 channel 当信号量 | 限制同时访问下游的数量 |
| `errs` 也预分配 | 多个失败不会互相覆盖 |
| 结束后按顺序检查 `errs` | 返回"输入顺序下第一个错误"，结果可预测 |

### 信号量 vs 固定 worker pool

```text
信号量：为每个 key 开一个 goroutine，只有 N 个能往下走
worker pool：只开 N 个 goroutine，循环从 jobs 取任务
```

| | 信号量 | worker pool |
|---|---|---|
| 协程总数 | 等于任务数 | 固定 N 个 |
| 内存开销 | 任务多时线性增长 | 恒定 |
| 实现复杂度 | 简单 | 稍复杂 |
| 适合 | 几十到几百个任务 | 几万条数据、批量任务 |

**任务数量没有上限时，用 worker pool；任务数量有明确上界时，信号量更简单。**

**怎么验证。** 用 fake loader 记录“当前并发数”和“历史最大并发数”，跑 100 个 key、限制 8 并发，断言最大值不超过 8；让第 17 个 key 返回错误，断言最终错误稳定是第 17 个，而不是随机某个错误；取消 context 后断言不再发起新的下游调用。

**错误案例。** 在循环里直接 `go load(key)`，几千个任务瞬间打满连接池；所有协程共享一个 `err` 变量，后完成的错误覆盖先完成的；没有 `wg.Wait()` 就读取结果，得到一半零值。

**边界。** 控制的是本进程内的并发，不是下游总 QPS。多个实例同时运行时，还要在客户端、服务端或网关上做全局限流。

---

## 20.6 模式四：不可变快照替代读写锁

配置、字典、规则列表这类数据：

- 读非常频繁；
- 写很少（定时刷新）；
- 允许短暂最终一致。

如果每次读都加 `RWMutex`，读多时锁竞争仍然存在。更好的方案是**整份快照原子替换**：

```go
type Snapshot struct {
	Version int64
	Entries map[string]string
}

type SnapshotStore struct {
	value atomic.Value // *Snapshot
}

func (s *SnapshotStore) Load() *Snapshot {
	v, ok := s.value.Load().(*Snapshot)
	if !ok {
		return nil
	}
	return v
}

func (s *SnapshotStore) Store(next *Snapshot) {
	s.value.Store(next)
}
```

刷新流程：

```text
查询数据 → 构造完整新快照 → 原子 Store
```

**铁律：Store 之后绝不能再修改 `Entries`。**

因为读方拿到的就是这份 map，你原地改它，读方会在无锁状态下看到半更新数据，甚至触发并发读写 panic。

> **JS 类比**：React/Redux 里"不要原地改 state，构造新对象再替换引用"。思路一样。

失败时怎么办？

```text
查询失败 → 不发布半成品，保留旧快照
查询为空 → 发布空快照（表示数据确实被清空）
```

这两种情况必须区分，否则"下游挂了"会被误判成"数据全没了"。

**怎么验证。** 开 20 个 reader goroutine 持续 `Load`，一个 writer 每毫秒 `Store` 一份新快照，在 `go test -race` 下运行；同时让 reader 验证每次读到的 `Entries` 都对应同一个版本，不能出现半更新。刷新失败时让 loader 返回 error，断言旧快照仍然可用；刷新成功但返回空集合时，断言新快照确实为空。

**错误案例。** `Store(snapshot)` 之后又执行 `snapshot.Entries["x"]="y"`；多个 goroutine 同时原地修改 map；为了省内存复用旧 map 但没做深拷贝。这些都会让读者拿到正在被改的数据。

**边界。** 快照模式适合“读多写少、可以短暂接受旧值”的字典/规则。需要严格串行写、跨多个对象保持一致性的数据，不能只靠一个 `atomic.Value`。

---

## 20.7 模式五：上下文感知的分批处理

刷数据、迁移、回填，最怕两件事：

1. 一批太大，锁表或主从延迟爆炸；
2. 上游取消了，任务还在傻跑。

`DrainPages` 把两件事一起解决：

```go
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
		if page.Rows == 0 {
			return nil
		}

		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}
```

要点：

- **每批之前检查 `ctx.Err()`**；
- **批间等待用 `select` 而不是 `time.Sleep`**，否则取消后还要睡完；
- **`timer.Stop()`** 防止 timer 泄漏；
- **`Rows == 0` 是唯一的正常结束条件**。

用的时候：

```go
err := patterns.DrainPages(ctx, 50*time.Millisecond,
	func(ctx context.Context) (patterns.PageResult, error) {
		return backfillOnePage(ctx, 1000)
	})
```

**怎么验证。** 让 `next` 先返回 3 页数据再返回 0 行，断言恰好调用 4 次；在第 2 页后取消 context，断言函数在 50ms 内返回 `context.Canceled`，而不是继续跑完。把每页耗时、处理行数和错误写进日志，出现半途退出时才能知道停在哪一页。

**错误案例。** 把 `Rows == 0` 当作“出错”；把 0 行当成“这页没有数据，下一批可能还有”，造成无限循环；批间用 `time.Sleep`，取消后还要睡完；在同一个事务里处理所有页，锁住整张表。

**边界。** 这个函数只负责节奏和取消，不负责幂等和断点续跑。刷数据脚本要按主键游标、可重入、可重复执行；执行到一半失败后，从头跑不能重复写业务结果。

---

## 20.8 模式六：子协程 panic 保护

gRPC 的 recovery 拦截器只包住 handler 主调用链：

```go
func (s *Server) Handler(ctx context.Context, req *pb.Req) (*pb.Resp, error) {
	go func() {
		panic("boom")   // ❌ 整个进程退出
	}()
	return &pb.Resp{}, nil
}
```

正确做法：

```go
func SafeGo(handler PanicHandler, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				handler(r, debug.Stack())
			}
		}()
		fn()
	}()
}
```

使用：

```go
patterns.SafeGo(func(r any, stack []byte) {
	logging.Error("panic in background goroutine", "panic", r, "stack", string(stack))
	metrics.BackgroundPanicTotal.Inc()
}, func() {
	doWork()
})
```

`recover` 必须写在**发生 panic 的那个 goroutine 的 defer 里**，外层协程 recover 不到内层。

**怎么验证。** 在 `SafeGo` 里触发 panic，等待回调写入一个 channel；断言回调收到 panic value 和 stack，主进程继续运行。再让回调本身不 panic，避免“处理 panic 的代码又 panic”。

**错误案例。** 只写 `defer recover()` 但 recover 返回值不处理；在子 goroutine 外层加 `defer`；把 panic 吞掉后继续处理被破坏的状态。业务 goroutine panic 通常意味着状态不完整，应该记录、计数并让这个任务失败，而不是假装成功。

**边界。** recover 是最后一道防线，不是参数校验工具。能用 `error` 表达的失败继续用 `error`；只有无法避免的运行时 panic 才需要兜底。

---

## 20.9 模式七：配置、日志、指标一起设计

一个请求出问题时，你需要三样东西：

| 工具 | 回答的问题 |
|---|---|
| 日志 | 这一次请求发生了什么 |
| 指标 | 整体是不是变慢了、错多了 |
| 链路追踪 | 慢在哪一跳、跨了哪些服务 |

最小接入：

```go
// 日志：结构化，能按字段检索
logging.Info("recharge created",
	"request_id", requestID,
	"uid", uid,
	"amount_cent", amountCent,
	"cost_ms", cost.Milliseconds(),
)

// 指标：可聚合、可报警
rechargeTotal.WithLabelValues("success").Inc()
rechargeDuration.Observe(cost.Seconds())

// 追踪：把 request_id 放进 span，跨服务传递
span, ctx := opentracing.StartSpanFromContext(ctx, "create_recharge")
defer span.Finish()
```

**三者的字段名要对齐**：日志里的 `request_id`、指标标签、trace 的 tag 用同一个名字，排障时才能串起来。

**怎么验证。** 发一个请求，拿到 `request_id`，然后分别按这个 ID 查日志、查 trace，再在 Grafana 里看同一分钟的 route QPS/错误率。三处应该能互相证明“同一个请求经过了同一条路径”，而不是三套互不相关的数据。

**错误案例。** 日志里每行拼一个随机 ID，trace 里不写这个 ID；指标 label 直接写用户 ID 或完整 URL，导致时间序列爆炸；请求失败但没有记录业务结果，只有 HTTP 500。指标不能回答“哪个用户失败”，日志不能回答“全站错了多少”，trace 不能回答“长期趋势”。

**边界。** 高基数 ID 只放日志/trace，不放 Prometheus label。日志正文不记录密码、完整 token、身份证号、银行卡号等敏感信息。

---

## 20.10 模式八：依赖注入，而不是到处 import 单例

不推荐：

```go
func (s *Service) Recharge(ctx context.Context, req *Req) error {
	// 直接依赖全局变量
	return globalDB.Exec(...)
}
```

推荐：

```go
type Repository interface {
	CreateOrder(ctx context.Context, order Order) error
	GetAccount(ctx context.Context, uid string) (Account, error)
}

type Service struct {
	repo     Repository
	clock    Clock
	metrics  *Metrics
}

func NewService(repo Repository, clock Clock, metrics *Metrics) *Service {
	return &Service{repo: repo, clock: clock, metrics: metrics}
}
```

好处：

- 测试时替换成内存实现，不用连真实数据库；
- 依赖关系显式，读代码就知道它依赖什么；
- 不会出现"跑单测还要起一整套中间件"。

**怎么验证。** 给 `Service` 传入一个返回固定错误的 fake `Repository`，断言错误被包装回上层；用一个 fake `Clock` 固定时间，断言订单时间和测试预期一致；并发测试中不要让两个 goroutine 共享可变 fake 状态而不加锁。

**错误案例。** 构造函数里偷偷 `new` 一个全局数据库客户端，导致测试永远连真实环境；接口定义得太大，包含几十个方法，fake 实现非常痛苦；把依赖注入理解成“所有东西都塞进一个容器”，反而看不出真实依赖。

**边界。** 依赖注入不是必须用框架。小项目显式传参就够；只有当对象很多、构建图很复杂时才考虑容器。接口应由使用方定义，尽量小。

---

## 20.11 脱敏后的完整代码在哪

本章的可运行代码在：

```text
code/mini-coin/internal/patterns/
├── batch.go
├── snapshot.go
├── drain.go
├── safego.go
└── patterns_test.go
```

运行：

```bash
cd code/mini-coin
go test ./internal/patterns -v
go test ./... -race     # 建议在 CI 或新容器里跑
```

这些代码全部是虚构的，但结构直接来自生产实践：信号量限流、预分配槽位、按输入顺序返回错误、不可变快照、可取消的分批处理、子协程 panic 保护。

---

## 20.12 习题

### 🟢 基础题

**1.** `LoadBatch` 为什么不需要给 `results` 加锁？

**2.** 为什么 `DrainPages` 的批间等待不能直接用 `time.Sleep`？

**3.** `Snapshot.Store` 之后为什么不能再修改 `Entries`？

### 🟡 动手题

**4.** 给 `LoadBatch` 加一个"并发上限必须 ≥ 1"的校验，并写测试验证 `concurrency = 0` 时的行为。

**5.** 把 `DrainPages` 改造成"最多处理 N 页就停"的版本，并补测试。

**6.** 用 `httptest` 模拟一个会 panic 的后台任务，验证 `SafeGo` 能捕获 panic 且不影响主进程。

### 🔴 综合题

**7.** 用 `LoadBatch` 实现"批量查 100 个账户余额，最多 8 个并发，任一失败返回错误"，并写表驱动测试覆盖：全部成功、部分失败、上下文取消三种情况。

**8.** 给 `SnapshotStore` 写一个并发测试：一个 goroutine 持续 `Store`，多个 goroutine 持续 `Load`，在 `-race` 下运行应无数据竞争。

---

## 20.13 参考答案

1. 每个 goroutine 只写 `results[i]`，切片在启动 goroutine 前已经分配好，所有 `i` 指向不同内存位置。`go test -race` 仍要跑，因为一旦误把 `append(results, ...)` 写成共享切片追加，结论就不成立了。验证时把实现故意改成共享 append，race detector 应报告数据竞争。

2. `time.Sleep` 不能响应取消，上游已经超时、用户已经离开或服务正在优雅退出时，它仍然会睡完。正确写法是 `timer := time.NewTimer(interval)`，在 `select` 中监听 `ctx.Done()` 和 `timer.C`。测试用 `context.WithCancel`，在等待中取消，断言函数在一个很短的超时内返回 `context.Canceled`，而不是继续处理下一页。

3. `Store` 时构造全新的 map，最后通过一次指针/原子替换发布；读方拿到的是不可变快照，只读所以不需要加锁。若读方原地修改 `Entries`，会出现半更新状态和并发 map 读写 panic。验证并发测试：一个 goroutine 持续 `Store` 新 map，多个 goroutine 持续 `Load` 并遍历，在 `go test -race -count=20` 下应无 race。不要复制 map 后再把副本暴露给外部修改。

4. 函数入口校验 `concurrency < 1`，返回 `ErrInvalidConcurrency` 或明确归一到 1；推荐显式返回错误，避免调用方把 bug 隐藏起来。表驱动测试覆盖 `-1/0/1/8`：非法值返回错误，合法值返回顺序正确的结果。若产品决定归一到 1，则测试要断言不 panic、不死锁，并且并发槽位从未超过 1。

5. 增加 `maxPages int`，循环开始先检查 `processed >= maxPages` 或 `ctx.Err()`，每处理一页递增并记录 `lastCursor`。测试构造 10 页、上限 3 页，断言只调用 3 次、返回游标可继续下一页、取消时返回 `context.Canceled`。不要用页数推算主键。

6. `SafeGo` 的 goroutine 入口必须 `defer recover()`，捕获后记录脱敏后的 panic 和可选堆栈，再通过 channel 或回调通知测试。测试里让任务 panic，等待完成信号，并断言主 goroutine 仍可执行、错误被记录、其他 worker 未被影响。不要吞掉所有错误后让程序看起来正常；运行手册要规定什么 panic 必须告警。

7. 用固定 worker/semaphore 包装 `QueryBalance(ctx, uid)`，结果按输入下标写入预分配切片。测试三组：全部成功时 100 个结果顺序和输入一致；第 7 个返回错误时总调用受限且错误包含该 uid；取消时不再启动新查询并尽快返回。再跑 `go test -race`，并检查任何 goroutine 都不会向已关闭 channel 发送。

8. 启动一个写 goroutine 循环 `Store` 新快照，8 个读 goroutine 循环 `Load`、遍历并校验总和；用 `sync.WaitGroup` 控制生命周期，测试结束关闭停止信号。命令为 `go test ./internal/patterns -run TestSnapshotStore -race -count=20`。如果实现里保留可变 map 并在外部再次修改，race detector 应报错；这是测试有效的证据。
