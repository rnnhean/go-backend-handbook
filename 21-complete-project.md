# 第 21 章 完整实战：从需求到上线实现 mini-coin

> 这一章不是知识点索引，而是一条可以照着走完的开发主线。
> 你会从一句需求开始，依次完成 API 契约、领域模型、MySQL 表、事务、幂等、HTTP 接口、缓存、消息、测试、容器化、监控和发布。代码与表名全部属于虚构项目 `mini-coin`。

---

## 21.1 需求不是“写一个充值接口”

先把一句模糊需求改写成可以验收的行为：

> 用户可以给账户充值；同一个请求重复提交只能成功一次；金额只能为正整数分；冻结账户不能充值；成功后能查询余额；接口要能处理并发；失败要返回稳定错误码。

这里已经包含六类信息：

| 类型 | 示例 | 如果不写清楚会怎样 |
|---|---|---|
| 功能 | 充值、查余额 | 开发完才发现少功能 |
| 规则 | 金额 > 0、单位分 | 前端传元、后端按分，金额差 100 倍 |
| 幂等 | 同一 `request_id` 不重复加钱 | 用户重试导致重复充值 |
| 状态 | 冻结账户拒绝 | 业务状态被绕过 |
| 非功能 | 并发、超时、监控 | 单测通过，上线出错 |
| 可观测 | 请求 ID、错误码、指标 | 出问题只会说“接口挂了” |

### 先写验收用例

```text
GIVEN 账户 u-1001 余额 100 分，状态正常
WHEN 提交 req_id=r-1、amount_cent=50
THEN 返回余额 150 分
AND 同一 req_id 再提交一次
THEN 仍返回余额 150 分，不能变成 200 分

GIVEN 账户不存在
WHEN 充值
THEN 返回 NOT_FOUND，不创建“幽灵”订单

GIVEN 账户已冻结
WHEN 充值
THEN 返回 ACCOUNT_FROZEN，余额不变

GIVEN 20 个并发请求，每个 req_id 都不同
WHEN 同时充值 10 分
THEN 余额精确增加 200 分，不丢更新
```

**判断需求是否可开发的标准**：任何一个验收句都能被自动化测试翻译出来。如果只能靠口头解释，就还没到开发阶段。

---

## 21.2 先定 API，再写实现

### 请求

```http
POST /v1/recharges
Content-Type: application/json
Authorization: Bearer demo-token
X-Request-Id: trace-demo-001

{
  "req_id": "r-20261101-001",
  "amount_cent": 50,
  "channel": "manual"
}
```

字段约束：

| 字段 | 类型 | 规则 |
|---|---|---|
| `req_id` | string | 1~64，客户端生成，同一业务请求复用 |
| `amount_cent` | int64 | 单位分，必须 > 0，设置业务上限 |
| `channel` | string | 枚举白名单，例如 `manual`、`coupon` |

`uid` **不从 body 读取**，而是从 `Authorization: Bearer <token>` 解析后放进 context。否则客户端可以伪造别人的用户 ID。

### 成功响应

首次创建返回 `201 Created`；同一个 `req_id` 幂等命中返回 `200 OK`，响应体结构相同。

```json
{
  "code": "OK",
  "message": "success",
  "request_id": "trace-demo-001",
  "data": {
    "order_id": "90001",
    "uid": "u-1001",
    "balance": 150,
    "duplicated": false
  }
}
```

### 失败响应

```json
{
  "code": "ACCOUNT_FROZEN",
  "message": "account is frozen",
  "request_id": "trace-demo-001"
}
```

**接口设计三条硬规则**：

1. HTTP 状态码表达协议层结果，业务 `code` 表达领域结果；
2. 错误码是稳定契约，不能被日志文案随意替换；
3. 成功和失败都必须带 `request_id`，否则线上无法串联日志。

### 查询余额

```http
GET /v1/accounts/u-1001/balance
```

查询接口不应该要求传用户余额，因为余额只能由服务端算出来。**客户端传来的金额、余额、状态都不可信。**

### 为什么先把契约定下来

接口契约是前后端并行开发的分界线。契约没定时，前端按“成功后返回 balance”写页面，后端却返回 `account.balance`；前端把金额当元，后端按分处理；前端重试时生成新的请求号，后端就无法幂等。到了联调阶段，这些差异会同时爆发。

先定契约并不等于一次定死。正确流程是：先确定动作语义和最小字段，再和调用方评审错误码、幂等键、超时时间，随后把示例写进 OpenAPI 并生成 Mock。实现阶段允许内部重构，但不能悄悄改字段名、类型、枚举和错误码。任何破坏性变化都要走版本或迁移期。

契约评审时至少要问：谁生成 `req_id`，重试是否复用；金额单位和上限是什么；哪些错误可重试；客户端超时后如何查询结果；响应是否包含能关联日志的 `request_id`；空值用 `null`、缺字段还是默认值。这些问题没有答案，接口就还不能交给前端。

---

## 21.3 目录结构：让依赖方向固定

```text
mini-coin/
├── cmd/server/main.go          # 组装依赖、启动、优雅退出
├── internal/httpapi/           # HTTP 适配层
├── internal/service/           # 业务规则和事务边界
├── internal/repository/        # MySQL 实现
├── internal/cache/             # Redis/内存缓存实现
├── internal/mq/                # Kafka/RabbitMQ/内存实现
├── internal/model/             # 领域数据结构
├── migrations/                 # 数据库迁移
├── api/openapi.yaml            # HTTP 契约
└── deploy/                     # Docker/K8s 配置
```

依赖只能这样流动：

```text
main -> httpapi -> service -> repository/cache/mq interfaces
                         ^
                         |
              MySQL/Redis/MQ adapters
```

反向依赖是错误信号：`repository` 不应 import `httpapi`；`model` 不应知道 HTTP 状态码；业务层不应直接读取环境变量。

### 为什么这样分，而不是把代码都放 handler

假设所有逻辑都写在 HTTP handler 里：解析 JSON、校验金额、开事务、更新余额、删缓存、发消息、拼错误响应全挤在一个函数。最初只有充值接口时看起来很快，但很快会出现四个问题：MQ 消费者也要充值，只能复制一份逻辑；单测必须构造 HTTP 请求；数据库错误和 HTTP 状态码绑死；缓存或消息组件一换，handler 全部重写。

分层的本质不是“多建几个文件夹”，而是**让变化方向不同的代码分开**：

| 层 | 只负责 | 不应该知道 |
|---|---|---|
| HTTP handler | 解析协议、鉴权上下文、参数格式、状态码映射 | SQL、事务、MQ topic |
| service | 业务规则、幂等状态机、事务边界 | HTTP header、JSON tag |
| repository | SQL、行锁、事务、持久化 | HTTP 状态码、用户提示文案 |
| adapter | Redis/Kafka/第三方 API 的具体客户端 | 业务规则 |

依赖方向固定后，`service` 通过小接口依赖 repository。测试时传内存实现，生产时传 MySQL 实现。接口由使用方定义，通常会写成 `RechargeStore`、`Publisher` 这样只包含当前业务需要的方法，而不是把数据库所有 CRUD 暴露给 service。

一个常见反例是 service 直接 `import` 全局 `db` 单例。这样测试必须连数据库，事务边界也不可见。正确做法是在 `main` 里组装依赖：先创建数据库连接，再构造 MySQL repository，再把它传给 service。`main` 是唯一知道所有具体实现的地方，其他包只依赖抽象。

**检查目录是否合理的方法**：尝试把 HTTP 层删掉，只保留一个 CLI 调用 service，业务是否仍能工作？尝试把 MySQL 换成内存实现，service 的测试是否不用改？如果答案是否定的，说明业务逻辑还粘在协议或基础设施上。

---

## 21.4 领域模型：金额、状态和请求 ID

```go
package model

type Account struct {
    ID        int64
    UID       string
    Balance   int64 // 分
    Status    int
    Version   int64
}

const (
    AccountNormal = 1
    AccountFrozen = 2
)

type RechargeOrder struct {
    ID        int64
    ReqID     string
    UID       string
    Amount    int64
    Channel   string
    Status    string
    CreatedAt int64
}
```

### 为什么金额用 int64

`0.1 + 0.2` 在浮点数里不一定等于 `0.3`。钱的金额、余额、手续费必须使用最小货币单位整数，例如“分”；如果业务需要更细精度，使用数据库 `DECIMAL` 和明确的定点数规则。

### 为什么不能只靠自增 ID 做幂等

自增 ID 只能区分“两次插入”，不能识别“同一个业务请求被客户端重试”。业务幂等键必须由请求方提供，例如 `req_id`，并在数据库中建立唯一索引。

---

## 21.5 建表：约束必须落到数据库

```sql
CREATE TABLE account (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    uid         VARCHAR(64) NOT NULL,
    balance     BIGINT NOT NULL DEFAULT 0 COMMENT '余额，单位：分',
    status      TINYINT NOT NULL DEFAULT 1 COMMENT '1正常 2冻结',
    version     BIGINT NOT NULL DEFAULT 0,
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
                 ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_uid (uid),
    CONSTRAINT chk_balance_nonnegative CHECK (balance >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

```sql
CREATE TABLE recharge_order (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    req_id      VARCHAR(64) NOT NULL,
    uid         VARCHAR(64) NOT NULL,
    amount_cent BIGINT NOT NULL,
    channel     VARCHAR(32) NOT NULL,
    status      TINYINT NOT NULL DEFAULT 0 COMMENT '0处理中 1成功 2失败',
    balance_after BIGINT NULL,
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
                 ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_req_id (req_id),
    KEY idx_uid_created (uid, created_at),
    KEY idx_status_created (status, created_at),
    CONSTRAINT chk_amount_cent_positive CHECK (amount_cent > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

```sql
CREATE TABLE recharge_flow (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    uid           VARCHAR(64) NOT NULL,
    order_id      BIGINT UNSIGNED NOT NULL,
    delta         BIGINT NOT NULL,
    balance_after BIGINT NOT NULL,
    created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_order (order_id),
    KEY idx_uid_created (uid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

三张表职责不同：

- `account` 保存当前余额，是高频点查；
- `recharge_order` 保存请求和幂等状态；
- `recharge_flow` 保存每一次余额变化，便于审计和对账。

**不要只保存最终余额。** 没有流水，出现“为什么少了 50 分”时无法解释。

### 建表时到底在防什么

建表不是把 Go struct 翻译成 SQL。数据库约束是最后一道并发防线，尤其当多个实例、多个服务或人工修数据同时发生时，应用层校验可能被绕过。

`req_id` 的唯一索引负责防止同一请求插入两笔订单；即使两个请求同时通过“查询不存在”，也只有一个能插入成功。`account.uid` 的唯一索引保证一个用户只有一个账户。`recharge_flow.order_id` 的唯一索引保证同一订单最多记一次成功流水。余额的 `CHECK (balance >= 0)` 只能兜底明显非法值，不能单独保证业务正确性，所以仍要配合行锁或条件更新。

订单表还需要记录 `amount_cent`、`channel`、`status`、`balance_after`。它们不是冗余随便加的：重复请求到来时要比较原参数，参数不一致必须返回冲突；成功重试要返回首次结果的 `balance_after`，不能返回“现在余额”，否则后续又发生别的充值后，同一个幂等请求会返回漂移的结果。

索引按查询设计，而不是每个字段都加。常见组合是：`uid + created_at` 查某个用户的充值记录，`status + created_at` 给后台扫描处理中订单，`req_id` 唯一索引负责幂等。索引越多写越慢，先用真实查询和 `EXPLAIN` 证明需要，再加。

---

## 21.6 正确的事务边界

充值必须在**同一个数据库事务**里完成：

```text
BEGIN
  1. INSERT recharge_order(req_id...)，唯一键冲突表示重复请求
  2. SELECT account ... FOR UPDATE
  3. 检查账户存在且状态正常
  4. UPDATE account SET balance = balance + ?, version = version + 1
  5. INSERT recharge_flow
  6. UPDATE recharge_order SET status = 成功, balance_after = ?
COMMIT
```

任何一步失败都回滚。尤其不能先插入订单、提交事务，再去加余额；进程在两步之间崩溃会留下“订单成功但钱没加”的脏数据。

### Go 中的事务轮廓

下面这段代码把订单、余额、流水和订单成功状态放在同一个事务里。重点看订单自增 ID 是怎么拿到的，以及重复请求怎样比较参数。

```go
import (
    "context"
    "database/sql"
    "errors"
    "fmt"

    "github.com/go-sql-driver/mysql"
)

func (r *MySQLRepository) Recharge(
    ctx context.Context,
    req model.RechargeOrder,
) (Result, error) {
    tx, err := r.db.BeginTx(ctx, &sql.TxOptions{
        Isolation: sql.LevelReadCommitted,
    })
    if err != nil {
        return Result{}, fmt.Errorf("begin recharge tx: %w", err)
    }
    defer tx.Rollback() // Commit 之后的 Rollback 返回 ErrTxDone，可以安全 defer

    insert, err := tx.ExecContext(ctx,
        `INSERT INTO recharge_order(req_id, uid, amount_cent, channel, status)
         VALUES (?, ?, ?, ?, 0)`,
        req.ReqID, req.UID, req.AmountCent, req.Channel,
    )
    if err != nil {
        if isDuplicateKey(err) {
            result, loadErr := r.loadDuplicateInTx(ctx, tx, req)
            if loadErr != nil {
                return Result{}, loadErr
            }
            if commitErr := tx.Commit(); commitErr != nil {
                return Result{}, fmt.Errorf("commit duplicate request: %w", commitErr)
            }
            return result, nil
        }
        return Result{}, fmt.Errorf("insert recharge order: %w", err)
    }

    // 不能用 req.ID。首次插入前数据库还没有生成订单主键。
    orderID, err := insert.LastInsertId()
    if err != nil {
        return Result{}, fmt.Errorf("read recharge order id: %w", err)
    }

    var acc model.Account
    err = tx.QueryRowContext(ctx,
        `SELECT id, uid, balance, status, version
           FROM account
          WHERE uid = ?
          FOR UPDATE`,
        req.UID,
    ).Scan(&acc.ID, &acc.UID, &acc.Balance, &acc.Status, &acc.Version)
    if errors.Is(err, sql.ErrNoRows) {
        return Result{}, ErrNotFound
    }
    if err != nil {
        return Result{}, fmt.Errorf("lock account: %w", err)
    }
    if acc.Status != model.AccountNormal {
        return Result{}, ErrFrozen
    }

    newBalance := acc.Balance + req.AmountCent
    update, err := tx.ExecContext(ctx,
        `UPDATE account
            SET balance = balance + ?, version = version + 1
          WHERE id = ? AND status = ?`,
        req.AmountCent, acc.ID, model.AccountNormal,
    )
    if err != nil {
        return Result{}, fmt.Errorf("update balance: %w", err)
    }
    affected, err := update.RowsAffected()
    if err != nil {
        return Result{}, fmt.Errorf("check balance update: %w", err)
    }
    if affected != 1 {
        return Result{}, fmt.Errorf("balance update affected %d rows", affected)
    }

    if _, err := tx.ExecContext(ctx,
        `INSERT INTO recharge_flow(uid, order_id, delta, balance_after)
         VALUES (?, ?, ?, ?)`,
        req.UID, orderID, req.AmountCent, newBalance,
    ); err != nil {
        return Result{}, fmt.Errorf("insert recharge flow: %w", err)
    }

    update, err = tx.ExecContext(ctx,
        `UPDATE recharge_order
            SET status = 1, balance_after = ?
          WHERE id = ? AND status = 0`,
        newBalance, orderID,
    )
    if err != nil {
        return Result{}, fmt.Errorf("finish recharge order: %w", err)
    }
    affected, err = update.RowsAffected()
    if err != nil {
        return Result{}, fmt.Errorf("check order update: %w", err)
    }
    if affected != 1 {
        return Result{}, fmt.Errorf("order update affected %d rows", affected)
    }

    if err := tx.Commit(); err != nil {
        return Result{}, fmt.Errorf("commit recharge: %w", err)
    }
    return Result{
        Balance:    newBalance,
        Duplicated: false,
    }, nil
}

func (r *MySQLRepository) loadDuplicateInTx(
    ctx context.Context,
    tx *sql.Tx,
    req model.RechargeOrder,
) (Result, error) {
    var (
        uid          string
        amountCent   int64
        channel      string
        status       int
        balanceAfter sql.NullInt64
    )

    err := tx.QueryRowContext(ctx,
        `SELECT uid, amount_cent, channel, status, balance_after
           FROM recharge_order
          WHERE req_id = ?
          FOR UPDATE`,
        req.ReqID,
    ).Scan(&uid, &amountCent, &channel, &status, &balanceAfter)
    if errors.Is(err, sql.ErrNoRows) {
        // 极少数情况下，插入冲突后订单又不可见；不能当作成功。
        return Result{}, fmt.Errorf("duplicate order not visible: %w", err)
    }
    if err != nil {
        return Result{}, fmt.Errorf("load duplicate order: %w", err)
    }

    if uid != req.UID || amountCent != req.AmountCent || channel != req.Channel {
        return Result{}, ErrReqIDConflict
    }

    switch status {
    case 1:
        if !balanceAfter.Valid {
            return Result{}, errors.New("successful order missing balance_after")
        }
        return Result{
            Balance:    balanceAfter.Int64,
            Duplicated: true,
        }, nil
    case 0:
        return Result{}, ErrProcessing
    case 2:
        return Result{}, ErrPreviouslyFailed
    default:
        return Result{}, fmt.Errorf("unknown recharge order status: %d", status)
    }
}

func isDuplicateKey(err error) bool {
    var mysqlErr *mysql.MySQLError
    return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
```

这段代码中的关键边界：

- `INSERT recharge_order` 成功后必须用 `LastInsertId()` 取得 `orderID`，再把同一个 ID 写进 `recharge_flow` 和最终的订单更新；
- 订单唯一键是幂等的第一道防线，`recharge_flow.order_id` 的唯一索引是第二道防线；
- 重复请求不能只看 `req_id` 已经存在，还要比较 `uid`、`amount_cent`、`channel`；
- 参数一致且原请求成功，返回原订单的 `balance_after` 和 `duplicated=true`；
- 参数不一致返回 `REQ_ID_CONFLICT`，绝不能把另一笔请求的余额当成成功结果；
- 原请求还在处理中，返回 `ErrProcessing`，不要另开一次充值；
- 任何一步失败都回滚，让占用的行锁和未提交的订单一起消失。

### 唯一键冲突后还能不能继续查

MySQL 中普通 `INSERT` 触发唯一键冲突时，语句失败但事务通常仍可继续，所以示例可以在同一个事务里查询已有订单。PostgreSQL 在语句报错后默认会把整个事务标记为 aborted，必须使用 `SAVEPOINT`/`ROLLBACK TO SAVEPOINT`，或者先独立查询再决定是否开启写入事务。**数据库不同，错误恢复语义也不同，不能照抄。**

另外，同一 `req_id` 再次出现时必须比较 `uid`、`amount_cent`、`channel` 是否与首次请求一致。如果同一个幂等键被用于不同参数，应返回 `REQ_ID_CONFLICT`，不能把另一笔请求的结果当成成功结果。

### `SELECT ... FOR UPDATE` 解决什么

两个请求同时给同一账户加 10 分。没有行锁时，它们可能都读到 100，再分别写 110，最终只增加 10 分。`FOR UPDATE` 让同一账户的更新串行化，后一个事务必须等待前一个提交，再读到 110 并写 120。

### 事务不能做的事

- 不要在持有数据库事务时调用慢速 HTTP/gRPC 下游；
- 不要在事务里发 Kafka、发短信或写日志到远程服务；
- 不要以为 Redis 操作会跟随 MySQL 回滚；
- 事务要短，先做参数校验，再开启事务。

---

## 21.7 幂等的正确理解

幂等不是“第二次请求直接返回成功”。要分清楚以下几种状态：

| 数据库里的请求状态 | 第二次请求应该怎么处理 |
|---|---|
| 不存在 | 首次执行 |
| 处理中 | 返回处理中，或短暂等待；不能重复加钱 |
| 已成功 | 返回首次结果或当前结果，`duplicated=true` |
| 已失败且可重试 | 按明确状态机重试，不能盲目插入 |
| 已失败且不可重试 | 返回原失败原因 |

最危险的做法是“先查再插”：

```text
请求 A 查询 req_id 不存在
请求 B 查询 req_id 不存在
请求 A 插入并执行
请求 B 也插入并执行
```

必须依赖数据库唯一键或原子条件写入来兜底。

### 唯一键冲突后的读取

冲突发生后，不要直接返回 `nil`，也不能只查 `status + balance_after` 就返回成功。正确处理在 21.6 的 `loadDuplicateInTx` 中，顺序固定为：

1. 用 `SELECT ... FOR UPDATE` 读出 **uid、amount_cent、channel、status、balance_after**；
2. 先比较 `uid / amount_cent / channel`，任何一项不同都返回 `REQ_ID_CONFLICT`；
3. 参数一致且状态为成功，返回原订单的 `balance_after`，并标记 `duplicated=true`；
4. 状态为处理中，返回“处理中”，让客户端稍后查询；
5. 状态为失败，返回原失败状态，不能偷偷改成成功。

为什么不能只查 `status + balance_after`？假设请求 A 用 `req_id=r-1` 给 `u-1` 充 10 元，调用方填错参数，又用同一个 `req_id` 给 `u-2` 充 1000 元。如果服务只看到“这个 req_id 已成功”，就把 A 的余额返回给 B，调用方会误以为 1000 元已经到账；如果不比较参数还继续执行，则可能重复加钱。两种情况都会破坏幂等语义。

可以用一张状态迁移表把规则固定下来：

| 当前状态 | 新请求参数一致 | 新请求参数不同 |
|---|---|---|
| 不存在 | 执行充值并写成功 | 不适用 |
| 处理中 | 返回处理中 | 返回参数冲突 |
| 已成功 | 返回首次结果 | 返回参数冲突 |
| 已失败 | 返回原失败结果 | 返回参数冲突 |

测试至少覆盖：同样请求重复两次、同 `req_id` 但金额不同、用户不同、渠道不同、原请求处理中、原请求已失败，以及一百个并发重复请求只加一次钱。只测“同样的请求发两次”无法发现最危险的错单复用问题。

### 幂等失败时的原则

**不要在同一事务里把订单标记失败后继续加钱。** 状态机必须清晰：先校验、再改变余额、最后把订单标记成功。失败时事务回滚，订单是否保留由业务决定；如果保留失败流水，要有单独的失败表或明确状态，不要伪装成成功。

---

## 21.8 HTTP 层：薄，不做业务

```go
type rechargeRequest struct {
    ReqID      string `json:"req_id"`
    AmountCent int64  `json:"amount_cent"`
    Channel    string `json:"channel"`
}

func (h *Handler) recharge(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        writeError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", nil)
        return
    }

    uid, ok := auth.UIDFromContext(r.Context())
    if !ok {
        writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", nil)
        return
    }

    var body rechargeRequest
    dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
    dec.DisallowUnknownFields()
    if err := dec.Decode(&body); err != nil {
        writeError(w, r, http.StatusBadRequest, "INVALID_JSON", nil)
        return
    }

    result, created, err := h.recharge.Recharge(r.Context(), service.RechargeRequest{
        ReqID:      body.ReqID,
        UID:        uid,
        AmountCent: body.AmountCent,
        Channel:    body.Channel,
    })
    if err != nil {
        writeServiceError(w, r, err)
        return
    }
    status := http.StatusOK
    if created {
        status = http.StatusCreated
    }
    writeJSON(w, status, successResponse(r, result))
}
```

`dec.Decode` 之后还要确认没有第二个 JSON 值，否则 `{} {}` 这种请求会被当成合法。错误响应只返回稳定业务码，不把 `err.Error()` 原样回给客户端。

### HTTP 层负责什么

| 职责 | 具体动作 | 为什么必须在这里 |
|---|---|---|
| 解析请求 | 限制 body 大小、解析 JSON、拒绝未知字段 | 防止超大/畸形输入进入业务层 |
| 格式校验 | 字段必填、类型、枚举、长度 | 尽早返回稳定 400，减少无意义数据库调用 |
| 传递上下文 | 把 request_id、认证 uid、deadline 传给 service | 让超时、日志、追踪、权限贯穿整条链路 |
| 错误映射 | 把领域错误映射成 HTTP 状态 + 稳定业务码 | 前端和调用方需要稳定契约 |
| 响应与观测 | 设置 Content-Type、request_id，记录耗时和状态码 | 统一协议和排障证据 |

HTTP 层的核心边界是“翻译协议”，不是“决定业务”。例如 handler 负责识别 JSON 字段名、类型和未知字段，并把 service 返回的 `ErrInvalidAmount` 映射成 `400 INVALID_AMOUNT`；但“金额上限是多少、账户是否冻结、是否重复请求、余额是否足够”必须由 service 和数据库约束判断。

### HTTP 层不负责什么

- 不直接拼 SQL、不直接读写 Redis，否则测试和事务边界会被拆散；
- 不决定“余额扣多少”，避免同一规则在多个接口重复；
- 不把底层数据库错误原样返回给前端，防止泄漏表名、SQL 和内部地址；
- 不把成功响应当成“业务必然成功”：业务码、状态码和最终数据库状态要一致。

**怎么验证边界。** 给 handler 注入一个记录调用次数的 fake service：非法 JSON、未知字段和缺少认证时，断言返回 4xx 且 service 调用次数为 0；合法 JSON 即使业务金额非法，也应只调用一次 service，并由 service 返回 `ErrInvalidAmount`。再让 fake 返回 `ErrReqIDConflict` 和内部数据库错误，检查分别映射为 409 和 500，响应中不出现底层错误文本。这样能同时证明“协议错误不触发业务”和“业务规则只在一处维护”。

### 一个请求进来后发生了什么

以 `POST /v1/recharges` 为例，handler 先把 body 限制在 64KB，解析 JSON，拒绝未知字段，再从认证上下文取得 `uid`；**绝不从 body 读取 uid**，否则客户端可以伪造他人身份。金额范围、`req_id` 长度和 `channel` 白名单由 service 校验，handler 只把领域错误映射成稳定的 HTTP 状态和业务码。

service 执行金额上限、幂等键、账户状态等业务规则，repository 在事务中完成订单、余额和流水写入。成功后 handler 把 `Balance`、`Duplicated` 转成响应；如果 service 返回 `ErrReqIDConflict`，映射成 409 和 `REQ_ID_CONFLICT`；数据库超时在本教学实现里映射成 500 `INTERNAL_ERROR`；真实服务若在 repository 外层接入熔断和依赖分类，可把“依赖不可用”稳定映射成 503 `SERVICE_UNAVAILABLE`。无论哪种，都只记录内部日志，不把 SQL 文本返回给用户。

这里最容易犯的错是把 HTTP 200 当成“业务一定成功”。如果响应体里还有业务码，客户端必须同时检查 HTTP 状态和业务码；服务端则要保证同一类错误永远使用同一组合。不要这次参数错误返回 200 + `INVALID_AMOUNT`，下次又返回 400，否则前端无法建立稳定分支。

### 错误映射示例

```go
func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
    switch {
    case errors.Is(err, service.ErrInvalidAmount):
        writeError(w, r, http.StatusBadRequest, "INVALID_AMOUNT", nil)
    case errors.Is(err, service.ErrInvalidReqID):
        writeError(w, r, http.StatusBadRequest, "INVALID_REQ_ID", nil)
    case errors.Is(err, service.ErrInvalidChannel):
        writeError(w, r, http.StatusBadRequest, "INVALID_CHANNEL", nil)
    case errors.Is(err, service.ErrMissingReqID):
        writeError(w, r, http.StatusBadRequest, "MISSING_REQ_ID", nil)
    case errors.Is(err, service.ErrReqIDConflict):
        writeError(w, r, http.StatusConflict, "REQ_ID_CONFLICT", nil)
    case errors.Is(err, service.ErrAccountNotFound):
        writeError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", nil)
    case errors.Is(err, service.ErrFrozen):
        writeError(w, r, http.StatusConflict, "ACCOUNT_FROZEN", nil)
    case errors.Is(err, service.ErrProcessing):
        writeError(w, r, http.StatusAccepted, "PROCESSING", nil)
    case errors.Is(err, service.ErrPreviouslyFailed):
        writeError(w, r, http.StatusConflict, "PREVIOUSLY_FAILED", nil)
    default:
        log.Printf("recharge failed request_id=%s err=%v", RequestIDFrom(r.Context()), err)
        writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", nil)
    }
}
```

---

## 21.9 缓存：只缓存能容忍短暂过期的数据

余额缓存是高危场景。缓存能提升查询 QPS，但必须回答：

1. 写入数据库后，缓存何时失效？
2. 删除缓存失败怎么办？
3. 两个请求同时读写，是否会把旧值写回？
4. 缓存是不是一致性来源？

推荐策略：

```text
读：先读缓存 -> 未命中读 DB -> 回填短 TTL
写：先提交 DB 事务 -> 删除缓存 -> 失败则重试/发失效消息
兜底：TTL 自动过期，监控缓存陈旧率
```

### Cache-Aside 的伪代码

```go
func (s *Service) Balance(ctx context.Context, uid string) (int64, error) {
    key := "account:balance:" + uid
    if v, ok := s.cache.Get(ctx, key); ok {
        return v.(int64), nil
    }

    acc, err := s.repo.GetAccount(ctx, uid)
    if err != nil {
        return 0, err
    }
    _ = s.cache.Set(ctx, key, acc.Balance, 30*time.Second)
    return acc.Balance, nil
}
```

更新后：

```go
if err := s.repo.Recharge(ctx, order); err != nil {
    return Result{}, err
}
if err := s.cache.Delete(ctx, "account:balance:"+order.UID); err != nil {
    // 记录指标，交给 TTL 和后台修复任务，不要让缓存故障伪装成充值失败。
    s.metrics.CacheDeleteFailed.Inc()
}
```

### 三个常见问题

| 问题 | 现象 | 处理 |
|---|---|---|
| 缓存穿透 | 查不存在的数据，每次都打 DB | 空值短 TTL、参数校验、布隆过滤器 |
| 缓存击穿 | 热 key 过期瞬间打爆 DB | 单飞/互斥回源、逻辑过期 |
| 缓存雪崩 | 大量 key 同时过期 | TTL 加随机抖动、分批预热 |

**重要边界**：余额查询可以短 TTL 缓存，但充值扣款判断必须读数据库事务中的最新值。不能拿缓存余额做资金判断。

---

## 21.10 消息队列：只传递已经提交的事实

充值成功后发送消息，消息应该表达事实，而不是命令另一个服务“必须加钱”：

```json
{
  "event_id": "evt-90001",
  "event_type": "recharge.succeeded",
  "occurred_at": "2026-11-01T10:00:00Z",
  "request_id": "r-20261101-001",
  "uid": "u-1001",
  "amount_cent": 50,
  "balance_after": 150
}
```

流程：

```text
DB 事务提交 -> 记录 outbox 事件 -> 异步发布 MQ -> 消费端按 event_id 幂等
```

如果直接在事务里调用 MQ，可能出现：数据库提交成功，但 MQ 发送失败；或者 MQ 发送成功，数据库回滚。使用 **Transactional Outbox** 可以把“业务成功”和“待发送事件”放进同一个 DB 事务，再由后台发布。

### 消费端幂等表

```sql
CREATE TABLE consumed_event (
    event_id    VARCHAR(64) NOT NULL,
    consumer    VARCHAR(64) NOT NULL,
    consumed_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (event_id, consumer)
) ENGINE=InnoDB;
```

消费逻辑：

```text
开始消费
  INSERT consumed_event(event_id, consumer)
  如果唯一键冲突 -> 已经处理过，直接 ACK
  执行本地业务
  提交本地事务
ACK
```

注意：消费成功但 ACK 丢失时，消息会再次投递；幂等表必须和本地业务处在同一个数据库事务里，或者使用等价的原子方案。

---

## 21.11 配置、日志、指标和追踪

### 配置

```json
{
  "address": "127.0.0.1:8081",
  "mysql": {
    "dsn_env": "MINICOIN_MYSQL_DSN",
    "max_open_conns": 20,
    "max_idle_conns": 10,
    "conn_max_lifetime_seconds": 300
  },
  "redis": {
    "addr": "127.0.0.1:6379",
    "dial_timeout_seconds": 1
  }
}
```

配置里不写密码。敏感值通过环境变量或秘密管理系统注入。

### 结构化日志

每次请求至少记录：

```text
level, timestamp, service, request_id, trace_id, route,
method, status, latency_ms, user_id_masked, error_code
```

不要打印密码、完整 token、完整手机号、身份证、银行卡和完整请求体。

### 指标

```text
http_requests_total{route="/v1/recharges", status="200"}
http_request_duration_seconds_bucket{route="/v1/recharges", le="0.1"}
recharge_total{result="success"}
recharge_failures_total{code="ACCOUNT_FROZEN"}
db_pool_in_use
redis_errors_total
mq_publish_failures_total
```

### 追踪

```go
ctx, span := tracer.Start(ctx, "RechargeService.Recharge")
defer span.End()

span.SetAttributes(attribute.String("request_id", req.ReqID))
```

原则：日志回答“发生了什么”，指标回答“有多少、多快、是否恶化”，追踪回答“慢在哪一段”。三者缺一不可。

---

## 21.12 测试：从规则到集成

### 单元测试

测试业务规则时使用可控的内存 repository，不启动 MySQL：

```go
func TestRechargeRejectsInvalidAmount(t *testing.T) {
    svc := NewRecharge(newMemoryRepo(), noopCache{}, noopPublisher{})
    _, err := svc.Recharge(context.Background(), RechargeRequest{
        ReqID: "r-1", UID: "u-1", AmountCent: 0,
    })
    if !errors.Is(err, ErrInvalidAmount) {
        t.Fatalf("got %v, want ErrInvalidAmount", err)
    }
}
```

### 表驱动测试

```go
tests := []struct{
    name string
    amountCent int64
    want error
}{
    {"zero", 0, ErrInvalidAmount},
    {"negative", -1, ErrInvalidAmount},
    {"normal", 1, nil},
}
```

### 幂等测试

```go
req := RechargeRequest{
    ReqID: "r-idem", UID: "u-1", AmountCent: 10, Channel: "manual",
}
first, err := svc.Recharge(ctx, req)
require.NoError(t, err)

second, err := svc.Recharge(ctx, req)
require.NoError(t, err)
require.True(t, second.Duplicated)
require.Equal(t, first.Balance, second.Balance)
```

只测“相同参数重复两次”还不够。再加一个同 `ReqID`、不同 `Amount/UID/Channel` 的用例，断言返回 `REQ_ID_CONFLICT` 且余额不变。这个用例能抓到“只判断 req_id 是否存在，却把另一笔请求结果当成成功”的严重 bug。

### 并发测试

```go
const n = 100
var wg sync.WaitGroup
for i := 0; i < n; i++ {
    wg.Add(1)
    go func(i int) {
        defer wg.Done()
        _, _ = svc.Recharge(ctx, RechargeRequest{
            ReqID: fmt.Sprintf("r-%d", i),
            UID: "u-1",
            AmountCent: 1,
        })
    }(i)
}
wg.Wait()

acc, _ := repo.GetAccount(ctx, "u-1")
if acc.Balance != initial+n {
    t.Fatalf("got %d, want %d", acc.Balance, initial+n)
}
```

### 集成测试

集成测试必须连接真实 MySQL 或 testcontainers，至少覆盖：

- 唯一键冲突；
- 事务回滚；
- `SELECT FOR UPDATE` 并发；
- 字段长度、CHECK、字符集；
- 数据库连接断开后的恢复；
- 迁移脚本从空库执行成功。

### `-race`

```bash
go test ./... -count=1
go test -race ./... -count=1
```

`-race` 只能发现实际访问到的竞争，不证明没有竞争；并发路径要设计能同时触发。

---

## 21.13 自测和接口验证

### curl 自测

```bash
curl -i -X POST http://127.0.0.1:8081/v1/recharges \
  -H 'Content-Type: application/json' \
  -H 'X-Request-Id: trace-demo-001' \
  -H 'Authorization: Bearer demo-token' \
  -d '{"req_id":"r-1","amount_cent":50,"channel":"manual"}'
```

### 验证幂等

```bash
for i in 1 2; do
  curl -sS -X POST http://127.0.0.1:8081/v1/recharges \
    -H 'Content-Type: application/json' \
    -H 'X-Request-Id: trace-idem-$i' \
    -H 'Authorization: Bearer demo-token' \
    -d '{"req_id":"r-idem","amount_cent":10,"channel":"manual"}'
done
```

第二次结果必须显示重复标记，余额不能增加两次。

### 验证错误路径

- `amount_cent=0`；
- 缺 `req_id`；
- 不存在账户；
- 冻结账户；
- 超大请求体；
- 非 JSON；
- 方法错误；
- 下游数据库超时。

真正的自测不是只测 happy path，而是把错误路径变成可重复的命令。

### 自测要留下什么证据

自测不是“点了下 Postman 看到 200”。每次验收至少保存：请求命令或 collection、响应状态和 body、`request_id`、执行前后的余额、服务日志中的关键行。对幂等测试还要保存两次响应，证明第二次 `duplicated=true` 且余额没有变化。

错误路径要写成表，逐项执行：

| 输入 | 预期 HTTP | 预期业务码 | 额外断言 |
|---|---|---|---|
| `amount_cent=0` | 400 | `INVALID_AMOUNT` | 订单数、余额不变 |
| 缺 `req_id` | 400 | `MISSING_REQ_ID` | 没有写数据库 |
| 冻结账户 | 409 | `ACCOUNT_FROZEN` | 余额不变 |
| 同 req_id 不同金额 | 409 | `REQ_ID_CONFLICT` | 返回原订单信息 |
| 非 JSON | 400 | `INVALID_JSON` | 日志有 request_id |
| 数据库超时 | 500（接入熔断后可映射 503） | `INTERNAL_ERROR`（503 时用 `SERVICE_UNAVAILABLE`） | 不泄漏 SQL |

如果某一项只能靠人工观察而无法重复，说明服务缺少可观测字段或接口契约不完整，应补测试，而不是把“这次看起来没问题”当作通过。

---

## 21.14 CI、镜像和发布

### CI 门禁

```yaml
stages: [lint, test, build]
lint:
  script:
    - test -z "$(gofmt -l .)"
    - go vet ./...
test:
  script:
    - go test ./... -count=1
    - go test -race ./... -count=1
build:
  script:
    - CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o app ./cmd/server
```

### 多阶段 Dockerfile

```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
USER nonroot:nonroot
EXPOSE 8081
ENTRYPOINT ["/server"]
```

发布前检查：

- 镜像带不可变 tag 或 digest；
- 不把 `.env`、证书、token 打进镜像；
- 进程以非 root 运行；
- 有 CPU/内存限制；
- 有启动、存活、就绪探针；
- 有回滚版本和数据库迁移回滚方案。

### 优雅退出

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
defer stop()
<-ctx.Done()

shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
_ = server.Shutdown(shutdownCtx)
_ = repo.Close()
```

收到 SIGTERM 后停止接新请求，等待在途请求完成，再退出。不能直接 `os.Exit(0)`，否则请求可能写一半。

### 数据库迁移顺序

1. 先执行向后兼容的 DDL；
2. 再发布能读写新结构的代码；
3. 观察指标；
4. 最后删除旧字段或旧代码。

禁止把不可回滚的 DROP 和代码发布绑在同一步。

---

## 21.15 上线后的观察顺序

上线后 10 分钟内按下面顺序看：

1. **流量**：QPS 是否达到预期，入口有没有被限流；
2. **错误率**：5xx、业务错误码、数据库错误、MQ 错误；
3. **延迟**：P50/P95/P99，是否出现长尾；
4. **饱和度**：CPU、内存、goroutine、连接池、Redis、磁盘；
5. **业务结果**：充值成功数、重复请求数、余额变化总量是否与订单一致；
6. **日志与追踪**：挑选几个失败 request_id 追踪完整链路。

报警要围绕用户影响，不要只报 CPU 80%。例如：

```text
5 分钟错误率 > 2%
P99 > 800ms 持续 5 分钟
连续 3 分钟没有成功充值
数据库连接池使用率 > 90% 持续 10 分钟
消息积压持续增长
```

### 怎么判断这次发布能不能继续

上线前先记录基线：正常时段的核心 QPS、P95/P99、错误率、成功充值数、连接池使用率和 MQ lag。发布后对比同一时间窗口，而不是只看“进程还在”。如果错误率超过基线两倍、P99 翻倍、核心业务连续几分钟为零，或者日志出现新的 panic/deadlock，就停止扩量并准备回滚。

观察顺序要从用户结果往基础设施倒推：先看充值成功数和余额变化，再看接口错误率和延迟，最后看 CPU、连接池、GC 和日志。CPU 高但业务完全正常，可能是流量增长；CPU 正常但成功数为零，可能是业务码错误或锁死。不要用单一资源指标替代业务健康判断。

回滚也要有验证标准：切回旧版本后，核心接口错误率恢复到基线、成功充值恢复、延迟下降、订单与流水对账一致。仅仅看到 Pod Running 不能说明回滚成功。

---

## 21.16 本章综合练习

### 练习 A：需求转验收

把“用户充值”改成至少 8 条可自动化验收用例，必须覆盖幂等、并发、金额、账户状态和错误响应。

### 练习 B：事务设计

写出“创建订单、锁账户、加余额、写流水、更新订单”的 SQL 事务，并解释为什么不能把 MQ 发送放在事务中。

### 练习 C：幂等状态机

分别画出 `不存在/处理中/成功/可重试失败/不可重试失败` 五种状态下的第二次请求流程。

### 练习 D：故障注入

人为让缓存删除失败、数据库提交超时、消息消费重复，分别写观察指标和恢复方案。

### 练习 E：上线演练

为一个只读接口设计发布、灰度和回滚步骤，要求说明如何判断是否可以扩大流量。

---

## 参考答案

<details>
<summary>展开参考答案</summary>

### A. 需求转验收：至少 8 条可自动化用例

每条都要写清输入、预期 HTTP、预期业务码、数据库最终状态和验证方式。

| # | 场景 | 输入 | 预期 |
|---|---|---|---|
| 1 | 首次充值 | 新 `req_id`，`amount_cent=100` | 201、`OK`、余额 +100、订单 1 条、流水 1 条 |
| 2 | 相同参数重试 | 同一 `req_id` 和参数 | 200、`duplicated=true`、余额不再增加 |
| 3 | 同键不同金额 | 同一 `req_id`，`amount_cent=200` | 409、`REQ_ID_CONFLICT`、无新订单 |
| 4 | 同键不同渠道 | 同一 `req_id`，`channel` 改变 | 409、`REQ_ID_CONFLICT` |
| 5 | 金额为 0 | `amount_cent=0` | 400、`INVALID_AMOUNT`、service 未被调用 |
| 6 | 金额为负 | `amount_cent=-1` | 400、`INVALID_AMOUNT` |
| 7 | 缺幂等键 | 无 `req_id` | 400、`MISSING_REQ_ID` |
| 8 | 未知字段 | body 带 `uid` | 400、`INVALID_JSON` |
| 9 | 未认证 | 无 Authorization | 401、`UNAUTHORIZED` |
| 10 | 账户冻结 | 冻结账户 + 新 `req_id` | 409、`ACCOUNT_FROZEN`、余额/订单不变 |
| 11 | 并发同键 | 20 个 goroutine 同 `req_id` | 只有一次加钱，其余返回首次结果或处理中 |
| 12 | 内部错误 | 注入 DB 错误 | 500、`INTERNAL_ERROR`、响应不含 SQL |

自动化落到 Go 测试：

```go
func TestRechargeAcceptance(t *testing.T) {
    cases := []struct{
        name       string
        reqID      string
        amountCent int64
        channel    string
        auth       bool
        wantStatus int
        wantCode   string
        wantDelta  int64
    }{
        {"first", "acc-1", 100, "manual", true, 201, "OK", 100},
        {"zero", "acc-2", 0, "manual", true, 400, "INVALID_AMOUNT", 0},
        {"negative", "acc-3", -1, "manual", true, 400, "INVALID_AMOUNT", 0},
        {"no auth", "acc-4", 100, "manual", false, 401, "UNAUTHORIZED", 0},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            // 记录初始余额，发请求，断言状态码/业务码/余额变化
        })
    }
}
```

**验证**：先让当前实现跑这套用例，至少应有一个失败；修复后全绿。没有先失败过的测试，不能证明它有效。

---

### B. 事务设计

```sql
BEGIN;

-- 1. 先插入订单，利用唯一键占住 req_id
INSERT INTO recharge_order(req_id, uid, amount_cent, channel, status)
VALUES (?, ?, ?, ?, 0);

-- 2. 锁账户，禁止其他事务同时改余额
SELECT id, balance, status, version
FROM account
WHERE uid = ?
FOR UPDATE;

-- 3. 校验账户状态后更新余额
UPDATE account
SET balance = balance + ?, version = version + 1
WHERE id = ? AND status = 1;

-- 4. 写流水（order_id 唯一，防重复记账）
INSERT INTO recharge_flow(uid, order_id, delta, balance_after)
VALUES (?, ?, ?, ?);

-- 5. 标记订单成功并保存首次结果
UPDATE recharge_order
SET status = 1, balance_after = ?
WHERE id = ? AND status = 0;

COMMIT;
```

任何一步失败都 `ROLLBACK`，并且检查 `RowsAffected`，不能假设 UPDATE 一定命中。

**为什么不能把 MQ 发送放进事务**：MQ 是外部副作用，不受数据库事务控制。可能出现：

```text
DB COMMIT 成功 -> MQ 发送失败  => 事件丢失
MQ 发送成功   -> DB ROLLBACK    => 下游收到不存在的充值
```

正确做法是 Transactional Outbox：在同一个数据库事务里写 `outbox` 表，提交后由投递器异步发送，消费端按 `event_id` 幂等。

**验证**：注入“第 5 步失败”，事务回滚后余额、流水、订单都不应留下成功状态；注入“outbox 投递失败”，重试后事件最终送达且不重复加钱。

---

### C. 幂等状态机

| 数据库状态 | 第二次请求参数一致 | 参数不同 |
|---|---|---|
| 不存在 | 执行充值 | 不适用 |
| 处理中 | 返回 202 `PROCESSING` | 409 `REQ_ID_CONFLICT` |
| 成功 | 200，返回首次 `balance_after`，`duplicated=true` | 409 `REQ_ID_CONFLICT` |
| 可重试失败 | 按状态机重新进入处理中 | 409 |
| 不可重试失败 | 409 `PREVIOUSLY_FAILED` | 409 |

实现顺序：

```text
1. INSERT ... ON DUPLICATE KEY 或捕获唯一键冲突
2. 冲突后 SELECT ... FOR UPDATE 读原订单
3. 先比较 uid / amount_cent / channel
4. 参数不同 -> REQ_ID_CONFLICT
5. 参数相同按状态返回首次结果、PROCESSING 或 PREVIOUSLY_FAILED
```

**验证**：用表驱动测试覆盖五种状态 × 参数一致/不一致；再开 20 个并发请求，最终余额只增加一次，订单只有一条。

---

### D. 故障注入

| 故障 | 观察指标 | 恢复方案 |
|---|---|---|
| 缓存删除失败 | 缓存删除错误数、脏读告警 | 短 TTL 兜底 + 后台按 key 重删 + 版本号校验 |
| 数据库提交超时 | 事务耗时、连接池占用、提交错误 | 按“结果未知”处理，用 `req_id` 查订单，确认未提交才重试 |
| 消息重复消费 | 消费次数、幂等命中数、余额差异 | 消费幂等表以 `event_id` 唯一键，重复消息直接 ACK |

代码验证示例：

```go
func TestConsumerIdempotent(t *testing.T) {
    evt := Event{EventID: "evt-1", UID: "u-1", AmountCent: 100}
    if err := consumer.Handle(ctx, evt); err != nil {
        t.Fatal(err)
    }
    if err := consumer.Handle(ctx, evt); err != nil {
        t.Fatal(err)
    }
    bal := mustBalance(t, "u-1")
    if bal != 100 {
        t.Fatalf("balance=%d, want 100", bal)
    }
}
```

**验证**：故意让第一次消费在“加钱后、ACK 前”崩溃，重投后余额仍只增加一次；如果失败，检查幂等记录是否和业务写入在同一个事务。

---

### E. 只读接口上线演练

```text
1. 预发：同版本镜像 + 同配置 + 同数据量跑 smoke test
2. 记录基线：QPS、P99、5xx、缓存命中率、DB 连接池
3. 灰度 1% 5 分钟：观察新版本与旧版本对比
4. 10% / 50% / 100%：每一步都看同一组指标
5. 停止条件：5xx > 1% 持续 5 分钟，或 P99 > 500ms 持续 5 分钟
6. 回滚：切回上一镜像 digest，验证接口和指标恢复
```

```bash
kubectl set image deployment/query-api app=registry.example.com/query-api:v1.2.3
kubectl rollout status deployment/query-api --timeout=5m
kubectl rollout history deployment/query-api
kubectl rollout undo deployment/query-api
```

**验证证据**：每一步的时间、流量比例、QPS/P99/5xx、错误日志样本、回滚命令执行结果。不能只凭“页面能打开”宣布成功。

</details>
