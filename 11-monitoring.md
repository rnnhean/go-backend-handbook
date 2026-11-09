# 第 11 章 Prometheus + Grafana 监控告警

> 目标：搭出一套"出问题 1 分钟内知道、5 分钟内定位"的监控。

> **JS 类比速查**（帮助建立直觉，不表示语义等价；差异列必须看）
>
> | 后端概念 | JS / 前端里的近似物 | 关键差异 |
> |---|---|---|
> | Prometheus | 时序数据库 + 查询语言 | 拉模式采集，PromQL 按窗口计算 |
> | Grafana | 图表看板 | 只负责展示 |
> | 报警 | 前端错误监控告警 | 阈值 + 持续时间 + 分级 + 静默 |
> | SLI / SLO | Core Web Vitals 目标 | 按成功率/延迟/饱和度定义 |


## 11.1 监控四层：从“用户是否可用”逐层找证据

监控分层不是为了把面板排版好看，而是为了排障时每一层回答不同的问题。以虚构的 `mini-coin` 充值服务为例，用户请求会经过入口、Go 进程、数据库和消息队列；任何一层异常，最终都可能表现成“充值失败”。

| 层 | 先回答什么问题 | mini-coin 的具体指标 | 异常时典型现象 |
|---|---|---|---|
| 业务 | 用户操作有没有成功 | 充值成功数、成功率、失败码、重复请求数 | 技术指标正常，但成功数突然归零 |
| 应用 | 服务处理请求是否正常 | QPS、5xx、P50/P95/P99、goroutine、GC | 某个 route 变慢或开始返回 5xx |
| 中间件 | 下游是否成为瓶颈 | MySQL 连接池/锁等待、Redis 命中率/延迟、Kafka lag | Go CPU 不高，但请求都卡在等数据库 |
| 基础设施 | 机器和进程是否有足够资源 | CPU、内存、磁盘、网络、fd、实例存活 | 单台机器 CPU 打满、磁盘写满、实例消失 |

### 四层要按“因果关系”读，不能各看各的

假设告警显示充值 P99 从 80ms 涨到 2s。正确的读法是：

```text
业务层：充值成功数下降，REQ_TIMEOUT 增加
  -> 应用层：只有 /v1/recharges 变慢，余额查询正常
  -> 中间件层：MySQL 连接池使用率 98%，查询 P99 2.4s
  -> 基础设施层：Go CPU 只有 25%，实例没有重启
```

这时能得出的结论是“Go 计算不是瓶颈，请求在等数据库”。如果只看 CPU，会误以为服务很闲；如果只看业务成功率，只知道用户失败，却不知道失败发生在哪一层。

### 每层至少保留一个“可用性”和一个“饱和度”信号

只有流量指标不够，只有资源指标也不够：

| 层 | 可用性信号 | 饱和度信号 |
|---|---|---|
| 业务 | 充值成功率 | 失败码中 `DB_TIMEOUT` 的占比 |
| 应用 | 5xx 比例 | 并发请求数、goroutine 数 |
| 中间件 | 查询错误率、MQ 发布失败率 | 连接池使用率、Kafka lag |
| 基础设施 | 存活实例数 | CPU、内存、磁盘、fd 使用率 |

“可用性”回答用户是否成功；“饱和度”回答系统离上限还有多远。饱和度上升但可用性尚未下降，正是扩容、限流或优化的窗口。

### JS 前端可以怎样类比

前端会用错误率、LCP、内存和网络瀑布图判断页面健康，这和“业务/应用/资源/链路”分层很像。但后端还多两个关键维度：**共享依赖**和**容量**。一个 Go 实例可能很健康，却因为所有实例共用的 MySQL 连接数耗尽而一起变慢。

## 11.2 Prometheus 数据模型：一条样本到底是什么

Prometheus 存储的不是“一张带很多列的日志表”，而是带标签的时间序列。一条样本写成：

```text
metric_name{label1="v1", label2="v2"} value timestamp
```

例如：

```text
http_requests_total{job="minicoin",instance="app-1:8080",method="POST",route="/v1/recharges",status="200"} 1287 1760000000000
```

可以把它拆成四部分：

| 部分 | 示例 | 含义 |
|---|---|---|
| 指标名 | `http_requests_total` | 度量什么 |
| 标签集 | `job`、`instance`、`route`、`status` | 这个度量的维度 |
| 值 | `1287` | 在当前时刻的累计值 |
| 时间戳 | `1760000000000` | 采样时间，毫秒 |

**指标名 + 完整标签集**唯一标识一条时间序列。标签值不同，序列就不同；标签越多、取值越散，时间序列越多。

### 四种指标类型分别解决什么问题

#### Counter：累计发生次数

Counter 单调递增，进程重启后会从 0 重新开始。例如：

```text
http_requests_total{route="/v1/recharges",status="200"} 1200
http_requests_total{route="/v1/recharges",status="500"} 7
```

不应该直接把 Counter 当 QPS。要做“每秒增长率”：

```promql
rate(http_requests_total{route="/v1/recharges"}[5m])
```

`rate` 用窗口首尾样本的增量除以时间。进程重启导致的下降会被 Prometheus 识别为 reset 并处理。

#### Gauge：当前值，可升可降

例如：

```text
go_goroutines{instance="app-1:8080"} 84
db_pool_in_use{instance="app-1:8080"} 17
```

Gauge 适合当前连接数、队列长度、内存和在线实例数。不要对 Gauge 直接套 `rate` 来声称这是“增长速率”；只有当你确实要观察趋势时才用 `deriv`，而且要先理解数据。

#### Histogram：按桶记录分布

Go 客户端通常暴露三组序列：

```text
http_request_duration_seconds_bucket{le="0.1"} 900
http_request_duration_seconds_bucket{le="0.5"} 980
http_request_duration_seconds_bucket{le="1"}   998
http_request_duration_seconds_bucket{le="+Inf"} 1000
http_request_duration_seconds_sum 123.4
http_request_duration_seconds_count 1000
```

`le` 表示“小于等于这个秒数的请求数量”，桶是累计的。`+Inf` 等于总请求数。`_sum / _count` 可以得到平均值，但平均值会掩盖长尾。

P99 用 `histogram_quantile(0.99, ...)` 估算。桶边界决定估算精度：如果所有请求都落在 `0.5` 和 `1` 之间，P99 只能在这个区间内插值。因此延迟桶要围绕 SLO 设计，例如把 `200ms`、`500ms`、`1s` 放进去。

#### Summary：客户端直接计算分位

Summary 暴露类似：

```text
rpc_duration_seconds{quantile="0.99"} 0.8
```

优点是客户端已经算好；缺点是分位数不能跨实例正确聚合。你不能把两个实例的 P99 相加或求平均来得到整体 P99。需要跨实例聚合时，通常优先用 Histogram。

### 标签设计：先问“会不会拿来筛选”，再问“基数”

每个标签都会形成时间序列。错误设计：

```text
http_requests_total{uid="u-1001",request_id="r-abc",route="/v1/recharges"} 1
```

即使只有 10 万个用户，也可能产生数十万到数百万条序列，Prometheus 的内存和磁盘会被高基数拖垮。正确设计：

```text
http_requests_total{method="POST",route="/v1/recharges",status="200"} 1
```

`uid` 和 `request_id` 用来查单次请求，应该放日志或 trace，不应该放指标标签。`route` 必须使用路由模板，不能把真实的 `/v1/accounts/u-1001/balance` 当成标签值。

### 采集间隔与时间窗口

Prometheus 通常每 15 秒抓取一次 `/metrics`。因此：

- `[1m]` 大约只有 4 个样本，瞬时抖动大；
- `[5m]` 更平滑，但对突发故障反应慢；
- `rate(...[5m])` 不是“过去 5 分钟的平均 QPS 的精确数学定义”，而是根据窗口内样本估算的速率；
- 查询窗口必须至少覆盖两个 scrape 间隔，否则可能没有足够样本。

指标是“低成本聚合信号”，不是审计流水。需要保留每一次充值请求时，应该写数据库或日志，而不是给每个请求创建一条 Prometheus 序列。

---

## 11.3 Go 接入：从请求到 `/metrics` 的完整最小例子

下面是一个可以单独运行的最小 HTTP 服务。它统计请求总数和延迟分布，并把指标暴露给 Prometheus。

```go
package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests.",
		},
		[]string{"method", "route", "status"},
	)
	httpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		},
		[]string{"method", "route"},
	)
)

func init() {
	prometheus.MustRegister(httpRequests, httpDuration)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func withMetrics(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		httpRequests.WithLabelValues(r.Method, route, strconv.Itoa(sw.status)).Inc()
		httpDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

func main() {
	recharge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond) // 模拟业务处理
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"code":0,"msg":"ok"}`)
	})

	http.Handle("/v1/recharges", withMetrics("/v1/recharges", recharge))
	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
```

运行：

```bash
mkdir -p /tmp/mini-metrics && cd /tmp/mini-metrics
go mod init example.com/mini-metrics
go get github.com/prometheus/client_golang@latest
# 把上面的 main.go 放进来
go run .
```

另开终端验证：

```bash
curl -s http://127.0.0.1:8080/v1/recharges >/dev/null
curl -s http://127.0.0.1:8080/metrics | grep -E 'http_requests_total|http_request_duration_seconds'
```

应该能看到类似：

```text
http_requests_total{method="POST",route="/v1/recharges",status="200"} 1
http_request_duration_seconds_count{method="POST",route="/v1/recharges"} 1
```

### 生产接入时必须修掉的三个细节

1. **路由模板化**：使用 `route="/v1/accounts/:uid/balance"`，不能用真实 URL。否则每个用户一条序列。
2. **捕获隐式 200**：如果 handler 不调用 `WriteHeader`，你的 `statusWriter` 必须默认 `200`，否则状态码会变成 `0`。
3. **中间件要处理全部出口**：panic、鉴权失败、404、超时都要被统计，不能只统计业务 handler 的成功路径。

### `/metrics` 的安全边界

`/metrics` 会暴露进程、依赖和业务内部信息。生产环境应：

- 只允许 Prometheus 所在网络或服务账号访问；
- 不通过公网入口暴露；
- 不把账号、密码、完整 DSN、用户 ID 写进 label 或 Help；
- 对高流量服务限制抓取超时和并发，避免监控抓取反过来拖慢服务。

## 11.4 PromQL 常用查询：每条查询都要说明“它回答什么”

PromQL 是查询语言，不是 SQL 的替代品。它主要对时间序列做筛选、窗口计算和按标签聚合。下面每条查询都以 `mini-coin` 为例。

### 1. 总 QPS：服务现在每秒处理多少请求

```promql
sum(rate(http_requests_total{job="minicoin"}[5m]))
```

读法：

- `http_requests_total{job="minicoin"}` 先筛选服务；
- `rate(...[5m])` 计算每条序列每秒增量；
- `sum(...)` 把所有实例、route、status 加总。

如果线上是 Grafana，推荐用 `$__rate_interval`，让窗口随面板时间范围自动调整：

```promql
sum(rate(http_requests_total{job="minicoin"}[$__rate_interval]))
```

不要把它写成 `sum(http_requests_total)`，那是进程启动以来的累计请求数，不是 QPS。

### 2. 按接口看 QPS：到底哪条路由有流量

```promql
sum by (route) (rate(http_requests_total{job="minicoin"}[5m]))
```

结果类似：

```text
{route="/v1/accounts/:uid/balance"} 120
{route="/v1/recharges"} 18
```

如果 `route` 没有模板化，这里会出现成千上万条曲线。看到图例爆炸，第一反应应查标签基数，而不是把图例隐藏。

### 3. 5xx 错误率：用户失败的比例

```promql
sum(rate(http_requests_total{job="minicoin",status=~"5.."}[5m]))
/
sum(rate(http_requests_total{job="minicoin"}[5m]))
```

注意分子分母都是 `rate`，而且时间窗口相同。常见错误是：

```promql
sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(http_requests_total)
```

这会把“最近 5 分钟的错误增长速度”除以“历史累计总请求数”，结果趋近于 0，掩盖真实错误。

按接口拆：

```promql
sum by (route) (rate(http_requests_total{job="minicoin",status=~"5.."}[5m]))
/
sum by (route) (rate(http_requests_total{job="minicoin"}[5m]))
```

分母为 0 时结果会消失或变成 NaN。Grafana 里可以设置 “No value: 0”，但要在告警侧区分“没有流量”和“错误率为 0”。

### 4. P99 延迟：99% 请求在多少时间内完成

```promql
histogram_quantile(
  0.99,
  sum by (le, route) (
    rate(http_request_duration_seconds_bucket{job="minicoin"}[5m])
  )
)
```

聚合顺序很重要：必须按 `le` 聚合所有实例的桶，再调用 `histogram_quantile`。如果先对每条序列算分位再平均，得到的不是整体 P99。

### 5. P50/P95/P99 同屏：不要只看一个数字

```promql
histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket{job="minicoin"}[5m])))
histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{job="minicoin"}[5m])))
histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{job="minicoin"}[5m])))
```

如果 P50 正常、P99 大幅上升，通常是少量请求遇到锁等待、冷缓存或下游超时，不一定所有用户都慢。仅看平均值会看不到这种长尾。

### 6. 实例存活：哪些进程抓不到了

```promql
up{job="minicoin"} == 0
```

`up` 是 Prometheus 自己产生的 Gauge：抓取成功为 1，失败为 0。`up == 0` 只说明 Prometheus 抓不到目标，不等于进程一定崩溃；也可能是网络、DNS、服务发现或 `/metrics` 路径错误。

### 7. goroutine 泄漏：数量是否持续上涨

```promql
go_goroutines{job="minicoin"}
```

单看一个点无法判断泄漏。应看趋势，并和 QPS 对照：

```promql
go_goroutines{job="minicoin"}
/
sum(rate(http_requests_total{job="minicoin"}[5m]))
```

这个比值不是严格的“每请求协程数”，但能辅助发现“流量没涨，协程却一直涨”。最终仍需用 pprof 的 goroutine profile 证明阻塞在哪里。

### 8. CPU：进程用了几颗核

```promql
rate(process_cpu_seconds_total{job="minicoin"}[5m])
```

结果 `0.35` 表示平均约 0.35 核。它不是宿主机 CPU 百分比。容器里还要结合 CPU limit 和 cgroup throttling 指标判断是否被限流。

### 9. 数据库连接池饱和度

```promql
db_pool_in_use{job="minicoin"}
/
db_pool_max{job="minicoin"}
```

如果达到 1，新的查询可能在等待连接。此时 Go CPU 可能很低，接口 P99 却很高。要同时查连接池等待时间、慢查询和锁等待。

### 10. `absent`：防止“指标消失”被误判成健康

```promql
absent(up{job="minicoin"})
```

如果服务被删除、服务发现失效或指标名写错，`up == 0` 也可能没有结果。`absent` 可以在“目标完全不存在”时触发告警。

### 查询排错清单

1. 指标名是否存在：先在 Prometheus UI 输入指标名看自动补全。
2. 标签值是否正确：用 `{job="minicoin"}` 逐步收紧。
3. 时间窗口是否太短：窗口内至少要有两个采样点。
4. Counter 是否用了 `rate`：累计值不能当瞬时值。
5. 聚合是否按正确标签：`by (le)`、`by (route)` 漏了会得到错误结果。
6. 是否被空数据误导：区分 `0`、`NaN`、无结果和抓取失败。

## 11.5 告警规则：告警不是“图表超过阈值”这么简单

一条可行动的告警至少要回答五个问题：

```text
发生了什么：充值接口 5xx 错误率持续超过 2%
影响谁：当前生产环境、所有实例、充值成功数下降
严重程度：P1，需要立即处理
去哪里看：Grafana dashboard + 日志检索链接 + runbook
应该做什么：先查 DB 连接池，再查最近发布，必要时回滚
```

### 一个完整的充值错误率告警

```yaml
groups:
  - name: mini-coin-api
    rules:
      - alert: MiniCoinRechargeHighErrorRate
        expr: |
          (
            sum(rate(http_requests_total{
              job="minicoin",
              route="/v1/recharges",
              status=~"5.."
            }[5m]))
            /
            sum(rate(http_requests_total{
              job="minicoin",
              route="/v1/recharges"
            }[5m]))
          ) > 0.02
          and
          (
            sum(rate(http_requests_total{
              job="minicoin",
              route="/v1/recharges"
            }[5m])) > 1
          )
        for: 2m
        labels:
          severity: critical
          service: mini-coin
          team: payments
        annotations:
          summary: "mini-coin 充值接口 5xx 错误率超过 2%"
          description: "当前错误率 {{ $value | humanizePercentage }}，持续 2 分钟。先看 Grafana 充值看板，再检查数据库连接池与最近发布。"
          dashboard_url: "https://grafana.example.com/d/mini-coin"
          runbook_url: "https://runbook.example.com/mini-coin/recharge-5xx"
```

这里有两个容易漏掉的条件：

- `for: 2m`：避免一次瞬时抖动就打电话；
- `> 1` QPS：低流量时 1 个错误就能制造 100% 错误率，不能直接触发最高级别告警。

### 延迟告警要按接口，不要用全站 P99

```yaml
- alert: MiniCoinRechargeHighLatency
  expr: |
    histogram_quantile(
      0.99,
      sum by (le) (
        rate(http_request_duration_seconds_bucket{
          job="minicoin",
          route="/v1/recharges"
        }[5m])
      )
    ) > 1
  for: 5m
  labels:
    severity: warning
```

全站 P99 可能被大量快速请求稀释，充值已经坏了但总 P99 仍然正常。告警应绑定用户关键路径。

### 实例消失和抓取失败要分开

```yaml
- alert: MiniCoinTargetDown
  expr: up{job="minicoin"} == 0
  for: 1m
  labels:
    severity: critical
  annotations:
    summary: "mini-coin 实例 {{ $labels.instance }} 抓取失败"
```

`up == 0` 是“抓取失败”，不是“业务失败”。如果服务仍在处理请求，先查网络、服务发现和 `/metrics`，不要盲目重启所有实例。

### 告警分级与升级

| 级别 | 例子 | 通知方式 | 响应要求 |
|---|---|---|---|
| P0 / critical | 全站不可用、资金重复入账 | 电话/值班升级 | 立即止损 |
| P1 / high | 充值错误率持续 > 5%、DB 连接池耗尽 | 群消息 + 电话 | 15 分钟内响应 |
| P2 / warning | P99 上升、磁盘 80%、lag 持续增长 | 工单/群消息 | 工作时间内处理 |
| P3 / info | 证书 30 天到期、趋势异常 | 日报 | 计划处理 |

分级不是按“图看起来吓人”，而是按用户影响、资金风险、可恢复时间和是否需要立即人工干预。

### 降噪：同一次故障不要制造一百条告警

使用 `group_by` 把同一服务的告警聚合；用 `inhibit_rules` 在“实例宕机”时抑制它的下游错误告警；在发布窗口设置静默，但静默必须有截止时间和审批记录。

```yaml
route:
  group_by: [service, alertname]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 2h
```

告警静默不等于问题消失。静默期间仍要保留指标、日志和发布记录，并在复盘时检查是否漏报。

### 告警规则也要测试

至少做三件事：

1. `promtool check rules rules.yml` 检查语法；
2. 用历史数据或测试规则验证“低流量、正常流量、错误率上升、无数据”四种场景；
3. 每次修改告警都在 staging 触发一次，确认通知能到达正确的人，并带正确链接。

## 11.6 看板怎么摆：不是凑五个标题，而是按排障顺序组织

Grafana 里的 **Row** 是“折叠分组”，不是一种特殊图表。你可以把一行理解成一组面板：

```text
Dashboard
├── Row 1：总览（先判断有没有事故）
├── Row 2：接口（定位受影响的路由）
├── Row 3：依赖（查找外部瓶颈）
├── Row 4：资源（查找进程/机器瓶颈）
└── Row 5：业务（确认用户结果和损失）
```

这五行的顺序是值班人的阅读顺序，不是随便排的：

```text
用户有没有受影响
  -> 哪个接口受影响
  -> 是不是数据库/Redis/MQ 的问题
  -> 是不是 CPU/内存/GC/协程的问题
  -> 最终影响多少业务结果
```

### 11.6.1 第一行：总览，10 秒内判断“是不是挂了”

第一行只放 4 个核心面板，不要塞十几个图。

| 面板 | 图类型 | PromQL 示例 | 单位 | 含义 |
|---|---|---|---|---|
| 总 QPS | Stat / Time series | `sum(rate(http_requests_total{job="minicoin"}[$__rate_interval]))` | req/s | 当前服务每秒处理多少请求 |
| 5xx 错误率 | Stat / Time series | `sum(rate(http_requests_total{job="minicoin",status=~"5.."}[$__rate_interval])) / sum(rate(http_requests_total{job="minicoin"}[$__rate_interval]))` | percent | 用户看到服务端错误的比例 |
| P99 延迟 | Stat / Time series | `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{job="minicoin"}[$__rate_interval])))` | seconds | 99% 请求都在这个时间内完成；最慢的 1% 可能更慢 |
| 存活实例数 | Stat | `count(up{job="minicoin"} == 1)` | 个 | 当前有多少实例在线 |

**为什么总览先放这四个？**

- QPS 突然变成 0：可能入口挂了、流量没进来、服务完全不可用；
- 错误率上升：用户已经开始失败；
- P99 上升：用户开始变慢；
- 实例数下降：先查发布、崩溃、探针和资源。

**面板要看趋势，不要只看一个数字。** Grafana 的 Stat 面板要开启 sparkline 或同时放一个 Time series，否则你只能看到“现在 300ms”，不知道它是从 100ms 涨上来的，还是一直如此。

#### 错误率为什么不是“错误数 / 请求数”

`http_requests_total` 是 Counter，只增不减。要在时间窗口内先算增长率：

```promql
sum(rate(http_requests_total{status=~"5.."}[5m]))
/
sum(rate(http_requests_total[5m]))
```

分子和分母必须是同一时间范围、同一组实例。否则得到的是“历史累计错误数 / 当前请求数”，没有意义。

#### P99 为什么不是平均值

假设 100 个请求：

```text
99 个请求 20ms
1 个请求 5s
```

平均值大约 `69.8ms`，看起来还能接受；但 P99 是 5s，说明有真实用户在等 5 秒。**平均值会掩盖长尾，后端看延迟必须看 P95/P99，必要时看 P999。**

### 11.6.2 第二行：按接口拆，定位“到底哪个接口出问题”

第一行告诉你“系统有问题”，第二行告诉你“问题在哪条路由”。

至少放：

1. **各接口 QPS**：`sum by (route) (rate(http_requests_total[$__rate_interval]))`
2. **各接口 5xx 错误率**：按 `route` 分组；
3. **各接口 P95/P99**：按 `route` 分组；
4. **最慢接口表格**：按 P99 排序，显示 route、实例、当前值。

```promql
# 每个接口的 P99
histogram_quantile(0.99,
  sum by (le, route) (
    rate(http_request_duration_seconds_bucket{job="minicoin"}[$__rate_interval])
  )
)
```

```promql
# 每个接口的 5xx 错误率
sum by (route) (
  rate(http_requests_total{job="minicoin",status=~"5.."}[$__rate_interval])
)
/
sum by (route) (
  rate(http_requests_total{job="minicoin"}[$__rate_interval])
)
```

#### `route` 必须归一化

正确：

```text
/v1/accounts/:uid/balance
/v1/recharges
```

错误：

```text
/v1/accounts/u-1001/balance
/v1/accounts/u-1002/balance
/v1/accounts/u-1003/balance
```

如果把真实用户 ID 放进 label，每个用户都会变成一条时间序列，Prometheus 内存和磁盘会被撑爆。这叫**高基数（high cardinality）**。

#### 加一个 `$route` 变量

在 Grafana 顶部加变量：

```text
Name: route
Type: Query
Query: label_values(http_requests_total{job="minicoin"}, route)
```

然后面板查询使用：

```promql
http_request_duration_seconds_bucket{job="minicoin",route="$route"}
```

这样值班人可以先看全局，再下钻某个接口。

### 11.6.3 第三行：依赖健康，判断瓶颈是不是数据库/Redis/MQ

Go 服务本身可能只有 20ms，但请求总共 3 秒，通常慢在下游。依赖行至少要有：

| 依赖 | 面板 | 关键指标 |
|---|---|---|
| MySQL | 连接池使用率 | `db_pool_in_use / db_pool_max` |
| MySQL | 查询延迟 | `histogram_quantile(0.99, rate(db_query_duration_seconds_bucket[...]))` |
| MySQL | 错误/超时/死锁 | `db_errors_total{type="timeout"}` 等 |
| Redis | 命令延迟 | Redis exporter 或应用侧直方图 |
| Redis | 错误率 | `redis_errors_total` |
| Kafka/RabbitMQ | 消费积压 | `kafka_consumergroup_lag` 或应用侧 lag |
| MQ | 发布失败 | `mq_publish_failures_total` |

示例：

```promql
# 数据库连接池使用率
db_pool_in_use{job="minicoin"}
/
db_pool_max{job="minicoin"}
```

```promql
# 数据库查询 P99
histogram_quantile(0.99,
  sum by (le) (rate(db_query_duration_seconds_bucket{job="minicoin"}[$__rate_interval]))
)
```

```promql
# MQ 消费积压持续增长
max by (topic, group) (kafka_consumergroup_lag)
```

**看依赖不要只看“有没有错误”，还要看延迟和饱和度。** 连接池 95% 使用率、查询 P99 从 10ms 涨到 2s，即使暂时没有错误，也已经在走向故障。

### 11.6.4 第四行：资源，判断是不是进程或实例自身扛不住

资源行建议按实例展示，图例写 `{{instance}}`，否则你只能看到总和，不知道哪台机器有问题。

| 面板 | PromQL 示例 | 说明 |
|---|---|---|
| CPU | `rate(process_cpu_seconds_total[$__rate_interval])` | 单位约等于“核数”，不是百分比 |
| 内存 RSS | `process_resident_memory_bytes` | 进程实际占用的物理内存 |
| Heap | `go_memstats_heap_inuse_bytes` | Go 正在使用的堆内存 |
| GC 平均暂停 | `rate(go_gc_duration_seconds_sum[$__rate_interval]) / rate(go_gc_duration_seconds_count[$__rate_interval])` | 不是 GC 总时间 |
| Goroutine | `go_goroutines` | 持续上涨可能是泄漏 |
| 磁盘/文件描述符 | `process_open_fds` | 句柄泄漏也要看 |

资源图要和流量图对照。CPU 高但 QPS 也高，可能只是正常扩容；QPS 没变但 CPU 持续上涨，才更像死循环、GC、泄漏或异常重试。

### 11.6.5 第五行：业务指标，确认用户结果

技术指标正常，不代表业务成功。业务行至少放：

- 充值成功数；
- 充值成功率；
- 充值失败按错误码分布；
- 重复请求数；
- 冻结账户拒绝数；
- 数据库余额变化与订单金额是否一致（离线对账指标）。

```promql
# 每秒成功充值数
sum(rate(mini_coin_recharge_total{result="success"}[$__rate_interval]))

# 充值结果分布
sum by (result) (rate(mini_coin_recharge_total[$__rate_interval]))
```

业务指标同样不能放 `uid`、`req_id`、订单号这类高基数字段。需要追单个请求时查日志和 trace，不要把它塞进 Prometheus label。

### 11.6.6 一个实际看板的排布

一个 12 列宽的 Grafana Dashboard 可以这样排：

```text
Row 1 总览
[QPS 3列] [错误率 3列] [P99 3列] [实例数 3列]

Row 2 接口
[各接口 P99 6列] [各接口错误率 6列]
[最慢接口表格 12列]

Row 3 依赖
[MySQL 池 4列] [MySQL 延迟 4列] [Redis 延迟 4列]
[Kafka lag 6列] [MQ 发布失败 6列]

Row 4 资源
[CPU 3列] [内存 3列] [GC 3列] [Goroutine 3列]

Row 5 业务
[成功数 3列] [成功率 3列] [失败码 3列] [重复请求 3列]
```

### 11.6.7 真的出问题时，怎么按看板走

假设值班人收到“充值接口 P99 > 1s”告警：

```text
1. 看总览：QPS 正常、5xx 1.8%、P99 1.6s、实例数没少
2. 看接口：只有 /v1/recharges 慢，余额查询正常
3. 看依赖：MySQL 连接池 98%，查询 P99 2.4s，Redis 正常
4. 看资源：Go CPU 只有 20%，goroutine 数量持续上涨
5. 看业务：充值成功数下降，错误码 ACCOUNT_FROZEN 没有异常
6. 看日志/trace：大量请求卡在 SELECT ... FOR UPDATE
7. 结论：数据库行锁等待，不是 Go 计算慢；继续查长事务和最近发布
```

这就是五行的意义：**用指标逐层缩小范围，最后用日志和 trace 证明根因。**

### 11.6.8 看板必须配的细节：让别人能在凌晨三点使用

看板不是给自己“看得爽”的。值班人可能不熟悉这个服务，也没有时间猜图例。一个可交付的看板至少要有下列信息。

#### 1. 顶部变量与全局筛选

建议放三个变量：

```text
env      = prod / staging
instance = All / app-1 / app-2
route    = All / /v1/recharges / /v1/accounts/:uid/balance
```

变量值来自标签，不要让用户手工输入 IP。切换 `env` 后，所有面板查询都必须使用 `$env`，否则会把 staging 和生产混在一起。

#### 2. 单位、图例和阈值

延迟面板单位写 `seconds`，QPS 写 `req/s`，错误率写 `percent (0-1)`，内存写 `bytes`。图例建议：

```text
{{instance}} {{route}} {{status}}
```

如果图例全是 `value`，说明面板不可用于排障。红色阈值只代表“需要关注”，不能代替真正的告警规则；两者应使用同一指标口径。

#### 3. 时间范围和刷新频率

默认最近 15 分钟，保留 1h、6h、24h。高流量服务可以每 30 秒刷新，低频服务不必每秒刷新。时间范围太短会看不到趋势，太长会让故障被平均掉。

#### 4. 发布和故障 annotation

每次发布在图上打一条竖线，标注版本、发布人和变更单。故障复盘时，如果没有发布时间线，很容易把“数据库本来就慢”和“新版本引入慢查询”混在一起。

#### 5. 从告警跳转到证据

告警 annotation 里放 dashboard URL；面板里放日志查询和 trace 查询链接。至少要能带上：

```text
service=mini-coin env=prod route=/v1/recharges from=... to=...
```

点击后直接过滤到对应时间窗和 route，不要让值班人重新手打查询。

#### 6. 看板版本化

把 Dashboard JSON 或 Grafana provisioning 文件放进仓库，和代码一起 review。手工在网页上改完不记录，下一次重建环境就会丢失，而且没人知道阈值为什么变了。

#### 7. 看板验收方法

让一个没参与开发的同学只看看板回答：

- 现在服务是否正常？
- 哪个接口最慢？
- 慢在数据库、Redis 还是 Go 进程？
- 影响多少充值成功数？
- 最近有没有发布？

如果他需要问你五个问题才能回答，这个看板就还没完成。

### 11.6.9 不要这样摆：四个真实反例

#### 反例一：一张图画所有接口

症状：20 条线叠在一起，图例不可读，P99 看起来永远正常。原因是低延迟接口把高延迟接口的问题稀释了。

修法：第一层看总览，第二层按 `route` 分组；需要总览时用表格列出每条 route 的 P99，而不是把所有线硬塞在一张图。

#### 反例二：用用户 ID 做标签

症状：Prometheus 内存持续上涨，查询越来越慢。修法：删除高基数 label；用户级问题改用日志和 trace，指标只保留 route、status、result 等有限枚举。

#### 反例三：仪表盘只有 CPU 和内存

症状：CPU 一切正常，但用户充值全部超时。原因是请求在等 MySQL 行锁，Go 进程几乎没有计算工作。修法：依赖层必须有连接池、查询延迟、错误和锁等待；应用层必须有 P99、错误率；业务层必须有成功率。

#### 反例四：告警和看板口径不同

症状：告警说错误率 5%，看板显示 0.5%。通常是一个用 5 分钟 `rate`，另一个用累计值或不同 route 过滤。修法：把 PromQL 抽成统一记录规则，告警和面板都引用同一个 `record`。

**判断标准：看板必须让值班人完成“总览 -> 定位接口 -> 查依赖 -> 看资源 -> 评估业务影响”这条路径。只展示数字，不帮助缩小范围，就不算合格看板。**

---

## 11.7 日志、指标、追踪分工：三种证据不能互相替代

三者都叫“可观测性”，但成本和用途不同：

| 维度 | 主要回答 | 数据形态 | 适合查什么 | 典型成本 |
|---|---|---|---|---|
| 指标 | 有没有异常、范围多大 | 聚合时间序列 | QPS、错误率、P99、饱和度 | 低 |
| 日志 | 这一次请求具体发生了什么 | 离散事件 | 错误详情、参数、状态转换 | 高 |
| 追踪 | 请求跨服务慢在哪一段 | 带父子关系的 span | 调用链、下游耗时、网络跳转 | 中 |

### 用同一个请求把三者串起来

虚构请求：

```text
request_id = req-demo-1001
uid = u-1001
route = /v1/recharges
```

排查时：

1. 指标显示 `/v1/recharges` P99 从 80ms 涨到 2s；
2. 用 trace 找到耗时集中在 `UPDATE account ...` 的 1.8s；
3. 用日志里的 `request_id` 找到具体事务、错误码和数据库等待；
4. 回到指标确认这不是单次偶发，而是所有实例的数据库延迟一起上升。

日志至少要包含 `timestamp`、`level`、`service`、`env`、`request_id`、`route`、`status`、`duration_ms` 和错误码。不要记录完整 token、密码、身份证、银行卡和完整请求体。

### 不要用日志算 QPS

日志量很大时，统计 QPS 既慢又贵，还会因为采样和解析错误而不准。QPS 用 Counter 和 `rate`；日志只用于解释某个异常请求。

### 不要用指标保存单请求详情

把 `request_id`、`uid` 放进 label 会让序列数量爆炸。单请求详情属于日志和 trace。指标只保留低基数维度，例如 `route`、`status`、`result`、`dependency`。

### 追踪采样要能解释

100% 采样成本高，1% 采样可能刚好错过故障。生产常见做法是：正常流量低采样，错误、慢请求和高优先级业务提高采样率；采样策略要写进代码和运行手册，不要“凭感觉”。

## 11.8 SLI / SLO / 错误预算：把“稳定”变成可计算的目标

### SLI 是可测量的服务质量

SLI（Service Level Indicator）必须是能从真实请求算出来的指标，例如：

```text
充值成功率 = 成功充值请求数 / 总充值请求数
充值延迟达标率 = 1 秒内完成的充值请求数 / 总充值请求数
```

“服务很稳定”不是 SLI，因为它无法测量；“CPU 低于 70%”也不是用户 SLI，因为 CPU 正常不代表充值成功。

### SLO 是目标

例如：

```text
月度充值成功率 >= 99.9%
月度充值 P99 <= 800ms
```

SLO 应该按用户关键路径定义，不要给每个内部接口都定一个奇怪的目标。对外承诺的 SLA 通常比内部 SLO 更保守，因为中间还涉及补偿、支持和法律口径。

### 错误预算怎么算

```text
错误预算 = 1 - SLO
```

30 天按 43,200 分钟计算：

| SLO | 允许不可用时间/月 | 允许失败请求比例 |
|---|---:|---:|
| 99% | 约 7 小时 12 分 | 1% |
| 99.9% | 约 43 分 12 秒 | 0.1% |
| 99.99% | 约 4 分 19 秒 | 0.01% |

如果充值 SLO 是 99.9%，一个月有 10 万次请求，那么允许约 100 次失败。实际计算还要明确：超时算不算失败、客户端取消算不算、计划维护是否扣除。口径必须写在 SLO 文档里。

### 错误预算不是“可以随便坏”

它有三个用途：

1. 判断是否该暂停高风险发布：预算消耗过快时，先修稳定性；
2. 让产品、研发、运维对“可接受风险”有共同语言；
3. 指导告警优先级：快速烧掉大量预算的事故，比长期轻微抖动更紧急。

一个可能的策略：

```text
预算剩余 > 50%：正常迭代
预算剩余 20%~50%：高风险变更需要额外评审
预算剩余 < 20%：冻结非必要发布，集中修复可靠性
预算耗尽：只允许修复、回滚和安全相关变更
```

### 用 PromQL 看预算消耗

以充值成功率为 SLI：

```promql
sum(rate(mini_coin_recharge_total{result="success"}[30d]))
/
sum(rate(mini_coin_recharge_total[30d]))
```

实际系统应使用记录规则预先计算，避免每次面板都扫描很长的窗口。短期燃烧率可以写成：

```promql
(
  1 - (
    sum(rate(mini_coin_recharge_total{result="success"}[5m]))
    /
    sum(rate(mini_coin_recharge_total[5m]))
  )
) / (1 - 0.999)
```

结果大于 1 表示当前消耗错误预算的速度快于预算允许的速度。生产告警通常用“1 小时 + 5 分钟”多窗口组合，既快速发现急剧故障，又避免瞬时抖动。

### 不要追求 100% SLO

100% 意味着没有任何失败预算，任何一次数据库切换、网络抖动和发布都会被视为违约；成本会高到不现实。SLO 应该来自用户能容忍的程度和业务成本，而不是“数字越大越好看”。

## 习题

1. 业务层、应用层、中间件层、基础设施层各解决什么问题？用 `mini-coin` 各举两个指标。
2. Counter 和 Gauge 的区别是什么？为什么 QPS 不能直接查询 `http_requests_total`？
3. Histogram 的 `_bucket`、`_sum`、`_count` 分别是什么？P99 为什么不能靠平均值算？
4. 为什么不能把 `uid` 和 `request_id` 放进 Prometheus label？应该去哪里查它们？
5. 写一条按 `route` 计算 5 分钟 5xx 错误率的 PromQL，并解释分子分母。
6. 写一条计算 `/v1/recharges` P99 延迟的 PromQL。
7. 为什么延迟告警要按 route 拆？全站 P99 可能怎样掩盖问题？
8. 告警里的 `for` 和 `group_by` 分别解决什么问题？低流量服务还要加什么条件？
9. 设计一个充值看板的五层结构，每层至少写两个面板和用途。
10. 日志、指标、追踪分别回答什么问题？举一个用三者联合定位数据库锁等待的例子。
11. SLO 99.9% 在一个 30 天月份里允许多少不可用时间？错误预算耗尽后应该采取什么行动？
12. 解释为什么“所有指标都正常”仍可能意味着业务失败，并给出一个业务监控指标。

## 参考答案

<details>
<summary>展开参考答案</summary>

### 1. 四层各解决什么问题

| 层 | 要回答的问题 | mini-coin 指标 A | mini-coin 指标 B |
|---|---|---|---|
| 业务 | 用户的操作到底成没成功 | `mini_coin_recharge_total{result="success"}` | `mini_coin_recharge_total{result="failed",reason="ACCOUNT_FROZEN"}` |
| 应用 | 服务处理请求是否正常 | `http_requests_total{status=~"5.."}` | `http_request_duration_seconds_bucket` |
| 中间件 | 是否卡在共享依赖 | `db_pool_in_use / db_pool_max` | `kafka_consumergroup_lag` |
| 基础设施 | 机器和进程是否扛得住 | `process_resident_memory_bytes` | `node_filesystem_avail_bytes` |

**验证自己有没有理解**：随便说一个现象，比如“充值成功率掉了”，你要能回答“下一层该看哪个指标”。如果答不出来，说明这一层只是背了名词。

正常时四层都平稳；故障时它们的**时间先后**才是线索。磁盘先满 → MySQL 写入报错 → 应用 5xx → 业务成功率下跌。反过来读，只能看到“用户在失败”，定位不到根因。

---

### 2. Counter 和 Gauge

Counter 单调递增，进程重启才归零；Gauge 可增可减。

```promql
# 错：这是服务启动以来的累计值，不是 QPS
http_requests_total{job="minicoin"}

# 对：5 分钟窗口内每秒新增多少请求
sum(rate(http_requests_total{job="minicoin"}[5m]))
```

预期输出类似 `42.7`，单位 `req/s`。

**失败场景**：刚重启的服务用 `[1h]` 窗口查 `rate`，分母里大部分时间没有样本，Grafana 会出现尖刺或断点。用 `$__rate_interval` 或在重启后等待至少两个抓取周期。直接对 Gauge 用 `rate` 也是常见错误，比如 `rate(go_goroutines[5m])` 没有物理意义。

---

### 3. Histogram 三个后缀

```text
http_request_duration_seconds_bucket{le="0.1"}   # 耗时 <= 100ms 的累计次数
http_request_duration_seconds_sum                # 所有请求耗时之和
http_request_duration_seconds_count              # 请求总数
```

`sum/count` 得到的是**平均值**：

```promql
rate(http_request_duration_seconds_sum[5m])
/
rate(http_request_duration_seconds_count[5m])
```

100 个请求里 99 个 20ms、1 个 5s，平均值约 69.8ms，但真实有一个用户在等 5 秒。P99 要从桶分布估算，不能用平均值代替。

**验证**：同时查平均值和 P99，故意用压测脚本制造 1% 的慢请求（`sleep 5s`），观察 P99 跳到 5s 附近而平均值只涨到几十毫秒。

---

### 4. 为什么 `uid` / `request_id` 不能做 label

Prometheus 的每条时间序列由“指标名 + 完整标签集”唯一确定。100 万用户就是 100 万条序列，每个序列还要按抓取周期长期保存，内存和磁盘会成倍膨胀；查询时 `sum by (uid)` 也会慢到不可用。

单请求信息放结构化日志和 trace：

```bash
# 日志里按 request_id 找一次完整调用
jq -c 'select(.request_id=="req-7f3a")' /var/log/minicoin/app.jsonl

# 用 trace id 找调用链（示例：Jaeger）
curl -s "http://jaeger.example.com/api/traces/$TRACE_ID" | jq '.data[0].spans[] | {operationName,duration}'
```

Prometheus 只保留**低基数**维度：`route`、`status`、`method`、`result`、`reason`。判断标准：这个标签的取值数量是否可控（几十、几百），而不是随用户数增长。

---

### 5. 按 route 的 5xx 错误率

```promql
sum by (route) (
  rate(http_requests_total{job="minicoin",status=~"5.."}[5m])
)
/
sum by (route) (
  rate(http_requests_total{job="minicoin"}[5m])
)
```

分子：该 route 每秒新增的 5xx 请求数。分母：该 route 每秒新增的全部请求数。两边都必须带同样的 `by (route)` 和时间窗口，否则标签对不齐，结果会是空或错位。

预期输出：

```text
{route="/v1/recharges"} 0.018
{route="/v1/accounts/:uid/balance"} 0.0002
```

**验证**：用 `hey` 打 100 个请求，其中 5 个带非法参数触发 400——错误率应保持 0；再把数据库停掉触发 500，错误率应上升到 1。如果 400 也被算进 5xx，检查状态码标签是不是写错了。

---

### 6. 单接口 P99

```promql
histogram_quantile(
  0.99,
  sum by (le) (
    rate(http_request_duration_seconds_bucket{
      job="minicoin",
      route="/v1/recharges"
    }[5m])
  )
)
```

必须保留 `le` 标签再聚合；少了 `by (le)`，`histogram_quantile` 会报错或返回无意义结果。

带 route 变量的通用写法：

```promql
histogram_quantile(
  0.99,
  sum by (le, route) (
    rate(http_request_duration_seconds_bucket{job="minicoin",route=~"$route"}[$__rate_interval])
  )
)
```

**常见失败**：

- 返回 `NaN`：该窗口内这个 route 没有请求，属正常；
- P99 永远等于某个桶边界：桶划分太粗，比如只有 `0.1/1/10`，要按业务目标加桶（`0.05,0.1,0.25,0.5,1,2.5,5`）；
- 数字比日志里的耗时低很多：客户端到网关的网络时间没被应用直方图统计，需另看入口指标。

---

### 7. 为什么延迟告警要按 route 拆

全站 P99 是所有请求混在一起算的。假设每秒 10000 次余额查询（20ms）和 20 次充值（3s），全站 P99 仍然只有 20ms 左右，但充值用户已经全部超时。

```promql
# 全站：看不出问题
histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))

# 单接口：立刻暴露
histogram_quantile(0.99, sum by (le, route) (rate(http_request_duration_seconds_bucket{route="/v1/recharges"}[5m])))
```

告警规则至少按“核心接口”分组；非核心批量任务可以单独设更宽的阈值。**验证**：只让充值接口 sleep 3 秒，全站 P99 几乎不动，按 route 的 P99 报警应该触发。

---

### 8. `for` 和 `group_by`

`for` 表示条件必须连续成立多久才发告警，过滤瞬时抖动：

```yaml
- alert: RechargeHighErrorRate
  expr: |
    sum(rate(http_requests_total{route="/v1/recharges",status=~"5.."}[5m]))
    /
    sum(rate(http_requests_total{route="/v1/recharges"}[5m]))
    > 0.02
  for: 5m
```

`group_by` 在 Alertmanager 里把同一故障的多条告警合并成一条通知：

```yaml
route:
  group_by: [alertname, service, route]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
```

低流量服务必须加最小请求数，否则 1 个请求失败就是 100%：

```promql
(
  sum(rate(http_requests_total{route="/v1/recharges",status=~"5.."}[5m]))
  /
  sum(rate(http_requests_total{route="/v1/recharges"}[5m]))
  > 0.02
)
and
sum(rate(http_requests_total{route="/v1/recharges"}[5m])) > 1
```

最后一行要求该接口每秒至少 1 个请求。**验证**：手动发 1 个失败请求，不应告警；持续压测制造 5% 错误并保持 6 分钟，才应收到通知。

---

### 9. 充值看板的五层结构

| 行 | 面板 | 查询/展示 | 回答什么问题 |
|---|---|---|---|
| 总览 | QPS、5xx 率、P99、存活实例 | 四个 Stat + sparkline | 现在是不是事故 |
| 接口 | 各 route 的 P99、错误率、慢接口表 | Time series + Table，按 P99 排序 | 哪个接口出问题 |
| 依赖 | MySQL 池使用率、查询 P99、Redis 延迟、Kafka lag | Time series，图例 `{{instance}}` | 是不是下游瓶颈 |
| 资源 | CPU、RSS、Heap、GC、goroutine、fd | Time series，按实例拆 | 进程/机器是否饱和 |
| 业务 | 成功数、成功率、失败码分布、重复请求数 | Stat + Time series + Bar gauge | 用户损失多大 |

排布见 11.6.6。**验收**：找一个不熟悉项目的人，只给他看板，让他在 2 分钟内回答“哪个接口慢、慢在应用还是数据库”。答不出来就说明面板缺少下钻路径或图例不清。

---

### 10. 日志、指标、追踪的分工与联合定位

| 证据 | 回答 | 代价 | 典型查询 |
|---|---|---|---|
| 指标 | 什么时候、多少、哪一类 | 低，长期保存 | PromQL |
| 日志 | 具体发生了什么错误 | 中，采样/保留期 | `request_id`、错误码 |
| 追踪 | 一次请求慢在哪个 span | 高，通常采样 | trace id、span duration |

联合定位数据库锁等待：

```text
1. 指标：/v1/recharges P99 从 80ms 涨到 2s，5xx 上升
2. 追踪：慢请求的 span 树显示 1.9s 花在 db.query(UPDATE account ...)
3. 日志：同一个 request_id 出现 "lock wait timeout exceeded"
4. 数据库指标：InnoDB row lock waits 上升，连接池使用率 98%
5. 结论：行锁等待，不是 Go CPU；继续查长事务和最近 SQL 变更
```

**验证**：开两个 MySQL 会话，一个 `BEGIN; SELECT ... FOR UPDATE;` 不提交，另一个执行同样更新，观察第二个会话阻塞，日志和 trace 出现对应耗时。

---

### 11. SLO 99.9% 的错误预算

30 天 = 30 × 24 × 60 = 43200 分钟。

```text
允许不可用时间 = 43200 × (1 - 0.999) = 43.2 分钟
```

按请求数算更准确：100 万请求允许 1000 次失败。

错误预算耗尽后的动作要提前写进流程：

1. 暂停非必要高风险发布和数据库变更；
2. 优先回滚、扩容、限流、降级；
3. 复盘预算消耗原因，补测试和告警；
4. 只有风险可控的修复才允许带变更评审上线。

**验证**：在 Grafana 用一个 30 天窗口计算 `1 - (成功请求 / 总请求)`，与 0.001 比较；再模拟一天消耗 50% 预算，检查发布流程是否真的被阻断，而不是只写在文档里。

---

### 12. 指标全绿但业务失败

技术指标只证明“请求被处理了”，不证明“处理对了”。典型原因：

- 代码把异常吞掉，HTTP 返回 200 但业务实际失败；
- 消息重复消费导致重复加钱；
- 缓存返回旧余额；
- 上游把失败请求算成成功；
- 对账任务没跑，账实不一致没人发现。

业务指标示例：

```promql
# 充值成功且流水与余额变化一致的比例（由对账任务导出）
mini_coin_reconcile_match_ratio
```

告警：

```yaml
- alert: ReconcileMismatch
  expr: mini_coin_reconcile_match_ratio < 0.9999
  for: 10m
```

**验证**：在测试库手工把一条订单状态改成成功但不加余额，对账任务应输出不一致记录并触发告警；修复数据后指标恢复。只看 `http_requests_total{status="200"}` 永远发现不了这类问题。

</details>
