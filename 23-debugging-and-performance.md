# 第 23 章 排障、压测与生产监控：从“感觉慢”到可验证结论

> 排障不是猜，而是把症状变成证据，再把证据变成最小可复现实验。
> 本章使用脱敏后的 `mini-coin` 场景，所有命令都可以在本地或测试环境复现。

---

## 23.1 先建立排障顺序

遇到线上问题，不要第一秒就重启。先判断影响，再决定止血还是定位。

```text
1. 影响面：哪些接口、哪些用户、从什么时候开始
2. 严重度：错误率、延迟、业务损失、是否还在扩大
3. 最近变化：发布、配置、流量、依赖、数据
4. 快速止血：限流、降级、回滚、切主、扩容
5. 保留现场：日志、指标、trace、堆栈、慢查询
6. 根因分析：复现、假设、实验、验证
7. 修复与回归：测试、灰度、观察
8. 复盘：为什么没提前发现，如何加防线
```

### 5 分钟内先看什么

```text
入口 QPS 变了吗？
错误率从什么时候开始上升？
P99 是全局变慢还是某个路由变慢？
CPU、内存、磁盘、连接池是否饱和？
下游 DB/Redis/MQ 是否变慢？
最近 30 分钟有没有发布或配置变更？
```

如果错误率快速上升且影响核心链路，先回滚最近变更或摘流量，不要为了“把根因查清楚”让事故继续扩大。

---

## 23.2 把故障描述变成可执行问题

错误描述：

> 充值接口很慢，用户投诉。

可执行问题：

> `POST /v1/recharges` 从 10:03 开始 P99 从 120ms 升到 3.5s，错误率从 0.2% 升到 12%，影响所有 `channel=manual` 请求；同一时间数据库连接池使用率 98%，MySQL 出现锁等待。最近 10:00 发布过一次版本。

包含：

- 时间；
- 路由；
- 指标；
- 影响范围；
- 伴随变化；
- 最近变更。

### 故障单模板

故障单不是形式主义。它把“大家都在群里说很慢”变成可核对的事实：时间窗、影响路由、当前指标、最近变更和已经做过的止血动作。没有这些字段，排查者会在不同版本的传言里反复绕圈。模板应在告警触发时就填第一版，后续每做一个动作就补一行。

```text
标题：充值接口 P99 升高且 5xx 增加
开始时间：2026-11-01 10:03:12 +08:00
发现方式：5xx 错误率告警
影响接口：POST /v1/recharges；GET /v1/balance 正常
影响用户范围：约 18% 充值请求，主要集中在 channel=manual
当前错误率：12.4%（发布前 0.2%）
当前 P99：3.5s（发布前 120ms）
最近发布：10:00 发布 v1.8.0，镜像 digest sha256:demo123
最近配置变更：09:55 将审计服务超时从 500ms 调整为 5s
已执行止血：10:08 关闭 remote_audit 功能开关
待验证假设：事务内调远程审计，行锁持有时间变长，导致连接池排队
负责人：oncall-backend / 服务 owner
```

填写时不要写“接口挂了”“流量很高”这种无法验证的话。每条事实都要能指向一个查询结果：

- 时间要和指标、日志、发布记录使用同一时区；
- 错误率要说明统计窗口，例如“5 分钟窗口”；
- P99 要说明 route、实例和是否包含下游等待；
- “影响用户范围”要从请求数、用户数或订单数里至少选一个量化；
- “最近变更”要带版本、镜像 digest、配置项和时间；
- 待验证假设必须写成可以证明或推翻的句子。

一个反例是：

```text
充值很慢，可能是数据库问题，先重启看看。
```

它没有说明是哪个 route、从何时开始、慢多少、影响多少请求，也没有给出“数据库问题”应该出现什么指标。按这种描述排查，重启只会破坏现场，而且无法证明问题是否恢复。

---

## 23.3 日志：先确保能串起来

一个请求至少要有：

```text
request_id, trace_id, route, method, status, latency_ms,
error_code, db_time_ms, redis_time_ms, downstream_time_ms
```

### 正确日志 vs 错误日志

一条可排障日志要能回答“哪个请求、哪个用户（脱敏后）、做了什么、结果如何、耗时花在哪”。字段化之后，日志系统才能按 `request_id`、错误码和路由过滤；把整个请求体拼进字符串既检索困难，又可能泄漏 token 和个人信息。

```go
// 好：结构化、字段可检索、敏感信息脱敏
logger.Info("recharge completed",
    zap.String("request_id", reqID),
    zap.String("uid", maskUID(uid)),
    zap.Int64("amount_cent", amountCent),
    zap.Int64("balance_after", balance),
    zap.Duration("latency", time.Since(start)),
)
```

```go
// 坏：拼字符串、打印完整请求体、缺少关联 ID
log.Printf("user %s recharge %+v success", uid, req)
```

### 日志排查命令

排查时先限定时间窗和 request_id，再按错误码聚合。直接从几十 GB 的日志文件 `tail -f` 会漏掉历史记录，还可能把磁盘 IO 打满。下面的命令假设日志是 JSON Lines；如果不是，先确认字段名和转义格式。

```bash
# 所有错误
rg '"level":"error"' app.log | tail -100

# 按 request_id 追踪
rg 'trace-demo-001' app.log

# 找数据库错误
rg 'db_error|deadlock|timeout' app.log | tail -100

# 统计错误码
rg -o '"error_code":"[^"]+"' app.log | sort | uniq -c | sort -nr
```

日志量太大时，不要无脑 `tail -f` 全量文件。先按时间、request_id、error_code、uid 哈希分片过滤。

---

## 23.4 指标：从症状定位到层

### 应用层

应用指标用来回答“请求有没有进来、成功没有、慢在哪条路由、Go 运行时是否异常”。先看按 `route` 分组的错误率和延迟，能避免总量被健康路由稀释；再看 goroutine 和 GC，判断是否出现阻塞或分配压力。所有标签都必须是可控的枚举，不能把 `uid`、订单号等业务 ID 放进去。

```promql
# QPS
sum(rate(http_requests_total[1m])) by (route)

# 5xx 错误率
sum(rate(http_requests_total{status=~"5.."}[5m])) by (route)
/
sum(rate(http_requests_total[5m])) by (route)

# P99
histogram_quantile(0.99,
  sum(rate(http_request_duration_seconds_bucket[5m])) by (le, route))

# goroutine 数
go_goroutines

# GC 暂停时间
rate(go_gc_duration_seconds_sum[5m])
```

### 资源层

资源指标必须和流量放在同一时间轴看。CPU 高同时 QPS 高，可能只是容量接近上限；QPS 没变而 CPU 持续升高，才更像死循环、异常重试或 GC。内存要看 RSS、heap inuse 和 GC 后基线，不能只看操作系统 `free`。

```promql
process_resident_memory_bytes
process_cpu_seconds_total
go_threads
go_memstats_heap_inuse_bytes
```

### 依赖层

请求总耗时远高于应用自身 CPU 时间时，瓶颈通常在连接池、数据库锁、Redis 延迟或下游 RPC。依赖层指标要覆盖错误率、延迟分位和饱和度：只有错误率没有延迟，会错过“还没报错但已经排队”的早期信号；只有平均延迟，会漏掉长尾请求。

```promql
# 连接池
db_pool_in_use / db_pool_max

# Redis 错误率
rate(redis_errors_total[5m])

# MQ 积压
kafka_consumergroup_lag

# 下游耗时
histogram_quantile(0.99,
  sum(rate(downstream_duration_seconds_bucket[5m])) by (le, service))
```

### 排查原则

指标不是越多越好。先建立一条从用户症状向下钻的路径：入口错误率/延迟 -> 具体 route -> 应用资源 -> 数据库/Redis/MQ -> 具体实例和版本。每一步都要问“这个指标异常能否解释上一层现象”，不能看到 CPU 高就认定它是根因。

- 先看入口，再看依赖；
- 先看比例，再看绝对量；
- 先看时间序列，再看单个点；
- 指标命名和标签低基数，绝不能把用户 ID、请求 ID 放进 label。

---

## 23.5 链路追踪：找到慢在哪一段

一条 trace 可能长这样：

```text
POST /v1/recharges                3200ms
├── auth middleware                  2ms
├── RechargeService                3180ms
│   ├── SELECT account FOR UPDATE 3100ms
│   └── UPDATE account               8ms
└── response serialize               1ms
```

说明不是 Go 计算慢，而是数据库行锁等待。需要继续查：

- 谁持有锁；
- 锁的是哪一行；
- 持锁事务做了什么；
- 是否有长事务或慢查询；
- 是否在同一事务中调用远程服务。

### Trace 标签

```text
service.name
http.method
http.route
http.status_code
db.system=mysql
db.operation=UPDATE
rpc.system=grpc
request.id
```

不要记录完整 token、密码、身份证号和高基数字段。

---

## 23.6 Go 程序调试：先跑测试，再跑进程

### 单元复现

单元测试是最便宜的复现工具：它固定输入、隔离时间和依赖，失败后可以反复重跑。先只跑相关测试，确认失败信息；再扩大到包和全仓，排除环境差异。`-count=1` 关闭测试缓存，`-race` 检查并发访问，二者不能互相替代。

```bash
go test ./internal/service -run TestRecharge -v -count=1
go test ./... -count=1
go test -race ./... -count=1
```

如果本地能过、测试环境失败，下一步不是继续改代码，而是对齐 Go 版本、环境变量、数据库隔离级别、时区和依赖版本。把这些差异写进复现记录。

### 编译和静态检查

编译错误、格式问题和静态检查应在复现前先清掉，否则日志会被无关问题淹没。`gofmt -l .` 有输出说明文件未格式化；`go vet` 关注可疑格式化、锁复制、不可达代码等问题；`go build ./...` 验证所有包都能编译。

```bash
gofmt -l .
go vet ./...
go build ./...
```

命令的预期结果是 `gofmt` 无输出、后两条退出码为 0。失败时先修复第一处真实错误，再重跑，不要在大量派生错误里猜。

### Delve 调试

先把问题缩成一个可重复的本地输入，再用 Delve 观察“实际走了哪个分支”。Delve 不是用来“随便看看”的，它每次暂停都会改变 goroutine 时序；在本地复现可以接受，线上 attach 则可能暂停整个进程并影响在途请求。

先确认调试器可用。`dlv version` 应正常输出版本；如果提示命令不存在，再安装固定版本，不要在生产机器上执行 `go install ...@latest`：

```bash
go version
dlv version
# 仅在开发机缺少工具时执行
go install github.com/go-delve/delve/cmd/dlv@v1.25.2
```

启动测试进程前准备一份最小输入，例如：

```json
{"req_id":"req-debug-001","amount_cent":0,"channel":"mockpay"}
```

然后从项目根目录启动。`--` 后面的参数属于被测程序，不属于 Delve：

```bash
dlv debug ./cmd/server -- --config ./config/local.json
```

进入 `(dlv)` 后按下面的顺序操作：

```text
(dlv) break internal/service/recharge.go:42
Breakpoint 1 set at 0x... for main.(*RechargeService).Recharge() ./internal/service/recharge.go:42
(dlv) continue
(dlv) args
req = service.RechargeRequest {ReqID: "req-debug-001", UID: "u-1001", Amount: 0, Channel: "manual"}
(dlv) print req.Amount
0
(dlv) next
(dlv) print err
error(*errors.errorString) *{s: "amount must be positive"}
(dlv) bt
0 ... main.(*RechargeService).Recharge(...)
1 ... main.(*HTTPHandler).createRecharge(...)
```

再用 curl 或 Postman 从另一个终端发出最小请求：

```bash
curl -sS -i -X POST 'http://127.0.0.1:8081/v1/recharges' \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -H 'X-Request-Id: req-debug-001' \
  -d '{"req_id":"req-debug-001","amount_cent":0,"channel":"manual"}'
```

预期是请求停在断点，`args` 中金额为 0，继续单步后 `err` 为金额校验错误，调用栈能说明是 handler 进入了 service。若断点一直不命中，检查三件事：源码路径是否与编译路径一致、请求是否打到了另一个端口/进程、断点行是否在没有执行的分支里。

常用命令的区别：

- `continue`：继续运行到下一个断点；
- `next`：执行当前行并停在下一行，不进入被调用函数；
- `step`：进入当前行调用的函数；
- `print expr`：查看变量或表达式；
- `goroutines`：列出所有 goroutine；
- `goroutine <id>` 后执行 `bt`：查看某个 goroutine 的栈；
- `quit`：退出调试。

不要在生产环境随意 attach 调试器。远程调试必须经过授权、隔离流量，并注意调试端口不能暴露到公网。

### 核心转储

进程崩溃且需要分析时，先确认系统是否开启 core dump：

```bash
ulimit -c
cat /proc/sys/kernel/core_pattern
```

拿到 core 后用 `dlv core` 和可执行文件分析。生产环境要考虑文件大小、权限和敏感数据。

---

## 23.7 数据竞争、死锁和 goroutine 泄漏

### 数据竞争

先构造一个必然并发的测试，而不是只看代码猜：

```go
func TestCounterRace(t *testing.T) {
    var count int
    var wg sync.WaitGroup
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            count++
        }()
    }
    wg.Wait()
    if count != 100 {
        t.Fatalf("count=%d, want 100", count)
    }
}
```

运行目标测试并让退出码保留下来：

```bash
set -o pipefail
go test -race ./internal/service -run TestCounterRace -count=10 2>&1 | tee /tmp/race.log
echo "exit=$?"
rg -n "WARNING: DATA RACE|Read at|Write at|Previous write" /tmp/race.log
```

如果确实存在竞争，预期会看到 `WARNING: DATA RACE`，后面跟着两个 goroutine 的读写栈；退出码非 0。若没有报错，只能说明这次执行路径没有观察到竞争，不能证明所有路径安全。

修复时用 `atomic.Int64` 或 `sync.Mutex`：

```go
var count atomic.Int64
count.Add(1)
```

修复后必须重跑同一命令，预期 `WARNING: DATA RACE` 消失、测试通过，并再跑受影响包的普通测试，确认没有改变业务结果。

### 死锁

死锁不能靠“程序卡住了”判断。先抓完整 goroutine 栈，再看每个 goroutine 等待的对象和谁应该唤醒它：

```bash
curl -sS 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=2' > /tmp/goroutines.txt
rg -n -C 3 'sync.runtime_Semacquire|chan receive|chan send|WaitGroup.Wait' /tmp/goroutines.txt
```

预期输出会包含多个 goroutine 的调用栈。例如 `goroutine 18` 停在 `chan receive`，它等待的 channel 只有 `goroutine 19` 能写入，而 `goroutine 19` 又在等 `goroutine 18` 持有的锁，这就形成闭环。若只看到 `select` 等待，可能是正常的空闲 goroutine，必须结合业务代码判断它是否应该退出。

最小验证方式是把并发数降到 2，并在测试里加超时：

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()
err := service.Run(ctx)
if errors.Is(err, context.DeadlineExceeded) {
    t.Fatal("deadlock: Run did not finish within 2s")
}
```

修复后同一测试必须在超时前返回；再用 goroutine profile 确认相关栈已经消失。

### goroutine 泄漏

常见原因：

- context 取消后 goroutine 没有退出；
- 向无人接收的 channel 发送；
- 等待永远不会关闭的 channel；
- 每来一个请求就 `go func()`，但没有上限；
- 下游调用没有超时。

一个可复现的泄漏写法是：

```go
func leak(ctx context.Context) {
    ch := make(chan struct{})
    go func() {
        <-ch // 没有任何路径关闭 ch
    }()
}
```

用测试确认回调结束后 goroutine 是否退出：

```go
func TestWorkerStops(t *testing.T) {
    before := runtime.NumGoroutine()
    ctx, cancel := context.WithCancel(context.Background())
    startWorker(ctx)
    cancel()
    time.Sleep(100 * time.Millisecond)
    after := runtime.NumGoroutine()
    if after >= before+1 {
        t.Fatalf("worker leaked: before=%d after=%d", before, after)
    }
}
```

正确实现要把退出条件绑定到 context 或明确的关闭信号：

```go
func startWorker(parent context.Context) {
    ctx, cancel := context.WithTimeout(parent, 300*time.Millisecond)
    defer cancel()
    go func() {
        select {
        case <-ctx.Done():
            return
        }
    }()
}
```

验证顺序是：先跑测试看到失败，再修复，最后重跑同一测试；生产上再对比修复前后的 `go_goroutines` 曲线和相同栈的 profile。每个 goroutine 都要回答：**什么条件下退出？退出时释放什么资源？**

---

## 23.8 pprof：CPU、内存和阻塞

在独立管理端口启用 pprof：

```go
import _ "net/http/pprof"

go func() {
    _ = http.ListenAndServe("127.0.0.1:6060", nil)
}()
```

### CPU profile

CPU profile 只能在有稳定负载时采。采样前确认请求正在持续到达、错误率可控，并记录采样时间和流量；否则得到的可能只是后台 GC 或偶尔的定时任务。开始后保持负载 30 秒，再用 `top` 看自身耗时、`top -cum` 看调用链。

```bash
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

进入交互：

```text
top
top -cum
list RechargeService.Recharge
web
```

例如 `top` 显示 JSON 序列化占 45%，但 `list` 发现响应里序列化了不应返回的大字段，修复方向应是缩小 DTO，而不是换更快的 JSON 库。优化后必须用同一负载复测 CPU、P99 和分配量。

### Heap profile

内存问题要先区分“当前占着”和“累计分配”。执行一次基准采样，跑一段稳定负载，再采第二次；不要只看一个时间点：

```bash
curl -sS 'http://127.0.0.1:6060/debug/pprof/heap?seconds=30' -o heap-after.pprof
go tool pprof -top -inuse_space ./bin/server heap-after.pprof
go tool pprof -top -alloc_space ./bin/server heap-after.pprof
```

预期先看 `inuse_space` 的最大对象类型和调用栈；如果 GC 后基线仍然持续上涨，再看是谁持有这些对象。`alloc_space` 高但 `inuse_space` 回到基线，通常是短生命周期对象分配多，不一定是泄漏。怀疑缓存时，同时记录缓存条目数、TTL、命中率和 `runtime.ReadMemStats` 的 `HeapInuse`。

如果确认某次改动让分配增加，用同一负载采集修复前后的 profile：

```bash
go tool pprof -base heap-before.pprof heap-after.pprof
```

预期差值为正的部分才是新增分配；修复后该函数不再出现在主要增量中，同时 RSS、P99 和 GC 次数没有恶化。

### Goroutine profile

先看 `go_goroutines` 是否随时间单调上升，再抓取栈。一次采样数量正常不代表没问题，关键看相同栈是否持续堆积，以及请求结束后 goroutine 是否退出。

```bash
curl -sS 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=2' > goroutines.txt
go tool pprof http://127.0.0.1:6060/debug/pprof/goroutine
```

在 pprof 里执行 `top`，再 `list` 热点函数。常见阻塞点是 channel、锁、数据库连接和 HTTP 调用。验证时在压测前后各采一次：

```text
top
list processRequest
goroutine <id>
bt
```

预期修复后相同业务栈的 goroutine 数量停止增长，并在停止压测后的 1～2 个采样周期内回落。若数量下降但内存不降，继续查 heap；若栈显示 `database/sql.(*DB).conn`，先查连接池和慢查询，不要直接把它归因于 goroutine 泄漏。

### Block / mutex profile

Block profile 观察 goroutine 在 channel、锁、网络等地方的阻塞；mutex profile 观察锁竞争。先在测试环境开启采样，再施加能够复现问题的负载：

```go
runtime.SetBlockProfileRate(1)          // 1 表示每次阻塞事件都记录
runtime.SetMutexProfileFraction(1)     // 1 表示记录每次锁竞争
```

```bash
curl -sS 'http://127.0.0.1:6060/debug/pprof/block?seconds=30' -o block.pprof
curl -sS 'http://127.0.0.1:6060/debug/pprof/mutex?seconds=30' -o mutex.pprof
go tool pprof -top ./bin/server block.pprof
go tool pprof -top ./bin/server mutex.pprof
```

预期输出会给出累计阻塞时间和调用栈。先找 `flat` 高的锁或 channel，再回到代码确认临界区是否包含数据库/HTTP 等慢操作。采样率会影响性能，不能长期全量开启；修复后关闭或调低采样，和修复前在相同负载下对比阻塞时间、P99 和 goroutine 数。

### 火焰图

火焰图的横轴是采样占比，不是时间轴；一条很宽的栈说明它消耗了大量采样，不一定是单次调用很久。鼠标悬停可看函数和占比，点击可下钻。适合和同事快速对齐“大头在哪”，但最终仍要回到代码和指标验证。

```bash
go tool pprof -http=:8080 http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

打开浏览器后先看 `Flame Graph`，再切到 `Top` 和 `Source` 对照。不要把“火焰图看起来红”当结论，记录采样时的 QPS、版本、实例和负载，优化前后使用相同条件。

---

## 23.9 数据库慢查询和锁等待

### 查当前连接

先确认连接究竟在“执行 SQL”还是“等锁/等网络”。`SHOW FULL PROCESSLIST` 是即时快照，间隔几秒连续看两次，能区分瞬时查询和长期占用。生产上避免一次返回过多 `Info` 内容，注意其中可能有敏感参数。

```sql
SHOW FULL PROCESSLIST;
```

关注：

- `Time` 很长；
- `State` 为锁等待；
- `Info` 是慢 SQL；
- 同一 SQL 大量重复。

### 查事务

长事务比单条慢 SQL 更危险：它可能已经执行完当前语句，却仍持有行锁不提交。按 `trx_started` 排序，找持续几十秒以上的事务，再结合 `trx_mysql_thread_id` 对照连接和业务代码。要确认事务里是否混入了 HTTP、MQ、文件上传或等待用户输入的操作。

```sql
SELECT trx_id, trx_state, trx_started, trx_mysql_thread_id, trx_query
FROM information_schema.innodb_trx
ORDER BY trx_started;
```

### 查锁等待

```sql
SELECT * FROM performance_schema.data_lock_waits;
SELECT * FROM performance_schema.data_locks;
```

字段名随 MySQL 版本略有差异，执行前确认版本。

### 慢查询日志

```sql
SET GLOBAL slow_query_log = 'ON';
SET GLOBAL long_query_time = 0.2;
```

生产不要随手全局修改，先确认容量和日志落盘策略。

### 处理顺序

不要看到锁等待就杀会话。先判断谁是阻塞者、它持有哪把锁、事务为什么这么久，以及杀掉后业务能否安全重试。处理顺序应从影响最小、可验证的动作开始：补索引/改 SQL、缩短事务、限制并发、摘流量，最后才考虑终止会话。

```text
慢 SQL 是否有索引？
索引是否被函数/隐式转换破坏？
扫描行数是否过大？
是否返回了不必要的大字段？
事务是否过长导致锁等待？
是否批量更新造成主从延迟？
是否需要归档/分区/异步任务？
```

---

## 23.10 Redis 问题排查

### 大 key

```bash
redis-cli --bigkeys
redis-cli memory usage key
```

大 key 可能导致：

- 单次操作阻塞；
- 网络传输慢；
- 删除时卡顿；
- 集群槽位不均衡。

修复：拆分 key、限制集合长度、异步删除、不要一次读取整个大 Hash/List。

### 热 key

现象：某个 key QPS 远高于其他 key，单节点 CPU 打满。

处理：

- 本地缓存；
- 读写分离/多副本；
- 热 key 拆分；
- 业务上限制刷新频率；
- 监控 key 访问分布。

### 缓存穿透/击穿/雪崩

| 问题 | 现象 | 根因 | 处理 |
|---|---|---|---|
| 穿透 | 查不存在的数据也一直打到 DB | 缓存没有记录“不存在” | 参数校验、短 TTL 空值缓存、布隆过滤器 |
| 击穿 | 单个热点 key 过期瞬间 DB 暴涨 | 并发请求同时回源 | 单飞/互斥重建、逻辑过期、热点永不过期 |
| 雪崩 | 大量 key 同时失效或 Redis 整体故障 | TTL 集中、容量/网络故障 | TTL 随机抖动、分批预热、限流降级、多副本 |

三类问题的共同点是：Redis 只是加速层，必须有数据库兜底和回源保护。回源时还要限制并发、设置超时，并避免缓存空值导致的新数据长期不可见。

### Redis 不要当唯一事实来源

缓存丢数据可以重建；账户余额不能只存在 Redis。核心数据必须有数据库事务和持久化备份。

---

## 23.11 消息队列积压

指标：

- consumer lag；
- 生产速率；
- 消费速率；
- 单条处理耗时；
- 重试次数；
- 死信数量。

排查顺序：

1. 是生产突增还是消费变慢？
2. 消费线程是否卡在下游 DB/HTTP？
3. 是否有毒消息反复重试？
4. 消费者实例是否变少？
5. 分区数是否限制并发？
6. 是否需要临时扩容消费者？

### 积压处理原则

积压是生产速率大于消费速率的结果，但原因可能是消费者挂了、单条处理变慢、分区数不足或毒消息阻塞。先画出两边速率和 lag 斜率：生产稳定而消费下降，修消费；消费者数量下降，查崩溃/rebalance；单条耗时变长，查下游；毒消息反复重试，隔离到死信队列。

- 不要无脑增加消费者，先确认下游能承受；
- 毒消息进入死信队列，不能让整个分区停住；
- 消费必须幂等；
- 重试要有上限和退避；
- 补数据任务和正常消费隔离资源。

---

## 23.12 网络、DNS、TLS

### 分层检查

网络问题要按 DNS、TCP、HTTP、TLS 四层验证。`curl -v` 能看到 DNS 解析、连接建立、TLS 握手和 HTTP 响应，但输出里可能包含 cookie 和内部地址，发给别人前要脱敏。`nc` 只证明端口可达，不证明 HTTP 路径和证书正确。

```bash
# 1. DNS：应该返回 A/AAAA 记录；NXDOMAIN 或空结果不要继续猜 TCP
dig api.example.com
getent hosts api.example.com

# 2. TCP：应该看到 connected；refused 表示端口未监听或被拒绝
nc -vz -w 3 api.example.com 443

# 3. HTTP：保留状态码、耗时和重定向
curl -sS -o /dev/null -w 'code=%{http_code} dns=%{time_namelookup}s tcp=%{time_connect}s tls=%{time_appconnect}s total=%{time_total}s\n' \
  --max-time 3 https://api.example.com/healthz

# 4. TLS：看证书主题、SAN 和有效期；证书链错误时 curl 也会失败
openssl s_client -connect api.example.com:443 -servername api.example.com -showcerts </dev/null 2>&1 \
  | openssl x509 -noout -subject -issuer -dates -ext subjectAltName
```

判读顺序是固定的：DNS 没解析到地址，先查域名和解析配置；`nc` 失败，查监听、端口、防火墙和安全组；TCP 通了但 curl 失败，继续看 HTTP 状态码、代理和 TLS；HTTP 返回 502/504，则问题已经进入代理和上游链路。`curl -v` 输出可能包含 cookie、Authorization 和内部地址，贴到群里前必须删除这些字段。

### 常见现象

| 现象 | 可能原因 |
|---|---|
| `no such host` | DNS、搜索域、网络配置 |
| `connection refused` | 服务未监听、端口错、防火墙 |
| `i/o timeout` | 网络不通、丢包、后端过载 |
| `certificate expired` | 证书过期 |
| `x509: name mismatch` | 证书域名不匹配 |
| 偶发 502/504 | 代理超时、上游重启、连接池问题 |

Go HTTP 客户端必须设置超时：

```go
client := &http.Client{
    Timeout: 3 * time.Second,
    Transport: &http.Transport{
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 20,
        IdleConnTimeout:     90 * time.Second,
    },
}
```

只设置连接超时不够。还要考虑总超时、响应体读取超时和 context 取消。

---

## 23.13 Bug 复现的标准流程

### 第一步：收集事实

先固定“哪一版、什么环境、什么输入、发生时间、能否重复”。没有版本和时间的 bug 描述无法和发布、配置、数据库变更对齐；没有输入和数据快照，本地只能靠猜。事实和猜测要分栏记录，不要把它们混成一段结论。

```text
环境：本地 / 测试 / 预发 / 生产
版本：commit、镜像 digest、配置版本
输入：请求 ID、参数、用户状态、数据快照
时间：开始和结束
频率：必现 / 偶发 / 并发时出现
错误：HTTP 状态、业务码、日志、trace
```

### 第二步：最小化

最小化的目标是删除所有不必要变量，同时保留失败。每次只删一个维度，删完立刻重跑：确认失败仍存在就继续，失败消失则把它加回来。不要一次改五个条件后得到一个“好了”，那通常只是把问题藏起来了。

把问题缩到最小输入：

```text
大请求 -> 单字段
100 个并发 -> 2 个并发
真实数据库 -> 内存 fake / 单测
完整服务 -> 单个 service 方法
```

### 第三步：形成可证伪假设

错误：

> 应该是数据库慢。

正确：

> 如果数据库行锁是瓶颈，那么在同一 `uid` 上并发 20 个充值应出现 P99 激增；换成不同 `uid` 后 P99 应明显下降。用两个压测组验证。

### 第四步：修复和回归

修复前先让测试稳定失败，并把失败信息写进工单。修复只针对已证明的根因，不顺手重构无关代码；否则回归失败时无法判断是谁引入的。回归范围要覆盖原路径、相邻参数和并发边界，最后在灰度环境观察与 bug 相关的指标。

- 先写能失败的测试，确认在旧代码上失败；
- 修复最小根因，保持改动可审查；
- 跑目标测试和受影响包的全量测试；
- 用原请求和最小复现脚本再验证一次；
- 灰度观察错误率、P99、日志和业务结果；
- 记录 commit、测试输出、发布版本和恢复时间。

### 第五步：补防线

- 单测/集成测试；
- 参数校验；
- 告警；
- 超时和重试上限；
- 迁移脚本防误操作；
- 灰度发布；
- 运行手册。

---

## 23.14 QPS 压测怎么做

### 先定义目标

“能扛多少 QPS”不是完整目标，因为一个服务可以在错误率 50% 时仍返回 5000 QPS。容量目标必须同时约束吞吐、延迟、错误率、持续时间和数据正确性，还要说明测试环境规格与数据分布。否则结果无法用于扩容或发布决策。

不要只问“能扛多少 QPS”。要写：

```text
接口：POST /v1/recharges
数据：80% 已存在账户、20% 新账户；请求体 300B
目标：P99 < 300ms，错误率 < 0.1%
持续时间：10 分钟稳定 + 5 分钟峰值
压测环境：与生产同规格，数据库数据量达到生产 30% 以上
```

### 分层压测

一次只改变一个变量。先做单请求基准，确认功能和依赖正常；再做阶梯并发，找系统从线性到饱和的拐点；然后保持目标负载足够久，观察内存、GC、连接池和队列是否累积；最后做短时破坏性测试和恢复测试，确认限流和自动恢复不会把系统留在雪崩状态。

1. 基准：单请求、无并发，确认正确性；
2. 阶梯：10、50、100、200、500 并发；
3. 稳定：在目标 QPS 持续 10~30 分钟；
4. 破坏：继续加量直到错误率或延迟明显恶化；
5. 恢复：停止压力，观察系统是否恢复。

### k6 脚本

下面的脚本把“请求正确”和“容量达标”分成两道门。`check` 验证每个请求的状态码和响应结构；`thresholds` 让整次运行在不满足 P99 或错误率时返回非零退出码。每个请求使用唯一幂等键，避免压测被幂等命中伪装成写入性能。

```js
import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  stages: [
    { duration: '1m', target: 50 },
    { duration: '3m', target: 200 },
    { duration: '5m', target: 200 }, // 稳定观察期
    { duration: '1m', target: 0 },
  ],
  thresholds: {
    http_req_failed: ['rate<0.001'],
    http_req_duration: ['p(95)<150', 'p(99)<300'],
    checks: ['rate>0.999'],
  },
};

export default function () {
  const key = `k6-${__VU}-${__ITER}`;
  const body = JSON.stringify({
    req_id: key,
    amount_cent: 1,
    channel: 'manual',
  });

  const res = http.post('http://127.0.0.1:8081/v1/recharges', body, {
    headers: {
      'Content-Type': 'application/json',
      'Authorization': 'Bearer demo-token',
      'X-Request-Id': key,
    },
  });

  check(res, {
    'http status is 201': (r) => r.status === 201,
    'response has request_id': (r) => {
      try { return Boolean(r.json('request_id')); } catch (_) { return false; }
    },
  });
  sleep(0.05);
}
```

先做单请求验证，再做小流量和完整压测：

```bash
# 1. 只执行一次：这一步失败说明脚本/鉴权/数据有问题，不能继续压
k6 run --vus 1 --iterations 1 scripts/loadtest.js

# 2. 小流量：确认状态码、响应体、数据库和监控都正常
k6 run --vus 5 --duration 30s scripts/loadtest.js

# 3. 完整运行并保留机器可读结果
k6 run --out json=/tmp/k6-result.json scripts/loadtest.js
echo "k6_exit=$?"
```

单请求验证的预期是：退出码为 0，`checks` 为 100%，响应状态为 201，响应 JSON 含 `request_id`，数据库多出一笔测试订单。若脚本返回 401，先修测试 token；若返回 409，说明幂等键重复；若返回 500，先看服务端日志，不要加并发掩盖问题。

压测结束后至少核对三组事实：

```bash
# 1. k6 汇总：请求数、失败率、P50/P95/P99（字段名随 k6 版本可能不同）
k6 inspect scripts/loadtest.js >/tmp/k6-plan.json
jq -s '[.[] | select(.type=="Point")] | group_by(.metric) | map({metric: .[0].metric, points: length})' \
  /tmp/k6-result.json | tee /tmp/k6-summary.json

# 2. 业务数据：先记录起止时间，再查同一窗口的订单数和金额总和
mysql -N -e "SELECT COUNT(*), SUM(amount_cent) FROM recharge_order WHERE created_at >= '2026-11-01 10:00:00' AND created_at < '2026-11-01 10:15:00';"

# 3. 服务指标：同一时间窗的错误率、连接池和 goroutine
# 在 Prometheus/Grafana 中选择相同起止时间，记录查询表达式和结果截图。
```

不要直接用固定的 `jq` 统计值当最终报告。先看 k6 终端汇总里的 `http_reqs`、`http_req_failed`、`http_req_duration` 和 `checks`，再用服务端指标核对同一时间窗；如果 k6 的请求数和数据库新增订单数明显不一致，先解释差异，例如业务失败、幂等命中、异步落库或测试数据被其他任务写入。

若设置 `K6_WEB_DASHBOARD=true`，可以在运行后打开终端提示的地址看趋势；这只是辅助查看，原始 `result.json`、服务端指标和数据库查询仍需归档。

停止条件要在开始前写死。例如下面任意一条持续 2 分钟就停止加量：

- 5xx 错误率超过 1%；
- P99 超过目标值的 2 倍；
- 数据库连接池使用率超过 90%；
- MySQL 出现持续锁等待或慢查询快速增加；
- 压测机 CPU 超过 80%，已经无法稳定发压；
- 业务成功率低于基线，即使 HTTP 200/201 仍然很多。

停止后等待 5～10 分钟，确认错误率、P99、连接池和 goroutine 回到基线，再决定是否继续。否则下一次测试会把上一次的未恢复状态混进来。

### 压测报告的必备字段

报告要让没参加压测的人也能复现和判断容量。只贴一张 k6 截图不够，必须说明测试环境、数据规模、请求分布、压测机资源、服务端瓶颈证据和停止条件。原始结果文件与脚本一起归档，后续版本用同一条件复测。

```text
目标 QPS / 实际 QPS
P50 / P95 / P99 / 最大延迟
错误率和错误码分布
CPU/内存/连接池/goroutine
DB QPS、慢查询、锁等待
Redis QPS 和错误
MQ 生产/消费速率
瓶颈证据（pprof/trace/EXPLAIN）
优化前后对比
```

### 压测常见错误

以下错误会让结论失真，甚至把真实问题掩盖成“性能很好”：

- 只压一次、没有基线；
- 压测客户端先成为瓶颈；
- 测试数据和生产分布完全不同；
- 只看平均延迟，不看 P99；
- 没监控数据库连接池；
- 没有确定停止条件；
- 用生产环境做破坏性压测；
- 把 fake repository 的 QPS 当成 MySQL 服务 QPS。

---

## 23.15 报警设计

### 告警要分级

告警分级由“用户影响和响应时限”决定，不由指标大小决定。CPU 90% 但 SLO 仍达标，通常进看板；核心充值成功率下降 2 个百分点，即使 CPU 只有 30%，也可能需要立即响应。每级都要绑定通知渠道、负责人和升级路径，否则分级只是标签。

| 级别 | 触发 | 动作 |
|---|---|---|
| P0 | 核心接口不可用、资金错误 | 立即电话/群告警，停止变更 |
| P1 | 错误率或 P99 明显恶化 | 5 分钟内响应 |
| P2 | 容量趋势、非核心异常 | 工作时间处理 |
| P3 | 信息通知 | 记录跟进 |

### 好的告警

好的告警描述用户症状、持续时间和可执行下一步，并尽量附带 dashboard、日志查询和 runbook。只写“错误率 > 2%”不够，值班人还要知道是哪个 route、哪个环境、影响多大、先回滚还是先扩缩容。

```text
连续 5 分钟 HTTP 5xx 错误率 > 2%
持续 10 分钟充值成功数为 0
P99 > 800ms 且持续 5 分钟
数据库连接池 > 90% 持续 10 分钟
Kafka lag 持续 10 分钟增长
磁盘使用率 > 90% 且 30 分钟内不会恢复
```

### 避免告警疲劳

告警疲劳的后果是真正故障被静音。要从“每个异常都告警”改成“只有需要人采取行动的变化才告警”：使用持续时间窗口、聚合相同故障、区分症状与原因、配置依赖关系抑制，并定期清理长期无人处理的规则。

- 不要每个请求失败都发一条告警；
- 使用 `for: 5m` 防止瞬时抖动；
- 同一故障聚合成一个事件；
- 区分症状告警和原因告警；
- 设置静默、升级和值班策略；
- 每条告警都附运行手册链接和影响说明。

### SLO

例如：

```text
充值接口 30 天可用性目标 99.9%
P99 小于 300ms 的时间比例 >= 99%
错误预算 = 每月允许的失败时间
```

告警应围绕 SLO 和用户影响，而不是只看机器 CPU。

---

## 23.16 一次完整故障演练

### 场景

发布后充值接口 P99 从 120ms 升到 3 秒，5xx 增加，数据库连接池接近上限。

### 证据

演练也要按真实故障取证：先固定时间线，再把入口指标、服务指标、依赖指标、日志、trace、pprof 和最近变更放到同一张图上。证据要能支持或推翻假设，而不是事后挑几条看起来相关的日志。下面的证据链指向“事务中远程调用导致锁持有时间变长”。

1. 指标：只有 `/v1/recharges` 变慢，查询余额正常；
2. 日志：出现 `db timeout` 和 `lock wait timeout`；
3. Trace：时间集中在 `SELECT ... FOR UPDATE`；
4. 数据：最近加入了“每笔充值写一条远程审计 HTTP 请求”，且它在事务中执行；
5. pprof：Go CPU 不高，大量 goroutine 阻塞在数据库锁；
6. 最近变更：发布版本把审计调用放进了事务。

### 根因

事务持有账户行锁期间调用远程审计服务。审计服务变慢，锁持有时间变长，其他充值请求排队，连接池被占满。

### 止血

先选择影响最小的动作。若新版本刚发布且错误率持续上升，回滚通常最快；如果无法立即回滚，关闭远程审计的功能开关；入口限流只能减少新增排队，不能释放已被长事务占用的连接。每做一个动作记录时间和指标变化，避免同时操作导致无法归因。

- 回滚版本或关闭远程审计；
- 对充值入口限流，保护数据库连接池；
- 观察锁等待、连接池、错误率和业务成功率是否恢复；
- 保留新版本日志、慢查询和 trace，供后续定位。

### 修复

根因是事务持锁期间调用远程服务，所以修复要同时消除根因和补防线。只把审计超时从 5s 改成 1s 仍会在下游抖动时拖慢事务；正确做法是把审计事件写入 Outbox，事务提交后由异步 worker 投递，并对投递设置重试上限、幂等和积压告警。

- 事务只包含数据库操作；
- 审计事件写入 outbox，事务提交后异步发送；
- 下游调用设置超时、重试上限和熔断；
- 增加锁等待、事务时长、outbox 积压告警；
- 增加“事务中禁止远程调用”的评审检查项；
- 用注入 2 秒审计延迟的集成/故障测试证明充值 P99 不再被拖垮。

### 复盘

复盘要回答四个问题：同类问题为什么可能发生、为什么测试没拦住、为什么监控没提前发现、为什么影响会扩大。每个改进项要有负责人、截止时间、验证方式和关闭标准；“加强测试”“注意代码质量”不是可执行的改进项。

```text
为什么测试没发现：集成测试没有模拟下游慢调用
为什么监控没提前发现：没有事务时长指标
为什么影响扩大：连接池等待没有快速失败
如何验证修复：注入 2 秒审计延迟，充值 P99 仍低于目标
```

---

## 23.17 习题

1. 5xx 从 0.1% 升到 8%，你先看哪五个指标？
2. Go 服务 CPU 不高但 P99 很高，可能是什么问题？
3. `go test -race` 通过是否证明没有数据竞争？为什么？
4. 写出定位 goroutine 泄漏的三步命令。
5. MySQL 出现大量锁等待，你会查哪些表/命令？
6. Redis 热 key 和大 key 的区别是什么？
7. 如何判断 MQ 积压是生产变快还是消费变慢？
8. 设计一个充值接口的错误率告警，要求避免瞬时抖动误报。
9. 压测只得到“QPS 5000”，还缺哪些信息才能做容量决策？
10. 给“缓存删除失败导致余额显示旧值”写一个排障和修复方案。

---

## 参考答案

<details>
<summary>展开参考答案</summary>

1. 先把故障单填到可执行版本：开始时间、影响 route、时间窗口、最近发布。然后按这个顺序看：①入口 `rate(http_requests_total{status=~"5.."}[5m]) / rate(http_requests_total[5m])`，按 route 和 instance 分组；②P99 直方图，确认是全部接口还是单个接口；③入口 QPS，区分流量突增和同样流量下故障；④实例数、`go_goroutines`、CPU/内存/GC，排除实例消失或运行时压力；⑤MySQL/Redis/MQ 的错误率、延迟和饱和度。每一步都要写“这个证据支持哪个假设”，最后用同时间窗日志和 trace 验证，而不是看到 CPU 高就宣布根因。
2. 先按“等待型问题”排查。常见原因有 MySQL 行锁/连接池等待、下游 HTTP/RPC 超时、Redis 慢命令、goroutine 排队、GC 停顿、网络丢包和代理超时。执行 `go tool pprof http://127.0.0.1:6060/debug/pprof/goroutine` 看是否大量停在 `database/sql.(*DB).conn`、`chan receive` 或 HTTP 读取；再看 `db.Stats()` 的 `InUse`、`WaitCount`、`WaitDuration` 和 trace 的最长 span。预期正确结论必须能被实验推翻，例如“锁等待是根因”应表现为同一 uid 并发时 P99 升高、不同 uid 时明显下降。
3. 不能。`go test -race` 只检测测试实际执行到的内存访问；没有被覆盖的 goroutine、只有在生产并发下才出现的时序，可能完全检测不到。正确做法是写覆盖该并发路径的测试，在 `-race -count=10` 下重复运行，再结合代码审查、锁/共享状态设计和线上 goroutine/profile。若命令输出 `WARNING: DATA RACE`，按两个冲突栈修复后重跑；若没有警告，只能记录“本次测试未发现”，不能写成“证明无竞争”。
4. ①先在 Grafana/Prometheus 查 `go_goroutines` 是否在请求结束后仍持续上涨；②执行 `curl -sS 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=2' -o /tmp/goroutines.txt`，用 `rg -n -C 3 'chan receive|chan send|Semacquire|database/sql' /tmp/goroutines.txt` 找重复栈；③用 `go tool pprof http://127.0.0.1:6060/debug/pprof/goroutine` 执行 `top` 和 `list` 定位函数，检查每个 goroutine 是否有 context、channel 或超时退出条件。修复后停止压测并重新采样，预期相同栈不再增长、数量回落。
5. 先连续两次执行 `SHOW FULL PROCESSLIST;`，找出长时间处于 `Waiting for ... lock` 的连接；再查 `information_schema.innodb_trx` 的 `trx_started`、`trx_mysql_thread_id` 和 `trx_query`，确认谁持有长事务；通过 `performance_schema.data_lock_waits` 和 `data_locks` 找到阻塞关系及对象。然后看慢查询日志、`EXPLAIN`、索引和事务边界。不要第一步就 `KILL`：先判断阻塞者是否可回滚、被杀后业务是否会重复扣款或留下半成品。
6. 热 key 是“访问次数集中”，例如一个账号被几十万 QPS 读取；大 key 是“单个 value 或集合太大”，例如一个 Hash 存了百万成员。热 key 用 `redis-cli --hotkeys`、分片 CPU 或应用侧 key 访问统计确认，处理是本地缓存、副本/拆分、限制刷新；大 key 用 `redis-cli --bigkeys`、`MEMORY USAGE key` 确认，处理是拆 key、限制集合大小、异步删除。两者可能同时出现，但只扩容一个方向通常解决不了另一个问题。
7. 在同一时间窗画三条线：生产速率、消费速率、consumer lag；同时记录消费者实例数、单条处理耗时、重试数和下游错误。若生产速率稳定而消费速率下降，且单条耗时上升，是消费变慢；若消费速率不变但生产速率突然上升，是流量突增；若消费者实例数减少或频繁 rebalance，先查实例崩溃/探针/分区分配。扩容前确认 partition 数和数据库容量，否则消费者变多可能只是把更多并发压到下游。
8. 告警规则要同时有阈值、持续时间和最小流量，例如：
```promql
(
  sum by (route) (rate(http_requests_total{route="/v1/recharges",status=~"5.."}[5m]))
  /
  sum by (route) (rate(http_requests_total{route="/v1/recharges"}[5m]))
) > 0.02
and
sum by (route) (rate(http_requests_total{route="/v1/recharges"}[5m])) > 1
for: 5m
```
`for: 5m` 过滤瞬时抖动，`> 1 QPS` 避免低流量时 1 个错误造成 100% 错误率。还要配套业务成功数和余额流水一致性告警；验证方法是人为制造持续 5 分钟错误，确认告警触发，再制造 30 秒抖动确认不触发。
9. “QPS 5000”缺少：P50/P95/P99 和最大延迟、错误率及错误码、测试持续时间、并发/VU 模型、请求和响应体大小、读写比、鉴权方式、缓存命中率、测试数据规模与分布、服务实例规格、数据库/Redis/MQ 规格、压测机是否饱和、瓶颈证据、是否有重试/幂等命中。容量结论应写成“在 2 实例、100 万行测试数据、80% 缓存命中、P99<300ms 和错误率<0.1% 下达到 5000 QPS，数据库连接池在 70% 达拐点”，而不是只写一个数字。
10. 先查清事实：写库事务是否提交成功，缓存删除是否报错，旧值 TTL 多长，影响哪些 uid。命令上查应用日志的 `cache_delete_error`、Redis 的 key TTL 和值、数据库流水与余额；用同一 uid 重放读请求确认旧值，再手动删除 key 验证读请求恢复。止血是停止继续写坏值、主动删除受影响 key 或临时读主库；根因修复是交易提交后通过可靠事件/重试删除缓存，设置短 TTL 兜底，增加缓存陈旧率和删除失败告警，并用并发写后读测试证明最终返回新值。资金余额不能把 Redis 当唯一事实来源。

</details>
