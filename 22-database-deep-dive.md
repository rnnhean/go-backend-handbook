# 第 22 章 数据库深度实战：建表、查询、迁移、刷数据与容量

> 第 4 章给的是速查框架，这一章把数据库工作拆成可执行的工程流程。
> 使用虚构 `mini-coin` 项目，默认 MySQL 8.0；PostgreSQL 的差异在每节末尾单列。

---

## 22.1 数据库选型不是背产品名

先问六个问题：

| 问题 | 为什么问 |
|---|---|
| 数据有没有强关系 | 决定关系型还是文档型 |
| 是否必须事务 | 钱、库存、订单通常必须 |
| 读写比例 | 写多时索引和锁成本更高 |
| 单次查询模式 | 决定索引、分区、是否需要搜索库 |
| 数据增长量 | 决定单表上限、归档、分片 |
| 一致性能容忍多久 | 决定主从、缓存、异步同步方案 |

### 默认选择

- **MySQL**：Web 业务、账户、订单、高并发简单查询，团队熟悉度高时首选。
- **PostgreSQL**：复杂 SQL、JSONB、地理、向量、更严格的类型和 DDL 事务。
- **MongoDB**：文档结构频繁变化、嵌套聚合明显，且能接受较弱多文档事务。
- **Redis**：缓存、短生命周期状态、计数、限流，不作为资金主库。
- **ClickHouse**：日志、指标、埋点等大批量分析。
- **Elasticsearch**：全文检索和日志搜索，不作为业务唯一事实来源。
- **Kafka / RabbitMQ**：异步事件、削峰、解耦，不代替数据库。

选型结论必须能回答：**为什么现在不用 MySQL？新增这一套存储后，备份、监控、升级、故障恢复谁负责？**

### 怎么把六个问题变成一次真实决策

先写访问模式，再写候选产品。例如充值系统的主要操作是“按 uid 读账户、按 req_id 查订单、按 uid + 时间分页查流水”，每个请求数据量小、要求事务和唯一约束、读写比例约 10:1。这个访问模式几乎直接指向 MySQL/PostgreSQL；引入 MongoDB 只会把唯一约束和事务复杂度转移到应用层，引入 Redis 则无法承担资金事实。

反过来，如果需求是“每天写入 20 亿条埋点，按任意维度聚合最近 90 天”，MySQL 单表既不适合这种扫描，也没必要承担 OLAP 压力。此时业务库仍可以是 MySQL，分析链路通过 CDC 写入 ClickHouse。**多一种存储不是替代，而是多一段同步、延迟、补偿和运维责任。**

选型评审至少写四行：

```text
核心访问模式：
必须保证的不变量：
预计 1 年数据量与峰值 QPS：
选择该产品后，新增的故障模式和负责人：
```

如果第四行答不出来，就不要上线。一个团队没人会备份、扩容和恢复的数据库，即使 benchmark 再漂亮，也不能作为核心存储。先做小规模验证：用接近生产的数据量跑真实查询、压测写入、模拟主从切换和恢复，再记录结论，而不是只看官网宣传。

### 一个常见误区：缓存不是数据库

Redis 查询快，不代表它能承担账户余额的唯一事实来源。Redis 持久化、主从切换和内存淘汰都可能造成数据丢失或回滚；账户余额、订单、流水必须有事务数据库和可审计备份。缓存可以降低读 QPS，但缓存失效后必须能回到数据库重建。

同样，Elasticsearch 适合检索，不适合作为唯一写入源；Kafka 适合传输事件，不适合保存“当前余额”；ClickHouse 适合分析，不适合高频单行更新。把每种存储放在它擅长的位置，比强行让一个产品包办所有场景更可靠。

---

## 22.2 从需求推导表，而不是先画字段

需求：

> 每个用户只有一个账户；每次充值请求有唯一请求号；充值成功后账户余额增加；每一笔余额变化都可追溯；同一请求不能重复加钱。

推导：

```text
用户 1 ── 1 账户
账户 1 ── N 余额流水
充值请求 1 ── 0..1 成功订单
```

于是得到：

- `account`：当前余额，按 `uid` 唯一；
- `recharge_order`：请求记录，按 `req_id` 唯一；
- `recharge_flow`：不可变流水，每次余额变化一行。

### 规范化

- 第一范式：字段不可再拆；
- 第二范式：非主键字段依赖完整主键；
- 第三范式：非主键字段不依赖其他非主键字段。

实际业务中会做少量反规范化，例如在订单表冗余 `balance_after`，目的是查询和审计更方便。反规范化的前提是：**知道冗余字段由谁维护，以及不一致时如何修复。**

---

## 22.3 字段类型和约束

### 常用类型选择

| 数据 | 推荐 | 不推荐 | 原因 |
|---|---|---|---|
| 金额 | `BIGINT` 分或 `DECIMAL(18,2)` | `FLOAT/DOUBLE` | 浮点误差 |
| 自增主键 | `BIGINT UNSIGNED` | 随机 UUID 主键 | 随机写导致页分裂 |
| 业务 ID | `VARCHAR(64)` + 唯一索引 | 无长度限制 TEXT | 索引和校验成本 |
| 状态 | `TINYINT` + 枚举注释 | 任意字符串 | 省空间、约束清晰 |
| 时间 | `DATETIME(3)` UTC | 本地时间字符串 | 时区和精度问题 |
| 布尔 | `TINYINT(1)` 或 `BOOLEAN` | `VARCHAR('yes')` | 类型和索引效率 |
| JSON | MySQL `JSON` / PG `JSONB` | 把核心字段全塞 JSON | 查询、约束、迁移困难 |

### 约束必须写进数据库

```sql
CREATE TABLE account (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    uid         VARCHAR(64) NOT NULL,
    balance     BIGINT NOT NULL DEFAULT 0,
    status      TINYINT NOT NULL DEFAULT 1,
    version     BIGINT NOT NULL DEFAULT 0,
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
                 ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_uid (uid),
    CONSTRAINT chk_account_balance CHECK (balance >= 0),
    CONSTRAINT chk_account_status CHECK (status IN (1, 2))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

约束的意义：

- 唯一索引防止重复请求；
- `NOT NULL` 防止业务代码漏判；
- `CHECK` 防止负数余额；
- 默认值让插入行为稳定；
- 字段长度在入口就拦住异常数据。

### NULL 的代价

`NULL` 表示未知，不是空字符串，也不是 0。它会带来：

- `NULL = NULL` 结果不是 true；
- `COUNT(column)` 不计 NULL；
- 索引和排序行为更复杂；
- 前端反序列化时容易把 `null` 和零值混淆。

原则：**能 NOT NULL 就 NOT NULL**。确实需要“未知”语义时再允许 NULL，并在 API 中明确表达。

---

## 22.4 主键、唯一键和普通索引

### 主键

InnoDB 的主键是聚簇索引，数据行按主键顺序存储。自增 `BIGINT` 的顺序写入对 B+Tree 友好。

不要用随机 UUID 做主键，因为随机值会让插入落到 B+Tree 的各个位置，造成页分裂和缓存命中下降。需要全局 ID 时，可以使用有序雪花 ID 或“外部 UUID + 内部自增主键”。

### 唯一索引

```sql
UNIQUE KEY uk_req_id (req_id)
```

唯一索引不仅是查询加速，更是并发幂等的最后防线。业务层的“先查后插”不能替代它。

### 联合索引和最左前缀

```sql
KEY idx_uid_status_created (uid, status, created_at)
```

| 查询 | 是否能用索引 |
|---|---|
| `WHERE uid=?` | 能 |
| `WHERE uid=? AND status=?` | 能 |
| `WHERE uid=? AND status=? AND created_at>?` | 能 |
| `WHERE status=?` | 不能单独利用该联合索引 |
| `WHERE uid=? AND created_at>?` | 通常只能用到 `uid` |

把等值条件列放前面，范围条件列放后面。索引列顺序应按真实查询模式设计，而不是按字段在表里的顺序。

### 覆盖索引

```sql
KEY idx_uid_created_amount (uid, created_at, amount)
SELECT amount FROM recharge_order WHERE uid=? AND created_at>?;
```

如果查询需要的列都在索引里，可以避免回表。`EXPLAIN` 的 `Extra` 出现 `Using index` 是覆盖索引信号。

### 索引不是越多越好

每个索引都要付出：

- 占用磁盘；
- 插入、更新时维护索引；
- 影响优化器选择；
- 增加迁移和备份成本。

低区分度的单列索引通常收益很低；应该优先设计能服务核心查询的联合索引。

---

## 22.5 用 EXPLAIN 看执行计划

```sql
EXPLAIN ANALYZE
SELECT id, amount, created_at
FROM recharge_order
WHERE uid = 'u-1001' AND created_at >= '2026-10-01'
ORDER BY created_at DESC
LIMIT 20;
```

重点字段：

| 字段 | 含义 | 危险信号 |
|---|---|---|
| `type` | 访问方式 | `ALL` 全表扫描 |
| `possible_keys` | 候选索引 | 为空通常说明没合适索引 |
| `key` | 实际索引 | 与预期不一致 |
| `rows` | 预估扫描行数 | 远大于最终结果 |
| `filtered` | 过滤比例 | 很低说明扫了很多无用行 |
| `Extra` | 额外操作 | `Using filesort`、`Using temporary` |

### 优化查询的固定顺序

1. 用真实数据量执行 `EXPLAIN ANALYZE`；
2. 看扫描行数和回表次数；
3. 检查 WHERE、JOIN、ORDER BY、GROUP BY；
4. 设计联合索引；
5. 改写 SQL，减少函数包裹字段；
6. 再测 P95/P99，不只看单次执行。

### 怎么读执行计划，而不是只看有没有索引

`type=ref` 只说明使用了索引，不代表扫描行数少；`rows=1000000` 仍然可能很慢。先看 `rows × filtered` 估算实际参与计算的行数，再看是否回表、是否排序、是否产生临时表。`Using filesort` 不一定是灾难，但如果结果集很大且每次请求都排序，就要考虑让索引顺序直接满足 `ORDER BY`。

`EXPLAIN ANALYZE` 会真正执行 SQL，生产上只能在只读副本、低峰或带超时保护时使用；普通的 `EXPLAIN` 只给估算，统计信息过期时可能严重偏差。比较优化前后时用同一份数据、同一组参数、同一并发，记录 P95/P99、扫描行数、返回行数和临时磁盘，而不是只截一张“key 不为空”的图。

索引设计还要考虑写入成本。给 `status` 这种低区分度字段单独加索引，查询可能仍扫描大量行；更好的做法是把高区分度的等值列放前面，把范围/排序列放后面，例如 `(uid, status, created_at)`。但最终仍要用真实查询验证，不能只靠口诀。

### 常见导致索引失效的写法

```sql
-- 对列做函数运算，可能无法使用普通索引
WHERE DATE(created_at) = '2026-11-01'

-- 前缀通配符通常无法有效使用索引
WHERE uid LIKE '%1001'

-- 隐式类型转换可能导致索引失效
WHERE uid = 1001   -- uid 是 VARCHAR，应该传 '1001'
```

改成范围查询：

```sql
WHERE created_at >= '2026-11-01 00:00:00'
  AND created_at <  '2026-11-02 00:00:00'
```

---

## 22.6 JOIN、聚合和深分页

### JOIN 先保证连接列有索引

```sql
SELECT o.id, o.amount, f.balance_after
FROM recharge_order o
JOIN recharge_flow f ON f.order_id = o.id
WHERE o.uid = ?
ORDER BY o.created_at DESC
LIMIT 20;
```

检查：

- `o.uid` 是否有索引；
- `f.order_id` 是否有索引；
- 连接字段类型是否一致；
- 是否先过滤再连接，避免大结果集参与 JOIN。

### 聚合不要先拉全表再算

```sql
SELECT uid, SUM(amount) AS total
FROM recharge_flow
WHERE created_at >= ? AND created_at < ?
GROUP BY uid;
```

如果报告很大，考虑：

- 预聚合表；
- 按天/小时汇总；
- ClickHouse 等 OLAP 存储；
- 异步任务生成报表。

### 深分页

错误写法：

```sql
SELECT * FROM recharge_order ORDER BY id DESC LIMIT 1000000, 20;
```

数据库仍需扫描并丢弃前一百万行。游标分页：

```sql
SELECT id, uid, amount, created_at
FROM recharge_order
WHERE id < ?
ORDER BY id DESC
LIMIT 20;
```

第一页不传 `id`，后续把上一页最后一个 `id` 作为游标。它要求排序字段稳定且唯一，通常用 `(created_at, id)` 组合。

### 查询不要 SELECT *

- 增加网络传输；
- 大字段被无意读取；
- 覆盖索引失效；
- 表结构变化可能改变客户端行为。

明确列出需要的列，并给返回字段建立 API 契约。

---

## 22.7 事务、隔离级别和锁

### ACID

| 特性 | 含义 | 失败后果 |
|---|---|---|
| 原子性 | 要么全部成功，要么全部回滚 | 订单成功但余额没加 |
| 一致性 | 约束和业务不变量成立 | 负数余额、重复流水 |
| 隔离性 | 并发事务互不产生非法影响 | 丢更新、脏读 |
| 持久性 | 提交后不因崩溃丢失 | 用户付款但数据消失 |

### 四种隔离级别

| 级别 | 脏读 | 不可重复读 | 幻读 | 说明 |
|---|---|---|---|---|
| READ UNCOMMITTED | 可能 | 可能 | 可能 | 很少用 |
| READ COMMITTED | 否 | 可能 | 可能 | 很多互联网业务选择 |
| REPEATABLE READ | 否 | 否 | InnoDB 用 Next-Key Lock 缓解 | MySQL 默认 |
| SERIALIZABLE | 否 | 否 | 否 | 并发最低 |

隔离级别不是越高越好。要在业务正确性、锁冲突和吞吐之间选择，并用测试证明关键约束。

### 行锁示例

```sql
BEGIN;
SELECT balance FROM account WHERE uid = 'u-1001' FOR UPDATE;
UPDATE account SET balance = balance + 50 WHERE uid = 'u-1001';
COMMIT;
```

`FOR UPDATE` 锁定匹配行。没有锁时两个事务可能都基于旧值计算，导致丢更新。

### 死锁

两个事务以不同顺序锁两行：

```text
T1: 锁 A -> 等 B
T2: 锁 B -> 等 A
```

处理原则：

1. 固定加锁顺序，例如总是按 `id` 从小到大；
2. 缩短事务，不在事务内做网络调用；
3. 捕获死锁错误并有限重试；
4. 重试必须幂等，不能重复加钱。

### 乐观锁

```sql
UPDATE account
SET balance = balance + ?, version = version + 1
WHERE id = ? AND version = ?;
```

受影响行数为 0 表示版本冲突，可以重新读取再尝试。它适合冲突不高的场景；余额充值这类强竞争操作通常直接用行锁更直观。

---

## 22.8 已有表加字段

### 安全流程

```text
评估表大小和写入量
  -> 选择加列方式
  -> 在测试环境执行并记录耗时
  -> 备份/确认回滚
  -> 低峰执行
  -> 观察锁等待和复制延迟
  -> 发布代码
  -> 数据回填
  -> 加约束/索引
```

### 加一个可空列

```sql
ALTER TABLE recharge_order
ADD COLUMN remark VARCHAR(255) NULL;
```

可空、无默认值的列通常比“带默认值并重写全表”的 DDL 更安全，但不同 MySQL 版本和 DDL 算法行为不同，必须实测。

### 加非空列

如果表已有数据，必须分阶段：

```sql
-- 第一步：允许 NULL
ALTER TABLE recharge_order ADD COLUMN source VARCHAR(32) NULL;

-- 第二步：应用同时兼容新旧数据
-- 第三步：分批回填
UPDATE recharge_order SET source = 'unknown'
WHERE source IS NULL AND id BETWEEN ? AND ?
LIMIT 1000;

-- 第四步：确认没有 NULL 后收紧约束
ALTER TABLE recharge_order MODIFY COLUMN source VARCHAR(32) NOT NULL;
```

### 加索引

```sql
ALTER TABLE recharge_order
ADD INDEX idx_uid_status_created (uid, status, created_at),
ALGORITHM=INPLACE, LOCK=NONE;
```

是否支持 `INPLACE/LOCK=NONE` 取决于 MySQL 版本、表引擎和索引类型。执行前先用测试库验证，且要监控：

- `SHOW PROCESSLIST` 里的 `Waiting for table metadata lock`；
- 主从延迟；
- 磁盘临时空间；
- 业务 P99。

### 大表不要直接做

面对千万级、亿级表：

- 使用 gh-ost / pt-online-schema-change；
- 在低峰执行；
- 先 shadow 表验证；
- 限制每次 DDL 的影响；
- 准备回滚和停止方案。

---

## 22.9 修改字段类型

字段类型修改比加字段更危险，因为可能改变数据、索引和程序行为。

### 原则

1. 先加新列，不直接改老列；
2. 双写新旧列；
3. 回填并校验；
4. 切换读取；
5. 观察一段时间；
6. 删除旧列。

例如把 `amount` 从 `VARCHAR` 改为 `BIGINT`：

```sql
ALTER TABLE recharge_order ADD COLUMN amount_new BIGINT NULL;
```

代码：

```text
读取 old amount 时尝试转换
写入时同时写 amount 和 amount_new
后台按 id 分批回填 amount_new
比较转换前后的数量、空值、负数、溢出
确认无误后切换读 amount_new
```

### 不能无损转换的情况

- 字符串包含非数字；
- 超出目标整数范围；
- `NULL` 是否允许改变；
- 小数转整数会丢失精度；
- 排序规则和字符集变化；
- 索引大小超限。

**先查数据，再改类型：**

```sql
SELECT COUNT(*) FROM recharge_order WHERE amount_new IS NULL;
SELECT COUNT(*) FROM recharge_order
WHERE amount REGEXP '^-?[0-9]+$' = 0;
```

生产修改前必须备份或保留可恢复快照，不要用“应该没问题”作为依据。

---

## 22.10 数据迁移和刷数据

### 迁移脚本版本化

```text
migrations/
  001_create_account.sql
  002_create_recharge_order.sql
  003_add_channel.sql
  004_backfill_channel.sql
```

规则：

- 每个脚本有唯一版本号；
- 只追加，不修改已上线的历史脚本；
- 一次迁移一个目的；
- 脚本可重复执行要有明确策略；
- 生产执行记录写入数据库或发布系统；
- 迁移失败后不能靠手工猜执行到哪一步。

### 分批刷数据

```sql
UPDATE recharge_order
SET channel = 'unknown'
WHERE channel IS NULL
ORDER BY id
LIMIT 1000;
```

Go 任务形式：

```go
package backfill

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// BackfillChannel 每次只处理一批，返回本批处理的最大 id 和行数。
// 调用方负责持久化 lastID，失败重启时从持久化的位置继续。
func BackfillChannel(ctx context.Context, db *sql.DB, lastID int64, batch int, sleep time.Duration) (int64, int, error) {
	if batch <= 0 || batch > 10000 {
		return lastID, 0, fmt.Errorf("batch must be in (0,10000]")
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id FROM recharge_order
		WHERE id > ? AND channel IS NULL
		ORDER BY id
		LIMIT ?`, lastID, batch)
	if err != nil {
		return lastID, 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return lastID, 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return lastID, 0, err
	}
	if len(ids) == 0 {
		return lastID, 0, nil
	}

	// 以实际读到的最大 id 作为游标，允许主键有空洞。
	newLast := ids[len(ids)-1]
	for _, id := range ids {
		if _, err := db.ExecContext(ctx,
			`UPDATE recharge_order SET channel='unknown' WHERE id=? AND channel IS NULL`, id); err != nil {
			return lastID, 0, fmt.Errorf("update id=%d: %w", id, err)
		}
	}
	time.Sleep(sleep)
	return newLast, len(ids), nil
}
```

游标取自“本批实际读到的最大 id”，不假设主键连续；即使中间有空洞或并发写入，也不会漏批。生产上建议把一批更新放进同一个短事务，或者用 `WHERE id IN (...)` 一次更新，具体取决于锁范围。

```go
for {
	next, n, err := backfill.BackfillChannel(ctx, db, lastID, 1000, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if err := saveCheckpoint(ctx, "backfill_channel", next); err != nil {
		return err
	}
	lastID = next
}
```

### 刷数据的安全要求

刷数据的正确顺序是“先选范围、再 dry-run、再小批量、再全量”，而不是直接跑 UPDATE。下面每一条都要有实现和证据：

1. **选范围**：先 `SELECT COUNT(*)`、最小/最大主键，确认目标行数和数据分布；条件必须可重复，例如 `channel IS NULL AND id > :last_id ORDER BY id LIMIT 1000`。
2. **dry-run**：用同一 WHERE 只 SELECT 主键和待改字段，统计命中量、非法值、冲突行，不写库。
3. **小批量**：先跑 100 行，观察执行时间、锁等待、主从延迟和业务 P99；没有异常再扩到 1000。
4. **可暂停恢复**：每批记录 `last_id`、处理数、失败数到进度表；任务重启从进度继续，不从头扫。
5. **幂等**：更新条件包含旧值，例如 `SET channel='unknown' WHERE id=? AND channel IS NULL`，重复执行不会覆盖用户后来写入的新值。
6. **与在线写入兼容**：先发布能读新旧值的代码，再回填；回填期间的新写入必须同时满足约束。
7. **避开高峰和备份冲突**：不要在业务峰值、全量备份和 DDL 同时执行，否则 IO、锁和主从延迟会叠加。
8. **前后校验**：比较剩余空值数、非法值数、总量、抽样明细和校验和；异常立即停止，不继续“跑完再说”。

**失败场景**：用 `OFFSET` 分批时，前面删除或新增数据会导致漏行/重复；长事务包住整个回填会积累 undo、阻塞 DDL；没有进度表，任务失败后只能猜跑到哪；只校验“执行成功”，不校验金额和行数，错误数据会被静默保留。

### 校验

```sql
-- 检查剩余未回填
SELECT COUNT(*) FROM recharge_order WHERE channel IS NULL;

-- 检查异常分布
SELECT channel, COUNT(*) FROM recharge_order GROUP BY channel;

-- 检查金额总量
SELECT SUM(amount) FROM recharge_flow WHERE created_at >= ?;
```

刷数据不是“跑一条 UPDATE 就完事”，必须有前后对账。

---

## 22.11 查询不影响线上性能

### 上线前

- 用生产量级数据做 `EXPLAIN ANALYZE`；
- 限制扫描行数和返回行数；
- 避免无索引 JOIN、全表聚合、深分页；
- 把大查询放到只读实例或离线库；
- 设置查询超时和最大执行时间；
- 先在小流量和低峰验证。

上线前不能只验证“这条 SQL 在测试库能跑”。要模拟真实数据量、真实索引、真实并发和真实参数分布。一个查询在 1 万行时 5ms，在 1 亿行时可能因为优化器选错索引变成全表扫描。把 SQL 的扫描行数上限、返回行数上限和超时写进代码或数据访问层，避免某个异常参数拖垮连接池。

发布新查询前还要评估连接池占用：一个请求持有连接 10 秒，100 个并发就会占满 20 个连接的池，后续正常请求全部排队。慢查询不是只影响自己，它会把整个服务拖死。

### 运行中

```sql
SHOW FULL PROCESSLIST;
SHOW ENGINE INNODB STATUS;
SELECT * FROM information_schema.innodb_trx
WHERE time > 10;
```

监控：

- 慢查询数量；
- 数据库 CPU、IOPS、连接数；
- 锁等待；
- 主从延迟；
- 临时表磁盘占用；
- 应用 P99。

看到慢查询后，先判断是“偶发一条”还是“持续增长”。偶发可能是一次报表或人工操作；持续增长往往说明索引缺失、数据量跨过阈值或某个参数分布异常。`SHOW PROCESSLIST` 只能看到当前瞬间，最好同时开启慢查询日志并记录 `request_id`，这样才能把数据库里的 SQL 关联回具体接口和用户操作。

如果连接数满了，不要第一反应把 `max_connections` 调大。先看应用连接池是否泄漏、事务是否长时间不提交、是否有慢查询占住连接。数据库连接是有限资源，盲目扩容只会把压力转移到内存和锁竞争上。

### 报表类查询隔离

报表不要直接打主库。典型架构：

```text
主库 -> 只读副本 -> 报表服务
主库 -> CDC -> ClickHouse -> BI
```

即使查只读库，也要限制并发和单次扫描量。只读库被报表打挂，依然会影响故障切换和备份。

---

## 22.12 每个表最多多少数据

MySQL 单个 InnoDB 表没有“超过 1000 万行就自动不能用了”的硬门槛。真实边界由以下因素共同决定：

- 表宽和索引数量；
- 查询是否能命中索引；
- DDL 和备份窗口；
- 单机磁盘和内存；
- 写入热点；
- 恢复时间目标；
- 团队运维能力。

### 经验区间

| 数据量 | 风险 | 建议 |
|---|---|---|
| 百万级 | 通常容易管理 | 正常索引和备份 |
| 千万级 | 需要关注大查询和 DDL | 压测、在线变更、归档 |
| 亿级 | 单机运维成本明显增加 | 分区/分库分表/冷热分离 |
| 十亿级 | 必须专门设计 | 分片、异步归档、专用存储 |

这些只是起点，不是硬性答案。一个 100 亿行的窄表按主键查可能仍然很快；一个 100 万行的宽表做无索引聚合也可能拖垮数据库。

### 判断是否该拆表

看这些信号：

- 单表磁盘和索引持续膨胀；
- DDL 无法在维护窗口完成；
- 备份恢复时间超过目标；
- 热点写入集中在一个索引页；
- 查询必须扫全表或大量回表；
- 单库连接、CPU、IO 达到瓶颈。

### 分表之前先做

1. 归档历史数据；
2. 优化索引和 SQL；
3. 冷热分离；
4. 读写分离；
5. 预聚合；
6. 再评估分表。

分表会带来分布式事务、跨表查询、扩容和迁移问题，不能当作第一反应。

---

## 22.13 备份、恢复和对账

备份不是“有文件”，而是“能在目标时间内恢复并验证”。

### 备份类型

| 类型 | 适合 | 优点 | 代价/失败场景 |
|---|---|---|---|
| 全量物理备份 | 日常恢复、大库 | 恢复快，接近原环境 | 占空间；跨版本/不同环境可能不兼容 |
| 增量/binlog | 时间点恢复 | 可恢复到误删前某一秒 | 链长、顺序错一次就恢复失败，必须定期做基线 |
| 逻辑备份 | 小库、跨版本迁移 | 可读、可筛选对象 | 大库慢，恢复要重放 SQL，容易漏触发器/权限 |
| 快照 | 云盘/存储层 | 快、对数据库影响小 | 必须验证一致性；跨区/跨账号权限要单独演练 |

生产通常不是二选一，而是“周期全量 + 持续 binlog + 定期快照”，再用恢复演练证明组合可用。备份系统显示成功不等于可恢复：要检查校验和、加密密钥、保留周期、异地副本和恢复权限。**失败场景**：备份在同一个故障域、备份账号能删备份、只备份数据不备份 schema/权限、恢复时才发现 binlog 链缺一段。

### 恢复演练

至少验证：

- 空环境能否恢复；
- 恢复到指定时间点；
- 表数量和行数是否一致；
- 关键金额汇总是否一致；
- 应用能否连接恢复后的数据库；
- 恢复耗时是否满足目标。

恢复演练要像真正事故一样做：从备份系统拉取文件，在独立环境启动，记录每一步耗时和失败点。不要只看备份任务显示绿色；备份文件可能损坏、权限可能丢失、恢复脚本可能依赖某个已经不存在的目录。只有实际恢复到可用数据库并通过应用查询，才算备份有效。

对资金数据还要做业务对账，而不只是比较行数。比如比较订单成功总额、流水增加总额和账户余额总和，确认三者关系符合业务定义；行数相同但金额错位，依然不能接受。恢复时间和数据丢失窗口（RPO/RTO）要写成明确目标，并通过演练验证。

### 资金对账

```sql
-- 流水增加总额
SELECT SUM(delta) FROM recharge_flow WHERE created_at >= ? AND created_at < ?;

-- 账户应有余额
SELECT SUM(balance) FROM account;

-- 订单成功总额
SELECT SUM(amount) FROM recharge_order WHERE status = 1 AND created_at >= ?;
```

对账不一定要每秒执行，但必须有定期任务和异常告警。

---

## 22.14 PostgreSQL 关键差异

| 主题 | MySQL | PostgreSQL |
|---|---|---|
| 自增 | `AUTO_INCREMENT` | `GENERATED ... AS IDENTITY` / `SERIAL` |
| JSON | `JSON` | `JSONB` 可索引 |
| 大小写 | 表名行为受系统配置影响 | 未加引号标识符折叠为小写 |
| DDL 事务 | 多数 DDL 隐式提交 | 支持事务内 DDL |
| 索引 | B+Tree 为主 | B-Tree/GIN/GiST/BRIN/表达式索引 |
| 更新 | InnoDB 聚簇索引 | MVCC 版本存储在堆表，VACUUM 很重要 |
| 连接 | 连接池常用 | 连接成本较高，PgBouncer 常见 |
| 分页 | `LIMIT offset,count` | `LIMIT count OFFSET offset` |

PostgreSQL 示例：

```sql
CREATE TABLE account (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uid VARCHAR(64) NOT NULL UNIQUE,
    balance BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    status SMALLINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_order_uid_created
ON recharge_order (uid, created_at DESC);
```

**迁移脚本不能把 MySQL 语法直接复制到 PostgreSQL。** 建表、索引、DDL 事务、时间类型、UPSERT 语法都要分别测试。

### 迁移到 PostgreSQL 时最容易踩的坑

不要把 MySQL 的 DDL 和 SQL 原样搬过去。PostgreSQL 的标识符大小写、类型转换、`ON CONFLICT`、序列、索引和事务错误恢复都不同。尤其是唯一键冲突：MySQL 中普通 `INSERT` 失败后事务通常还可以继续查询；PostgreSQL 默认会把整个事务标记为 aborted，后续语句直接报 `current transaction is aborted`。需要 `SAVEPOINT`/`ROLLBACK TO SAVEPOINT`，或者先查询再决定是否写。

PostgreSQL 的 MVCC 会产生死元组，长期运行需要 `VACUUM`/`autovacuum`；连接成本较高，常见做法是 PgBouncer 加应用连接池。迁移前要用真实查询验证执行计划、锁行为和参数化 SQL，不要假设“都是 SQL，所以完全兼容”。

---

## 22.15 完整实验

### 启动 MySQL

```bash
docker run --name mini-mysql \
  -e MYSQL_ROOT_PASSWORD=local_only_password \
  -e MYSQL_DATABASE=mini_coin \
  -p 3306:3306 \
  -d mysql:8.4
```

> 这里的密码只用于本地一次性实验，不能用于任何共享或生产环境。

### 执行迁移

```bash
mysql -h 127.0.0.1 -uroot -p mini_coin < migrations/001_create_account.sql
```

**预期结果。** 命令退出码为 0；`SHOW TABLES` 能看到 `account`；`SHOW CREATE TABLE account` 的字段、索引和字符集与迁移文件一致；迁移记录表新增版本号。**失败场景**：SQL 在中途报错但前半段已执行，下一次重跑又因表已存在失败；连接到了错误 schema；迁移脚本依赖本机没有的扩展。执行前应在空库和目标版本各演练一次，并把输出和版本号保存为证据。

### 构造测试数据

```sql
INSERT INTO account(uid, balance) VALUES ('u-1001', 100);
INSERT INTO recharge_order(req_id, uid, amount_cent, channel, status)
VALUES ('r-1', 'u-1001', 50, 'manual', 0);
```

先确认插入结果：`SELECT uid,balance FROM account WHERE uid='u-1001'` 应返回一行；重复插入相同 `req_id` 应因唯一索引失败。只有一条数据时 `EXPLAIN` 很容易选择全表扫描，所以建议再生成至少 10 万行脱敏测试数据，或从测试数据生成器灌入，才能观察索引选择。

### 观察执行计划

```sql
EXPLAIN ANALYZE
SELECT id, amount_cent FROM recharge_order
WHERE uid = 'u-1001'
ORDER BY created_at DESC
LIMIT 20;
```

重点看 `type/access_type`、`key`、`rows`、`filtered`、`actual time` 和 `actual rows`。预期使用 `(uid, created_at)` 索引，扫描行数接近 20；如果 `key=NULL` 或 `rows` 远大于返回行数，说明索引选择不对。`EXPLAIN` 只是估算，`EXPLAIN ANALYZE` 会真实执行查询，生产上只能在只读副本或受控测试环境用，并设置超时。

### 制造并发事务

打开两个终端：

```sql
-- 终端 A
BEGIN;
SELECT * FROM account WHERE uid='u-1001' FOR UPDATE;
-- 先不要 COMMIT

-- 终端 B
BEGIN;
SELECT * FROM account WHERE uid='u-1001' FOR UPDATE;
-- 观察等待
```

回到 A：

```sql
UPDATE account SET balance=balance+10 WHERE uid='u-1001';
COMMIT;
```

B 随后获得锁，再执行自己的更新并提交。观察最终余额，理解事务串行化。

---

## 22.16 习题

### 基础题

1. 为什么金额不能用 `FLOAT`？
2. `VARCHAR(64)` 和 `TEXT` 作为用户 ID 各有什么问题？
3. 为什么唯一索引比业务代码里的“先查后插”更可靠？
4. `WHERE uid=? AND created_at>?` 使用 `(uid,status,created_at)` 索引时通常能用到哪些列？
5. 深分页为什么慢？如何改成游标分页？
6. `FOR UPDATE` 解决什么问题？什么时候不适合使用？

### 迁移题

7. 给已有千万行订单表增加 `channel NOT NULL` 字段，写出安全的五步流程。
8. 把 `amount VARCHAR` 改成 `BIGINT`，如何保证不丢数据？
9. 为什么不能用 `OFFSET 1000000` 分批刷数据？应该使用什么游标？
10. 大表加索引时观察哪些线上指标？

### 设计题

11. 设计一个“查询用户最近 20 条充值记录”的索引，并写 SQL。
12. 设计对账任务，如何发现“账户余额和流水总和不一致”？
13. 单表 5 亿行时，先做哪三种优化，再考虑分表？
14. 设计一次备份恢复演练的验收清单。

---

## 参考答案

<details>
<summary>展开参考答案</summary>

### 1. 金额为什么不能用 `FLOAT`

浮点用二进制表示小数，`0.1 + 0.2` 可能不等于 `0.3`；金额经过多次累加、利息计算后会出现分差，对账永远对不平。

```sql
SELECT 0.1 + 0.2 = 0.3;   -- 0

CREATE TABLE demo_money (v DOUBLE);
INSERT INTO demo_money VALUES (0.1),(0.2);
SELECT SUM(v)=0.3 FROM demo_money;   -- 0

CREATE TABLE demo_money2 (v DECIMAL(12,2));
INSERT INTO demo_money2 VALUES (0.1),(0.2);
SELECT SUM(v)=0.3 FROM demo_money2;  -- 1
```

生产推荐 `BIGINT amount_cent`（分）或 `DECIMAL(18,2)`。Go 用 `int64` 或 `decimal.Decimal`，禁止 `float64`。

**验证**：插入 100 万条 `0.01`，`SUM` 结果必须精确等于 10000.00；`FLOAT/DOUBLE` 版本通常会出现尾差。

---

### 2. `VARCHAR(64)` 与 `TEXT` 做用户 ID

| 类型 | 优点 | 问题 |
|---|---|---|
| `VARCHAR(64)` | 有上限、可建高效唯一索引、行内存储 | 需确认 ID 最长长度 |
| `TEXT` | 理论上不限长 | 索引必须前缀、排序/比较成本高、容易塞入异常长值 |

用户 ID 是业务键，应该：

```sql
uid VARCHAR(64) NOT NULL,
UNIQUE KEY uk_uid (uid)
```

```sql
-- 验证索引
SHOW INDEX FROM account WHERE Key_name='uk_uid';
EXPLAIN SELECT id FROM account WHERE uid='u-1001';
```

预期 `type=const/ref`、`key=uk_uid`。如果用了 `TEXT`，MySQL 建唯一索引必须指定前缀长度，可能把两个不同 ID 误判为冲突。入口层还要做长度和字符集校验，不能依赖数据库兜底。

---

### 3. 唯一索引 vs 先查后插

两个并发请求同时执行：

```text
请求 A: SELECT ... WHERE req_id='r-1'  -> 不存在
请求 B: SELECT ... WHERE req_id='r-1'  -> 不存在
请求 A: INSERT ...                     -> 成功
请求 B: INSERT ...                     -> 也成功（没有唯一约束时）
```

“先查后插”在并发下存在检查与写入之间的窗口。唯一索引把判断和写入合并为一个原子操作，数据库保证只有一个成功：

```sql
ALTER TABLE recharge_order ADD UNIQUE KEY uk_req_id (req_id);
```

业务代码捕获重复键：

```go
if isDuplicateKey(err) {
    // 回查已有订单，比较参数后返回首次结果或 409
}
```

**验证**：用 20 个 goroutine 同时插入同一个 `req_id`，最终表中只能有 1 行，其余 19 个收到重复键错误。`go test -race` 只能验证 Go 代码的数据竞争，不能替代数据库唯一约束。

---

### 4. `(uid, status, created_at)` 索引

```sql
-- 查询
SELECT id, amount_cent
FROM recharge_order
WHERE uid = 'u-1001' AND created_at > '2026-01-01'
ORDER BY created_at DESC
LIMIT 20;
```

索引列顺序是 `uid, status, created_at`：

- `uid` 等值：能用；
- `status` 等值：能用；
- `created_at` 范围：能用，但**范围列之后的列**通常不能再用于等值定位；
- 查询没有 `status` 条件时，索引只能稳定用 `uid`，`status` 之后的 `created_at` 无法直接用于排序。

```sql
EXPLAIN ANALYZE SELECT ...;
```

看 `key`、`key_len`、`rows`、`actual rows`。`key_len` 能帮助判断实际用了前几列。

**正确做法**：按真实查询建索引。查“某用户某状态按时间倒序”就建 `(uid, status, created_at, id)`；查“某用户所有状态按时间倒序”就建 `(uid, created_at, id)`。不要用一个索引硬套所有查询。

---

### 5. 深分页与游标分页

```sql
-- 慢：扫描并丢弃前 100 万行
SELECT id, uid, amount_cent
FROM recharge_order
WHERE uid = 'u-1001'
ORDER BY created_at DESC, id DESC
LIMIT 1000000, 20;
```

游标分页：

```sql
SELECT id, uid, amount_cent, created_at
FROM recharge_order
WHERE uid = 'u-1001'
  AND (created_at, id) < ('2026-10-01 10:00:00.000', 9001)
ORDER BY created_at DESC, id DESC
LIMIT 20;
```

索引：

```sql
ALTER TABLE recharge_order
  ADD KEY idx_uid_created_id (uid, created_at, id);
```

**为什么加 `id`**：`created_at` 可能相同，只用时间做游标会漏行或重复。`(created_at, id)` 构成稳定全序。

**验证**：

```bash
# 两种写法各跑 5 次，比较实际耗时
mysql -e "EXPLAIN ANALYZE SELECT ..." minicoin
```

游标分页的 `actual time` 应稳定在毫秒级，不随页数增长；`rows` 应接近 `LIMIT` 而不是百万级。

---

### 6. `FOR UPDATE`

`SELECT ... FOR UPDATE` 对选中的行加排他锁，让并发事务串行修改同一行，避免“读-改-写”丢失更新。

```sql
BEGIN;
SELECT balance FROM account WHERE uid='u-1001' FOR UPDATE;
UPDATE account SET balance = balance + 100 WHERE uid='u-1001';
COMMIT;
```

适用：同一行需要“先读后判断再写”的强一致场景，如扣余额前检查冻结状态。

不适合：

- 持有锁时调用 HTTP/RPC/MQ 等慢操作；
- `WHERE` 没有索引，导致锁大量行；
- 只需要单条原子更新时，直接 `UPDATE ... SET balance=balance+? WHERE ...` 更简单；
- 高并发读多写少的场景，锁竞争会拖垮吞吐。

**验证**：开两个会话，A 执行 `FOR UPDATE` 不提交，B 执行同一行更新会阻塞；A 提交后 B 继续。再用 `SELECT * FROM performance_schema.data_lock_waits` 观察等待关系。若 B 阻塞很久，检查 A 是否在事务里做了远程调用。

---

### 7. 千万行订单表加 `channel NOT NULL`

五步：

```sql
-- 1. 加可空列（先在预发验证 DDL 算法和耗时）
ALTER TABLE recharge_order
  ADD COLUMN channel VARCHAR(16) NULL,
  ALGORITHM=INSTANT;

-- 2. 应用双写：新订单写 channel，老代码可忽略
-- 3. 分批回填
UPDATE recharge_order
SET channel='unknown'
WHERE id > ? AND channel IS NULL
ORDER BY id LIMIT 1000;

-- 4. 校验
SELECT COUNT(*) FROM recharge_order WHERE channel IS NULL;

-- 5. 收紧约束
ALTER TABLE recharge_order
  MODIFY COLUMN channel VARCHAR(16) NOT NULL DEFAULT 'unknown',
  ALGORITHM=INPLACE, LOCK=NONE;
```

**回滚**：第 1～4 步随时停掉双写、保留可空列即可；第 5 步前先在预发演练反向 DDL。不要在未备份的情况下 `DROP COLUMN`。

**验证证据**：DDL 耗时、MDL 等待、主从延迟、`channel IS NULL` 计数、订单行数和金额汇总、双版本应用 smoke test。

---

### 8. `amount VARCHAR` 改 `BIGINT`

不能直接 `ALTER COLUMN`，要先查数据是否能无损转换。

```sql
-- 找非法值
SELECT id, amount FROM recharge_order
WHERE amount NOT REGEXP '^[0-9]+$'
LIMIT 100;

-- 找溢出值（BIGINT 上限 9223372036854775807）
SELECT id, amount FROM recharge_order
WHERE CAST(amount AS DECIMAL(30,0)) > 9223372036854775807
LIMIT 100;
```

迁移步骤：

```sql
-- 1. 加新列
ALTER TABLE recharge_order ADD COLUMN amount_cent BIGINT NULL;

-- 2. 双写：应用同时写 amount 和 amount_cent
-- 3. 分批转换
UPDATE recharge_order
SET amount_cent = CAST(amount AS SIGNED)
WHERE id > ? AND amount_cent IS NULL
  AND amount REGEXP '^[0-9]+$'
LIMIT 1000;

-- 4. 校验
SELECT COUNT(*) FROM recharge_order WHERE amount_cent IS NULL;
SELECT COUNT(*) FROM recharge_order
WHERE amount_cent <> CAST(amount AS SIGNED);
SELECT SUM(amount_cent), SUM(CAST(amount AS SIGNED)) FROM recharge_order;

-- 5. 切换读新列，观察后删旧列
```

**失败场景**：字符串带小数点（`"10.5"`）直接转整数会截断；带货币符号或千分位会转换失败；超出范围会报错或饱和。所有非法值必须先人工确认，不能 `UPDATE IGNORE` 静默丢弃。

---

### 9. 为什么不用 `OFFSET` 刷数据

```sql
-- 错
UPDATE recharge_order SET channel='unknown'
WHERE channel IS NULL
ORDER BY id
LIMIT 1000 OFFSET 1000000;
```

问题：

- `OFFSET` 越大越慢，每批都要重新扫描并丢弃前 N 行；
- 回填期间有新行插入/删除，偏移量会错位，导致漏行或重复；
- 失败后无法准确恢复。

正确做法：记录上一批的最大主键。

```sql
UPDATE recharge_order SET channel='unknown'
WHERE id > :last_id AND id <= :max_id AND channel IS NULL
ORDER BY id LIMIT 1000;
```

```sql
-- 取本批边界
SELECT id FROM recharge_order
WHERE id > :last_id AND channel IS NULL
ORDER BY id LIMIT 1000;
```

用实际读到的最大 `id` 作为下一轮游标，写入进度表。**验证**：任务中断后重启，最终 `COUNT(*) WHERE channel IS NULL = 0`，且没有行被重复更新（可用 `updated_at` 或审计表核对）。

---

### 10. 大表加索引观察什么

| 指标 | 为什么 |
|---|---|
| `Waiting for table metadata lock` | DDL 拿不到 MDL 会阻塞后续所有查询 |
| 主从延迟 `Seconds_Behind_Master` | 大 DDL 在主库执行后同步到从库需要时间 |
| 磁盘临时空间 | INPLACE 重建索引可能写临时文件 |
| 数据库 CPU/IOPS | 建索引会消耗大量 IO |
| 业务 P99、错误率 | 最终判断用户是否受影响 |
| 锁等待 | 确认没有阻塞在线事务 |

```sql
SHOW PROCESSLIST;
SHOW ENGINE INNODB STATUS\G
SELECT * FROM sys.innodb_lock_waits;
SHOW REPLICA STATUS\G
```

**验证**：DDL 前后各压测 10 分钟，比较 P99、QPS、错误率；如果 DDL 期间 P99 翻倍，立即停止并改到低峰或用 gh-ost。

---

### 11. 查询用户最近 20 条充值记录

```sql
ALTER TABLE recharge_order
  ADD KEY idx_uid_created_id (uid, created_at, id);

SELECT id, req_id, amount_cent, channel, status, created_at
FROM recharge_order
WHERE uid = 'u-1001'
ORDER BY created_at DESC, id DESC
LIMIT 20;
```

**验证**：

```sql
EXPLAIN ANALYZE
SELECT id, amount_cent FROM recharge_order
WHERE uid='u-1001'
ORDER BY created_at DESC, id DESC
LIMIT 20;
```

预期 `key=idx_uid_created_id`、`rows` 接近 20、`Extra` 不含 `Using filesort`。如果分页，用 `(created_at, id)` 作为游标，不要 `OFFSET`。

---

### 12. 对账任务

目标：发现“账户余额 ≠ 流水累计”的账户。

```sql
-- 按账户汇总成功流水
SELECT uid, SUM(delta_cent) AS flow_sum
FROM account_flow
WHERE status = 1
GROUP BY uid;

-- 与账户余额比较
SELECT a.uid, a.balance, COALESCE(f.flow_sum, 0) AS flow_sum
FROM account a
LEFT JOIN (
  SELECT uid, SUM(delta_cent) AS flow_sum
  FROM account_flow
  WHERE status = 1
  GROUP BY uid
) f ON f.uid = a.uid
WHERE a.balance <> COALESCE(f.flow_sum, 0);
```

大表不能直接跑全量，按 `uid` 哈希分片、按时间窗口增量对账，结果写 `reconcile_result` 表：

```sql
CREATE TABLE reconcile_result (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  batch_id VARCHAR(64) NOT NULL,
  uid VARCHAR(64) NOT NULL,
  db_balance BIGINT NOT NULL,
  flow_sum BIGINT NOT NULL,
  diff BIGINT NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_batch (batch_id),
  KEY idx_uid (uid)
);
```

**验证**：手工制造一条不一致，对账结果必须出现该 `uid` 且 `diff != 0`；修复后下一批次消失。告警阈值不能是“只要有一条就 P0”，要区分金额、账户等级和持续时间。

---

### 13. 5 亿行先做哪三种优化

1. **归档冷数据**：把 2 年前的订单迁到历史库/对象存储，主表只保留热数据；
2. **优化索引和 SQL**：消除全表扫描、深分页、无谓回表，按真实查询建联合/覆盖索引；
3. **冷热分离/读写分离**：报表和列表查询走只读副本或 ClickHouse，主库只承担交易写入。

还可以做预聚合、分区表、缓存热点结果。最后才分库分表，因为分表会引入分布式事务、跨片查询、扩容和迁移成本。

**验证**：每做一项都用生产同等数据量压测，记录 P99、QPS、IOPS、buffer pool 命中率和备份/DDL 时间；没有数据的“优化”不算完成。

---

### 14. 备份恢复演练验收清单

```text
□ 在空环境从备份恢复成功
□ 恢复耗时满足 RTO（例如 < 30 分钟）
□ 表数量、行数、索引、字符集与源库一致
□ 关键业务汇总一致（账户总额、订单数、流水总额）
□ 随机抽样 100 行逐字段一致
□ 应用能连接恢复库并跑通核心接口
□ 恢复后的复制/备份任务正常
□ 记录备份文件、MySQL 版本、恢复命令、开始/结束时间、执行人
□ 演练中发现的问题有负责人和截止时间
```

命令示例：

```bash
# 逻辑备份
mysqldump --single-transaction --routines --triggers minicoin > minicoin.sql

# 恢复到新库
mysql -e 'CREATE DATABASE minicoin_restore'
mysql minicoin_restore < minicoin.sql

# 行数校验
mysql -N -e "SELECT COUNT(*) FROM minicoin.account"
mysql -N -e "SELECT COUNT(*) FROM minicoin_restore.account"
```

**失败场景**：备份文件损坏、字符集不一致、恢复后自增 ID 冲突、增量 binlog 没应用。演练必须真的恢复到空环境，不能只在原库 `SELECT` 一下备份文件。

</details>
