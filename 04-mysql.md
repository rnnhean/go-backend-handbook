# 第 4 章 MySQL 实战：建表、索引、迁移、刷数据

> 目标：能独立设计一张业务表，写出走索引的查询，安全地给已有大表加字段/改类型，写一个可中断可重跑的刷数据脚本，并知道单表容量的边界。
>
> 本章所有 SQL 都可以在本地 MySQL 8.0 执行。示例库叫 `minicoin`，金额单位是分。

---

## 4.1 建表：每一列都在表达业务规则

先看三张表：

```sql
CREATE TABLE account (
  uid         VARCHAR(32)  NOT NULL,
  balance     BIGINT       NOT NULL DEFAULT 0,
  status      TINYINT      NOT NULL DEFAULT 1 COMMENT '1正常 2冻结',
  version     BIGINT       NOT NULL DEFAULT 0,
  created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
                ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE recharge_order (
  id          BIGINT       NOT NULL AUTO_INCREMENT,
  req_id      VARCHAR(64)  NOT NULL COMMENT '客户端幂等键',
  uid         VARCHAR(32)  NOT NULL,
  amount      BIGINT       NOT NULL COMMENT '分',
  channel     VARCHAR(16)  NOT NULL,
  status      TINYINT      NOT NULL COMMENT '0待支付 1已支付 2失败 3已退款',
  created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
                ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_req_id (req_id),
  KEY idx_uid_created (uid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE recharge_flow (
  id          BIGINT       NOT NULL AUTO_INCREMENT,
  order_id    BIGINT       NOT NULL,
  uid         VARCHAR(32)  NOT NULL,
  amount      BIGINT       NOT NULL,
  balance_after BIGINT     NOT NULL,
  created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_order (order_id),
  KEY idx_uid_created (uid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 设计原则逐条解释

**1. 主键用自增 BIGINT，不用业务字段**

业务字段会变（手机号、邮箱），而且随机字符串做主键会让 InnoDB 聚簇索引频繁页分裂。`BIGINT AUTO_INCREMENT` 顺序写入，性能稳定。

如果必须用 UUID：

- 用 `BINARY(16)` 而不是 `CHAR(36)`，节省空间；
- 用有序 UUID（UUIDv7）避免随机写；
- 或者用自增主键 + 业务唯一索引。

**2. 金额用 BIGINT 存分**

```sql
-- 错
amount DECIMAL(10,2)  -- 可以，但运算要小心
amount DOUBLE         -- 绝对不要，浮点误差
-- 对
amount BIGINT         -- 单位分，应用层保证换算
```

**3. 每个字段 NOT NULL + 默认值**

`NULL` 参与索引和比较时有额外语义，容易写出 `!= NULL` 这种错误。能不给 NULL 就不给。时间字段用 `DATETIME(3)` 保留毫秒，跨时区业务可考虑 `TIMESTAMP` 或统一存 UTC。

**4. 唯一约束放在数据库**

```sql
UNIQUE KEY uk_req_id (req_id)
```

应用层“先查再插”在并发下会同时查到不存在，然后都插入。唯一索引由数据库原子保证，只有一个成功。

**5. 索引按查询建，不按字段建**

```sql
KEY idx_uid_created (uid, created_at)
```

支持 `WHERE uid = ? ORDER BY created_at DESC`。如果只建 `(created_at)`，按用户查还是要扫描大量数据。

**6. 字符集统一 utf8mb4**

emoji 和部分中文需要 4 字节，`utf8`（MySQL 的 utf8mb3）存不下。排序规则按业务选，不要混用。

**7. 大字段拆表**

文章正文、图片二进制、JSON 大对象不要和频繁查询的列表字段放同一张表。行太大导致页分裂、缓存命中率下降、备份变慢。

---

## 4.2 索引：B+Tree 到底怎么工作

InnoDB 的主键索引是聚簇索引：**叶子节点存整行数据**。二级索引叶子存“索引列 + 主键值”。

```sql
CREATE INDEX idx_uid_created ON recharge_order(uid, created_at);
```

查询：

```sql
SELECT id, amount, status
FROM recharge_order
WHERE uid = 'u-1001'
ORDER BY created_at DESC
LIMIT 20;
```

执行过程：

1. 在 `idx_uid_created` 找到 `uid='u-1001'` 的区间；
2. 区间内按 `created_at` 有序，直接取最后 20 条；
3. 每条拿到主键 `id`，回表到聚簇索引取 `amount/status`；
4. 返回。

如果索引包含所有需要的列（覆盖索引），第 3 步可以省掉：

```sql
CREATE INDEX idx_uid_created_cover ON recharge_order(uid, created_at, amount, status);
```

### 最左前缀

联合索引 `(uid, created_at)` 可以高效支持 `WHERE uid = ? ORDER BY created_at`，但单独按 `created_at` 查询通常用不上它。原因不是 MySQL“挑剔”，而是索引按第一列、第二列依次排序；没有第一列时无法直接定位有序区间。设计索引前先列出真实查询条件，而不是看到字段就建索引。

索引 `(a, b, c)` 能支持：

- `WHERE a = ?`
- `WHERE a = ? AND b = ?`
- `WHERE a = ? AND b = ? AND c = ?`
- `WHERE a = ? ORDER BY b`

不能高效支持：

- `WHERE b = ?`（跳过 a）
- `WHERE a = ? AND c = ?`（c 用不上，但 a 能用）
- `WHERE a > ? AND b = ?`（范围后 b 通常用不上）

### 什么时候不该建索引

索引会加速读，但会增加写入、存储和 DDL 成本。低选择性字段、写多读少的小表、频繁更新的列和已有联合索引覆盖的前缀，通常不值得单独建索引。

- 表很小（几百行），全表扫描更快；
- 区分度极低的列（性别、状态只有几个值），单独建索引收益低；
- 写多读少的表，每个索引都增加写入成本；
- 已经有能覆盖该查询的联合索引，不要重复建。

### 用 EXPLAIN 验证，不要猜

```sql
EXPLAIN ANALYZE
SELECT id, amount FROM recharge_order
WHERE uid = 'u-1001' AND created_at >= '2026-11-01'
ORDER BY created_at DESC LIMIT 20;
```

重点看：

| 列 | 关注点 |
|---|---|
| `type` | `ALL` 全表扫描、`index` 全索引扫描、`range` 范围、`ref` 等值、`const` 主键/唯一 |
| `key` | 实际用了哪个索引；`NULL` 表示没用到 |
| `rows` | 预估扫描行数，越大越危险 |
| `filtered` | 过滤后剩余比例 |
| `Extra` | `Using filesort`、`Using temporary`、`Using index`（覆盖） |

`EXPLAIN` 是预估，`EXPLAIN ANALYZE` 会真实执行并给出实际耗时/行数。线上大查询不要随便跑 `ANALYZE`。

### 深分页

```sql
-- 越往后越慢：要扫描并丢弃前 100000 行
SELECT * FROM recharge_order
WHERE uid = 'u-1001'
ORDER BY created_at DESC
LIMIT 100000, 20;
```

优化一：记住上一页的位置（游标分页）：

```sql
SELECT id, amount, created_at
FROM recharge_order
WHERE uid = 'u-1001'
  AND (created_at, id) < ('2026-11-01 10:00:00', 123456)
ORDER BY created_at DESC, id DESC
LIMIT 20;
```

优化二：延迟关联，先用覆盖索引拿主键，再回表：

```sql
SELECT o.*
FROM recharge_order o
JOIN (
  SELECT id FROM recharge_order
  WHERE uid = 'u-1001'
  ORDER BY created_at DESC
  LIMIT 100000, 20
) t ON o.id = t.id;
```

产品上更推荐游标分页，避免“页码越深越慢”。

### 索引失效的常见写法

```sql
-- 函数包裹列：索引失效
WHERE DATE(created_at) = '2026-11-01'
-- 改成范围
WHERE created_at >= '2026-11-01' AND created_at < '2026-11-02'

-- 前导模糊：索引失效
WHERE name LIKE '%demo%'
-- 需求如果是搜索，交给 ES；前缀匹配可用 LIKE 'demo%'

-- 隐式类型转换
WHERE uid = 1001          -- uid 是 VARCHAR，可能触发转换
WHERE uid = '1001'

-- OR 两边字段没索引
WHERE uid = 'u-1' OR channel = 'mockpay'
```

---

## 4.3 给已有表加字段：先判断会不会锁表

线上 `ALTER TABLE` 的风险来自三件事：是否重建表、是否需要拿元数据锁、执行时间是否超过业务容忍窗口。先查 MySQL 版本和表的行数/大小，再用 `ALGORITHM`、`LOCK` 显式声明预期；不要让数据库悄悄选择 COPY。大表还要评估主从延迟、磁盘余量和回滚窗口。

MySQL 8.0 的 `ALTER TABLE` 有三种算法：

| 算法 | 含义 | 是否锁写 |
|---|---|---|
| `INSTANT` | 只改元数据 | 几乎不锁 |
| `INPLACE` | 重建/修改，允许并发 DML | 通常不锁写，但占 IO |
| `COPY` | 复制整表到新表 | 锁写，最慢 |

### 加可空字段/带默认值的字段

```sql
ALTER TABLE recharge_order
  ADD COLUMN client_ip VARCHAR(64) NULL,
  ALGORITHM=INSTANT;
```

MySQL 8.0.12+ 对“在末尾加列”等操作支持 INSTANT。执行前用：

```sql
ALTER TABLE recharge_order
  ADD COLUMN client_ip VARCHAR(64) NULL,
  ALGORITHM=INSTANT, LOCK=NONE;
```

如果报错“ALGORITHM=INSTANT is not supported”，说明这个操作不支持，需要评估 INPLACE/COPY，或者用 gh-ost/pt-online-schema-change。

### 加非空且无默认值的字段

```sql
-- 错：已有行没有值，会失败或填默认值，可能锁表
ALTER TABLE recharge_order ADD COLUMN region VARCHAR(16) NOT NULL;
```

正确做法：

```sql
-- 1. 先加可空列
ALTER TABLE recharge_order ADD COLUMN region VARCHAR(16) NULL, ALGORITHM=INSTANT;

-- 2. 分批回填
UPDATE recharge_order SET region = 'unknown'
WHERE region IS NULL AND id BETWEEN ? AND ? LIMIT 1000;

-- 3. 确认无 NULL 后加约束
ALTER TABLE recharge_order MODIFY region VARCHAR(16) NOT NULL DEFAULT 'unknown';
```

### 改字段类型：扩展-收缩

直接把 `VARCHAR(32)` 改成 `VARCHAR(64)` 通常是 INPLACE，但把 `VARCHAR` 改成 `INT`、缩短长度、改字符集可能重建表。

安全流程：

```sql
-- 阶段 1：加新列，双写
ALTER TABLE account ADD COLUMN balance_v2 BIGINT NULL;
-- 应用同时写 balance 和 balance_v2
-- 回填历史数据

-- 阶段 2：校验一致性
SELECT COUNT(*) FROM account WHERE balance <> balance_v2;

-- 阶段 3：切读新列
-- 阶段 4：停止写旧列，观察
-- 阶段 5：删除旧列（低峰，确认无回滚需求）
```

### 加索引

```sql
ALTER TABLE recharge_order
  ADD INDEX idx_channel_created (channel, created_at),
  ALGORITHM=INPLACE, LOCK=NONE;
```

大表加索引仍会消耗大量 IO 和磁盘（需要额外空间），要在低峰执行并监控。MySQL 8.0 支持 Online DDL，但不等于零成本。

---

## 4.4 迁移管理：脚本要可重复、可追溯

迁移不是“上线时执行一段 SQL”，而是代码、数据和环境的版本契约。脚本必须只追加、可在新库从零执行、在每个环境有记录，并与应用发布顺序配合。大表变更通常拆成“扩展结构 → 双写 → 回填 → 切读 → 清理旧结构”，不能期待一条迁移完成所有事。

推荐目录：

```
migrations/
  0001_create_account.up.sql
  0001_create_account.down.sql
  0002_add_region.up.sql
  0002_add_region.down.sql
```

规则：

1. **只追加，不改历史脚本**：已经执行过的迁移不能改，否则不同环境结构不一致；
2. **每个迁移有 up/down**：down 可以只是注释说明“不可逆”，但要有交代；
3. **迁移和代码分开部署**：先加字段/双写，再发代码，再删旧字段；
4. **执行前备份/确认**：大表变更先看执行计划；
5. **记录版本**：用 `schema_migrations` 表记录已执行版本。

```sql
CREATE TABLE schema_migrations (
  version VARCHAR(64) PRIMARY KEY,
  applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
);
```

Go 里可以用 `golang-migrate`、`goose` 或团队自研工具。不要用 `AutoMigrate` 在生产大表上跑不可控的 DDL。

---

## 4.5 刷数据：分批、限速、可中断、可重跑

刷数据的目标不是尽快改完，而是在可控压力下把错误数据修正，并且失败后能继续。每批按主键推进、记录断点、限制 QPS、支持 dry-run，更新条件带幂等谓词；执行中观察锁等待、复制延迟和磁盘增长。任何“一条 SQL 更新千万行”的方案都不应直接上生产。

错误写法：

```sql
-- 1000 万行一次更新：长事务、锁大量行、binlog 暴涨、主从延迟
UPDATE recharge_order SET region = 'unknown' WHERE region IS NULL;
```

正确脚本骨架（Go）：

```go
func backfillRegion(ctx context.Context, db *sql.DB) error {
    const batch = 1000
    var lastID int64

    for {
        rows, err := db.QueryContext(ctx, `
            SELECT id FROM recharge_order
            WHERE region IS NULL AND id > ?
            ORDER BY id LIMIT ?`, lastID, batch)
        if err != nil { return err }

        var ids []int64
        for rows.Next() {
            var id int64
            if err := rows.Scan(&id); err != nil { rows.Close(); return err }
            ids = append(ids, id)
        }
        rows.Close()
        if len(ids) == 0 { return nil }

        // 用主键批量更新，避免长事务
        if _, err := db.ExecContext(ctx, `
            UPDATE recharge_order SET region = 'unknown'
            WHERE id >= ? AND id <= ? AND region IS NULL`,
            ids[0], ids[len(ids)-1]); err != nil {
            return err
        }

        lastID = ids[len(ids)-1]
        log.Printf("backfilled up to id=%d", lastID)

        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(50 * time.Millisecond): // 限速
        }
    }
}
```

要点：

- 按主键递增，避免 `OFFSET` 越翻越慢；
- 每批 500～2000 行，按数据库压力调整；
- 记录 `lastID`，失败可从断点继续；
- 更新条件带 `region IS NULL`，重复执行安全；
- 低峰执行，观察 CPU、IO、锁等待、主从延迟；
- 大表变更前确认 binlog 保留时间足够。

---

## 4.6 查询怎么不影响线上

查询性能风险来自扫描行数、锁持有时间、临时表和并发度。上线前用 `EXPLAIN` 和真实数据量验证，给查询设置超时与 LIMIT；线上保护包括只读副本、连接池上限、慢查询阈值和异常时熔断。不要只看“开发库执行很快”，开发库的数据量和缓存命中率通常不代表生产。

### 上线前的检查

```sql
EXPLAIN ANALYZE SELECT ...;
```

看扫描行数、是否 filesort、是否临时表。扫描行数超过几万就要警惕。

### 线上保护

线上执行 SQL 前要设置超时、限制扫描行数和并发，并优先在只读副本或影子库验证。批量更新要分批提交，观察主从延迟、锁等待、CPU 和磁盘；出现锁等待应立即暂停任务，而不是把批次调大硬冲。

- 所有查询带 `LIMIT`；
- 禁止 `SELECT *` 到大字段；
- 报表/导出走只读从库或离线库；
- 设置语句超时：`SET SESSION max_execution_time = 2000`；
- 连接池限制最大连接数；
- 慢查询日志 + 告警。

### 慢查询排查

```sql
-- 开启慢查询
SET GLOBAL slow_query_log = ON;
SET GLOBAL long_query_time = 0.2;

-- 看当前连接
SHOW PROCESSLIST;

-- 看 InnoDB 锁等待
SELECT * FROM performance_schema.data_lock_waits;
```

Go 侧给每个查询带 context 超时：

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
rows, err := db.QueryContext(ctx, query, args...)
```

---

## 4.7 一张表最多能放多少数据

理论极限和实际可用是两回事。

| 限制 | 数值 | 说明 |
|---|---|---|
| InnoDB 表空间 | 64TB | 单表理论上限 |
| BIGINT 主键 | 约 922 亿亿 | 实际不会先用完 |
| 单索引键长度 | 3072 字节（DYNAMIC） | 联合索引总长度受限 |
| 行大小 | 约 8KB（不含 TEXT/BLOB 页外存储） | 列太多会报错 |
| 实际建议 | 单表 1000 万～1 亿行 | 取决于行宽、索引、查询 |

真正决定“能不能放”的不是行数，而是：

- 索引是否还能放进内存；
- 查询扫描行数是否可控；
- DDL/备份/恢复时间是否可接受；
- 单实例写入是否到瓶颈。

### 什么时候分区/分表

分区和分表解决的是“单表数据量导致索引、DDL、备份或热点不可控”，不是解决所有慢查询。先证明瓶颈来自数据量或单表写热点，再按时间、用户或业务键设计；如果只是缺索引、SQL 写得差，分表只会增加复杂度。

- 单表超过几亿且查询只按时间范围：按时间分区；
- 写入到单实例瓶颈：分库分表；
- 历史数据访问少：归档到冷存储；
- 先做索引优化、读写分离、归档，再考虑分片。

分表键要按查询设计。按 `uid` 分片后，按 `order_id` 查询需要额外路由表或基因法。

---

## 4.8 事务、隔离级别与锁

### ACID

ACID 是事务的四个保证：原子性保证失败回滚，一致性保证约束和业务不变量，隔离性规定并发事务互相看见什么，持久性保证提交后不丢。不同数据库和隔离级别的具体行为不同，不能只背字母；例如 MySQL InnoDB 的默认隔离级别是 REPEATABLE READ。

- **Atomicity**：要么全成功，要么全失败；
- **Consistency**：约束不被破坏；
- **Isolation**：并发事务互不干扰；
- **Durability**：提交后即使宕机也不丢。

### 隔离级别

隔离级别是在并发正确性和性能之间做取舍。级别越高，脏读、不可重复读和幻读的风险越低，但锁和版本检查成本可能越高。选级别要结合业务不变量：资金扣减常用唯一约束、行锁或条件更新兜底，不能只依赖“默认级别看起来没问题”。

| 级别 | 脏读 | 不可重复读 | 幻读 |
|---|---|---|---|
| READ UNCOMMITTED | 可能 | 可能 | 可能 |
| READ COMMITTED | 否 | 可能 | 可能 |
| REPEATABLE READ（MySQL 默认） | 否 | 否 | InnoDB 用间隙锁基本避免 |
| SERIALIZABLE | 否 | 否 | 否 |

### 行锁与间隙锁

```sql
-- 事务 A
START TRANSACTION;
SELECT * FROM account WHERE uid = 'u-1001' FOR UPDATE; -- 锁住这一行
-- 事务 B 更新同一行会等待
```

没有索引的 `FOR UPDATE` 可能锁大量行甚至全表。**锁的范围取决于索引。**

### 死锁

```
事务 A：锁行 1 → 请求行 2
事务 B：锁行 2 → 请求行 1
```

排查：

```sql
SHOW ENGINE INNODB STATUS\G
```

预防：统一加锁顺序、缩短事务、不要在事务里做远程调用。

---

## 4.9 零停机改字段：完整流程

零停机不是“执行一条 ALTER 不报错”，而是让旧代码、新代码和数据库结构在一个发布窗口里同时兼容。正确流程通常是先加兼容结构、双写或回填、切读、观察、再删除旧结构；每一步都要有回滚方案。

以把 `status TINYINT` 改成更清晰的 `status VARCHAR(16)` 为例：

1. 加新列 `status_v2 VARCHAR(16) NULL`（INSTANT）；
2. 应用双写旧列和新列；
3. 分批回填历史数据；
4. 校验：`SELECT COUNT(*) WHERE status_v2 IS NULL`；
5. 灰度切换读新列；
6. 观察一个发布周期；
7. 停止写旧列；
8. 低峰删除旧列（确认回滚窗口已过）。

每一步都可独立回滚，避免“一次 ALTER 改完，出问题只能恢复备份”。

---

## 4.10 连接池

Go 的 `database/sql` 是连接池：

```go
db.SetMaxOpenConns(50)          // 最大打开连接数
db.SetMaxIdleConns(25)          // 最大空闲连接
db.SetConnMaxLifetime(30 * time.Minute)
db.SetConnMaxIdleTime(5 * time.Minute)
```

原则：

- `MaxOpenConns` 要小于数据库 `max_connections` 按实例均分后的值；
- 连接不是越多越快，过多会导致数据库上下文切换和锁竞争；
- 设置生命周期，避免网络中间设备断开旧连接；
- 监控 `db.Stats()` 的 `WaitCount`、`WaitDuration`，等待高说明池太小或查询太慢。

---

## 习题

### 基础题

1. 为什么主键推荐自增 BIGINT？UUID 做主键有什么问题？
2. 金额为什么不用 FLOAT/DOUBLE？
3. 联合索引 `(uid, created_at)` 能支持哪些查询？不能支持哪些？
4. `EXPLAIN` 的 `type=ALL` 和 `Extra=Using filesort` 分别意味着什么？
5. 深分页为什么慢？写出游标分页的 SQL。
6. `ALGORITHM=INSTANT`、`INPLACE`、`COPY` 的区别是什么？
7. 为什么迁移脚本执行后不能再修改？
8. 刷数据脚本为什么要按主键分批并记录断点？
9. 单表行数到多少需要分表？决定因素是什么？
10. 没有索引的 `SELECT ... FOR UPDATE` 会有什么风险？

### 动手题

11. 给“评论表”设计 schema：支持按文章查评论、按用户查评论、软删除、分页。写出索引并解释。
12. 写一条 SQL，删除 `recharge_order` 中 2025 年之前、状态为失败的订单，每次最多 1000 条，可重复执行。
13. 用 `EXPLAIN` 分析一条慢查询，写出你会看的字段和优化步骤。
14. 写一个 Go 刷数据脚本，要求支持 `-batch`、`-sleep`、`-dry-run` 参数。

### 综合题

15. 一张 8000 万行的订单表要加 `region VARCHAR(16) NOT NULL DEFAULT 'unknown'`。设计完整的低风险变更方案，包含 DDL、双写、回填、验证和回滚。

---

## 参考答案

<details>
<summary>展开参考答案</summary>

### 1. 为什么主键推荐自增 BIGINT

InnoDB 的主键是聚簇索引，行数据按主键顺序存放。自增 BIGINT 让新行总在索引尾部追加，页分裂少、写入局部性好。

UUIDv4 是随机的，插入位置在整棵树里乱跳：每次都可能命中不同的页，脏页和随机 IO 变多，索引也更占空间（字符串 36 字节 vs 8 字节）。

```sql
-- 推荐
id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY

-- 必须用 UUID 时：二进制存储 + 有序生成
id BINARY(16) NOT NULL PRIMARY KEY   -- 应用写入 UUIDv7
```

**验证**：用 `sysbench` 或自写脚本分别向两张表各插 100 万行，对比耗时、表大小和 `Innodb_buffer_pool_pages_dirty`。不要只凭“UUID 慢”这句话下结论，数据分布和磁盘才是决定因素。

注意：自增主键会暴露业务量（可被爬虫估算订单数），这不是选型的第一因素；需要隐藏时用外部 ID（如 `order_no`）单独对外，不要为此换成随机主键。

---

### 2. 金额为什么不用 FLOAT/DOUBLE

二进制浮点无法精确表示 `0.1`、`0.2` 这类十进制小数，累加会产生误差：

```sql
SELECT 0.1 + 0.2 = 0.3;   -- 返回 0
```

金额有三种可靠做法：

```sql
-- A. 最小货币单位整数（推荐）
amount_cent BIGINT NOT NULL CHECK (amount_cent > 0)

-- B. 定点小数
amount DECIMAL(18,2) NOT NULL CHECK (amount > 0)

-- C. 高精度货币代码 + 定点
amount DECIMAL(20,4) NOT NULL
```

**验证**：

```sql
CREATE TABLE t_float (v DOUBLE);
INSERT INTO t_float VALUES (0.1),(0.2);
SELECT SUM(v) = 0.3 FROM t_float;   -- 0

CREATE TABLE t_dec (v DECIMAL(10,2));
INSERT INTO t_dec VALUES (0.1),(0.2);
SELECT SUM(v) = 0.3 FROM t_dec;     -- 1
```

Go 侧对应 `int64` 或 `decimal.Decimal`，不要用 `float64` 接金额。接口字段名写 `amount_cent`，让单位在类型系统外也可见。

---

### 3. 联合索引 `(uid, created_at)`

最左前缀原则：索引先按 `uid` 排，再按 `created_at` 排。

| 查询 | 能否用索引 |
|---|---|
| `WHERE uid=?` | 能，用到 `uid` |
| `WHERE uid=? ORDER BY created_at DESC` | 能，且避免排序 |
| `WHERE uid=? AND created_at >= ?` | 能，两列都用 |
| `WHERE created_at >= ?` | 不能（跳过了最左列） |
| `WHERE uid=? AND status=?` | 只能用到 `uid`，`status` 需回表过滤 |

```sql
-- 验证
EXPLAIN SELECT id FROM recharge_order
WHERE uid = 'u-1001' AND created_at >= '2026-01-01'
ORDER BY created_at DESC LIMIT 20;
```

预期 `key` 为 `idx_uid_created`，`Extra` 不含 `Using filesort`。如果出现了，检查是不是 `ORDER BY created_at, id` 而索引里没有 `id`。

---

### 4. `type=ALL` 和 `Using filesort`

`type=ALL`：全表扫描，没有可用索引或优化器认为全表更快。百万行表上出现它，先看 `rows` 和 `filtered`。

`Using filesort`：无法利用索引顺序完成 `ORDER BY`，需要额外排序。注意它不一定写磁盘，但会消耗内存/CPU，数据量大时很慢。

```sql
EXPLAIN SELECT * FROM recharge_order
WHERE uid='u-1001' ORDER BY created_at DESC LIMIT 20;
```

正常应看到 `type=ref`、`key=idx_uid_created`、`Extra` 只有 `Using index condition` 之类；若出现 `Using filesort`，建立 `(uid, created_at)`。

**排查顺序**：先看 `key` 是否为空 → 再看 `rows` 是否接近全表 → 再看 `Extra` 是否有 `Using temporary; Using filesort` → 最后才考虑加索引。加索引前用 `EXPLAIN ANALYZE`（MySQL 8.0.18+）确认真实耗时分布。

---

### 5. 深分页与游标分页

`LIMIT 1000000, 20` 需要先读出并丢弃前 100 万行，越翻越慢，而且期间有新数据插入会导致重复/漏行。

```sql
-- 慢
SELECT id, uid, amount_cent, created_at
FROM recharge_order
WHERE uid = 'u-1001'
ORDER BY created_at DESC, id DESC
LIMIT 1000000, 20;
```

游标分页：把上一页最后一行作为起点。

```sql
-- 第一页
SELECT id, uid, amount_cent, created_at
FROM recharge_order
WHERE uid = 'u-1001'
ORDER BY created_at DESC, id DESC
LIMIT 20;

-- 下一页：上一页最后一条是 (created_at='2026-10-01 10:00:00', id=9001)
SELECT id, uid, amount_cent, created_at
FROM recharge_order
WHERE uid = 'u-1001'
  AND (created_at, id) < ('2026-10-01 10:00:00', 9001)
ORDER BY created_at DESC, id DESC
LIMIT 20;
```

索引要匹配排序：`KEY idx_uid_created_id (uid, created_at, id)`。

**验证**：对 100 万行数据分别执行两种 SQL，用 `EXPLAIN ANALYZE` 对比 `actual time`；游标分页应该稳定在毫秒级，不随页码增长。

---

### 6. `ALGORITHM=INSTANT / INPLACE / COPY`

| 算法 | 做什么 | 是否重建表 | 并发 DML | 适用 |
|---|---|---|---|---|
| INSTANT | 只改元数据 | 否 | 允许 | 加列（MySQL 8.0 部分场景）、改默认值 |
| INPLACE | 原地重建/整理 | 可能 | 通常允许（有 MDL） | 加索引、部分改列 |
| COPY | 复制整表到新表 | 是 | 通常阻塞写 | 老版本、复杂类型变更 |

```sql
ALTER TABLE recharge_order
  ADD COLUMN region VARCHAR(16) NOT NULL DEFAULT 'unknown',
  ALGORITHM=INSTANT;
```

如果 MySQL 返回 `ALGORITHM=INSTANT is not supported`，说明版本或列位置不支持，不要硬改成 COPY 在生产高峰执行；改走“可空列 + 双写 + 回填 + 加约束”的扩展-收缩流程。

**验证**：执行前后查 `SHOW CREATE TABLE`、`SELECT COUNT(*)`、`SHOW PROCESSLIST`，观察 MDL 等待和主从延迟。DDL 期间用另一个会话持续写入，确认没有长时间阻塞。

---

### 7. 为什么迁移脚本执行后不能再修改

迁移是按顺序、一次性应用的历史记录。已经执行过的脚本再改内容，会导致：

- 老环境执行的是旧版本，新环境执行的是新版本，结构不一致；
- 无法根据迁移记录重现数据库；
- 回滚和审计失去依据。

正确做法是新增一个迁移文件：

```text
migrations/
  001_create_account.sql
  002_create_recharge_order.sql
  003_add_channel.sql          -- 已执行，永不修改
  004_add_region.sql           -- 新变更
```

**验证**：CI 里用空库从头执行全部迁移，再与目标环境 `SHOW CREATE TABLE` 做 diff；任何差异都说明有人改过历史脚本。

---

### 8. 刷数据为什么要分批 + 断点

一次性 `UPDATE` 几百万行会：

- 产生长事务，undo log 膨胀，锁大量行；
- 主从延迟飙升；
- binlog 过大；
- 失败后无法知道做到哪里。

分批模板：

```sql
-- 每批 1000 行，按主键推进
UPDATE recharge_order
SET region = 'unknown'
WHERE id > ? AND id <= ?
  AND region IS NULL
LIMIT 1000;
```

断点表：

```sql
CREATE TABLE data_backfill_job (
  job_name VARCHAR(64) PRIMARY KEY,
  last_id BIGINT NOT NULL DEFAULT 0,
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
);
```

每批提交后更新 `last_id`，失败后从断点继续；脚本必须可重复执行（`WHERE region IS NULL` 保证幂等）。

**验证**：中途 `kill` 脚本，重新运行应从中断处继续，最终 `SELECT COUNT(*) WHERE region IS NULL` 为 0，且总额/行数与源数据一致。

---

### 9. 单表多少数据要分表

没有统一数字。评估因素：

| 因素 | 说明 |
|---|---|
| 索引是否能放进内存 | 热点索引超出 buffer pool，随机 IO 上升 |
| 单表 B+ 树高度 | 千万级通常 3～4 层，影响不大 |
| DDL/备份/恢复时间 | 大表改结构、备份恢复窗口是否可接受 |
| 写入热点 | 自增主键尾部竞争、唯一索引冲突 |
| 查询模式 | 是否能按用户/时间天然分片 |

常见经验是 **千万到亿级开始评估**，但 1000 万行的窄表可能很健康，500 万行的宽表+多个索引也可能很慢。先做归档、索引优化、读写分离、冷热分离，最后才分库分表。

**验证**：用生产同等数据量做压测，观察 P99、buffer pool 命中率、磁盘 IOPS 和备份耗时；用这些数据写容量报告，而不是拍脑袋。

---

### 10. 无索引的 `SELECT ... FOR UPDATE`

InnoDB 在没有可用索引时会扫描并锁定大量行（取决于隔离级别和索引条件），可能锁住整张表范围内的记录，阻塞其他事务，甚至死锁。

```sql
-- 危险：status 无索引
BEGIN;
SELECT * FROM recharge_order WHERE status = 0 FOR UPDATE;
-- 其他事务的写入可能全部等待
```

**验证**：开两个会话，会话 A 执行上面的语句不提交；会话 B 插入一条 `status=1` 的订单，观察 B 是否被阻塞。再给 `status` 加索引后重复，阻塞范围应缩小。

原则：`FOR UPDATE` 的 `WHERE` 必须能走索引，事务要短，锁住后不要调用远程服务。

---

### 11. 评论表 schema

```sql
CREATE TABLE article_comment (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  article_id BIGINT UNSIGNED NOT NULL,
  uid VARCHAR(32) NOT NULL,
  parent_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  content VARCHAR(2000) NOT NULL,
  status TINYINT NOT NULL DEFAULT 0 COMMENT '0正常 1隐藏 2删除',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  KEY idx_article_created (article_id, deleted_at, created_at, id),
  KEY idx_uid_created (uid, deleted_at, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

对应查询：

```sql
-- 按文章分页（游标）
SELECT id, uid, content, created_at
FROM article_comment
WHERE article_id = ? AND deleted_at IS NULL
  AND (created_at, id) < (?, ?)
ORDER BY created_at DESC, id DESC
LIMIT 20;

-- 按用户查
SELECT id, article_id, content, created_at
FROM article_comment
WHERE uid = ? AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 20;
```

**设计说明**：

- 软删除用 `deleted_at`，查询必须显式带 `IS NULL`；索引里把 `deleted_at` 放第二列，能同时服务“未删除”的过滤；
- `content` 用 `VARCHAR(2000)` 而不是 `TEXT`，限制长度并避免大字段影响行外存储；
- 回复用 `parent_id`，一级评论列表查询时加 `parent_id=0` 可再建 `(article_id, parent_id, created_at)`；
- 验证：`EXPLAIN` 两条查询都应命中对应索引，且 `Extra` 不含 `Using filesort`。

---

### 12. 分批删除历史失败订单

```sql
DELETE FROM recharge_order
WHERE status = 2
  AND created_at < '2025-01-01'
ORDER BY id
LIMIT 1000;
```

循环执行直到 `ROW_COUNT() = 0`，每批 sleep 100ms：

```bash
while true; do
  mysql -N -e "
    DELETE FROM recharge_order
    WHERE status=2 AND created_at < '2025-01-01'
    ORDER BY id LIMIT 1000;
    SELECT ROW_COUNT();
  " minicoin > /tmp/deleted.txt
  n=$(tail -1 /tmp/deleted.txt)
  echo "deleted=$n"
  [ "$n" -eq 0 ] && break
  sleep 0.1
done
```

**可重复执行**：删除条件固定，已删除的行不会再次出现。**验证**：执行前后统计满足条件的行数、表大小、主从延迟；最终剩余应为 0，且业务查询 P99 没有明显上升。

---

### 13. `EXPLAIN` 分析慢查询

```sql
EXPLAIN ANALYZE
SELECT id, uid, amount_cent, created_at
FROM recharge_order
WHERE uid = 'u-1001'
  AND status = 1
ORDER BY created_at DESC
LIMIT 20;
```

按顺序看：

| 字段 | 关注点 |
|---|---|
| `type` | `ALL`/`index` 要警惕，`ref`/`range` 较好 |
| `possible_keys` / `key` | 是否选中预期索引 |
| `rows` | 预估扫描行数是否过大 |
| `filtered` | 过滤后比例，低说明索引选择性差 |
| `Extra` | `Using filesort`、`Using temporary`、`Using index condition` |
| `actual time`（ANALYZE） | 真实耗时和循环次数 |

优化步骤：

1. 确认 `WHERE`、`ORDER BY`、`LIMIT` 与索引列顺序匹配；
2. 用 `SHOW INDEX FROM table` 看基数和列顺序；
3. 考虑覆盖索引减少回表；
4. 改写 SQL（避免函数包裹索引列、避免 `%前缀` 模糊查询）；
5. 再 `EXPLAIN ANALYZE` 对比，最后在预发压测。

**失败场景**：`WHERE DATE(created_at) = '2026-01-01'` 会让索引失效，改成 `created_at >= '2026-01-01' AND created_at < '2026-01-02'`。

---

### 14. Go 刷数据脚本

```go
package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN")
	batch := flag.Int("batch", 1000, "rows per batch")
	sleep := flag.Duration("sleep", 100*time.Millisecond, "sleep between batches")
	dryRun := flag.Bool("dry-run", false, "only count rows")
	flag.Parse()

	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	ctx := context.Background()
	var lastID int64

	for {
		if *dryRun {
			var n int64
			if err := db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM recharge_order WHERE region IS NULL").Scan(&n); err != nil {
				log.Fatal(err)
			}
			log.Printf("dry-run: %d rows need backfill", n)
			return
		}

		res, err := db.ExecContext(ctx, `
			UPDATE recharge_order
			SET region = 'unknown'
			WHERE id > ? AND region IS NULL
			ORDER BY id
			LIMIT ?`, lastID, *batch)
		if err != nil {
			log.Fatalf("batch after id=%d: %v", lastID, err)
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			log.Printf("done, last_id=%d", lastID)
			return
		}

		if err := db.QueryRowContext(ctx,
			"SELECT id FROM recharge_order WHERE id > ? AND region IS NOT NULL ORDER BY id LIMIT 1",
			lastID).Scan(&lastID); err != nil {
			log.Fatalf("read last id: %v", err)
		}

		log.Printf("updated=%d last_id=%d", affected, lastID)
		time.Sleep(*sleep)
	}
}
```

运行：

```bash
go run ./cmd/backfill -dsn "$DSN" -batch 1000 -sleep 100ms -dry-run
go run ./cmd/backfill -dsn "$DSN" -batch 1000 -sleep 100ms
```

**边界**：`batch` 必须为正；`sleep` 防止打满 IO；`dry-run` 只读。生产运行前先在预发跑一遍并记录断点，失败后从 `lastID` 继续。

---

### 15. 8000 万行订单表加 `region`

目标：加 `region VARCHAR(16) NOT NULL DEFAULT 'unknown'`，不锁表、可回滚、可验证。

**第 0 步：评估与备份**

```sql
SELECT COUNT(*) FROM recharge_order;
SHOW CREATE TABLE recharge_order\G
```

记录表大小、索引、主从延迟基线，确认 MySQL 版本是否支持 `ALGORITHM=INSTANT`。在预发用同等数据量演练。

**第 1 步：加可空列（低风险）**

```sql
ALTER TABLE recharge_order
  ADD COLUMN region VARCHAR(16) NULL,
  ALGORITHM=INSTANT;
```

如果 INSTANT 不支持，使用 `pt-online-schema-change` 或 `gh-ost`，并在低峰执行。

**第 2 步：应用双写**

新代码写入订单时同时写 `region`；老代码不认识的列可忽略。发布期间新旧实例共存，保证两边都能工作。

```go
_, err := tx.ExecContext(ctx, `
  INSERT INTO recharge_order (req_id, uid, amount_cent, channel, status, region)
  VALUES (?, ?, ?, ?, ?, ?)`,
  req.ReqID, req.UID, req.AmountCent, req.Channel, statusPending, req.Region)
```

**第 3 步：分批回填**

```sql
UPDATE recharge_order
SET region = 'unknown'
WHERE id > ? AND region IS NULL
ORDER BY id
LIMIT 1000;
```

用第 14 题的脚本，记录 `last_id`，每批提交后 sleep。观察主从延迟、锁等待、IOPS。

**第 4 步：校验**

```sql
SELECT COUNT(*) FROM recharge_order WHERE region IS NULL;          -- 必须为 0
SELECT COUNT(*) FROM recharge_order WHERE region = '';             -- 业务上不允许为空串
SELECT region, COUNT(*) FROM recharge_order GROUP BY region;       -- 分布合理
```

再抽样对比新旧数据、订单总额和行数。

**第 5 步：加约束**

确认无 NULL 后：

```sql
ALTER TABLE recharge_order
  MODIFY COLUMN region VARCHAR(16) NOT NULL DEFAULT 'unknown',
  ALGORITHM=INPLACE, LOCK=NONE;
```

若 MySQL 不支持在线改，改用影子列 + 双写切换，不要在高峰硬改。

**第 6 步：灰度读新列**

先让 1% 读流量使用 `region`，对比新旧结果；确认一致后逐步放大到 100%。

**回滚方案**

- 第 1～3 步：代码停止双写，列可保留（可空不影响旧代码）；
- 第 5 步之后：先回滚应用，再评估是否把列改回可空；不要直接 `DROP COLUMN`，先观察一个发布周期；
- 数据回滚：保留回填批次记录，必要时按批次反向修正，禁止无备份全表 `UPDATE`。

**验收证据**

```text
□ 预发同等数据量演练通过
□ DDL 期间业务 P99 无显著上升，无 MDL 长等待
□ region IS NULL 计数为 0
□ 订单行数、金额汇总与变更前一致
□ 新旧版本应用都能正常读写
□ 灰度 1%→10%→50%→100% 每步有指标
□ 回滚命令在预发实际执行过
```

</details>
