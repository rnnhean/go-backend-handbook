# 第 9 章 配置、日志、链路追踪与指标

> 目标：线上出问题时，你能在 5 分钟内回答：**哪个实例、哪个请求、慢在哪一层、错误是什么**。这靠的不是“多打日志”，而是配置、日志、指标、追踪四件事一起设计。

---

## 9.1 配置管理：代码一份，环境多份

### 配置分层

配置分层的目标是让同一份构建产物能在本地、测试和生产运行，同时不让敏感值进入代码。默认值只适合安全的开发值，环境变量覆盖部署差异，密钥由专门系统注入；启动时校验类型、范围和必填项。配置加载失败应直接阻止启动，而不是带病运行。

```
默认值（代码里）
  < 配置文件（configs/app.yaml）
  < 环境变量（APP_HTTP_ADDR）
  < 命令行参数（--addr）
  < 配置中心（动态开关）
```

后面的覆盖前面的。敏感配置只放环境变量/密钥管理，不进 Git。

```go
type Config struct {
    HTTP struct {
        Addr string `yaml:"addr" env:"HTTP_ADDR" default:":8080"`
    } `yaml:"http"`
    MySQL struct {
        DSN          string `yaml:"dsn" env:"MYSQL_DSN"`
        MaxOpenConns int    `yaml:"max_open_conns" env:"MYSQL_MAX_OPEN" default:"50"`
    } `yaml:"mysql"`
    Log struct {
        Level string `yaml:"level" env:"LOG_LEVEL" default:"info"`
    } `yaml:"log"`
}
```

### 原则

- 配置结构体启动时**一次性校验**：缺 DSN、端口非法直接启动失败；
- 打印配置时脱敏（密码、token 显示 `***`）；
- 动态配置要有默认值和回滚；
- 不要用配置控制核心业务分支到无法测试的程度；
- 配置变更也要走评审和灰度。

```go
func (c *Config) Validate() error {
    if c.MySQL.DSN == "" {
        return errors.New("mysql.dsn is required")
    }
    if c.MySQL.MaxOpenConns <= 0 {
        return errors.New("mysql.max_open_conns must be positive")
    }
    return nil
}
```

**启动即失败**比运行到一半才发现配置错更好。

---

## 9.2 结构化日志：能被检索，才有价值

### 不要这样打

```go
log.Printf("user %s recharge %d failed: %v", uid, amount, err)
```

问题：字段位置固定、难解析、无法按 uid 聚合、容易拼接注入。

### 用结构化日志

```go
slog.InfoContext(ctx, "recharge failed",
    "request_id", requestID,
    "uid", uid,
    "order_id", orderID,
    "amount_cent", amount,
    "error", err,
)
```

输出 JSON：

```json
{"time":"2026-11-01T10:00:00.123+08:00","level":"ERROR","msg":"recharge failed","request_id":"req-1","uid":"u-1001","order_id":"123","amount_cent":10000,"error":"mysql: timeout"}
```

### 日志规范

日志是结构化事件，不是随手 `fmt.Println`。固定字段让检索和聚合可靠，trace/request id 让一次请求可串联，错误码让统计可做；如果每条日志格式都不同，出故障时只能靠人眼 grep。

| 字段 | 必须 | 说明 |
|---|---|---|
| `time` | 是 | RFC3339，带时区 |
| `level` | 是 | debug/info/warn/error |
| `msg` | 是 | 简短、稳定、可检索 |
| `request_id` | 是 | 贯穿整个请求 |
| `trace_id` | 是 | 跨服务串联 |
| `uid` | 视情况 | 不要打手机号/身份证 |
| `error` | 错误时 | 保留错误链 |
| `duration_ms` | 关键路径 | 性能分析 |

### 什么时候打什么级别

级别要表达“谁会因为这条日志采取行动”。DEBUG 用于开发细节，INFO 记录正常状态变化，WARN 表示可恢复的异常，ERROR 表示请求失败或需要处理；不要把每个数据库 miss 都打成 ERROR，否则真正的错误会被淹没。

- `DEBUG`：开发排查细节，生产默认关闭；
- `INFO`：关键业务事件（下单成功、充值成功），不要每个循环都打；
- `WARN`：可恢复异常（重试成功、降级触发）；
- `ERROR`：需要人关注的失败（请求失败、依赖不可用）。

**不要用 ERROR 打“用户输错密码”**，那是正常业务，会淹没真正的错误。

### 不要记录敏感信息

日志会被复制到集中平台、被多人检索并长期保留，因此密码、token、cookie、身份证、银行卡和完整手机号默认不能进日志。确需排查时做掩码或哈希，并限制访问权限。

- 密码、token、cookie；
- 完整银行卡/身份证/手机号；
- 请求体里的密钥；
- 数据库连接串。

需要排查时用脱敏值：`138****1234`、`tok_***`。

### 日志采样

高流量服务不能对每个成功请求打全量详细日志。可以全量记录错误、低比例采样成功请求、对特定 request_id 强制保留；采样策略要固定并可解释，否则事故时无法还原。

高 QPS 下不要每个请求都打 INFO。可以：

- 错误全量，成功采样（如 1%）；
- 按 trace_id 采样，保证同一条链路完整；
- 慢请求全量。

---

## 9.3 链路追踪：一个请求经过了哪些服务

### 核心概念

日志回答“这一次发生了什么”，指标回答“整体趋势和异常幅度”，Trace 回答“一个请求在哪些服务/操作上花了多久”。三者用 request_id/trace_id 关联，但职责不能混：不要用日志统计 QPS，不要把用户 ID 塞进指标标签，也不要指望 trace 保存完整业务数据。

- **Trace**：一次完整请求；
- **Span**：一个操作（HTTP 调用、SQL、Redis）；
- **Trace ID**：贯穿整个链路的 ID；
- **Span ID**：单个操作 ID；
- **Parent Span**：父子关系。

```
trace-abc
├─ HTTP POST /v1/recharges (span 1, 120ms)
│  ├─ MySQL INSERT order (span 2, 20ms)
│  ├─ MySQL UPDATE account (span 3, 15ms)
│  └─ Kafka publish (span 4, 5ms)
```

### Go 里怎么用

用 OpenTelemetry：

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
)

func (s *RechargeService) Recharge(ctx context.Context, req Request) (Response, error) {
    ctx, span := otel.Tracer("recharge").Start(ctx, "Recharge")
    defer span.End()

    span.SetAttributes(
        attribute.String("request.id", req.RequestID),
        attribute.Int64("amount_cent", req.AmountCent),
        attribute.String("recharge.channel", req.Channel),
    )

    resp, err := s.repo.Create(ctx, req)
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, "create recharge failed")
        return Response{}, err
    }
    return resp, nil
}
```

这里有两个要点。第一，调用下游时必须把 `ctx` 继续传下去，不能重新用 `context.Background()`；第二，子 span 要挂在父 span 上，数据库 span 的 parent span ID 才会指向 `Recharge`。HTTP 入口和 gRPC 出口分别用标准约定的中间件/拦截器：

```go
// HTTP 入口：从 traceparent 提取上下文，初始化 server span
handler = otelhttp.NewHandler(mux, "http-server")
req = req.WithContext(ctx) // 业务层收到的是当前请求上下文

// HTTP 出口：把当前 span 上下文注入 traceparent
client = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}

// gRPC 客户端：当前 ctx 中的 trace 信息会写入 metadata
conn, err := grpc.NewClient(target,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
    grpc.WithUnaryInterceptor(otelgrpc.UnaryClientInterceptor()),
)
```

验证方法不是“代码看起来对”，而是让一次请求跨过 HTTP → gRPC → MySQL，然后在追踪平台按 `trace_id` 查：最外层 HTTP span 是根，gRPC span 的 parent 是调用它的 handler/service，SQL span 的 parent 是 gRPC 调用；每一层耗时相加应大致覆盖总耗时。若链路断成两个根 trace，优先检查 `context` 是否被替换、HTTP 出口是否注入、gRPC 是否安装拦截器。生产上不要把完整 uid、SQL 参数和 token 放进 span attribute。

### 追踪的价值

指标告诉你“哪里变慢”，trace 告诉你“这五秒花在哪个 span”。没有 trace 时，跨服务问题只能靠日志猜时间差；有 trace 才能看到网关、服务、数据库、MQ 的父子关系和各自耗时。

- 看到每个下游耗时，定位“慢在数据库还是第三方”；
- 看到调用链中的错误传播；
- 按 trace_id 查完整日志。

### 注意

trace 不是越多越好。采样率、标签基数和 span 数量都会影响成本；不要把用户 ID、完整 SQL 参数等高基数信息放进 tag，敏感字段也要脱敏。

- 采样率：全量追踪成本高，通常 1%～10%，错误请求全采；
- 不要把手机关号等敏感信息放进 span attribute；
- span 名称要稳定，不要带用户 ID（高基数）。

---

## 9.4 指标：能聚合、能告警

### 四类黄金指标

延迟、流量、错误和饱和度是服务健康的四个基本维度。延迟要看分位而不是只看平均，流量要区分总量和关键 route，错误要区分客户端错误和服务端错误，饱和度要提前于故障触发扩容或限流。

| 类型 | 例子 | 用途 |
|---|---|---|
| 流量 | QPS、请求数 | 容量 |
| 延迟 | P50/P95/P99 | 体验 |
| 错误 | 5xx 比例、业务失败率 | 正确性 |
| 饱和度 | CPU、内存、连接池、队列长度 | 瓶颈 |

### Prometheus 指标类型

Counter 只增不减，适合请求数和错误数；Gauge 可增可减，适合连接数、队列长度；Histogram 把观测值放进桶里，适合算分位；Summary 在客户端计算分位，跨实例聚合更困难。选型错误会导致查询语义错误，例如对 Gauge 直接 `rate`、用 Counter 表示当前队列长度。

- **Counter**：只增，如请求总数；
- **Gauge**：可增可减，如当前连接数；
- **Histogram**：分桶统计，算 P99；
- **Summary**：客户端分位数，聚合困难，少用。

```go
import (
    "errors"
    "net/http"
    "strconv"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/collectors"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
    httpRequests = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "http_requests_total", Help: "total requests"},
        []string{"route", "method", "status"},
    )
    httpDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "http_request_duration_seconds",
            Help:    "request duration",
            Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
        },
        []string{"route"},
    )
    registry = prometheus.NewRegistry()
)

func init() {
    registry.MustRegister(httpRequests, httpDuration)
    registry.MustRegister(collectors.NewGoCollector())
    registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

func metricsHandler() http.Handler {
    return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
```

这里最容易漏掉的是“注册”和“暴露”两件事：只创建 `CounterVec` 不会自动出现在 `/metrics`，必须注册到实际使用的 registry；同时管理端口要单独启动，不能把 `/metrics` 混在公网业务路由里。

```go
func startAdminServer() {
    mux := http.NewServeMux()
    mux.Handle("/metrics", metricsHandler())
    go func() {
        if err := http.ListenAndServe("127.0.0.1:9091", mux); err != nil &&
            !errors.Is(err, http.ErrServerClosed) {
            slog.Error("admin server stopped", "error", err)
        }
    }()
}
```

HTTP 中间件负责在请求结束时记录状态码和耗时。`route` 应来自路由模板，例如 `/v1/recharges`，不能直接用包含订单号的 `r.URL.Path`：

```go
type statusWriter struct {
    http.ResponseWriter
    status int
}

func (w *statusWriter) WriteHeader(code int) {
    w.status = code
    w.ResponseWriter.WriteHeader(code)
}

func instrument(route string, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
        next.ServeHTTP(sw, r)
        httpRequests.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).Inc()
        httpDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
    })
}
```

启动服务后做一次真实验证：

```bash
curl -sS -o /dev/null -X POST http://127.0.0.1:8081/v1/recharges \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -d '{"req_id":"metric-demo-1","amount_cent":10,"channel":"manual"}'
curl -sS http://127.0.0.1:9091/metrics | rg 'http_requests_total|http_request_duration_seconds'
```

预期能看到 `route="/v1/recharges"`、`method="POST"`、`status="201"` 的计数，以及对应的 `_bucket`、`_sum`、`_count` 序列。若 `/metrics` 返回 404，检查管理端口是否挂载了 `metricsHandler`；若没有业务指标，检查一次请求是否真的经过中间件；若整页为空，检查 registry 是否和 handler 使用的是同一个实例。

### 标签基数

**不要把 `user_id`、`request_id`、`order_id` 放进 label**。每个唯一值生成一条时间序列，几万个用户就能让 Prometheus 内存爆炸。

适合放 label 的：`route`（归一化后的路由模板）、`method`、`status`、`instance`。

```go
// 错：/v1/orders/123 和 /v1/orders/456 是两个 label
route := r.URL.Path
// 对：/v1/orders/{id}
route := routePattern(r)
```

### 业务指标

技术指标正常不代表业务成功。充值要监控成功数、失败原因和余额对账；登录要监控成功率和验证码失败率；订单要监控创建、支付、取消的漏斗。业务指标是最终用户价值的证据。

技术指标告诉你“系统活着”，业务指标告诉你“用户成功了吗”：

- 充值成功率；
- 下单转化率；
- 支付回调成功率；
- 队列积压；
- 对账差异数。

---

## 9.5 三者怎么配合

指标负责发现异常，trace 负责定位链路，日志负责解释单次事件。正确顺序是先用指标确定时间、服务和 route，再用 trace 找慢 span，最后用日志和业务数据证明根因；不要一开始就全量 grep。

一个请求出问题：

```
1. 告警：5xx 错误率 > 1%
2. 指标：定位到 route=/v1/recharges、instance=pod-3
3. 日志：按 request_id 找到具体错误
4. 追踪：按 trace_id 看到慢在 MySQL UPDATE
5. 数据库：慢查询/锁等待确认
```

没有 trace_id，日志只能按时间猜；没有指标，不知道影响面；没有日志，不知道具体错误。**四件套要一起设计。**

---

## 9.6 常见坑

这些坑的共同点是“看起来有数据，实际无法用于排障”：字段不一致、时间不同步、日志没有 request id、指标标签爆炸、trace 采样不可解释。上线前要按一次真实请求走通查询路径。

| 坑 | 后果 | 正确做法 |
|---|---|---|
| 日志没有 request_id | 无法串联 | 入口生成并透传 |
| 指标 label 放用户 ID | 基数爆炸 | 只放归一化 route/status |
| 所有日志都 ERROR | 告警疲劳 | 按级别语义打 |
| 记录密码/token | 泄漏 | 脱敏 |
| 追踪全量采样 | 成本高 | 采样 + 错误全采 |
| 配置打印不脱敏 | 泄漏 | 密码显示 `***` |
| 只监控 CPU | 看不到业务失败 | 加业务指标 |

---

## 习题

### 基础题

1. 配置的优先级通常是什么？敏感配置放哪？
2. 结构化日志至少应包含哪些字段？
3. 什么级别打什么日志？为什么用户输错密码不该打 ERROR？
4. Trace、Span、Trace ID 分别是什么？
5. Counter、Gauge、Histogram 各适合什么指标？
6. 为什么不能把 user_id 放进 Prometheus label？
7. 指标、日志、追踪分别解决什么问题？
8. 什么是采样？为什么需要？

### 动手题

9. 用 `slog` 打一条充值失败日志，包含 request_id、uid、order_id、error。
10. 给 HTTP 接口加 Prometheus 中间件，统计 QPS、P99、5xx。
11. 设计一个 trace 上下文透传方案：HTTP → gRPC → MySQL。
12. 写一个配置结构体，包含 HTTP 端口、MySQL DSN、日志级别，并做启动校验。

### 综合题

13. 线上充值接口 P99 从 100ms 升到 2s，错误率 5%。写出你用指标、日志、追踪定位问题的完整步骤。

---

## 参考答案

<details>
<summary>展开</summary>

1. 命令行 > 环境变量 > 配置文件 > 默认值；敏感配置放环境变量/密钥管理。
2. time、level、msg、request_id、trace_id、关键业务字段、error、duration。
3. DEBUG 细节、INFO 关键事件、WARN 可恢复异常、ERROR 需关注失败；输错密码是正常业务，打 ERROR 会淹没真错误。
4. Trace 是一次请求；Span 是一个操作；Trace ID 贯穿链路。
5. Counter 累计请求数；Gauge 当前连接数/队列长度；Histogram 延迟分桶算 P99。
6. 每个用户生成一条时间序列，基数爆炸，Prometheus 内存和查询成本失控。
7. 指标看趋势和告警；日志看具体错误；追踪看调用链和耗时分布。
8. 只记录一部分请求，降低成本；错误/慢请求全量，成功请求采样。
9. 可直接写成：
```go
slog.ErrorContext(ctx, "recharge failed",
    "request_id", requestID,
    "uid", maskUID(uid),
    "order_id", orderID,
    "amount_cent", amountCent,
    "error", err,
)
```
预期 JSON 中 `level` 为 `ERROR`、`msg` 为 `recharge failed`，并且四个关联字段都存在。验证方式是用固定 `request_id=req-demo-1` 调用一次测试用例，再从日志文件执行 `rg 'req-demo-1' app.log`，应能找到这条错误但不能出现完整 token、手机号或银行卡号。

10. 一个可运行的最小中间件如下，关键点是先包住 `ResponseWriter` 才能拿到真实状态码，`route` 必须传路由模板：
```go
type statusWriter struct {
    http.ResponseWriter
    status int
}
func (w *statusWriter) WriteHeader(code int) {
    w.status = code
    w.ResponseWriter.WriteHeader(code)
}
func instrument(route string, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
        next.ServeHTTP(sw, r)
        httpRequests.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).Inc()
        httpDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
    })
}
```
把路由挂成 `instrument("/v1/recharges", handler)`，启动后发一次请求，再执行 `curl -sS http://127.0.0.1:9091/metrics | rg 'http_requests_total|http_request_duration_seconds'`。预期能看到 `route="/v1/recharges"`、`method="POST"`、`status="201"`；若状态码永远是 200，检查 handler 是否调用了 `WriteHeader` 而包装器没有转发。

11. 透传方案按层执行：
```go
// HTTP 服务端：提取 traceparent，span 挂到请求 ctx
handler := otelhttp.NewHandler(mux, "http-server")
// 业务层把同一个 ctx 传给下游
func (s *Service) Create(ctx context.Context, req Request) error {
    ctx, span := otel.Tracer("recharge").Start(ctx, "Create")
    defer span.End()
    return s.grpcClient.Create(ctx, req) // gRPC 客户端拦截器注入 metadata
}
// gRPC 服务端：拦截器从 metadata 提取上下文
grpc.NewServer(grpc.UnaryInterceptor(otelgrpc.UnaryServerInterceptor()))
// MySQL：使用支持 otel 的驱动或手动创建 SQL span，但必须继续传递 ctx
row := db.QueryRowContext(ctx, "SELECT balance FROM account WHERE uid=?", uid)
```
验证时保存一次请求的 `trace_id`，在追踪系统里应看到同一 trace 下的 HTTP、gRPC、SQL span，父子关系正确；把 HTTP 出口的 transport 换成普通客户端后重跑，链路应断开，说明验证方法能发现回归。

12. 完整的最小配置和启动校验：
```go
type Config struct {
    HTTPAddr string `env:"HTTP_ADDR"`
    MySQLDSN string `env:"MYSQL_DSN"`
    LogLevel string `env:"LOG_LEVEL"`
}
func (c Config) Validate() error {
    if c.HTTPAddr == "" { return errors.New("HTTP_ADDR is required") }
    if c.MySQLDSN == "" { return errors.New("MYSQL_DSN is required") }
    switch strings.ToLower(c.LogLevel) {
    case "debug", "info", "warn", "error":
    default: return fmt.Errorf("invalid LOG_LEVEL %q", c.LogLevel)
    }
    return nil
}
func main() {
    var cfg Config
    if err := env.Parse(&cfg); err != nil { log.Fatal(err) }
    if err := cfg.Validate(); err != nil { log.Fatal(err) }
    // 校验通过后再启动 HTTP server 和数据库连接
}
```
验证：不配置 `MYSQL_DSN` 启动，进程应非 0 退出并打印缺少字段；配置合法值后应正常监听。不要把 DSN 原文打印出来。

13. 先不要重启，按证据链走：①在 Grafana 固定 10:00 前后的时间窗，查 `http_requests_total` 按 route/instance 分组的错误率和 P99，确认是否只有 `/v1/recharges`、是否从某个实例开始；②查 QPS、实例数、CPU、GC、goroutine，判断流量突增、实例消失还是阻塞；③从告警或响应中取一个 `request_id`，用 `rg 'req-...' app.log` 找第一条 ERROR，并记录错误码；④用 `trace_id` 找 trace，比较 HTTP、DB、Redis、MQ span，若时间停在 `SELECT ... FOR UPDATE` 就转数据库排查；⑤执行 `SHOW FULL PROCESSLIST`、查 `innodb_trx` 和 `data_lock_waits`，确认锁持有者和事务边界；⑥对比最近发布、配置、数据任务，若时间和新版本一致，优先关闭功能开关或回滚止血，同时保存指标、日志、trace 和慢查询。恢复标准是错误率、P99、连接池和业务成功率回到发布前基线；之后补失败测试、告警和事务内禁止远程调用的检查。

</details>
