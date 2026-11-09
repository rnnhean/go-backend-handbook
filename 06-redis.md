# 第 6 章 Redis 实战：缓存、锁、限流与高可用

> 目标：能判断什么数据该进 Redis、怎么写缓存一致性、怎么实现可用的分布式锁和限流、怎么发现大 key/热 key，并知道 Redis 挂了业务会怎样。
>
> Redis 是单线程执行命令（网络 IO 在新版本多线程，命令执行仍主要单线程），所以**一条慢命令会阻塞所有请求**。这是理解所有 Redis 问题的前提。

---

## 6.1 数据结构与典型用途

| 结构 | 命令示例 | 典型用途 | 注意 |
|---|---|---|---|
| String | `SET/GET/INCR` | 缓存、计数、锁 | 大 value 会阻塞网络 |
| Hash | `HSET/HGETALL` | 对象字段 | 大 Hash 删除/读取慢 |
| List | `LPUSH/BRPOP` | 简单队列 | 阻塞消费、无 ACK |
| Set | `SADD/SISMEMBER` | 去重、共同好友 | 大集合运算耗 CPU |
| ZSet | `ZADD/ZINCRBY/ZREVRANGE` | 排行榜、延迟队列 | 按 score 排序 |
| Stream | `XADD/XREADGROUP` | 带消费组的消息队列 | 比专业 MQ 弱 |
| Bitmap | `SETBIT/BITCOUNT` | 签到、活跃统计 | 稀疏数据浪费内存 |
| HyperLogLog | `PFADD/PFCOUNT` | 近似 UV | 有误差，不能取具体元素 |

```redis
SET account:u-1001:balance 12000 EX 300
GET account:u-1001:balance

HSET user:u-1001 name demo level 3
HGETALL user:u-1001

ZINCRBY rank:20261101 1 p-1001
ZREVRANGE rank:20261101 0 99 WITHSCORES
```

---

## 6.2 缓存模式：Cache Aside 是默认答案

```
读：
  GET cache
  ├─ hit  → 返回
  └─ miss → SELECT db → SET cache（带 TTL）→ 返回

写：
  UPDATE db
  DEL cache
```

### 为什么写是“删缓存”而不是“更新缓存”

假设两个并发写：

```
请求 A：DB 更新为 1
请求 B：DB 更新为 2
请求 B：缓存更新为 2
请求 A：缓存更新为 1   ← 缓存留下旧值
```

数据库最终是 2，缓存是 1。删除缓存后，下一次读会回源，能自愈。

### 删除缓存仍然有窗口

```
读请求 C：缓存 miss，读到 DB=1
写请求 A：DB 更新为 2，删除缓存
读请求 C：把旧值 1 写回缓存
```

常见缓解：

1. **延迟双删**：写完 DB 删一次，延迟 300～500ms 再删一次；
2. **binlog 订阅**：数据库变更后异步删缓存，顺序更可靠；
3. **版本号**：缓存值带版本，写入时比较版本；
4. **短 TTL**：接受短暂不一致，靠过期自愈。

没有一种方案能同时做到强一致和高性能。先问业务能接受多长的不一致。

### 缓存粒度

缓存粒度决定一次读写搬多少数据。缓存整个用户对象简单，但一个字段变化就要整体失效；缓存单字段灵活，但会产生很多 key 和多次往返。通常按“页面/接口一次读取需要的整体”缓存，同时限制 value 大小，避免大 key。

- 缓存整个对象：读快，但更新要整体失效；
- 缓存字段：灵活，但 key 多、一致性复杂；
- 缓存列表：注意分页和排序变化，容易缓存错页。

---

## 6.3 三大缓存问题：穿透、击穿、雪崩

### 缓存穿透：查不存在的数据

攻击者用随机 ID 查询，缓存永远 miss，全部打到数据库。

解决：

```go
// 1. 缓存空值，TTL 短（如 60s）
if err == sql.ErrNoRows {
    cache.Set(ctx, key, "null", time.Minute)
    return nil, ErrNotFound
}

// 2. 布隆过滤器提前判断
if !bloom.MayContain(id) {
    return nil, ErrNotFound
}
```

布隆过滤器有误判（可能把不存在判为存在），但不会把存在判为不存在。

### 缓存击穿：热点 key 过期瞬间

一个 10 万 QPS 的热点 key 过期，所有请求同时回源。

解决：

1. **互斥重建**：只让一个请求回源，其他等待；
2. **逻辑过期**：value 里存过期时间，过期后异步刷新，旧值继续服务；
3. **热点 key 永不过期** + 后台更新。

```go
// 简化版互斥重建
val, err := cache.Get(ctx, key)
if err == nil { return val }

lockKey := "lock:" + key
ok, _ := cache.SetNX(ctx, lockKey, token, 3*time.Second)
if !ok {
    time.Sleep(20 * time.Millisecond)
    return cache.Get(ctx, key) // 重试读缓存
}
defer cache.Del(ctx, lockKey)

val, err = loadFromDB(ctx, key)
if err == nil { cache.Set(ctx, key, val, 5*time.Minute) }
return val
```

### 缓存雪崩：大量 key 同时过期或 Redis 挂了

雪崩的本质是大量请求在同一时刻回源，把数据库从“缓存 miss 的少量请求”推成“全部请求直打数据库”。修复要同时做两件事：让过期时间带随机抖动，避免同一秒失效；在回源路径加互斥、限流和降级，保证数据库不会被瞬时打穿。

解决：

- TTL 加随机抖动（如 5 分钟 ± 60 秒）；
- 多级缓存（本地 + Redis）；
- Redis 高可用（主从 + Sentinel/Cluster）；
- 降级：Redis 挂了直接回源，但要限流保护数据库；
- 预热：发布后先加载热点数据。

---

## 6.4 分布式锁：`SET NX PX` + 唯一 token + Lua 释放

### 错误写法

最危险的锁写法是“`SET NX` 成功、执行业务、再 `DEL`”，既不加过期时间，也不确认锁是不是自己的：

```redis
SET lock:order:123 1 NX
-- 业务执行
DEL lock:order:123
```

问题：

1. 没有过期时间，进程崩溃后锁永远不释放；
2. 释放时不校验持有者，可能删掉别人的锁；
3. 业务执行超过锁过期时间，锁已失效，另一个请求进入。

### 正确写法

正确流程有四步：生成每次抢锁唯一的 token；用 `SET NX PX` 原子地“只有不存在才写，并带租期”；业务完成前确认自己仍持有锁；释放时用 Lua 原子比较 token 再删除。

```go
token := uuid.NewString()

ok, err := rdb.SetNX(ctx, "lock:order:123", token, 10*time.Second).Result()
if err != nil || !ok {
    return ErrLockBusy
}
defer func() {
    // 只删自己的锁
    script := redis.NewScript(`
        if redis.call("GET", KEYS[1]) == ARGV[1] then
            return redis.call("DEL", KEYS[1])
        end
        return 0
    `)
    _ = script.Run(ctx, rdb, []string{"lock:order:123"}, token).Err()
}()
```

**执行过程与验证。** 启动两个客户端并发抢同一个 key，预期只有一个拿到锁；让持锁进程在 `DEL` 前退出，等 TTL 到期后另一个客户端应能拿到，证明不会永久死锁；持锁 token 是 A 时，让 B 尝试执行释放脚本，预期返回 0 且 A 的锁仍在。测试还要覆盖 Redis 返回连接错误：此时不能把“没拿到锁”误判成“可以继续执行”。

### 锁的三个坑

分布式锁不是拿到 key 就万事大吉。必须设置过期时间防止死锁，释放时比较唯一 token 防止删掉别人的锁，还要接受主从切换、网络分区和业务执行超过锁租期。对资金类写操作，数据库唯一约束和 fencing token 通常比单纯 Redis 锁更可靠。

1. **锁过期但业务没结束**：需要看门狗续期（如 Redisson），或者把业务控制在锁时间内；
2. **主从切换丢锁**：主节点写入锁后未同步就宕机，从节点升主，另一个客户端也能拿到锁。强一致场景需要 RedLock 或数据库唯一约束/ fencing token；
3. **锁不是事务**：锁只保证互斥，不保证操作原子，关键更新仍要数据库约束。

### 更可靠的方案：fencing token

每次拿锁返回一个单调递增的 token，写数据库时带上 token，数据库拒绝比已见 token 小的写入。这样即使旧锁持有者恢复后继续写，也会被拒绝。

---

## 6.5 限流：固定窗口、滑动窗口、令牌桶

### 固定窗口

固定窗口把时间切成离散桶，例如 `rl:{uid}:{minute}` 在 12:01:00 到 12:01:59 共用一个计数。实现最短，但“窗口最后 10ms 打满 + 下一窗口立刻再打满”会让 20ms 内通过两倍配额。

```lua
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local n = redis.call('INCR', key)
if n == 1 then redis.call('EXPIRE', key, ttl) end
if n > limit then return 0 end
return 1
```

问题：窗口边界双倍流量。

**怎么验证。** 把 limit 设为 3，在 `59.900s` 连续请求 3 次，全部成功；在 `60.100s` 再连续请求 3 次，也全部成功。虽然任何单个一分钟桶都没超限，但跨越边界的 200ms 里实际通过了 6 次。测试必须使用可控时间戳，不能只 `sleep 60s`。

### 滑动窗口（ZSet）

滑动窗口保存最近一个真实时间窗内的请求时间，每次先删过期成员，再统计数量。它消除了固定窗口边界，但每个请求都保存一个 ZSet 成员；高 QPS 下内存和命令开销会明显上升。

```lua
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, now - window)
local count = redis.call('ZCARD', KEYS[1])
if count >= limit then return 0 end
redis.call('ZADD', KEYS[1], now, now .. ':' .. math.random())
redis.call('PEXPIRE', KEYS[1], window)
return 1
```

精确但每个请求存一个成员，内存随 QPS 增长。

示例里用 `now` 毫秒时间戳和 `now .. ':' .. math.random()` 做成员，生产还要保证同一毫秒内的唯一性，否则 ZADD 可能覆盖。验证时传固定 `now`：窗口 1000ms、limit 3，`t=0/100/900` 成功，`t=1100` 时前三个过期，应该再次允许。

### 令牌桶

令牌桶按时间补充令牌，允许短突发但限制长期平均速率。`rate` 决定每秒补多少，`burst` 决定最多存多少。它适合“平时很少、偶发一批请求”的接口；如果下游完全不能承受突发，应该用漏桶或并发限制。

```lua
-- 简化：用 Hash 存 tokens 和 last_refill
local now = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local data = redis.call('HMGET', KEYS[1], 'tokens', 'last')
local tokens = tonumber(data[1]) or burst
local last = tonumber(data[2]) or now
tokens = math.min(burst, tokens + (now - last) / 1000 * rate)
if tokens < 1 then
  redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last', now)
  redis.call('EXPIRE', KEYS[1], 60)
  return 0
end
redis.call('HMSET', KEYS[1], 'tokens', tokens - 1, 'last', now)
redis.call('EXPIRE', KEYS[1], 60)
return 1
```

关注：Lua 脚本要短、key 要带过期、集群模式下所有 key 在同一 slot。

**执行过程与验证。** 设 `rate=2`、`burst=3`，初始连续 3 次应成功，第 4 次立即失败；等待 500ms 后应恢复 1 个令牌。测试用注入的 `now` 推进时间，避免真实 sleep 造成 CI 波动。还要测试 Redis 不可用时是拒绝还是放行；限流器故障不能悄悄变成无限流量。

---

## 6.6 大 key 与热 key

### 大 key

判定：String 超过 10KB、Hash/List/Set/ZSet 元素超过几千、总大小超过几 MB。

危害：

- 网络传输慢，阻塞其他请求；
- 删除大 key 会阻塞（用 `UNLINK` 异步删除）；
- 主从同步延迟；
- 内存分布不均。

排查：

```bash
redis-cli --bigkeys
redis-cli --memkeys
redis-cli MEMORY USAGE key
```

治理：拆分 key、只取需要的字段（`HMGET` 而不是 `HGETALL`）、设置上限、异步删除。

### 热 key

热 key 是单个 key 的访问量远高于其他 key，可能把某个 Redis 分片或网络连接打满。先通过监控和慢日志确认是哪个 key，再做本地小缓存、拆分 key、读副本或限流；不要盲目扩容整个集群，因为热点仍然会集中到一个分片。

某个 key 的 QPS 远高于其他 key，打满单个 Redis 节点。

排查：`redis-cli --hotkeys`（需 LFU 策略）、监控单 key QPS、业务日志。

治理：

- 本地缓存；
- key 加随机后缀分片（读时随机选一个）；
- 读写分离；
- 热点数据预热。

---

## 6.7 持久化与高可用

| 机制 | 作用 | 代价 |
|---|---|---|
| RDB | 定时快照 | 两次快照间数据会丢 |
| AOF everysec | 每秒刷盘 | 最多丢约 1 秒 |
| AOF always | 每条刷盘 | 性能差 |
| 主从 | 读扩展、故障切换 | 异步复制可能丢写 |
| Sentinel | 自动选主 | 需额外节点和演练 |
| Cluster | 分片 | 多 key 操作受 slot 限制 |

### Redis 挂了业务会怎样

Redis 故障时，系统必须明确哪些请求可以降级、哪些必须失败。余额和订单不能把 Redis 当唯一事实来源；读缓存可以回源数据库，写缓存可以删除后稍后重建；限流和锁如果丢失，需要数据库约束兜底。没有降级方案的服务，Redis 一抖就会变成全站故障。

必须提前定义：

- 缓存挂：回源数据库，但要限流，否则数据库被打垮；
- 锁服务挂：关键操作是拒绝还是降级为数据库锁？
- 限流服务挂：放行还是拒绝？
- 会话挂：用户是否被登出？

**Redis 不是唯一真相。** 核心数据必须在 MySQL/PostgreSQL，Redis 只做加速和协调。

---

## 6.8 使用规范

这些规范的目的不是“让 Redis 看起来整齐”，而是控制故障半径。key 前缀方便隔离和清理，value 有上限避免网络阻塞，命令有超时避免 goroutine 堆积，TTL 防止无限增长，监控命中率和内存防止悄悄退化。

1. 所有 key 带业务前缀和 TTL（除明确要求永久）；
2. 禁止 `KEYS *`，用 `SCAN`；
3. 禁止 `FLUSHALL` 作为“清理缓存”手段；
4. 大 value 拆分，单 value 控制在 10KB 内；
5. 批量操作用 pipeline，减少 RTT；
6. 不在 Redis 里做复杂计算；
7. 连接池设置上限和超时；
8. 监控内存、命中率、evicted、慢日志、主从延迟。

```bash
redis-cli INFO memory
redis-cli INFO stats
redis-cli SLOWLOG GET 10
redis-cli --latency
```

---

## 6.9 Go 客户端示例

```go
rdb := redis.NewClient(&redis.Options{
    Addr:         "127.0.0.1:6379",
    Password:     "",
    DB:           0,
    PoolSize:     50,
    DialTimeout:  500 * time.Millisecond,
    ReadTimeout:  300 * time.Millisecond,
    WriteTimeout: 300 * time.Millisecond,
})

ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
defer cancel()

val, err := rdb.Get(ctx, "account:u-1001:balance").Result()
if err == redis.Nil {
    // cache miss，回源数据库
} else if err != nil {
    // Redis 故障，决定降级策略
}
```

超时是必须的。没有超时的 Redis 调用会在网络抖动时拖住整个请求。

---

## 习题

### 基础题

1. Redis 为什么会被一条慢命令阻塞？
2. Cache Aside 为什么更新数据库后删缓存，而不是更新缓存？
3. 缓存穿透、击穿、雪崩分别是什么？各给两种解决方案。
4. `SET NX` 做锁为什么必须加过期时间？为什么释放要用 Lua？
5. 大 key 和热 key 的区别是什么？分别怎么排查？
6. RDB 和 AOF 的丢数据窗口分别是多少？
7. Redis 主从切换为什么可能丢锁？
8. 为什么禁止 `KEYS *`？

### 动手题

9. 用 Go 写一个 Cache Aside 的读函数，要求处理 cache miss、Redis 错误、数据库错误。
10. 用 Redis Lua 实现“每个用户每分钟 60 次”的限流。
11. 设计一个排行榜：按日、周、总榜，写出 key 和命令，说明过期策略。
12. 模拟热点 key：同一个商品 10 万 QPS，设计本地缓存 + Redis 分片的方案。

### 综合题

13. 一个秒杀系统用 Redis 扣库存，如何防止超卖、重复下单和 Redis 挂掉后的数据不一致？写出关键流程和补偿方案。

---

## 参考答案

<details>
<summary>展开：每题都给出可执行步骤、预期结果和失败排查</summary>

### 1. Redis 为什么会被一条慢命令阻塞

Redis 执行命令的主线程是单线程事件循环。一条 `KEYS *`、大 `ZRANGE`、大 `HGETALL` 或复杂 Lua 脚本执行时，其他客户端的命令只能排队。慢的不是网络，而是 Redis 主线程被占住了。

复现和观察：

```bash
# 终端 A：制造连续延迟，观察 p99
redis-cli --latency-history -i 1

# 终端 B：准备 10 万个 key 后执行 O(N) 命令
for i in $(seq 1 100000); do redis-cli SET "demo:$i" "$i" >/dev/null; done
redis-cli KEYS 'demo:*' >/dev/null
```

预期：第二条命令执行期间，终端 A 出现明显尖峰；`redis-cli --slowlog get 10` 能看到耗时命令。失败排查顺序：先看 `SLOWLOG` 和 `LATENCY DOCTOR`，再用 `SCAN` 替代全量遍历；不要先怀疑“Redis 网络慢”。

### 2. Cache Aside 为什么更新数据库后删缓存，而不是更新缓存

因为缓存可能不是单一来源，两个并发写会出现乱序覆盖：

```text
A 写 DB=10 -> A 更新 cache=10
B 写 DB=20 -> B 更新 cache=20
A 的旧 cache 更新晚到 -> cache=10
DB=20，cache=10，长期不一致
```

删除缓存则让下一次读回源，至少能自愈。实际步骤：

```text
读：GET cache -> miss -> SELECT DB -> SET cache TTL
写：事务提交 DB -> DEL cache -> 失败则重试/发失效消息
```

验证：写完后 `GET` 缓存应 miss；下一次读后缓存等于数据库。删除失败时检查重试指标和 TTL，不能把缓存删除失败当成“充值失败”。

### 3. 缓存穿透、击穿、雪崩

| 问题 | 触发条件 | 两种处理 | 怎么验证 |
|---|---|---|---|
| 穿透 | 查不存在的 key，每次都回源 | 缓存空值短 TTL；参数校验/布隆过滤器 | 对不存在 uid 连续请求，观察 DB QPS 不随请求线性增长 |
| 击穿 | 热点 key 过期瞬间大量回源 | 单飞/互斥重建；逻辑过期 | 并发 1000 次同 key，数据库只应被回源一次 |
| 雪崩 | 大量 key 同时过期或 Redis 故障 | TTL 随机抖动；多级缓存、限流、高可用 | 批量写入同 TTL，确认过期时间分散；注入 Redis 故障观察降级 |

空值也要设置较短 TTL，例如 30 秒；布隆过滤器只能判断“可能存在”，不能替代数据库最终校验。缓存异常时必须记录指标，不能静默返回伪造的成功数据。

### 4. `SET NX` 锁为什么加过期时间，为什么用 Lua 释放

没有过期时间时，持有锁的进程崩溃，锁永远不释放。释放必须比较 token 再删除，否则可能删掉别人重新获得的锁：

```bash
# 申请锁：value 是本次持有的随机 token
SET lock:recharge:u-1001 <token> NX PX 30000
```

```lua
-- 释放锁：比较和删除必须在同一个原子脚本里
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
```

执行与预期：

```bash
redis-cli EVAL 'if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0' 1 lock:recharge:u-1001 wrong-token
# 返回 0，锁仍然存在

redis-cli EVAL 'if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0' 1 lock:recharge:u-1001 <token>
# 返回 1，锁被删除
```

如果业务执行时间可能超过锁 TTL，必须使用 fencing token 或延长锁的方案；不能只把 TTL 调大，因为进程停顿和网络抖动不可预测。

### 5. 大 key 和热 key 的区别

- 大 key：单个 key 的 value 很大，例如 100 MB 的 hash/list；主要增加内存、网络传输和单命令耗时。
- 热 key：某个 key 的访问频率远高于其他 key；主要制造单分片 CPU 和网络热点。

排查命令：

```bash
redis-cli --bigkeys
redis-cli MEMORY USAGE product:1001
redis-cli --hotkeys
redis-cli INFO commandstats
```

预期：大 key 看 `MEMORY USAGE` 的数量级；热 key 看 `--hotkeys` 或分片监控中的 QPS 偏斜。失败排查：没有命中时先确认 Redis 版本是否支持 `--hotkeys`、是否启用了 LFU；不能把 `KEYS *` 当成排查手段。

### 6. RDB 和 AOF 的丢数据窗口

RDB 丢“最后一次快照之后”的写入；AOF `appendfsync everysec` 在正常情况下最多约丢 1 秒。AOF `always` 的持久性更高但写放大和延迟更大；`no` 的窗口取决于操作系统刷盘。

验证配置：

```bash
redis-cli CONFIG GET save
redis-cli CONFIG GET appendonly
redis-cli CONFIG GET appendfsync
```

不要只看到 `appendonly yes` 就宣布“不会丢数据”。还要验证持久化文件能否恢复，以及主从/故障切换后的数据窗口。

### 7. 主从切换为什么可能丢锁

Redis 主从复制默认是异步的：客户端在主节点拿到锁，主节点还没把 `SET` 同步给从节点就宕机，Sentinel 提升从节点，新主节点不知道这把锁，另一个客户端可以再次获得。结果互斥失效。

这是“锁丢失”，不是简单的缓存不一致。资金或库存场景要用数据库唯一约束、fencing token 或具备更强一致性的协调服务兜底。验证方式是在测试环境暂停复制、写入锁、触发切换，再用第二个客户端尝试获取同一锁。

### 8. 为什么禁止 `KEYS *`

`KEYS` 会扫描全部 key 并在主线程返回结果，key 数量大时阻塞所有请求。生产应使用游标扫描：

```bash
redis-cli SCAN 0 MATCH 'demo:*' COUNT 100
```

预期返回一个新的游标，下一次继续 `SCAN <cursor>` 直到游标为 `0`。即使使用 `SCAN`，也不要在业务请求路径里遍历海量 key；它适合离线清理和巡检，并要限速。

### 9. Go Cache Aside 读函数

下面是最小实现，重点是指定超时、区分 `redis.Nil` 和 Redis 故障，并且数据库错误不写入缓存：

```go
func (s *Service) Balance(ctx context.Context, uid string) (int64, error) {
    key := "account:balance:" + uid

    ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
    defer cancel()

    value, err := s.redis.Get(ctx, key).Int64()
    switch {
    case err == nil:
        return value, nil
    case errors.Is(err, redis.Nil):
        // cache miss，继续回源
    default:
        s.metrics.CacheError.Inc()
        // Redis 故障时仍可回源，但要受数据库保护和限流
    }

    balance, err := s.repo.GetBalance(ctx, uid)
    if err != nil {
        return 0, err
    }

    if err := s.redis.Set(ctx, key, balance, 30*time.Second).Err(); err != nil {
        s.metrics.CacheSetError.Inc()
    }
    return balance, nil
}
```

验证：

```bash
go test ./internal/service -run TestBalanceCache -count=1
```

测试要覆盖：缓存命中不查数据库、miss 查一次并回填、Redis 错误仍回源、数据库错误不缓存、超时及时返回。失败排查：如果数据库被反复回源，先看 `redis.Nil` 是否被误判为普通错误；如果旧值长期存在，检查写路径是否删除缓存以及 TTL 是否生效。

### 10. Redis Lua 每分钟 60 次限流

固定窗口实现：

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
key := "rl:u-1001:" + minute
allowed, err := script.Run(ctx, rdb, []string{key}, 60, 65).Bool()
```

key 按用户和分钟分桶，TTL 略大于 60 秒。验证脚本：前 60 次返回 1，第 61 次返回 0；另一个 uid 仍从 1 开始；过期后重新放行。固定窗口有边界突刺，如果要求严格平滑，改用令牌桶或滑动窗口。

### 11. 日榜、周榜、总榜

```bash
ZINCRBY rank:day:20261101 10 u-1001
ZINCRBY rank:week:2026W44 10 u-1001
ZINCRBY rank:total 10 u-1001

ZREVRANGE rank:day:20261101 0 99 WITHSCORES
ZREVRANK rank:day:20261101 u-1001
```

日榜设置 2 天过期，周榜设置 14 天过期，总榜保留或定期快照；跨日时由定时任务创建新 key，不把不同周期写进同一个 key。验证：加分后名次变化；过期后 `EXISTS` 返回 0；重复事件不能重复加分，除非业务规定累加。

### 12. 热点商品：本地缓存 + Redis 分片

方案：

```text
读请求
  -> 进程内 LRU（1~5 秒，容量有上限）
  -> Redis 分片 key：product:{id}:shard:{0..N}
  -> MySQL 只读兜底
```

本地缓存要设置容量和 TTL，避免把进程内存撑爆；Redis 分片 key 要能稳定映射，读可随机/按实例分片，写库存必须走同一原子脚本或数据库条件更新。验证时观察单分片 CPU、Redis QPS、本地命中率和 DB QPS，不能只看总命中率。

### 13. 秒杀：库存、重复下单和故障补偿

关键流程：

```text
请求 -> 鉴权/限流 -> Redis Lua 原子检查并预扣库存 + 写入 user 去重 key
     -> 创建订单（req_id/uid+sku 唯一约束）
     -> 投递 MQ/Outbox
     -> 消费端落库，重复 event_id 直接 ACK
     -> 超时未支付则补偿恢复库存，但补偿必须幂等
```

Lua 示例：

```lua
-- KEYS[1]=stock:sku:42, KEYS[2]=dedup:sku:42:u-1001
if redis.call('EXISTS', KEYS[2]) == 1 then
  return -2 -- 重复下单
end
local stock = tonumber(redis.call('GET', KEYS[1]) or '-1')
if stock <= 0 then
  return -1 -- 无库存
end
redis.call('DECR', KEYS[1])
redis.call('SET', KEYS[2], '1', 'EX', 86400)
return stock - 1
```

验证清单：

1. 100 个用户并发抢 10 个库存，成功数必须不超过 10；
2. 同一用户并发 10 次，只能产生一个订单；
3. Redis 重启/主从切换后，用数据库库存和订单表对账，发现差异进入人工/自动补偿；
4. MQ 重复投递不能重复扣库存或重复创建订单。

Redis 只能是快速预扣层，最终库存和订单仍要有数据库唯一约束、状态机和对账，否则不能声称资金/库存安全。

</details>
