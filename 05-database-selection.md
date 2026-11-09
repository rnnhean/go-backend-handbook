# 第 5 章 数据库选型：MySQL 之外还有什么

> 目标：拿到一个需求，你能说清楚数据长什么样、怎么读写、要什么一致性、量有多大，然后**用证据**选出存储，而不是背一张“MySQL 管事务、Redis 管缓存”的口诀。
>
> 学完本章你应该能完成三件事：
> 1. 给一个业务画出实体关系和访问路径；
> 2. 用同一份需求对比 2～3 个候选存储，写出放弃理由；
> 3. 知道每种数据库在生产里会怎么坏、坏了怎么查。

> **JS 类比**
>
> MongoDB 的文档看起来像 JS 对象，这是它最容易让人误解的地方。**“长得像对象”不等于“可以像操作内存对象一样操作它”**：写进去要建索引，查询要遵守执行计划，更新会锁文档，数组无限增长会撑爆单文档上限。
>
> Redis 像跨进程共享的 `Map`，但每次操作都是网络往返，还有内存上限、淘汰策略和持久化窗口，不能按操作本地 `Map` 的习惯使用。

---

## 5.1 选型不是选产品，是选“访问模式”

先不要问“MySQL 和 MongoDB 哪个好”。这个问题没有答案。要先把需求翻译成下面这张表：

| 问题 | 为什么决定选型 | 例子 |
|---|---|---|
| 实体和关系 | 有关系就要 JOIN、外键、事务 | 订单 → 明细 → 支付 → 退款 |
| 单条数据多大 | 文档库适合整文档读写，关系库适合拆行 | 商品详情 5KB vs 日志 2KB/条 |
| 读写比例 | 读多写少可以加缓存/副本；写多要看写入模型 | 文章读 10000:1；埋点几乎全写 |
| 查询方式 | 主键、范围、全文、聚合、图遍历是完全不同的索引 | “包含关键词”不是 B+Tree 擅长的 |
| 一致性 | 钱不能丢；点赞数可以晚几秒 | 余额 vs 阅读数 |
| 数据量 | 1 亿行和 1000 亿行是两种运维 | 订单 vs 日志 |
| 峰值 QPS | 决定单机、分片、缓存和预算 | 秒杀 5 万 QPS |
| 保留时间 | 决定分区、TTL、冷热分离 | 监控数据保留 30 天 |
| 团队能力 | 没人会修的数据库就是负债 | 小团队上 Cassandra |

### 一个可执行的判断顺序

选数据库先看数据模型、事务边界、查询模式和一致性，再看团队是否会运维。不要因为某个产品“很火”就把它放进架构；一个只需要唯一约束和事务的账务系统，MySQL/PostgreSQL 往往比文档库更合适。

```
1. 能不能用 MySQL/PostgreSQL 解决？
   → 能，就用它。它是默认答案。
2. 不能的点是什么？
   → 全文检索？超大规模写入？时序聚合？图遍历？缓存？
3. 引入新存储后，主数据放哪？同步链路怎么保证？
4. 新存储挂了，业务降级成什么样？
5. 谁负责备份、扩容、升级、告警？
```

第 1 步能过滤掉 80% 的“技术选型”。第 5 步能过滤掉剩下 80% 的冲动。

---

## 5.2 关系型数据库：MySQL 与 PostgreSQL

### 数据模型

关系型的核心不是“表”，而是**实体 + 约束 + 事务**：

```sql
CREATE TABLE account (
  uid          VARCHAR(32) PRIMARY KEY,
  balance      BIGINT NOT NULL DEFAULT 0,   -- 分，不用浮点
  status       TINYINT NOT NULL DEFAULT 1,
  version      BIGINT NOT NULL DEFAULT 0,
  updated_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB;

CREATE TABLE recharge_order (
  id           BIGINT PRIMARY KEY AUTO_INCREMENT,
  req_id       VARCHAR(64) NOT NULL,
  uid          VARCHAR(32) NOT NULL,
  amount       BIGINT NOT NULL,
  status       TINYINT NOT NULL,
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_req_id (req_id),
  KEY idx_uid_created (uid, created_at)
) ENGINE=InnoDB;
```

这段 DDL 里每一行都在表达业务规则：

- `balance BIGINT`：金额用“分”，避免 `0.1 + 0.2` 的浮点误差；
- `uk_req_id`：数据库层保证同一个请求只充值一次，应用层“先查再插”挡不住并发；
- `idx_uid_created`：支持“查某个用户最近的充值记录”，顺序不能反；
- `status`：订单状态机，不允许物理删除。

### 一次转账为什么必须用事务

```sql
START TRANSACTION;
UPDATE account SET balance = balance - 100 WHERE uid = 'u-1001' AND balance >= 100;
-- 检查受影响行数是否为 1，不是就 ROLLBACK
UPDATE account SET balance = balance + 100 WHERE uid = 'u-1002';
INSERT INTO transfer_flow(from_uid, to_uid, amount, req_id)
VALUES ('u-1001', 'u-1002', 100, 'req-20261101-001');
COMMIT;
```

如果第二条失败而第一条已提交，钱就凭空消失了。事务保证这四条语句**要么全成功，要么全失败**。

Go 里的事务必须检查每一处错误，任何一步失败都 `Rollback`：

```go
tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
if err != nil { return err }
defer tx.Rollback() // 已 Commit 后再 Rollback 返回 ErrTxDone，可忽略

res, err := tx.ExecContext(ctx,
    "UPDATE account SET balance = balance - ? WHERE uid = ? AND balance >= ?",
    amount, fromUID, amount)
if err != nil { return err }
n, _ := res.RowsAffected()
if n != 1 { return ErrInsufficientBalance }

if _, err := tx.ExecContext(ctx,
    "UPDATE account SET balance = balance + ? WHERE uid = ?", amount, toUID); err != nil {
    return err
}
if _, err := tx.ExecContext(ctx,
    "INSERT INTO transfer_flow(from_uid,to_uid,amount,req_id) VALUES (?,?,?,?)",
    fromUID, toUID, amount, reqID); err != nil {
    return err
}
return tx.Commit()
```

**预期现象**：余额不足时返回业务错误，两个账户余额都不变；重复 `req_id` 时插入唯一键冲突，整个事务回滚。

### MySQL vs PostgreSQL 的真实差异

| 维度 | MySQL | PostgreSQL | 什么时候这个差异重要 |
|---|---|---|---|
| 事务隔离默认值 | REPEATABLE READ | READ COMMITTED | 迁移时要重新验证并发行为 |
| DDL | 8.0 支持部分 Online DDL，仍可能锁表 | DDL 可放进事务，回滚更完整 | 频繁改表结构 |
| JSON | JSON 类型；函数和索引能力弱于 PG | JSONB 可建 GIN 索引 | 半结构化字段多 |
| 索引 | B+Tree、全文、空间 | B+Tree、GIN、GiST、BRIN、部分/表达式索引 | 复杂查询、模糊检索 |
| 扩展 | 插件生态 | PostGIS、pgvector、TimescaleDB | 地理、向量、时序 |
| 复制 | 主从、组复制 | 流复制、逻辑复制 | 跨库同步、CDC |
| 运维熟悉度 | 国内 Web 团队更普遍 | 复杂查询强，但调优门槛高 | 团队没人会就是成本 |

**结论**：没有明确理由选 MySQL。需要 JSONB、地理、向量、复杂查询再选 PostgreSQL。两者都能支撑绝大多数业务，不要因为“听说 PG 更高级”就迁移。

### 关系型不适合什么

关系型数据库不是“任何数据都能存”。超大规模全文检索、海量稀疏字段、超高写入的日志/时序、跨地域强一致写和多租户隔离等场景，可能需要专用系统。先确认自己是否真的遇到这些边界，而不是提前把系统做复杂。

- 每天几十亿条日志，按时间聚合：行存 + B+Tree 写入成本太高，选列式；
- 全文相关性排序：`LIKE '%keyword%'` 会全表扫描，选搜索引擎；
- 超大规模稀疏图遍历：多跳 JOIN 会爆炸，选图数据库；
- 纯 KV 超高并发：关系库事务和 SQL 解析是额外开销，选 Redis（但只做缓存）。

---

## 5.3 文档型：MongoDB

### 它到底是什么

MongoDB 存 BSON 文档，一个文档就是一条完整记录，可以嵌套数组和对象。它适合**一次读写一整块、结构经常变化、关系不深**的数据。

```js
// 商品：属性因品类而异，用文档很自然
db.products.insertOne({
  _id: "p-1001",
  name: "机械键盘",
  category: "keyboard",
  price: 39900,                 // 分
  attrs: { switch: "red", layout: "87" },
  tags: ["hot", "gift"],
  createdAt: new Date()
})

db.products.createIndex({ category: 1, price: 1 })
db.products.find({ category: "keyboard", price: { $lte: 50000 } })
  .sort({ price: 1 }).limit(20)
```

### 什么时候选它

选某种数据库的判断标准应包括：数据是否有清晰关系、是否需要事务、查询是否以主键/索引为主、写入量和保留周期、是否允许最终一致，以及团队是否能监控、备份和恢复。没有这些信息，直接问“MySQL 还是 MongoDB”没有意义。

- 商品/内容属性差异大，关系库要开几十个稀疏列；
- 一个聚合根天然是一个文档，例如“一篇文章 + 它的段落”总是一起读；
- schema 迭代快，早期字段不稳定；
- 写入量大、按分片键水平扩展。

### 什么时候不要选

不要选的理由通常比“能不能用”更重要：团队不会备份恢复、业务需要跨文档事务、查询模式需要复杂连接、数据量增长无法预估，或引入后没有对应的监控和容量模型。技术选型不是把复杂度从代码转移到运维盲区。

- 订单、支付、账务：需要多表事务和严格约束；
- 需要大量多表 JOIN 和复杂报表；
- 团队没有 MongoDB 索引、分片、备份经验；
- 你只是“想直接存 JSON”，但查询其实很固定——MySQL JSON 列或普通表更简单。

### 生产坑，逐条解释

1. **单文档 16MB 上限**。把评论无限 `$push` 进文章文档，迟早写失败。正确做法是评论单独集合，按 `articleId` 建索引。
2. **数组增长导致文档搬迁**。更新越频繁、文档越大，写放大越明显。热点计数不要塞进大文档。
3. **无 schema 不等于无约束**。应用层要有 schema 校验（JSON Schema validator），否则同名字段一会儿是字符串一会儿是数字，查询和聚合全乱。
4. **索引不会自动为所有查询建**。没有索引的 `find` 会全集合扫描；用 `explain("executionStats")` 看 `totalDocsExamined`。
5. **分片键选错无法轻易改**。按低基数或单调递增键分片会造成热点；分片前先压测。
6. **事务有成本**。4.0+ 支持多文档事务，但跨分片事务更慢，不应把它当关系库用。

### 运维要准备什么

引入数据库前要准备备份、恢复演练、版本升级、容量监控、慢查询分析、连接数上限和故障切换手册。没有恢复演练的“高可用”只是配置，不是能力。

- 副本集（至少 3 节点）而不是单机；
- 定期 `mongodump`/快照，并**实际做过恢复演练**；
- 监控连接数、慢查询、复制延迟、磁盘、缓存命中率；
- 版本升级路径要查官方兼容矩阵。

---

## 5.4 键值/缓存：Redis

Redis 是内存数据结构服务器。它的价值是**把一次昂贵计算或远程查询的结果放在离服务更近的地方**，以及提供原子计数、排行榜、简单锁等能力。

```redis
SET account:u-1001:balance 12000 EX 300
GET account:u-1001:balance

HSET user:u-1001 name demo level 3
HGETALL user:u-1001

ZINCRBY rank:20261101 1 p-1001
ZREVRANGE rank:20261101 0 99 WITHSCORES
```

### 正确用法

每种存储都有它擅长的主键/查询模式。正确用法通常意味着按访问路径设计 key、控制 value 大小、设置 TTL 和淘汰策略，并明确系统不可用时的降级行为。

- 缓存数据库查询结果，设置合理 TTL；
- 计数器、排行榜、去重、限流；
- 会话（要评估丢失影响）；
- 分布式锁（Redisson 类库或 `SET NX PX` + 唯一 token + Lua 释放）。

### 不能怎么用

不要把 Redis、搜索库或文档库当成“万能数据库”：缺少跨 key 事务、复杂约束或持久化保证时，业务一致性要由上层重新设计。先明确它是不是事实来源，再决定能否接受丢数据和最终一致。

- **不能作为账户余额的唯一存储**：异步持久化有丢写窗口，主从切换可能回退；
- **不能用 `KEYS *`**：单线程执行，大实例上会阻塞所有请求，用 `SCAN`；
- **不能把大集合整个取出再过滤**：网络和内存都会爆，用合适的数据结构；
- **不能假设缓存一定命中**：代码必须能回源数据库；
- **不能无限往一个 Hash/List 里塞**：大 key 会造成网络拥塞和删除卡顿。

### 缓存一致性：先更新数据库，再删缓存

```
读：GET cache → miss → SELECT db → SET cache
写：UPDATE db → DEL cache
```

为什么不是“更新缓存”？因为并发写时两个请求的更新顺序可能和数据库提交顺序相反，缓存会留下旧值。删除让下一次读自然回源。

仍然存在极短窗口：读请求在删缓存前读到旧值，删完后把旧值写回。生产上常用：

- 延迟双删（写完延迟几百毫秒再删一次）；
- 订阅 binlog 删除缓存；
- 给缓存值带版本号，写入时比较版本。

### 持久化与高可用

持久化回答“重启后数据还在不在”，高可用回答“节点故障时服务能不能继续”。两者不是一回事：有备份也可能恢复很慢；有主从也可能主从延迟。选型时必须把 RPO（能丢多少）和 RTO（多久恢复）写成数字。

| 机制 | 作用 | 代价 |
|---|---|---|
| RDB | 定时快照 | 两次快照之间的数据会丢 |
| AOF everysec | 每秒刷盘 | 最多丢约 1 秒 |
| 主从 | 读扩展、故障切换 | 异步复制可能丢写 |
| Sentinel | 自动选主 | 需要额外节点和演练 |
| Cluster | 分片 | 多 key 操作受 slot 限制 |

Redis 再快也只是缓存层。核心数据必须落事务型数据库。

---

## 5.5 列式/OLAP：ClickHouse

ClickHouse 不是“更快的 MySQL”。它把同一列的数据连续存储，所以做“扫描大量行但只读几列”的统计时，IO 和压缩效率很高；代价是单行更新、事务和点查不擅长。选它之前先确认业务是分析型查询，而不是把交易表换一个引擎。

### 为什么它快

行存数据库按整行读，适合“查一条订单”；列存按列读，适合“扫描 10 亿行只算 `sum(amount)`”。ClickHouse 还会压缩列、向量化执行、按分区裁剪。

```sql
CREATE TABLE event_log (
  event_date Date,
  uid String,
  event String,
  amount Int64
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(event_date)
ORDER BY (event_date, event);

SELECT event, count() AS cnt, sum(amount) AS total
FROM event_log
WHERE event_date = '2026-11-01'
GROUP BY event
ORDER BY cnt DESC
LIMIT 20;
```

### 适合

判断是否适合 ClickHouse，不要看“数据多不多”，要看查询是不是**大批量扫描 + 少数列聚合**。下面这些场景通常合适：

- 埋点、日志、监控指标：一次扫描一天甚至一个月的数据，按维度聚合，历史数据很少改；
- 亿级数据的聚合报表：例如“每个渠道每天的支付成功率和总金额”，只读时间和金额等少数列；
- 宽表扫描、批量导入：数据先进入 Kafka/对象存储，再按批次写入 ClickHouse，查询主要做统计。

一个可验证的验收例子是：导入 1000 万行事件，查询最近 7 天按事件类型聚合。若 P95 在业务目标内、扫描量能通过分区裁剪降到合理范围，才说明模型适合；只导入 1 万行测出来的毫秒数没有参考价值。

### 不适合

- 高频单行 `UPDATE`/`DELETE`：每次修改都会产生新的 part，合并有成本，不能把它当在线订单表；
- 强事务：不能依赖它完成“扣余额 + 写流水 + 改订单”这种原子操作；
- 点查为主、要求毫秒级返回的最新数据：按主键查一条记录应回 MySQL/Redis，不该让分析库承担在线读；
- 小数据量或低查询频率：收益抵不过集群、导入链路和运维成本。

**失败场景**：如果团队把 ClickHouse 当主库，用户刚充值完查不到、退款时尝试单行更新，或者一条未限制时间范围的查询扫全表，最终会把分析链路和在线业务一起拖慢。遇到这些需求应先回到 MySQL/PostgreSQL，而不是继续给 ClickHouse 加机器。

### 数据怎么进去

业务库 MySQL 是主数据，通过 CDC（如 Debezium）或应用双写 + Outbox 同步到 Kafka，再批量写 ClickHouse。**不要**让在线业务直接同步写 ClickHouse，分析库抖动会拖垮交易链路。

---

## 5.6 搜索：Elasticsearch / OpenSearch

搜索引擎的核心是倒排索引：先找到包含词项的文档，再按相关度排序。它适合“搜得到、排得合理”，不适合承担账户余额这类必须强一致的主数据。正确架构通常是主数据库负责事实，搜索索引负责查询体验。

倒排索引解决的是“哪些文档包含这个词”，以及“按相关度排序”。

```http
PUT products
{
  "mappings": {
    "properties": {
      "name":  { "type": "text", "analyzer": "standard" },
      "brand": { "type": "keyword" },
      "price": { "type": "integer" }
    }
  }
}

GET products/_search
{
  "query": { "match": { "name": "机械 键盘" } },
  "sort": [{ "_score": "desc" }, { "price": "asc" }]
}
```

### 适合

- 商品/文章全文检索、分词、高亮、相关性排序；
- 日志检索：按关键词、时间、服务名过滤，再按天滚动索引；
- 多维过滤 + 聚合：品牌、价格区间、分类等组合筛选。

验证方式也要围绕搜索质量：准备一组真实但脱敏的查询，记录召回率、Top 10 命中率、P95 延迟和索引大小。只用一条“能搜到”的手工请求，不足以证明搜索方案可用。

### 不适合

- 主数据存储：refresh 有延迟、更新成本高、事务弱；写入成功不等于立刻可搜；
- 需要强一致和唯一约束的订单、余额、库存；
- 只按主键查：这类查询 MySQL 更便宜、更稳定。

**失败场景**：应用先写 ES 再写 MySQL，ES 成功了但数据库失败，就会出现“搜得到、点进去却不存在”的脏数据；反过来只写 ES 且把它当唯一真相，索引重建或集群故障时会丢失业务事实。正确方向始终是主库先提交事实，再通过可重放链路更新索引。

### 典型架构与一致性

```
MySQL（主数据） → binlog/MQ → 同步服务 → Elasticsearch（索引）
```

搜索结果是**最终一致**的：下单后可能几百毫秒到几秒才搜得到。产品设计要接受这一点，或者在详情页直接查主库。

### 生产坑：症状、原因和处理

- **mapping 改类型要 reindex**：把 `price` 从 `text` 改成 `integer` 时，旧索引不能原地改字段类型。症状是写入报 mapping conflict 或排序结果错误。做法是新建索引、双写/回填、校验文档数和抽样结果，再切别名；不要直接删除旧索引。
- **分片数建索引时确定**：分片过少会导致单分片过大，过多会增加查询协调和集群开销。症状是数据增长后单分片磁盘高、查询变慢。上线前按数据量和保留周期估算，后续通过新索引和别名迁移。
- **深分页有上限**：`from + size` 很大时协调节点要收集和排序大量结果。翻到很后面应使用 `search_after` + 稳定排序键，或限制产品只展示前 N 页。
- **集群黄/红要告警**：黄色通常表示副本未分配，红色表示主分片不可用。只监控进程“活着”不够，要监控集群状态、未分配分片、磁盘水位和写入拒绝。
- **不要用 ES 存唯一真相**：refresh、副本、重建和误删都可能让索引落后或丢失。订单是否存在必须查主库；ES 只回答“怎么搜”。

---

## 5.7 时序：Prometheus / InfluxDB / TimescaleDB

时序库针对“时间范围扫描、按时间聚合、自动过期”优化。它的价值不在存单条记录，而在低成本保存大量带时间戳的数值并快速聚合；如果数据需要复杂事务和频繁更新，就应回到关系型数据库。

时序数据的特点是：**按时间追加、按时间范围聚合、旧数据自动过期**。

```promql
# 最近 5 分钟每个接口的 P99
histogram_quantile(0.99,
  sum by (route, le) (rate(http_request_duration_seconds_bucket[5m]))
)
```

| 方案 | 适合 | 注意 |
|---|---|---|
| Prometheus | 监控指标、告警 | 拉模型、本地存储、不适合长期高基数 |
| InfluxDB | IoT、业务时序 | 生态相对独立 |
| TimescaleDB | 想用 SQL 做时序 | 基于 PostgreSQL，运维更熟 |

**高基数**是时序库的头号杀手：把 `user_id`、`request_id` 放进 label，会生成海量时间序列，内存和磁盘爆炸。业务维度用日志/追踪，不要塞进 metrics label。

---

## 5.8 宽列：Cassandra / HBase / ScyllaDB

适合“写入量极大、按分区键访问、可接受最终一致”的场景，例如海量消息、IoT、日志。

```sql
CREATE TABLE events_by_user (
  user_id text,
  event_time timestamp,
  event_id uuid,
  payload text,
  PRIMARY KEY ((user_id), event_time, event_id)
) WITH CLUSTERING ORDER BY (event_time DESC);
```

查询必须带分区键 `user_id`；不带分区键的查询通常要全集群扫描，直接不可用。

**中小团队谨慎**：运维复杂度、数据建模难度、团队经验要求都高。绝大多数业务用 MySQL + 分区/归档就够。

---

## 5.9 分布式关系库：TiDB / OceanBase / CockroachDB

它们解决“单机 MySQL 容量/写入到顶，但业务 SQL 不想大改”的问题。

- 兼容 MySQL/PostgreSQL 协议；
- 自动分片、多副本、强一致；
- 代价：延迟通常高于单机、运维复杂、成本高、事务模型有细节差异。

**不要因为“以后可能大”就上分布式库。**先做容量评估、索引优化、归档、读写分离；确实到瓶颈再迁移。迁移前要压测事务、热点写入和跨分片查询。

---

## 5.10 图数据库：Neo4j

```cypher
MATCH (a:User {id:'u-1001'})-[:FRIEND]->(b)-[:FRIEND]->(c)
WHERE a <> c
RETURN c.id, count(*) AS common
ORDER BY common DESC LIMIT 10;
```

适合社交关系、风控关联、推荐、知识图谱。关系库也能用递归 CTE 做 2～3 跳，但深度大时 JOIN 数量爆炸。**非图场景不要用**，运维和查询语言都是额外成本。

---

## 5.11 嵌入式：SQLite

单文件、零运维，适合本地工具、测试、边缘设备、小型桌面应用。写并发是单写者，不适合高并发服务端。Go 里常用 `modernc.org/sqlite` 或 `mattn/go-sqlite3`。

---

## 5.12 一个完整选型案例：内容社区

需求：用户发文章、评论、点赞；首页信息流；搜索文章；运营看每日数据。

| 数据 | 存储 | 理由 |
|---|---|---|
| 用户、文章、评论、点赞关系 | MySQL | 关系清晰，需要事务和唯一约束 |
| 文章详情缓存、点赞计数 | Redis | 读多写少，计数可异步落库 |
| 文章搜索 | Elasticsearch | 分词、相关性、高亮 |
| 每日统计报表 | ClickHouse | 大规模聚合，不影响主库 |
| 图片/视频 | 对象存储 | 数据库不存大文件 |

同步链路：

```
MySQL --binlog--> CDC --> Kafka --> ES / ClickHouse
点赞 Redis --> 定时/异步 --> MySQL 点赞表
```

故障降级：

- ES 挂了：搜索页显示“搜索暂不可用”，文章详情不受影响；
- Redis 挂了：回源 MySQL，接口变慢但正确；
- ClickHouse 挂了：报表延迟，交易链路不受影响；
- Kafka 积压：搜索/报表延迟，主站正常。

这就是“引入多存储”的正确姿势：**每一套都有明确职责、同步方式和降级策略**。

---

## 5.13 选型决策记录（ADR）模板

ADR 不是“把最后选了什么写下来”，而是让未来的人知道当时有哪些候选、依据什么证据排除、接受了什么代价。一个可评审的 ADR 至少要有可量化背景、候选方案、决策、反证条件和复审时间。下面的模板可以直接复制到 `docs/adr/003-search-storage.md`；尖括号内容是占位符，发布前必须替换成项目自己的脱敏数据。

```markdown
# ADR-003 文章搜索存储选型

## 状态
已接受 / 待复审

## 背景
文章 500 万篇，需要中文分词、相关性排序、高亮；当前 MySQL LIKE 查询 P99 2.3s。

## 候选
1. MySQL 全文索引：改动小，但中文分词和相关性弱。
2. Elasticsearch：能力强，需新增集群和同步链路。
3. PostgreSQL：需整体迁移，成本高。

## 决策
选择 Elasticsearch 作为搜索索引，MySQL 仍是主数据。

## 理由
- 满足分词/相关性/高亮；
- 不迁移主库，风险可控；
- 团队有 ES 运维经验。

## 代价与风险
- 新增一套集群和监控；
- 索引最终一致，需产品接受；
- 需要 reindex 预案。

## 复审时间
上线后 3 个月，根据查询延迟和运维成本复审。
```

填写时按下面的顺序自检：

1. **背景有数字**：数据量、QPS、P95/P99、保留时间、峰值写入，而不是“数据比较多”；
2. **候选有边界**：至少比较默认关系库和一个新存储，写清各自不满足什么；
3. **理由能验证**：例如“当前 MySQL LIKE P99 2.3s，ES 原型 P95 120ms”，不能只写“ES 更专业”；
4. **代价能兜底**：新链路挂了业务怎么样、谁负责备份恢复、数据如何重建；
5. **决策可反悔**：写明复审日期和触发条件，例如“搜索 QPS 翻 3 倍或索引成本超预算则重评”。

评审时可以故意问反方问题：“如果不用 ES，MySQL 全文索引差在哪？”“如果 ES 集群挂了，搜索页显示什么？”“reindex 要多久？”答不出来，说明 ADR 还只是产品宣传。

---

## 5.14 常见错误决策

| 错误说法 | 问题 | 正确做法 |
|---|---|---|
| “MongoDB 不用建表，开发快” | 后期查询和治理成本更高 | 先明确访问模式，建索引和校验 |
| “Redis 快，所以数据都放 Redis” | 内存贵、持久化有窗口 | 只做缓存/计数，主数据落库 |
| “先上微服务和多存储，以后不用改” | 复杂度立刻产生，收益未必出现 | 单体 + 一个主库起步 |
| “ES 能搜，就顺便当主库” | 最终一致、更新成本高 | 主数据 MySQL，ES 只做索引 |
| “分库分表解决所有性能问题” | 跨分片查询/事务更难 | 先优化 SQL 和索引，再考虑 |
| “ClickHouse 快，实时写进去就行” | 高频小写不合适 | 批量导入，链路隔离 |

---

## 习题

### 基础题

1. 为什么金额字段用 `BIGINT` 存分，而不用 `FLOAT`？
2. `UNIQUE(req_id)` 和应用层“先查再插”相比，为什么前者能挡住并发重复？
3. MongoDB 单文档 16MB 上限会导致什么设计约束？
4. Redis 的 RDB 和 AOF 各有什么丢数据窗口？
5. 列式存储为什么适合聚合、不适合高频单行更新？
6. Elasticsearch 为什么不能作为订单主库？
7. Prometheus 的 label 里放 `user_id` 会发生什么？
8. 什么情况下应该从 MySQL 迁移到 TiDB？

### 动手题

9. 给“短链接服务”设计存储：写入一条长链接，按短码 301 跳转，统计点击量。写出表结构和 Redis 用法。
10. 给“商品搜索”画出 MySQL → ES 的同步链路，并说明 ES 挂了的降级行为。
11. 写一份 1 页 ADR，在 MySQL JSON 列和 MongoDB 之间选择，场景是“商品属性因品类不同而变化”。

### 综合题

12. 一个创业团队 3 个后端，要做内容社区。请给出第一版存储方案，要求：不超过 2 套存储、能支撑 100 万用户、说明未来扩展路径。

---

## 参考答案

<details>
<summary>展开</summary>

1. `FLOAT/DOUBLE` 是二进制浮点，无法精确表示 0.1 等十进制小数，累加会出现误差。金额应使用最小货币单位的整数（如 `BIGINT` 分），或数据库 `DECIMAL` 并明确精度和舍入规则。

2. “先查再插”是两个并发动作，两个请求都可能查到不存在，然后都执行 INSERT。唯一索引在数据库写入时原子判重，只有一个请求能成功，另一个得到重复键冲突。

3. 单文档 16MB 是硬上限，不能把无限增长的数组、评论、日志或大文件塞进同一文档。大对象应拆集合或对象存储，按父 ID 建索引，并考虑文档更新导致整文档重写的成本。

4. RDB 丢失最后一次快照之后的数据；AOF `everysec` 通常最多丢约 1 秒；AOF `always` 基本不丢但每次写都要持久化，性能和磁盘压力更高。选择取决于数据能否重建和可接受丢失窗口。

5. 列存把同一列连续存储，压缩率高，聚合只需读相关列；单行更新要写多个列文件或标记删除，高频小写入成本高。ClickHouse 适合分析，不适合直接承担订单主库。

6. Elasticsearch 的 refresh 有延迟，更新成本高，事务和唯一约束能力弱，也不是唯一事实来源。订单金额、状态和幂等必须落事务数据库，ES 只能作为搜索索引。

7. 每个 user_id 都会生成新的 label 值，时间序列数量随用户数增长，最终造成 Prometheus 内存/磁盘耗尽、查询变慢甚至 OOM。单用户信息应放日志或 trace。

8. 单表达到数十亿、单实例写入/QPS 已到瓶颈、业务确实需要水平扩展，并且索引优化、归档、读写分离都无法解决时，评估 TiDB。迁移前要验证事务语义、热点写入、SQL 兼容、成本和运维能力。

9. 表结构和 Redis 用法：

```sql
CREATE TABLE short_link (
  code       VARCHAR(16) PRIMARY KEY,
  long_url   TEXT NOT NULL,
  uid        VARCHAR(64) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_uid_created (uid, created_at)
);

CREATE TABLE click_daily (
  code   VARCHAR(16) NOT NULL,
  day    DATE NOT NULL,
  clicks BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (code, day)
);
```

跳转链路：

```text
GET /{code}
-> Redis GET link:{code}
-> miss 时 SELECT long_url FROM short_link WHERE code = ?
-> 回填 Redis，TTL 24h
-> Redis INCR click:{code}:{YYYYMMDD}
-> 定时批量 UPSERT 到 click_daily，成功后清理/保留短期计数
```

返回 301/302 前要校验目标 URL；计数失败不能让跳转失败，但应有补偿。

10. 同步链路：

```text
MySQL 事务提交
-> binlog
-> CDC 消费
-> Kafka topic search.article.changed
-> 索引消费者写 ES
-> 记录 binlog offset / event_id 以便重放
```

ES 故障时，发布和详情接口仍走 MySQL；搜索接口返回降级提示，或只提供基于 MySQL 标题的有限查询。恢复后从已提交 offset 重放并全量 reindex。验收：删除 ES 索引后能重建，重复事件不产生错误数据。

11. 一页 ADR 可以这样写：

```markdown
# ADR: 商品属性存储选型

状态：提议
背景：不同品类属性差异大，需要按属性筛选，商品与订单/库存需要事务。
候选：
A. MySQL 商品主表 + JSON 属性列
B. MongoDB 商品文档

决策：第一版选 A。
理由：
- 商品、库存、价格和订单需要事务与唯一约束；
- 高频筛选属性可以抽成固定列/关联表，JSON 仅存低频展示属性；
- 团队已有 MySQL 运维和备份体系；
- 避免同时维护两套事务边界和同步链路。

代价：复杂属性查询可能不如文档数据库灵活。
触发重新评估的证据：属性筛选查询 P99 持续超标，EXPLAIN/压测证明 JSON 方案已无法优化，
且业务接受最终一致和双存储运维成本。
验证：用 1000 万商品压测属性筛选；检查备份恢复、迁移和回滚演练。
```

如果属性查询完全不固定、整文档读写、几乎不需要跨文档事务，且团队有 MongoDB 运维经验，MongoDB 才是合理选择；不能只因为“字段灵活”就选它。

12. 3 人团队第一版最多两套存储：

- MySQL：用户、文章、评论、点赞关系、审核状态，所有需要事务和唯一约束的数据；
- Redis：热点缓存、计数器、限流和短期队列。

第一版搜索用 MySQL 标题前缀/简单全文，报表走离线任务。容量规划按 100 万用户估算日活、文章量、读写比和存储增长，先保证索引、分页、归档和备份。扩展顺序：SQL/索引优化 → 读写分离 → 冷热归档 → 引入 ES/ClickHouse → 最后再分库分表。每次扩容都要有压测和回滚证据。

</details>
