# 第 13 章 性能压测与调试排障

> 目标：会设计一次有结论的压测，会用 pprof 找到 CPU/内存/goroutine 热点，会用 Delve 和日志复现 bug，并且知道线上问题先止损再定位。
>
> 性能不是“感觉快”，是**在明确负载下、满足明确延迟和错误率目标**。

---

## 13.1 压测基本概念

| 指标 | 含义 | 看什么 |
|---|---|---|
| QPS | 每秒请求数 | 系统吞吐 |
| 并发数 | 同时在途请求数 | 资源占用 |
| P50/P95/P99 | 延迟分位数 | 长尾体验 |
| 错误率 | 失败请求比例 | 正确性 |
| 饱和度 | CPU/内存/连接池使用率 | 瓶颈 |

Little's Law：

```
并发 ≈ QPS × 平均延迟(s)
```

例：1000 QPS × 0.05s = 50 并发。

### 压测必须有目标

```markdown
目标：POST /v1/recharges 在 500 QPS 下 P99 < 300ms，错误率 < 0.1%
环境：与生产同规格 2 个实例，MySQL 同规格，测试数据 100 万订单
流量：读写比 1:1，缓存命中率 80%
观察：CPU、内存、GC、goroutine、连接池、慢查询、Redis 延迟
```

没有目标的压测只能叫“打了一下”。

---

## 13.2 k6 压测

k6 是压测客户端，它负责按设定的并发模型持续发请求，并统计吞吐、延迟分位和失败率。压测结果是否可信，取决于三件事：请求是否和真实业务一致、测试数据分布是否接近生产、压测机本身是否还有余量。下面的脚本为每个虚拟用户、每次迭代生成独立的 `req_id`，避免把幂等命中误当成真实写入，再通过 `thresholds` 让 k6 在指标不达标时返回非零退出码，方便放进 CI 或发布演练。

```js
import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  stages: [
    { duration: '30s', target: 50 },   // 爬坡
    { duration: '2m',  target: 200 },  // 稳定
    { duration: '30s', target: 0 },    // 降坡
  ],
  thresholds: {
    http_req_duration: ['p(99)<300'],
    http_req_failed: ['rate<0.001'],
  },
};

export default function () {
  const key = `k6-${__VU}-${__ITER}`;
  const payload = JSON.stringify({
    req_id: key,
    amount_cent: 100,
    channel: 'manual',
  });
  const params = {
    headers: {
      'Content-Type': 'application/json',
      'Authorization': 'Bearer demo-token',
      'X-Request-Id': key,
    },
  };
  const res = http.post('http://127.0.0.1:8081/v1/recharges', payload, params);
  check(res, {
    'status is 201': (r) => r.status === 201,
    'body is OK': (r) => r.json('code') === 'OK',
  });
  sleep(0.1);
}
```

```bash
k6 run --out json=result.json load.js
```

### 一次压测从启动到结论

第一次不要直接跑 200 并发。先用 1 个虚拟用户确认鉴权、请求体、状态码和数据库结果都对，再逐步增加压力：

```bash
# 1. 只跑一个迭代，先验证脚本没有打错接口
k6 run --vus 1 --iterations 1 load.js

# 2. 小流量确认数据确实写入，且路由/鉴权正常
k6 run --vus 5 --duration 30s load.js

# 3. 再执行完整阶段
k6 run --out json=result.json load.js
```

一次成功运行至少留三类证据：

1. **k6 汇总**：`http_reqs` 是总请求数，`http_req_failed` 看失败率，`http_req_duration` 看 P50/P95/P99，阈值失败会明确打印；
2. **服务端事实**：同一时间窗口的错误日志、数据库写入行数、连接池使用率、`go_goroutines` 和 CPU/内存曲线；
3. **数据核对**：压测前记录余额/订单数，结束后查唯一订单数和金额总和，不能只看 HTTP 200。

常见失败不是“服务扛不住”，而是压测脚本本身有问题：所有请求都用同一个幂等键会测成读缓存；测试账号余额不足会大量返回业务失败；压测机 CPU 先打满会让 k6 发出请求的节奏失真；测试库数据量太小会让 SQL 比生产快一个数量级。出现这些情况先修实验条件，不要急着扩容服务。

### 压测原则

- 不要压生产（除非有审批和隔离）；
- 测试数据量/分布接近生产；
- 先单实例找拐点，再集群；
- 逐步加压，观察哪个指标先饱和；
- 压测时看数据库，不只看应用；
- 一次只改一个变量，否则不知道收益来自哪。

压测的停止条件也要提前写死。例如错误率超过 1%、P99 超过目标两倍或数据库连接池连续 2 分钟超过 90% 就停止加量。否则系统已经进入雪崩区，压测机还继续加请求，得到的只是“如何把服务打挂”，而不是容量拐点。

---

## 13.3 pprof：Go 的性能 X 光

pprof 采集的是 Go 进程内部的采样数据，解决“CPU 花在哪个函数”“内存被谁分配”“goroutine 卡在哪里”这类问题。它不是一直开着就越多越好：profile 本身有采样开销，heap/goroutine 还可能包含路径和业务标识，所以生产环境应放在受保护的管理端口，只允许值班人员或调试网络访问。

### 开启

```go
import _ "net/http/pprof"

go func() {
    log.Println(http.ListenAndServe("127.0.0.1:6060", nil))
}()
```

**不要暴露到公网**，仅内网/本地访问。

### CPU profile

CPU profile 是在“有负载”时采样调用栈。空闲服务没有热点，采 30 秒也只能得到噪声，所以要先让压测或回放流量稳定运行，再采同一时间段的 CPU 与指标。

```bash
# 采样 30 秒；命令执行期间保持业务负载
go tool pprof -seconds=30 http://127.0.0.1:6060/debug/pprof/profile

# 也可以先保存文件，事后和同事一起分析
curl -sS 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30' -o cpu.pprof
go tool pprof ./bin/server cpu.pprof
```

交互命令：

```text
top 20        # 按 flat 时间排序，先看函数自身消耗
top -cum      # 按累计时间排序，看完整调用链
list Func     # 看该函数每一行的耗时分布
web           # 生成调用图；装了 graphviz 才能用
```

读结果时不要只抄第一名的函数。`flat` 高说明函数自身在烧 CPU；`cum` 高可能只是它调用了真正慢的下游。比如 `encoding/json.Marshal` 自身耗时高，可能是响应体无意义地包含了巨大字段；`runtime.gcBgMarkWorker` 高，要继续看分配来源，而不是“优化 GC”。优化后用同一负载、同一时长的 profile 和 P99 对比，才能证明收益。

### 内存 profile

内存排查先区分“当前占着多少”和“累计分配了多少”。`inuse_space` 是采样时仍存活的对象，适合查泄漏和缓存膨胀；`alloc_space` 是程序启动以来累计分配，适合查高频临时对象。GC 后 `inuse_space` 能回落，通常不是泄漏；每次 GC 后基线仍持续抬高，才需要看对象类型和分配栈。

```bash
go tool pprof http://127.0.0.1:6060/debug/pprof/heap
# 进入交互后：
top -inuse_space
top -alloc_space
list SomeFunction
```

如果怀疑缓存无上限，还要核对缓存条目数、TTL 和命中率；如果怀疑大对象，查请求体大小和返回 JSON 的字段数。只看 `process_resident_memory_bytes` 会把 Go 尚未归还操作系统的内存误判成泄漏。

### goroutine

goroutine profile 要同时看数量和栈。数量缓慢上涨只是线索，真正定位要按栈聚合，找出同一类 goroutine 为什么没有退出：

```bash
curl -sS 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=2' > goroutines.txt
go tool pprof http://127.0.0.1:6060/debug/pprof/goroutine
```

在 pprof 里执行 `top`，再对热点执行 `list`。典型栈包括 `chan send`、`chan receive`、`sync.runtime_Semacquire`、`database/sql.(*DB).conn`。每个泄漏都对应一个缺失的退出条件：context 取消、channel 关闭、超时或连接归还。修复后在同等负载下观察数量和栈是否回落，并加一个带超时的并发测试。

### block / mutex

block profile 找 goroutine 在 channel、锁、网络上的等待；mutex profile 找锁竞争。先在受控环境开启采样，再施加能复现问题的负载：

```go
runtime.SetBlockProfileRate(1)
runtime.SetMutexProfileFraction(1)
```

```bash
curl -sS 'http://127.0.0.1:6060/debug/pprof/block?seconds=30' -o block.pprof
curl -sS 'http://127.0.0.1:6060/debug/pprof/mutex?seconds=30' -o mutex.pprof
go tool pprof -top ./bin/server block.pprof
go tool pprof -top ./bin/server mutex.pprof
```

预期输出按累计阻塞时间排序。若热点指向一个包含 HTTP 请求、SQL 或文件 IO 的临界区，应缩小锁范围或把慢操作移出锁；若热点是 channel 发送，检查消费者是否退出或处理太慢。修复后用相同负载采第二次 profile，确认阻塞时间、P99 和 goroutine 数下降。采样率本身有开销，生产不能长期设成 1，排查完要关闭或调低。

---

## 13.4 数据库、Redis 和 MQ 的专项排查

性能问题很少只在 Go 代码里。应用 CPU 不高但 P99 很高时，要沿调用链检查依赖的等待时间、并发上限和错误重试。下面三类依赖要分开看，不能用一个“下游慢”概括。

### 数据库：先看连接池，再看 SQL 和锁

`database/sql` 的连接池满时，请求会排队，表现是 CPU 不高、P99 上升、goroutine 停在获取连接。先看 `db.Stats()` 的 `InUse`、`WaitCount`、`WaitDuration`，再看数据库侧的慢查询和锁等待：

```sql
SHOW FULL PROCESSLIST;
SELECT trx_id, trx_state, trx_started, trx_mysql_thread_id, trx_query
FROM information_schema.innodb_trx
ORDER BY trx_started;
```

如果连接池满但数据库 CPU 低，优先怀疑长事务、行锁等待、连接泄漏或某个查询超时；如果数据库 CPU 高，再看执行计划、扫描行数和临时表。不要为了“让连接池不报错”直接把 `MaxOpenConns` 调大，这可能把更多请求同时压到已经饱和的数据库上。

### Redis：延迟、大 key 和热 key

Redis 变慢时先看客户端耗时和服务端慢日志，再查单个 key 的大小和访问分布：

```bash
redis-cli --latency
redis-cli --bigkeys
redis-cli --hotkeys       # 需要 LFU 淘汰策略
redis-cli slowlog get 20
```

大 key 会导致单次网络传输和删除阻塞；热 key 会让单个分片先饱和。前者应拆 key、限制集合长度并异步删除，后者应做本地缓存、复制/拆分或限制刷新频率。应用侧的 Redis 超时必须设置，否则 Redis 抖动会把 HTTP handler 的 goroutine 全部占住。

### MQ：积压是结果，不是根因

消费 lag 上升时同时比较生产速率、消费速率、单条处理耗时、错误/重试数量和消费者实例数。生产稳定、消费变慢，通常是下游数据库或 HTTP 调用阻塞；消费者实例变少，可能是崩溃、探针失败或 rebalance；毒消息反复重试，应进入死信队列而不是无限重试。扩容消费者前先确认 partition 数和下游容量，否则只是把压力转移给数据库。

---

## 13.5 常见性能瓶颈

| 现象 | 可能原因 | 验证 |
|---|---|---|
| CPU 高 | 热循环、JSON、加密、GC | CPU profile |
| 内存涨 | 泄漏、缓存无上限、大对象 | heap profile |
| goroutine 涨 | 泄漏、下游慢、无超时 | goroutine profile |
| P99 高但 CPU 低 | 锁等待、DB、网络 | block/mutex、trace |
| 数据库慢 | 无索引、锁、连接池 | 慢查询、EXPLAIN |
| GC 频繁 | 分配多、堆小 | GODEBUG=gctrace=1 |

### 优化顺序

1. 先定位瓶颈，不要猜；
2. 优化最大头；
3. 每次只改一个点；
4. 用 benchmark/压测验证；
5. 确认没有引入新问题。

---

## 13.6 Go 性能小技巧

- 预分配切片：`make([]T, 0, n)`；
- 避免循环里字符串拼接，用 `strings.Builder`；
- 避免不必要的 JSON 反复序列化；
- 对象池 `sync.Pool` 只用于高频短生命周期对象，不要过早优化；
- 减少锁粒度，但先保证正确；
- 数据库批量操作代替 N+1；
- 设置连接池和超时；
- 用 `context` 取消无用工作。

不要为了“性能”牺牲可读性，除非 profile 证明它是热点。

---

## 13.7 调试工具

### Delve

Delve 适合本地复现“代码走到了哪个分支、变量在什么时候变错、返回值从哪里来”。它不适合在生产环境随手 attach：调试器会暂停进程、改变时序，还需要额外权限。正确做法是从日志/trace 拿到输入和 request_id，在本地或隔离环境用同样数据启动 Delve。

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
dlv debug ./cmd/server -- --config ./config/dev.json
```

一个实际会话可以这样走：

```text
(dlv) break internal/service/recharge.go:42
Breakpoint 1 set at .../recharge.go:42
(dlv) continue
(dlv) args
req = service.RechargeRequest {ReqID: "req-demo-1", UID: "u-1001", Amount: 0}
(dlv) next
(dlv) print err
error(*errors.errorString) *{s: "amount must be positive"}
(dlv) bt
```

断点应打在“能证明假设”的位置，而不是到处乱停。先记录期望值，再单步看实际值；`next` 跨过当前行，`step` 进入被调用函数，`print` 看变量，`bt` 看调用栈。调试结束后删除临时断点，把复现输入写进测试；只靠手工调试修好的 bug，下一个人还会再踩一次。

### 日志 + 断点 + 最小复现

线上不能随便 attach 调试器。流程：

1. 从日志/trace 找到 request_id；
2. 用相同参数在本地复现；
3. 写一个失败测试固定 bug；
4. 修复；
5. 测试通过后发布。

### 复现 bug 的标准步骤

复现不是“再点一次看看”。要把不可控的线上现象变成可重复的实验：固定版本、配置、数据和输入，记录期望与实际，再逐步删掉无关因素。只有能稳定失败，后续修复才有证据。

```markdown
1. 现象：什么接口、什么参数、什么时间、影响多少用户
2. 期望：应该发生什么
3. 实际：实际返回/日志/数据
4. 环境：版本、配置、依赖状态
5. 最小输入：能稳定复现的最少步骤
6. 根因：代码/数据/依赖/配置
7. 修复：代码 + 测试
8. 验证：本地、预发、线上观察
```

例如“充值偶发 500”可以缩成：同一个 `uid`、两个并发请求、相同 `req_id`，在 MySQL 事务隔离级别为默认值时稳定触发。先把并发从 20 降到 2，再把真实服务换成 service + 测试数据库，最后得到一个失败的 Go 测试。这个测试既是复现证据，也是修复后的回归门禁。

---

## 13.8 线上 bug 排查流程

线上排障要并行做两件事：一边确认影响面和止血，一边保留证据。不能为了“先回滚看看”把日志、堆栈和慢查询冲掉；也不能为了查根因让用户持续失败。工程上通常先切只读、摘流、回滚或关闭新功能，确认影响停止扩大，再在隔离环境复现。

```text
1. 确认影响面：路由、用户范围、开始时间、错误率和业务损失
2. 找变化：最近发布、配置、流量、数据任务、依赖升级
3. 看指标：入口 QPS/错误率/P99、实例数、资源、依赖饱和度
4. 看日志：按 request_id/trace_id 串起请求，找第一个异常
5. 看依赖：DB 锁和连接池、Redis 延迟、MQ lag、第三方超时
6. 止损：回滚、降级、切流、限流、扩容，选影响最小的动作
7. 定位根因：提出可证伪假设，用 trace、profile、SQL、复现实验验证
8. 修复 + 回归：失败测试先红后绿，灰度观察
9. 复盘：补测试、告警、容量、变更门禁和运行手册
```

**先止损再定位**不等于跳过证据。回滚前保存发布版本、镜像 digest、错误日志时间窗、关键指标截图和慢查询样本；回滚后再用同一时间窗定位。一个合格的复盘要能回答：为什么发生、为什么没提前拦住、为什么影响扩大、修复后如何证明不会重演。

---

## 13.9 典型 bug 案例

### 案例 1：goroutine 泄漏

现象：内存和 goroutine 持续上涨，重启恢复。

排查：

```bash
curl 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=2'
```

发现大量 goroutine 卡在：

```go
ch <- result // 无人接收
```

原因：下游超时后调用方返回，worker 还在向无缓冲 channel 发送。

修复：channel 带缓冲 + `select { case ch <- r: case <-ctx.Done(): }`。

### 案例 2：数据库连接池耗尽

现象：P99 飙升，`db.Stats().WaitCount` 增长。

原因：某个查询没有走索引，单请求耗时 2s，连接被占满。

修复：加索引 + 查询超时 + 限制并发。

### 案例 3：缓存击穿

现象：某个热点 key 过期瞬间数据库 QPS 暴涨。

修复：互斥重建 + 逻辑过期 + 限流。

---

## 13.10 性能回归防护

性能回归往往是缓慢发生的：某次提交多了一次数据库查询，某次配置把连接池调小，某个新字段让响应体翻倍。单次测试可能只慢 5ms，但上线后叠加高并发就会放大。防护要覆盖代码、发布和运行三个层次。

- 对稳定的纯函数和热点算法写 benchmark，使用 `-benchmem` 记录分配；
- 对关键接口保留固定数据和固定压测脚本，发布前比较 P50/P95/P99、错误率和数据库 QPS；
- CI 跑短时间 smoke 压测，夜间或发布环境跑长时间容量测试；
- 监控 GC、goroutine、连接池等待、慢查询和 Redis/MQ 延迟，设置趋势告警；
- 大促前做容量演练和故障注入，确认限流、熔断、扩容和回滚真的有效。

性能目标必须带环境、数据量、并发模型和持续时间。只报“QPS 5000”没有延迟、错误率、资源瓶颈和数据分布，无法用于容量决策。

---

## 习题

### 基础题

1. QPS、并发、延迟的关系是什么？
2. 为什么压测要看 P99 而不是平均值？
3. CPU profile、heap profile、goroutine profile 分别解决什么？
4. 线上服务为什么不能随便开 pprof 公网访问？
5. 线上 bug 为什么先止损再定位？
6. Delve 适合什么场景？线上为什么不能随便 attach？
7. 连接池耗尽有哪些信号？
8. 如何判断是内存泄漏还是正常缓存增长？

### 动手题

9. 写一个 k6 脚本，对充值写接口做 100 QPS、1 分钟、P99 < 200ms 的压测。
10. 给一个 Go 服务加 pprof，采集 30 秒 CPU profile 并写出 top 命令。
11. 复现一个 bug：接口偶发 500，写出你的最小复现步骤。
12. 用 benchmark 对比 `strings.Builder` 和 `+=` 拼接 1000 次。

### 综合题

13. 线上接口 P99 从 100ms 升到 1.5s，CPU 正常，数据库 CPU 正常，但连接池 active 满。写出完整排查路径和可能原因。

---

## 参考答案

<details>
<summary>展开</summary>

1. 并发是同时在途请求数，QPS 是每秒完成数，平均延迟是每个请求平均耗时。稳定时近似 `并发 = QPS × 平均延迟`；例如 200 QPS、平均 50ms，约需 10 个并发。但平均值会掩盖长尾，容量要同时看 P99 和错误率。
2. 假如 99 个请求 20ms、1 个请求 5s，平均值约 69.8ms，但 1% 用户等了 5s。P99 能暴露这种长尾，避免用“平均很快”掩盖超时和重试。
3. CPU profile 找计算热点和调用链；heap profile 找当前存活对象与累计分配来源；goroutine profile 找泄漏和阻塞栈；block/mutex profile 找锁和 channel 等待。
4. pprof 端点可能暴露堆栈、路径、函数名和运行时行为，攻击者还能利用 profile 消耗 CPU/内存。管理端口应只监听内网或本机，增加鉴权和访问控制，排查完关闭临时入口。
5. 用户仍在失败时，先回滚、切流、限流或降级，避免影响扩大；同时保存日志、指标、trace、慢查询和版本信息，止损后在隔离环境复现。
6. Delve 适合本地单步查看分支和变量；生产 attach 会暂停进程、改变时序、影响在途请求，还可能留下敏感数据。线上应先通过日志、trace、pprof 和离线复现定位。
7. `database/sql` 的 `WaitCount`、`WaitDuration` 上升，`InUse` 接近 `MaxOpenConns`，请求 P99 上升但 CPU 不高；数据库侧还会看到长事务、锁等待或慢查询。
8. GC 后 heap `inuse_space` 能回到基线，多半是缓存/临时对象；每次 GC 后基线持续抬高，并且对象类型集中在缓存、连接或 goroutine 引用的数据上，才更像泄漏。还要看缓存条目数和 TTL。
9. 100 QPS 应使用 `constant-arrival-rate`，不要用固定 VU 猜并发：
```js
import http from 'k6/http';
import { check } from 'k6';

export const options = {
  scenarios: {
    query: {
      executor: 'constant-arrival-rate',
      rate: 100,
      timeUnit: '1s',
      duration: '1m',
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.001'],
    http_req_duration: ['p(99)<200'],
  },
};

export default function () {
  const key = `k6-${__VU}-${__ITER}`;
  const payload = JSON.stringify({
    req_id: key,
    amount_cent: 1,
    channel: 'manual',
  });
  const res = http.post('http://127.0.0.1:8081/v1/recharges', payload, {
    headers: {
      'Content-Type': 'application/json',
      'Authorization': 'Bearer demo-token',
      'X-Request-Id': key,
    },
  });
  check(res, {
    'status is 201': (r) => r.status === 201,
    'code is OK': (r) => r.json('code') === 'OK',
  });
}
```
先执行 `k6 run --vus 1 --iterations 1 load.js` 验证脚本，再 `k6 run --out json=result.json load.js`。预期 `http_reqs` 约 6000（100×60，允许边界误差），P99 小于 200ms，失败率小于 0.1%；若 `dropped_iterations` 很高，说明 `maxVUs` 或压测机不足以维持 100 QPS，要先修实验条件。

10. 在独立管理端口启用：
```go
import _ "net/http/pprof"

go func() {
    if err := http.ListenAndServe("127.0.0.1:6060", nil); err != nil {
        log.Printf("pprof server: %v", err)
    }
}()
```
启动业务负载后执行：
```bash
curl -sS 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30' -o cpu.pprof
go tool pprof -top -cum ./bin/server cpu.pprof
go tool pprof -http=:8080 ./bin/server cpu.pprof
```
`top` 看自身耗时，`top -cum` 看调用链，`list 函数名` 看具体行。验证是否可信要记录采样时的 QPS、版本、实例和负载；优化后用同一命令、同负载复测，预期热点占比下降且 P99 改善。若采样期间没有流量，profile 的结论不可用。

11. 例子：`GET /v1/orders/{id}` 偶发 500。先固定输入和证据：记录测试订单 ID、请求时间、版本、`request_id`，从日志找到完整错误栈。把并发从 20 降到 1，删除无关 header/参数；若仍失败，用 `httptest` 或直接调用 service 建立最小测试。假设是“订单存在但金额字段为 NULL 导致扫描失败”，测试数据只保留这一条件，预期旧代码稳定返回 500；修复扫描或约束后，同一测试应通过。最后用原 curl 重放，并在数据库确认订单状态没有被错误修改。

12. 测试文件示例：
```go
func BenchmarkConcatPlus(b *testing.B) {
    for i := 0; i < b.N; i++ {
        s := ""
        for j := 0; j < 1000; j++ { s += "x" }
        _ = s
    }
}

func BenchmarkConcatBuilder(b *testing.B) {
    for i := 0; i < b.N; i++ {
        var sb strings.Builder
        sb.Grow(1000)
        for j := 0; j < 1000; j++ { sb.WriteString("x") }
        _ = sb.String()
    }
}
```
执行 `go test ./... -run '^$' -bench 'BenchmarkConcat' -benchmem -count=3`。预期 Builder 的 `ns/op` 更低且 `allocs/op` 更少；若结果相反，先检查编译器优化、输入规模和是否把结果赋给逃逸变量，不要直接宣布结论。

13. 按证据链处理：①看入口 P99、QPS、错误率，确认只有哪个接口/实例异常；②用 trace 找最长 span，若停在 `database/sql.(*DB).conn`，说明在等连接而不是 SQL 执行；③看 `db.Stats()` 的 `InUse`、`WaitCount`、`WaitDuration`，确认连接池是否满以及等待从何时开始；④查 `innodb_trx`、`data_lock_waits` 和 `SHOW FULL PROCESSLIST`，找长事务、锁等待和慢 SQL；⑤对比发布、流量和数据量，若新代码把远程调用放进事务，优先关闭开关或回滚止血；⑥修复后压测同一 uid 和多个 uid，验证连接池等待下降、P99 回到目标且业务余额/流水一致。

</details>
