# 第 10 章 限流、熔断、降级与 QPS

> 目标：当流量超过系统能力、或下游开始超时时，你的服务能**有策略地慢下来**，而不是一起雪崩。
>
> 本章的每个算法都会给出可运行代码和预期现象。不要只记名字——面试能背“令牌桶允许突发”，线上却把限流器放在单机内存里，等于没限。

---

## 10.1 先把五个词分清楚

限流、熔断、降级、重试、超时解决不同问题：限流控制进入系统的流量，熔断阻止持续调用坏下游，降级提供可接受的替代结果，重试处理短暂失败，超时防止无限等待。把它们混成一个“保护”开关，常常会把故障放大。

| 词 | 回答的问题 | 作用点 | 失败时的表现 |
|---|---|---|---|
| QPS | 每秒能处理多少请求 | 容量指标 | 排队、延迟上升 |
| 限流 | 主动拒绝多少请求 | 入口/下游调用 | 返回 429 |
| 熔断 | 下游已经坏了，还要不要继续调 | 调用方 | 快速失败，不占线程 |
| 降级 | 功能不可用时给什么替代结果 | 业务层 | 返回缓存/默认值 |
| 隔离 | 一个依赖坏了，如何不拖垮全部 | 资源池 | 只有部分功能受影响 |

它们不是一回事：

- 限流是**保护自己不被压垮**；
- 熔断是**保护自己不被下游拖死**；
- 降级是**坏的时候还能提供什么**；
- 隔离是**把故障关在笼子里**。

一个完整的防护链通常是：

```
入口限流 → 鉴权 → 业务处理
                     ├─ 缓存（挡读流量）
                     ├─ 下游调用：超时 + 重试预算 + 熔断 + 舱壁
                     └─ 降级返回
```

---

## 10.2 QPS 与容量估算：先算，再压测

### 三个基本量

```
QPS = 请求数 / 秒
并发数 ≈ QPS × 平均延迟（Little's Law）
所需实例数 = 峰值 QPS / 单实例安全 QPS
```

例子：接口平均延迟 50ms，峰值 2000 QPS：

```
并发 ≈ 2000 × 0.05 = 100
```

如果单实例压测在 P99 < 200ms 时能稳定扛 300 QPS，那么：

```
2000 / 300 ≈ 6.7 → 至少 7 个实例，再留 30% 余量 → 10 个
```

### 为什么不能只看平均值

平均值会把少量极端慢请求抹平，而限流和熔断往往由长尾触发。例如 99 个请求 10ms、1 个请求 2s，平均值约 30ms，但用户体验和连接占用已经被那 1% 拖垮。因此容量判断至少同时看 P99/P999、错误率和并发量，不能只报一个平均值。

还要区分不同位置的 QPS，否则会把“入口收到了多少请求”误当成“数据库实际承受了多少查询”：

- 入口 QPS（网关统计）；
- 应用 QPS（handler 统计）；
- 数据库 QPS（`SHOW GLOBAL STATUS LIKE 'Questions'`）；
- 缓存 QPS；
- 下游 QPS。

只压入口不看数据库，等于只量了水管口径，没看水库。

### 压测前必须确定的四件事

没有目标的压测只会得到一堆漂亮数字。先确定要验证的接口、目标 QPS、可接受的 P99/错误率、压测时长和流量模型；还要确认下游是否允许被打、是否需要独立环境、失败时如何止血。

1. **目标**：验证 2000 QPS 下 P99 < 200ms，错误率 < 0.1%；
2. **数据**：测试库数据量和生产同量级，否则索引命中率失真；
3. **流量模型**：读写比例、参数分布、缓存命中率；
4. **观察面**：CPU、内存、GC、goroutine、连接池、慢查询、下游延迟。

没有这四件事的压测，只能叫“把服务打挂了一次”。

---

## 10.3 限流算法：四种，各自解决不同问题

### 固定窗口：最简单，但有边界突刺

按秒计数，超过阈值拒绝：

```go
type FixedWindow struct {
    mu     sync.Mutex
    window int64
    count  int
    limit  int
}

func (f *FixedWindow) Allow(now int64) bool {
    f.mu.Lock()
    defer f.mu.Unlock()
    if now != f.window {
        f.window = now
        f.count = 0
    }
    if f.count >= f.limit {
        return false
    }
    f.count++
    return true
}
```

问题：限制 100/s 时，第 1 秒最后 10ms 来 100 个、第 2 秒最初 10ms 又来 100 个，**20ms 内通过了 200 个**，瞬间是 10 倍。

固定窗口适合对突刺不敏感的粗粒度限制，不适合保护脆弱下游。

### 滑动窗口：按真实时间回看

维护最近 N 秒的请求时间戳，统计窗口内数量：

```go
type SlidingWindow struct {
    mu     sync.Mutex
    times  []time.Time
    window time.Duration
    limit  int
}

func (s *SlidingWindow) Allow(now time.Time) bool {
    s.mu.Lock()
    defer s.mu.Unlock()

    cutoff := now.Add(-s.window)
    i := 0
    for i < len(s.times) && s.times[i].Before(cutoff) {
        i++
    }
    s.times = s.times[i:] // 丢掉窗口外的

    if len(s.times) >= s.limit {
        return false
    }
    s.times = append(s.times, now)
    return true
}
```

**执行过程**：窗口 1s、限制 3。请求到达时间 `0ms, 100ms, 900ms` 都通过；`950ms` 被拒；到 `1100ms` 时，`0ms` 已滑出窗口，又可以通过。

问题：每个请求存一个时间戳，单机内存随 QPS 线性增长；分布式要放 Redis ZSet，成本更高。

生产常用**滑动窗口计数器**：把 1 秒切成 10 个 100ms 小格，统计最近 10 格，误差最多 100ms，内存固定。

### 令牌桶：允许突发，长期平均受限

桶容量 `burst`，每秒补充 `rate` 个令牌。请求取不到令牌就拒绝或等待。

```go
type TokenBucket struct {
    mu       sync.Mutex
    tokens   float64
    burst    float64
    rate     float64 // 每秒补充
    last     time.Time
}

func (t *TokenBucket) Allow(now time.Time) bool {
    t.mu.Lock()
    defer t.mu.Unlock()

    elapsed := now.Sub(t.last).Seconds()
    t.last = now
    t.tokens = math.Min(t.burst, t.tokens+elapsed*t.rate)
    if t.tokens < 1 {
        return false
    }
    t.tokens--
    return true
}
```

**现象**：`rate=10, burst=20`，空闲一段时间后突然来 20 个请求，全部通过（消耗桶里积累的令牌）；之后稳定在每秒 10 个。

适合：允许短突发的 API 入口、下游有缓冲能力的场景。

### 漏桶：恒定速率流出，绝对平滑

请求先进入队列，以固定速率被处理，队列满则拒绝。

```
入水（任意速率） → [队列] → 出水（固定速率）
```

适合：必须保护极脆弱下游、要求严格平滑的场景。代价是增加排队延迟。

### 四种算法对比

固定窗口简单但临界点会有双倍突发；滑动窗口更平滑但成本更高；漏桶强制恒定输出；令牌桶允许短突发但平均速率受限。选择取决于业务能否接受突发，而不是哪个算法名字更高级。

| 算法 | 突发 | 内存 | 实现复杂度 | 典型场景 |
|---|---|---|---|---|
| 固定窗口 | 边界双倍 | O(1) | 低 | 粗粒度配额 |
| 滑动窗口 | 受控 | O(窗口请求数) | 中 | 精确限流 |
| 令牌桶 | 允许 | O(1) | 中 | API 网关 |
| 漏桶 | 不允许 | O(队列) | 中 | 保护脆弱下游 |

---

## 10.4 限流放在哪：位置决定效果

```
客户端 → CDN/WAF → 网关（全局限流） → 服务（接口级限流） → 下游调用（依赖级限流）
```

- **网关**：按 IP/用户/租户限流，挡住恶意流量；
- **服务入口**：按接口限流，保护自己的 goroutine 和连接池；
- **下游调用**：按依赖限流，保护数据库/第三方，防止一个功能打垮共享资源。

### 单机限流 vs 分布式限流

单机内存限流：`limit × 实例数` 才是集群总限额。扩容后总限额自动变大，缩容后变小。适合“每个实例自我保护”。

分布式限流：用 Redis + Lua 原子执行，所有实例共享配额：

```lua
-- KEYS[1] = 限流 key
-- ARGV[1] = 当前毫秒, ARGV[2] = 窗口毫秒, ARGV[3] = 上限
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])

redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, now - window)
local count = redis.call('ZCARD', KEYS[1])
if count >= limit then
  return 0
end
redis.call('ZADD', KEYS[1], now, now .. ':' .. math.random())
redis.call('PEXPIRE', KEYS[1], window)
return 1
```

代价：每个请求一次 Redis 往返；Redis 故障时要么放行（可能压垮后端）要么拒绝（误伤用户），必须提前决定。

### 限流 key 怎么选

限流粒度决定了公平性和保护对象。按 IP 防止单客户端刷接口，按用户防止单账号滥用，按接口保护某个昂贵操作，按租户分配配额；key 太粗会误伤所有人，太细会让攻击者换 key 绕过。

- 按 IP：防爬虫，但 NAT 后大量用户共享 IP；
- 按用户：公平，但未登录请求没有用户 ID；
- 按租户/AppID：多租户系统必备；
- 按接口：保护单个重接口；
- 组合：`user:{uid}:api:{route}`。

### 被限流后返回什么

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 1
Content-Type: application/json

{"code":"RATE_LIMITED","message":"too many requests","request_id":"req-1"}
```

告诉客户端多久后重试，客户端才能做退避。只返回 500 会让客户端立即重试，越打越满。

---

## 10.5 熔断：下游坏了就别再调了

### 三个状态

熔断器在关闭、打开、半开之间变化。关闭时正常放行；错误率超阈值后打开，快速失败；经过冷却时间进入半开，只放少量探测请求，成功才恢复。没有半开就会在依赖恢复后仍永久拒绝。

```
        失败率超阈值
CLOSED ───────────────→ OPEN
  ↑                       │
  │  试探成功              │ 冷却时间到
  └──── HALF_OPEN ←───────┘
           │
           └─ 试探失败 → 回到 OPEN
```

- **CLOSED**：正常调用，统计成功率；
- **OPEN**：直接失败，不发起真实调用；
- **HALF_OPEN**：放少量请求试探，成功则恢复，失败则继续断开。

### 一个最小实现

```go
type Breaker struct {
    mu        sync.Mutex
    state     string // closed/open/half
    failures  int
    threshold int
    openUntil time.Time
    cooldown  time.Duration
}

func (b *Breaker) Allow(now time.Time) bool {
    b.mu.Lock()
    defer b.mu.Unlock()

    switch b.state {
    case "open":
        if now.Before(b.openUntil) {
            return false
        }
        b.state = "half"
        return true
    default:
        return true
    }
}

func (b *Breaker) Record(err error) {
    b.mu.Lock()
    defer b.mu.Unlock()

    if err != nil {
        b.failures++
        if b.state == "half" || b.failures >= b.threshold {
            b.state = "open"
            b.openUntil = time.Now().Add(b.cooldown)
            b.failures = 0
        }
        return
    }
    b.state = "closed"
    b.failures = 0
}
```

生产直接用成熟库（如 `sony/gobreaker`），但要知道它的参数含义：

- 统计窗口多长？
- 最少多少个请求才开始判断？（避免 1 个失败就熔断）
- 失败率阈值多少？
- OPEN 冷却多久？
- HALF_OPEN 放多少请求？

**最小实现怎么测。** 给熔断器注入可控 `now`，不要靠真实等待。测试顺序应是：连续 5 次失败后，第 6 次 `Allow` 返回 false；把时间推进到冷却结束，第一次探测返回 true 且状态进入 HALF_OPEN；探测成功一次后，后续请求恢复；如果探测失败，则重新 OPEN。还要跑一个并发测试，确认多个 goroutine 同时越过冷却时间时，不会把上百个探测请求全部放给刚恢复的下游。

**失败场景。** 只统计失败次数、不设最小请求量，发布后第一个偶发超时就会打开熔断；HALF_OPEN 不限制并发，会在下游刚恢复时瞬间打回原形；只熔断 HTTP 500，却把连接超时当成成功，永远触发不了。统计口径必须覆盖超时、连接失败和明确的 5xx，参数则要由压测和故障演练验证。

### 熔断和重试必须一起设计

重试会给下游增加流量。如果下游已经被熔断或错误率很高，继续重试只会放大雪崩。正确做法是只对幂等、可恢复的错误重试，使用指数退避和抖动，并在熔断打开时直接失败或走降级。

下游超时 → 调用方重试 3 次 → 下游压力变 3 倍 → 更超时。正确做法：

- 只对**幂等**请求重试；
- 限制重试次数和总预算（如最多 2 次、总耗时不超过 1s）；
- 加指数退避 + 抖动；
- 熔断打开时**不要重试**；
- 区分错误：400 不重试，超时/503 可重试。

---

## 10.6 降级：坏的时候给什么

降级不是“返回一个假成功”。要让用户知道当前是降级状态，并保证资金、权限、库存等关键规则不被伪造。常见降级包括读旧缓存、关闭非核心推荐、返回排队中、只保留核心支付链路。

降级不是“把功能关掉”，而是提前定义**每一层坏掉时的替代行为**：

| 依赖 | 正常 | 降级 |
|---|---|---|
| 推荐服务 | 个性化推荐 | 热门榜单 |
| 搜索 | ES 全文搜索 | 标题前缀查询 / 提示稍后重试 |
| 用户画像 | 实时特征 | 默认人群 |
| 余额查询 | 主库强一致 | 不允许降级，直接报错 |
| 评论数 | 实时计数 | 缓存值，允许延迟 |

降级要满足：

1. **可开关**：配置中心能动态打开，不用发版；
2. **有默认值**：不能降级后返回 `null` 让前端崩；
3. **可观测**：降级次数要打点，否则坏了没人知道；
4. **不掩盖核心错误**：钱、库存等强一致数据不能“降级成猜一个”。

降级必须演练。把推荐服务主动改成超时，预期推荐接口返回热门榜单且响应头/响应体带 `degraded=true`，同时 `recommendation_degraded_total` 增长；再把余额数据库停掉，余额接口必须明确失败，不能返回 0 或旧缓存冒充实时余额。两种结果都符合预期，才说明降级策略既有用户体验又有业务边界。

---

## 10.7 隔离：把故障关进笼子

### 舱壁模式

给不同下游分配独立资源池：

```go
type Semaphore chan struct{}

func (s Semaphore) Acquire(ctx context.Context) error {
    select {
    case s <- struct{}{}:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

func (s Semaphore) Release() { <-s }

var (
    searchPool = make(Semaphore, 20)  // 搜索最多 20 并发
    userPool   = make(Semaphore, 50)  // 用户服务最多 50 并发
)
```

搜索变慢时，最多占 20 个 goroutine，不会把整个进程的 goroutine 和连接池吃光。

### 其他隔离维度

除线程池外，还要按依赖、实例、租户和时间隔离。数据库连接池耗尽时，非核心报表不能抢核心支付连接；单租户异常不能拖垮其他租户。隔离的目标是限制故障传播范围。

- **连接池隔离**：核心业务和报表用不同数据库连接池；
- **线程/协程池隔离**：慢任务放独立 worker pool；
- **集群隔离**：核心服务和离线任务不同集群；
- **租户隔离**：大客户独立实例，避免影响小客户。

---

## 10.8 自适应保护：让系统自己踩刹车

固定阈值需要人猜。自适应限流根据**当前延迟/并发**动态调整：

- 并发超过安全值 → 拒绝新请求；
- P99 超过目标 → 降低允许并发；
- CPU 超过 80% → 触发保护。

最常用的是“并发上限 + 延迟反馈”。例如目标 P99 200ms，当最近 30 秒 P99 连续超过 300ms，就把允许并发从 100 降到 80；延迟恢复后再每次增加 5，而不是瞬间回到 100。这样系统会把资源让给已经进入的请求，同时避免恢复瞬间再次过载。

```text
每 10 秒计算一次：
  if p99 > 300ms for 3 windows: limit = max(minLimit, limit * 0.8)
  else if p99 < 180ms for 3 windows: limit = min(maxLimit, limit + 5)
  在途请求 >= limit: 直接返回 429 + Retry-After
```

**怎么验证。** 在压测环境人为把数据库延迟从 20ms 调到 300ms，观察允许并发是否下降、入口 429 是否上升、P99 是否被压回目标、下游连接池是否停止增长。恢复数据库延迟后，系统应在几十秒内平滑恢复，而不是立刻把并发打满。

**失败场景。** 用瞬时 P99 调控会让限流器持续抖动；只降不升会让系统在短暂抖动后永久低容量；阈值比上游超时还宽松，等自适应生效时请求早已堆满。自适应保护是最后一层，不能替代索引、缓存、连接池和超时。

---

## 10.9 一次故障的完整防御链

场景：搜索服务依赖 ES，ES 因大查询变慢。

```
1. 入口限流：搜索接口单实例 100 QPS，挡住突发
2. 超时：ES 查询 300ms 超时，不让 goroutine 无限等
3. 舱壁：搜索最多 20 并发，不占满全部 goroutine
4. 熔断：30 秒内失败率 > 50% 且请求数 > 20 → OPEN
5. 降级：OPEN 期间返回热门榜单，标注“个性化推荐暂不可用”
6. 重试：只对超时重试 1 次，且总预算 500ms
7. 监控：搜索错误率、熔断状态、降级次数告警
8. 恢复：冷却 10 秒后 HALF_OPEN 放 5 个请求，成功则恢复
```

没有第 2、3 步，熔断也救不了你——请求已经堆在 goroutine 里了。没有第 5 步，熔断只是把错误更快地返回给用户。**这些机制是一套，不是单点。**

---

## 10.10 常见错误

这些错误的共同点是“保护措施本身造成更大故障”：重试没有上限、超时比上游还长、熔断没有半开、限流只在一个实例做、降级返回假数据。上线前要做故障注入验证。

| 错误 | 后果 | 正确做法 |
|---|---|---|
| 只在单机内存限流，却以为限的是集群 | 实际总流量 = 限额 × 实例数 | 明确单机/分布式语义 |
| 限流阈值拍脑袋 | 要么误伤，要么没保护 | 压测 + 观察 + 动态调整 |
| 熔断后还重试 | 雪崩加速 | 熔断打开时快速失败 |
| 所有错误都重试 | 参数错误被放大 | 只重试幂等 + 可恢复错误 |
| 没有超时 | goroutine/连接堆积 | 每个外部调用都要 ctx 超时 |
| 降级返回 `null` | 前端崩溃 | 定义默认值和契约 |
| 只监控入口 QPS | 看不到数据库瓶颈 | 全链路指标 |

---

## 习题

### 基础题

1. 限流、熔断、降级、隔离分别解决什么问题？
2. 固定窗口在边界为什么能通过双倍流量？画时间线说明。
3. 令牌桶的 `rate` 和 `burst` 分别控制什么？
4. 熔断器三个状态如何转换？HALF_OPEN 的作用是什么？
5. 为什么“超时 + 重试”不加控制反而会放大故障？
6. 单机限流 100 QPS、10 个实例，集群总限额是多少？扩容到 20 个实例呢？
7. 什么数据不能降级？举例说明。
8. 舱壁模式如何防止一个慢依赖拖垮整个服务？

### 动手题

9. 用 Go 实现一个令牌桶，要求并发安全，写测试验证：空闲后能突发 `burst` 个，之后稳定在 `rate/s`。
10. 用 Redis Lua 实现“每个用户每分钟 60 次”的限流，说明 key 设计和过期策略。
11. 给一个调用第三方支付的接口设计：超时、重试、熔断、降级分别怎么配？支付请求能随便重试吗？

### 综合题

12. 设计一个秒杀接口的防护链：入口、库存、下单、支付。说明每层限流阈值怎么估算，哪些环节必须强一致，哪些可以异步。

---

## 参考答案

<details>
<summary>展开：概念答案、Go/Lua 实现、预期输出和故障验证</summary>

### 1. 四个保护机制分别解决什么

| 机制 | 保护对象 | 典型动作 | 不该做什么 |
|---|---|---|---|
| 限流 | 自己的入口容量 | 超过阈值返回 429/排队 | 不能把已进入线程/协程的请求变没 |
| 熔断 | 自己和故障下游 | 连续失败后快速失败 | 打开后不能继续无限重试 |
| 降级 | 用户可接受的部分功能 | 返回缓存、默认值、只读结果 | 资金、库存、支付状态不能猜 |
| 隔离 | 故障影响范围 | 依赖独立连接池、并发池、实例池 | 不能所有依赖共享一个无限池 |

在 `mini-coin` 中，入口可以按 uid/route 限流；支付下游连续超时可以熔断；余额查询可返回短时缓存或明确错误；支付、Redis、报表各用独立并发池，避免一个慢依赖占满全部 goroutine。

### 2. 固定窗口为什么会有双倍突刺

设限额为 100 次/秒，窗口边界在整秒：

```text
第 1 秒 00.900~00.999：通过 100 次
第 2 秒 01.000~01.099：通过 100 次
实际 200ms 内通过 200 次
```

任何时刻的瞬时 QPS 可以接近 `2 × limit`。验证时不要等真实一分钟，写一个注入时钟的窗口实现：

```go
type FixedWindow struct {
    mu          sync.Mutex
    limit       int
    windowStart time.Time
    count       int
}

func (w *FixedWindow) Allow(now time.Time) bool {
    w.mu.Lock()
    defer w.mu.Unlock()
    if now.Sub(w.windowStart) >= time.Second {
        w.windowStart = now.Truncate(time.Second)
        w.count = 0
    }
    if w.count >= w.limit {
        return false
    }
    w.count++
    return true
}
```

测试在 `00.999` 和 `01.001` 各放 100 个，预期全部通过，从而证明边界问题。修复可用滑动窗口或令牌桶。

### 3. `rate` 和 `burst`

令牌桶以 `rate` 个/秒补充令牌，桶最多存 `burst` 个。`rate` 控制长期平均吞吐，`burst` 控制空闲后允许的瞬时突发。

例子：`rate=10/s, burst=20`，空闲 5 秒后最多一次通过 20 个，不会因为空闲 100 秒就积攒 1000 个。验证：先空闲到桶满，连续请求 21 次，预期前 20 次成功、第 21 次失败；推进 100ms 后应恢复约 1 个令牌。

### 4. 熔断器三态

```text
CLOSED --失败率/连续失败超阈值--> OPEN
OPEN --冷却时间到--> HALF_OPEN
HALF_OPEN --探测成功--> CLOSED
HALF_OPEN --探测失败--> OPEN
```

HALF_OPEN 只允许少量探测请求，避免下游刚恢复就被全量流量再次打穿。验证：用 fake clock 推进冷却时间，断言 OPEN 时调用返回快速错误、HALF_OPEN 只放行配置数量、成功后恢复 CLOSED，并统计下游调用次数。

### 5. 超时加重试为什么放大故障

一个请求超时后重试 3 次，最坏会让下游收到 4 倍请求；如果上游也重试，会继续相乘。重试还要有总预算，不能每个重试都重新给完整超时。

```text
入口超时 3s
下游单次超时 1s
最多重试 2 次
总预算仍为 3s
```

只对幂等请求、连接失败、429/503 等可恢复错误重试；参数错误、401、403、业务冲突不能重试。验证：注入 2 秒延迟，观察下游 QPS 不超过配置的放大上限，入口 P99 不超过总预算。

### 6. 单机 100 QPS、10 个实例

理论集群上限是 `100 × 10 = 1000 QPS`；20 个实例是 `2000 QPS`。但这只是入口的数学上限，数据库、Redis 和外部依赖未必能承受。验证时把每个实例的限流指标相加，再用网关总 QPS 对照；扩缩容后阈值要重新计算或改为集中式配额。

### 7. 什么数据不能降级

不能猜的数据包括余额、库存、支付状态、优惠券核销状态、订单最终状态。可以降级的通常是推荐、排行榜、非关键展示字段、头像或统计报表。

例如支付超时只能返回：

```json
{"code":"PAYMENT_PENDING","message":"payment result is pending","request_id":"req-1"}
```

不能让前端显示“支付成功”，也不能返回一个假余额。正确恢复方式是提供查单接口、幂等键和对账任务。

### 8. 舱壁模式

为每个依赖分配独立的有界并发池，慢依赖只能占满自己的池：

```go
type Bulkhead struct {
    sem chan struct{}
}

func NewBulkhead(n int) *Bulkhead { return &Bulkhead{sem: make(chan struct{}, n)} }

func (b *Bulkhead) Do(ctx context.Context, fn func() error) error {
    select {
    case b.sem <- struct{}{}:
        defer func() { <-b.sem }()
        return fn()
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

支付、库存、报表分别建池；不要共享一个无限 goroutine 池。验证：让支付依赖永久慢，其他接口仍能在自己的池中完成；如果所有接口一起变慢，说明隔离边界画错了。

### 9. 并发安全令牌桶和测试

```go
type TokenBucket struct {
    mu       sync.Mutex
    rate     float64 // tokens per second
    burst    float64
    tokens   float64
    last     time.Time
}

func NewTokenBucket(rate, burst float64, now time.Time) *TokenBucket {
    return &TokenBucket{rate: rate, burst: burst, tokens: burst, last: now}
}

func (b *TokenBucket) Allow(now time.Time) bool {
    b.mu.Lock()
    defer b.mu.Unlock()
    elapsed := now.Sub(b.last).Seconds()
    if elapsed < 0 { elapsed = 0 }
    b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
    b.last = now
    if b.tokens < 1 { return false }
    b.tokens--
    return true
}
```

测试用可控时间，不要依赖真实 sleep：

```go
func TestTokenBucket(t *testing.T) {
    now := time.Unix(0, 0)
    b := NewTokenBucket(2, 3, now)
    for i := 0; i < 3; i++ {
        if !b.Allow(now) { t.Fatalf("burst %d should pass", i) }
    }
    if b.Allow(now) { t.Fatal("fourth request should be limited") }
    if !b.Allow(now.Add(500 * time.Millisecond)) {
        t.Fatal("500ms should refill one token")
    }
}
```

并发验证：100 个 goroutine 在同一时刻调用，统计成功数不能超过 burst；再用 `go test -race`。预期失败排查：如果把 `last` 更新放在补充令牌之前，时间会丢；如果不加锁，会出现数据竞争和超额放行。

### 10. Redis Lua 每分钟 60 次

```lua
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local n = redis.call('INCR', key)
if n == 1 then
  redis.call('EXPIRE', key, ttl)
end
return n <= limit
```

Go 调用：

```go
script := redis.NewScript(`
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local n = redis.call('INCR', key)
if n == 1 then redis.call('EXPIRE', key, ttl) end
return n <= limit
`)
minute := time.Now().UTC().Format("200601021504")
ok, err := script.Run(ctx, rdb, []string{"rl:u-1001:" + minute}, 60, 65).Bool()
```

key 使用用户和分钟，TTL 65 秒是为了覆盖时钟和清理延迟。验证前 60 次为 1、第 61 次为 0，换 uid 独立，等待过期后恢复。失败排查：检查是否把 `INCR` 和 `EXPIRE` 分成两次调用，导致进程崩溃时 key 永不过期；检查 key 是否包含不受控的高基数 ID。

### 11. 第三方支付接口的保护设计

| 项目 | 建议 |
|---|---|
| 超时 | 连接 500ms，整体 2s，使用 `context.WithTimeout` |
| 幂等 | 客户端/服务端生成支付业务号，重试复用同一个号 |
| 重试 | 只在明确未受理或可查询的失败上重试一次，总预算 3s |
| 熔断 | 30 秒窗口至少 20 次、失败率 > 50% 打开，冷却 30 秒 |
| 降级 | 返回 `PAYMENT_PENDING` + 查单入口，不返回成功/失败猜测 |
| 观测 | 成功率、P99、超时、重试、熔断状态、上游错误码 |

流程是“提交支付 -> 超时后查单 -> 只有查单确认不存在才允许重试 -> 熔断打开时直接进入待确认”。不能因为 HTTP 超时就再次扣款；这是资金系统最常见的重复支付事故。

### 12. 秒杀防护链

```text
客户端
  -> 网关：按 IP/uid 做粗粒度限流
  -> 应用：按活动/uid 做细粒度限流，校验活动状态
  -> Redis Lua：原子预扣库存 + 用户去重
  -> MQ：削峰，消息带 event_id
  -> 订单消费者：唯一键幂等落库
  -> 支付：独立限流、超时、熔断、查单补偿
  -> 对账：库存、订单、支付最终一致
```

阈值不能拍脑袋。先用单实例压测得到安全 QPS，再按实例数、数据库连接数、Redis 分片能力和支付通道配额取最小值；至少做一次 2 倍峰值、Redis 故障、MQ 积压和重复消费演练。库存预扣与订单状态是强一致边界，通知、积分和报表可以异步。

</details>
