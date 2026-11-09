# 第 7 章 消息队列：Kafka / RabbitMQ / NATS

> 目标：知道什么时候该用 MQ、怎么保证消息不丢不重、怎么处理积压和重复消费，并能写出一个可落地的 Outbox 方案。
>
> 一个常见误解：**MQ 不是“异步”的同义词**。它解决的是解耦、削峰、可靠投递和广播，但会引入最终一致、重复消费、积压和运维成本。

---

## 7.1 为什么需要 MQ

MQ 解决的是“生产者不想或不能同步等待消费者”的问题，典型价值是解耦、削峰、异步和广播。它不是免费的性能按钮：引入后要处理重复、乱序、积压、消息丢失、版本兼容和运维。同步强一致、调用链很短、团队没有消费监控能力时，不要为了“架构先进”上 MQ。

| 场景 | 不用 MQ | 用 MQ |
|---|---|---|
| 下单后发积分/短信 | 同步调用，失败影响下单 | 下单成功即返回，积分异步处理 |
| 秒杀削峰 | 请求直接打数据库 | 写入队列，按能力消费 |
| 多系统订阅同一事件 | 每个下游都要被调用 | 发布一次，多消费组订阅 |
| 跨团队解耦 | 接口强耦合、一起发布 | 通过消息契约协作 |
| 失败重试 | 调用方自己重试 | MQ 持久化 + 消费重试 |

不适合：

- 需要强一致、同步返回结果的场景；
- 只是把代码拆开，却不需要缓冲/解耦；
- 团队没有 MQ 运维能力，却上 Kafka 集群。

---

## 7.2 核心概念

Kafka 的 partition 是并行和有序的基本单位：同一 key 进入同一 partition，才谈得上该 key 内有序；增加消费者只能提高 partition 级并行度。RabbitMQ 则通过 exchange 和 binding 把消息路由到 queue，ack 表示消费成功，nack/死信用于失败处理。先把投递模型分清，再讨论“会不会丢”。

以 Kafka 为例：

```
Producer → Topic（partition 0/1/2）→ Consumer Group
                                      ├─ consumer A 读 partition 0
                                      └─ consumer B 读 partition 1/2
```

- **Topic**：消息类别；
- **Partition**：有序日志分片，同一 partition 内消息有序；
- **Offset**：消费者在 partition 中的位置；
- **Consumer Group**：一组消费者共同消费一个 topic，每条消息只被组内一个消费者处理；
- **Rebalance**：消费者增减时重新分配 partition。

RabbitMQ 的模型不同：

```
Producer → Exchange → Binding → Queue → Consumer
```

- Exchange 按 routing key 决定投递到哪些队列；
- Queue 是实际存储；
- 支持 ack、nack、死信队列。

NATS 更轻量，适合低延迟、云原生场景；JetStream 提供持久化。

---

## 7.3 主流 MQ 对比

选 MQ 先看投递模型和运维能力，而不是只看吞吐数字。Kafka 适合高吞吐日志/事件流，RabbitMQ 适合复杂路由和延迟队列，云消息服务适合不想维护集群的团队；业务量很小且只在一个进程内时，本地任务队列可能更简单。

| 维度 | Kafka | RabbitMQ | NATS/JetStream |
|---|---|---|---|
| 模型 | 分布式日志 | 消息代理/队列 | 轻量消息/PubSub |
| 吞吐 | 极高 | 中高 | 高 |
| 顺序 | partition 内有序 | 单队列有序 | 视配置 |
| 延迟 | 毫秒级 | 微秒～毫秒 | 很低 |
| 重放 | 按 offset 重放 | 较难 | JetStream 支持 |
| 路由 | 简单 | Exchange 灵活 | Subject 通配 |
| 运维 | 重 | 中 | 轻 |
| 典型场景 | 日志、事件流、大数据 | 业务任务、RPC 解耦 | 云原生、IoT |

**选型建议**：业务异步任务优先 RabbitMQ；事件流/高吞吐/需要重放选 Kafka；团队已有哪种、运维能力如何，比“哪个 benchmark 高”更重要。

---

## 7.4 投递语义：最多一次、至少一次、正好一次

| 语义 | 含义 | 怎么实现 | 风险 |
|---|---|---|---|
| At most once | 最多一次 | 先 ack 再处理 | 可能丢 |
| At least once | 至少一次 | 处理成功再 ack | 可能重复 |
| Exactly once | 正好一次 | 事务/幂等/去重 | 成本高，跨系统难 |

现实中最常用的是 **at least once + 消费端幂等**。

### 为什么“正好一次”很难

```
消费者处理成功 → 准备 ack → 进程崩溃
```

重启后 MQ 没收到 ack，会重投。要让“处理”和“ack”原子，需要事务消息或幂等表。跨数据库和 MQ 时，通常只能做到“至少一次 + 去重”。

---

## 7.5 消费端幂等：幂等占位和业务写入必须原子

先看一个**错误示例**。它先把事件 ID 写进去重表，再执行业务，业务失败就删除占位：

```go
ok, err := repo.MarkProcessed(ctx, msg.EventID, msg.ConsumerGroup)
if err != nil { return err }
if !ok { return nil }

if err := doBusiness(ctx, msg); err != nil {
    _ = repo.UnmarkProcessed(ctx, msg.EventID, msg.ConsumerGroup)
    return err
}
return nil
```

问题在于 `MarkProcessed` 和 `doBusiness` 不是原子操作：

```text
写入占位成功
执行业务部分成功（例如已经给账户加了积分）
进程在“删除占位”之前崩溃
消息重投时看到占位，直接 ACK
结果：业务可能只做了一半，却永远不再重试
```

反过来，如果先做业务再去重，也会出现“业务成功、去重记录没写、消息重投后重复执行”。结论是：**去重记录必须和本地业务写入在同一个数据库事务里提交或回滚。**

### 正确方案：同一数据库事务

表：

```sql
CREATE TABLE consumed_event (
  event_id       VARCHAR(64) NOT NULL,
  consumer_group VARCHAR(64) NOT NULL,
  created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (event_id, consumer_group)
) ENGINE=InnoDB;
```

消费伪代码：

```go
func (h *Handler) Handle(ctx context.Context, msg Message) error {
    tx, err := h.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx,
        `INSERT INTO consumed_event(event_id, consumer_group) VALUES (?, ?)`,
        msg.EventID, h.consumerGroup,
    )
    if isDuplicateKey(err) {
        // 另一个事务已经完整提交过这个事件；本次只需 ACK。
        return tx.Commit()
    }
    if err != nil {
        return err
    }

    // 所有本地业务写入必须使用同一个 tx。
    if err := h.applyBusiness(ctx, tx, msg); err != nil {
        return err // defer 回滚，占位和业务一起消失，之后可以重试
    }

    // 只有业务和去重占位一起提交成功后，调用方才 ACK。
    return tx.Commit()
}
```

对应的执行顺序是：

```text
BEGIN
  INSERT consumed_event(event_id, consumer_group)  -- 唯一键兜底
  如果唯一键冲突：说明已处理，COMMIT 后 ACK
  执行本地业务 SQL（必须使用同一 tx）
COMMIT
ACK
```

为什么这样没有“占位成功、业务失败”的窗口？

- 业务 SQL 失败：事务回滚，占位也回滚，消息重投后可以重试；
- 业务和占位都成功但 ACK 丢失：消息重投，唯一键冲突，直接 ACK，不会重复执行业务；
- 进程在提交前崩溃：事务回滚，消息重投；
- 进程在提交后崩溃：业务和占位已同时生效，消息重投时命中唯一键。

### 不要用 `UnmarkProcessed` 兜底

删除占位看似“允许重试”，但会把失败窗口扩大：业务已经部分提交、删除占位成功、进程又崩溃时，消息会再次执行业务。正确做法是让失败整体回滚，而不是在失败后手工擦掉幂等记录。

### 如果业务涉及外部系统

上面的方案只适用于“本地数据库事务能覆盖全部副作用”。如果消费后还要调用外部 HTTP、发短信或写另一个数据库，那么本地事务无法和这些副作用原子提交。此时需要：

1. 在本事务里写业务状态和 outbox 事件；
2. 后台可靠投递 outbox；
3. 下游用 `event_id` 再做一次幂等；
4. 用对账和补偿处理最终不一致。

**Redis `SET NX` 只能降低重复概率，不能替代“本地业务与去重记录的原子提交”。**

## 7.6 不丢消息：生产、存储、消费三段都要可靠

### 生产端

生产端要回答三件事：消息是否持久化、发送失败是否重试、重复发送是否可接受。设置 `acks`、重试和超时只能降低丢失概率，不能把发送和本地数据库提交变成原子操作；因此涉及业务状态时要用 Outbox 或事务消息，并让消费者按事件 ID 幂等。

```go
// Kafka 示例：等待所有 ISR 确认
producer, _ := kafka.NewWriter(kafka.WriterConfig{
    Brokers:      []string{"127.0.0.1:9092"},
    Topic:        "recharge.succeeded",
    RequiredAcks: kafka.RequireAll,
    Async:        false,
})
```

- 设置 `acks=all`（Kafka）；
- 失败要重试，但要有上限和退避；
- 消息带唯一 `event_id`，方便消费端去重；
- 不要 `Async` 发完就不管。

### 存储端

存储端的目标是“消息已经确认写入后，不因单节点故障消失”。这需要副本、持久化、足够的保留时间和磁盘容量；只配置副本数但不验证 ISR、磁盘水位和故障切换，仍然可能在关键时刻丢消息。

- Kafka 副本数 ≥ 3，`min.insync.replicas=2`；
- RabbitMQ 队列持久化 + 消息持久化；
- 磁盘和保留时间要足够，避免未消费就被删。

### 消费端

消费端的关键顺序是“业务成功后再 ACK”，失败要有限重试并进入死信或人工处理。ACK 过早会丢消息，ACK 过晚会重复消费；因此消费逻辑必须幂等，不能假设 MQ 只投递一次。

- 处理成功再 ack/提交 offset；
- 处理失败按策略重试，超过次数进死信队列；
- 不要在处理前就 ack。

---

## 7.7 本地消息表（Outbox）：解决“数据库提交了，消息没发出去”

Outbox 的核心是把“业务写库”和“待发消息写库”放进同一个本地事务，再由独立 worker 扫描并投递。它消除了业务提交成功但消息没写进去的窗口，但投递仍然是至少一次：worker 可能发送成功后崩溃，重启又发一次，所以消费者必须去重。

问题：

```
1. 更新订单为已支付（事务提交）
2. 发 Kafka
   └─ 发失败 → 下游永远不知道
```

或者：

```
1. 发 Kafka
2. 更新数据库失败
   └─ 下游收到一个不存在的订单
```

### 方案：把消息写进同一个数据库事务

```sql
CREATE TABLE outbox (
  id          BIGINT PRIMARY KEY AUTO_INCREMENT,
  event_id    VARCHAR(64) NOT NULL,
  topic       VARCHAR(64) NOT NULL,
  payload     JSON NOT NULL,
  status      TINYINT NOT NULL DEFAULT 0, -- 0待发送 1已发送
  retry_count INT NOT NULL DEFAULT 0,
  next_retry_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_event_id (event_id),
  KEY idx_status_next (status, next_retry_at)
) ENGINE=InnoDB;
```

业务事务：

```go
tx, _ := db.BeginTx(ctx, nil)
defer tx.Rollback()

// 更新余额、订单状态
if _, err := tx.ExecContext(ctx, "UPDATE account SET balance = balance + ? WHERE uid = ?", amount, uid); err != nil {
    return err
}
if _, err := tx.ExecContext(ctx, "UPDATE recharge_order SET status = 1 WHERE id = ?", orderID); err != nil {
    return err
}
// 同一个事务写 outbox
if _, err := tx.ExecContext(ctx,
    "INSERT INTO outbox(event_id, topic, payload) VALUES (?, ?, ?)",
    eventID, "recharge.succeeded", payload); err != nil {
    return err
}
return tx.Commit()
```

后台投递：

```go
for {
    rows, _ := db.Query(`SELECT id, event_id, topic, payload FROM outbox
                         WHERE status = 0 AND next_retry_at <= NOW()
                         ORDER BY id LIMIT 100`)
    for _, row := range rows {
        if err := producer.Send(ctx, row.Topic, row.Payload); err != nil {
            // 指数退避
            db.Exec(`UPDATE outbox SET retry_count = retry_count + 1,
                     next_retry_at = DATE_ADD(NOW(), INTERVAL ? SECOND)
                     WHERE id = ?`, backoff(row.RetryCount), row.ID)
            continue
        }
        db.Exec(`UPDATE outbox SET status = 1 WHERE id = ?`, row.ID)
    }
    time.Sleep(time.Second)
}
```

Outbox 保证“业务数据和消息意图”原子写入；投递是至少一次，消费端仍需幂等。

---

## 7.8 顺序与并发

顺序只在同一分区/队列内成立。需要同一订单有序时，用订单 ID 做分区键，让同一订单进入同一个 partition；如果为了吞吐把同一订单拆到多个消费者，消费端就必须用状态机处理乱序。

Kafka 只保证**同一 partition 内**有序。要保证同一订单的消息有序：

- 以 `order_id` 作为 partition key；
- 同一订单的消息进入同一 partition；
- 消费者按 partition 串行处理。

不要为了全局有序只用一个 partition，吞吐会受限。

消费并发提高后，同一 key 可能被多个消费者并发处理。需要：

- 按 key 路由到固定 worker；
- 或消费端对业务 key 加锁；
- 或接受乱序，用版本号/状态机拒绝旧事件。

---

## 7.9 积压与重平衡

### 积压怎么发现

Kafka：

```bash
kafka-consumer-groups.sh --bootstrap-server 127.0.0.1:9092 \
  --describe --group recharge-consumer
```

看 `LAG`：最新 offset - 已提交 offset。持续上涨说明消费速度跟不上。

### 积压怎么处理

积压先看是生产变快、消费变慢，还是分区/消费者数量不匹配。不要第一反应就扩容消费者：如果单条处理被慢数据库拖住，扩容只会放大下游压力。先看消费速率、单条耗时、错误率、下游瓶颈和 rebalance，再决定优化、限流、扩容或降级。

积压先判断是生产突增、消费变慢、消费者挂掉，还是分区数不足。顺序通常是先恢复消费者、再限流或暂停非核心生产、然后扩容到 partition 上限、最后优化消费逻辑。不要只加机器，消费者数超过 partition 数不会继续提高并行度。

1. 先看是流量突增、消费变慢还是消费者挂了；
2. 临时扩容消费者（不超过 partition 数）；
3. 优化消费逻辑（批量、减少远程调用）；
4. 降级非核心消费；
5. 消息过期前评估是否需要补偿。

### Rebalance 的坑

- 消费者增减会触发 partition 重新分配；
- 重平衡期间消费暂停；
- 处理时间过长会导致心跳超时，被踢出组，反复重平衡。

优化：缩短单条处理时间、异步化重活、合理设置 `session.timeout.ms` 和 `max.poll.interval.ms`。

---

## 7.10 延迟队列

延迟队列的关键不是“等 30 分钟”，而是“到期时能可靠触发，并且重复触发不会造成二次扣款或重复关单”。无论用 TTL、ZSet 还是时间轮，都要处理重复、取消、精度和大量积压。

需求：订单 30 分钟未支付自动关闭。

方案：

1. **RabbitMQ 死信队列 + TTL**：消息过期后转到死信交换机；
2. **Kafka + 定时扫描**：不适合精确延迟；
3. **Redis ZSet**：score 存到期时间，定时 `ZRANGEBYSCORE` 取出；
4. **时间轮**：适合大量定时任务，内存成本高；
5. **专业延迟队列**（如有）。

无论哪种，都要处理：

- 重复投递；
- 取消订单后消息仍到达（消费时检查状态）；
- 延迟精度；
- 大量消息的存储和清理。

---

## 7.11 消息设计规范

```json
{
  "event_id": "evt-20261101-0001",
  "event_type": "recharge.succeeded",
  "version": 1,
  "occurred_at": "2026-11-01T10:00:00.123+08:00",
  "trace_id": "trace-abc",
  "payload": {
    "order_id": "123456789",
    "uid": "u-1001",
    "amount_cent": 10000
  }
}
```

规范：

- `event_id` 全局唯一，用于去重；
- `event_type` 表意明确，不要用 `update`；
- `version` 支持演进；
- `occurred_at` 是业务发生时间，不是发送时间；
- `trace_id` 串联链路；
- payload 只放必要字段，避免消费者依赖内部结构；
- 消息体不要过大，大文件放对象存储传 URL。

### 兼容性

消息 schema 是跨服务契约。新增字段要保证旧消费者忽略未知字段，删除字段要等所有消费者升级，改类型要发新版本事件；消费者对未知枚举要有默认分支，否则新增一个状态就可能让线上消费崩溃。

- 新增字段：消费者应忽略未知字段；
- 删除字段：先确认没有消费者使用；
- 改字段类型：发新版本事件，不要原地改；
- 枚举新增值：消费者要有默认分支。

---

## 习题

### 基础题

1. MQ 解决什么问题？什么场景不该用？
2. Kafka 的 partition、offset、consumer group 分别是什么？
3. 最多一次、至少一次、正好一次有什么区别？为什么常用“至少一次 + 幂等”？
4. 生产端、存储端、消费端分别怎么防止丢消息？
5. Outbox 解决什么问题？为什么消息仍可能重复？
6. 如何保证同一订单的消息有序？
7. 消费积压持续上涨，你会按什么顺序排查？
8. Rebalance 为什么会导致消费暂停？

### 动手题

9. 写一个消费端幂等处理的伪代码，要求占位和业务在同一事务。
10. 设计一个“订单超时关闭”的延迟队列方案，说明重复/取消怎么处理。
11. 给“充值成功”事件写一份消息 schema，包含版本、trace_id 和幂等键。

### 综合题

12. 设计一个“下单后发积分”的可靠链路：数据库、MQ、积分服务。要求：下单不能因为积分服务失败而失败；消息不能丢；积分不能重复加。

---

## 参考答案

<details>
<summary>展开</summary>

1. MQ 用于异步解耦、削峰、广播和可靠重试。强一致同步返回、业务简单、团队没有消费幂等和监控能力、只是为了“看起来架构先进”时不该用。

2. partition 是有序分片和并行单位；offset 是消费者在 partition 中的位置；consumer group 是一组共同消费 topic 的消费者，同一 partition 在同一组内只会分给一个消费者。

3. 最多一次可能丢，至少一次可能重复，正好一次成本高且跨系统很难保证。业务常用“至少一次投递 + 消费端基于 event_id 唯一键幂等”。

4. 生产端使用 acks、重试、超时和 event_id；存储端使用多副本、持久化和足够保留时间；消费端处理成功后才提交 offset，失败进入重试/死信，并监控 lag。

5. Outbox 解决数据库事务与消息发送不一致：业务数据和 outbox 记录在同一事务提交，后台再投递。投递和 ack 之间仍可能失败，因此消费者仍可能收到重复消息。

6. 用 order_id 作为 partition key，让同一订单的事件进入同一 partition；消费端按 key 串行，或用版本号/状态机拒绝旧事件。不要为了全局有序只用一个 partition。

7. 先看消费者是否存活、消费速率、单条耗时、错误率和下游延迟，再看生产流量和 partition 数。处理顺序是恢复消费者、限制非核心生产、优化消费逻辑、扩容到 partition 上限、最后评估补偿。

8. Rebalance 会暂停 partition 的旧分配并重新分配，消费者在交接期间不能安全继续处理同一分区。处理时间过长、心跳超时和频繁发布都会导致反复 rebalance。

9. 消费端伪代码：

```text
BEGIN
  INSERT INTO consumed_event(event_id, consumer, consumed_at)
  VALUES (?, ?, NOW()) ON DUPLICATE KEY UPDATE event_id = event_id;
  affected_rows = 0 表示已经处理过 -> COMMIT，然后 ACK

  UPDATE account SET balance = balance + ? WHERE uid = ?;
  INSERT INTO recharge_flow(event_id, uid, amount_cent) VALUES (...);
COMMIT;

只有 COMMIT 成功后才 ACK；
数据库失败 -> 不 ACK，按退避重试；
业务永久错误 -> 写死信并告警。
```

关键是“幂等占位 + 业务副作用”必须在同一个数据库事务里。只先写 Redis 去重再写数据库，仍然可能在两者之间崩溃造成漏处理。

10. 订单超时关闭方案：

```text
创建订单时写 MySQL 订单(status=pending)
-> 同一事务写 outbox(event_type=order.created)
-> 投递延迟消息 {order_id, due_at, version}
-> 消费者收到后锁定订单
-> 若 status 仍为 pending 且 now >= due_at，更新为 closed 并写关闭流水
-> 若已支付/已取消，直接 ACK
```

重复消息由订单状态机幂等；取消后消费时检查状态；长时间积压要监控 lag；消息丢失由定时扫描 `pending + expired` 的补偿任务兜底。

11. 完整的充值成功事件：

```json
{
  "event_id": "evt-20261101-0001",
  "event_type": "recharge.succeeded",
  "version": 1,
  "occurred_at": "2026-11-01T10:00:00.123+08:00",
  "trace_id": "trace-demo-001",
  "aggregate_id": "order-90001",
  "payload": {
    "order_id": "order-90001",
    "uid": "u-1001",
    "amount_cent": 10000,
    "channel": "manual",
    "balance_after_cent": 15000
  }
}
```

`event_id` 是消费幂等键；`event_type` 表达业务事实；`version` 是 schema 版本；`occurred_at` 是业务发生时间；payload 只放消费者需要的字段。发布前用 JSON Schema 校验，消费者必须忽略未知字段并对未知枚举走默认分支。

12. 可靠链路：

```text
下单事务：
  INSERT order
  INSERT outbox(send_points, event_id)
COMMIT
-> outbox worker 投递 Kafka/RabbitMQ，至少一次
-> 积分服务消费 event_id
-> 同一事务：INSERT points_event(event_id unique) + UPDATE points
COMMIT
-> ACK
```

下单接口不等待积分服务；积分失败不回滚订单。积分服务永久失败进入死信并告警，定时对账用订单/流水补发。验证证据：杀掉积分服务后下单仍成功、恢复后积分补上、重复投递 10 次只加一次、对账差异为 0。

</details>
