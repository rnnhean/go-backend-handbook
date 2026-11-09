# 第 2 章 服务架构与请求生命周期

> 目标：拿到一个陌生 Go 后端项目，能在 20 分钟内从 `main` 追到一个请求的落库路径；知道每一层该写什么、不该写什么；能设计一个可测试、可替换依赖的服务。
>
> 本章用 `mini-coin` 的虚构结构：HTTP handler → service → repository → MySQL，外加 Redis 和 MQ。

---

## 2.1 分层不是形式主义

```
HTTP/gRPC 接入层  →  service 业务层  →  repository 数据访问层  →  MySQL
        ↓                  ↓                    ↓
     鉴权/校验          事务/幂等/编排          SQL/索引/连接池
```

| 层 | 只该做什么 | 不该做什么 |
|---|---|---|
| handler/api | 解析协议、校验格式、映射错误码 | 写 SQL、写业务规则 |
| service | 业务规则、事务边界、幂等、编排 | 关心 HTTP header/状态码 |
| repository | 数据访问，隔离存储细节 | 写业务判断 |
| model | 领域数据结构 | 依赖框架 |

### 为什么不能全写在 handler 里

handler 直接操作数据库会同时承担协议解析、业务规则、事务和存储细节。短期代码少，长期会产生四类问题：换协议要重写业务、无法用 fake 单测、多个写操作的原子性没人负责、SQL 错误直接泄漏给客户端。分层的目标是让每一层只回答一种问题，而不是为了目录好看。

```go
// 反例：所有东西塞在 handler
func Recharge(w http.ResponseWriter, r *http.Request) {
    var req Request
    json.NewDecoder(r.Body).Decode(&req)

    row := db.QueryRow("SELECT balance FROM account WHERE uid = ?", req.UID)
    var balance int64
    row.Scan(&balance)

    db.Exec("UPDATE account SET balance = ? WHERE uid = ?", balance+req.Amount, req.UID)
    db.Exec("INSERT INTO recharge_order ...")
    // ...
}
```

问题：

- 不能单测，必须连数据库；
- 事务边界不清，失败一半；
- 并发下读-改-写丢更新；
- 换协议（gRPC）要重写业务；
- 错误码和 SQL 错误混在一起。

### 分层后的调用链

读这段代码时重点看“消息如何变形”：HTTP 层把 JSON 变成请求对象，service 接收业务参数并返回领域结果，repository 再把结果映射成 SQL。各层之间不要互相泄漏 `*http.Request`、SQL 行或数据库错误；这样同一业务才能被 HTTP、gRPC 和定时任务复用。

```go
// handler：只负责 HTTP
func (h *Handler) Recharge(w http.ResponseWriter, r *http.Request) {
    var req RechargeRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        respondError(w, http.StatusBadRequest, "INVALID_JSON")
        return
    }
    result, err := h.svc.Recharge(r.Context(), req)
    if err != nil {
        respondServiceError(w, err)
        return
    }
    status := http.StatusCreated
    if result.Duplicated {
        status = http.StatusOK
    }
    respondJSON(w, status, result)
}
```

```go
// service：业务规则
func (s *RechargeService) Recharge(ctx context.Context, req RechargeRequest) (Result, error) {
    if req.AmountCent <= 0 {
        return Result{}, ErrInvalidAmount
    }
    return s.repo.Recharge(ctx, req)
}
```

```go
// repository：SQL/事务
func (r *Repo) Recharge(ctx context.Context, req RechargeRequest) (Result, error) {
    tx, err := r.db.BeginTx(ctx, nil)
    // ...
}
```

---

## 2.2 从 main 开始追：依赖注入

`cmd/server/main.go` 是唯一组装对象的地方：

```go
func main() {
    cfg := config.MustLoad()
    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

    db := mustOpenDB(cfg.MySQL)
    defer db.Close()

    cache := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
    producer := kafka.NewProducer(cfg.Kafka.Brokers)

    repo := repository.NewMySQL(db)
    svc := service.NewRecharge(repo, cache, producer, logger)
    handler := httpapi.New(svc, logger)

    srv := &http.Server{
        Addr:              cfg.HTTP.Addr,
        Handler:           handler.Routes(),
        ReadHeaderTimeout: 2 * time.Second,
        ReadTimeout:       5 * time.Second,
        WriteTimeout:      10 * time.Second,
    }

    go func() {
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logger.Error("server failed", "error", err)
            os.Exit(1)
        }
    }()

    // 优雅退出
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    <-ctx.Done()

    shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    _ = srv.Shutdown(shutdownCtx)
}
```

### 为什么要依赖注入

依赖注入（DI）不是框架术语，而是把“Service 需要什么”从函数内部的全局变量改成构造参数。这样测试可以传内存仓库，生产可以传 MySQL 仓库；同一个业务逻辑可以在不启动数据库的情况下验证。关键是依赖接口要小，不能为了 DI 把所有细节都暴露成接口。

- service 依赖接口，不依赖具体 MySQL；
- 测试传 fake；
- 替换实现不改业务代码；
- 依赖关系显式，启动时就能发现缺配置。

```go
type RechargeRepo interface {
    Recharge(ctx context.Context, req RechargeRequest) (Result, error)
}
```

---

## 2.3 一次充值请求的完整链路

```http
POST /v1/recharges
Authorization: Bearer <token>
Content-Type: application/json

{"req_id":"req-001","amount_cent":100,"channel":"manual"}
```

```
1. 网关：TLS、路由、限流、生成/透传 request_id
2. handler：
   - 解析 JSON
   - 校验 req_id/amount_cent/channel 格式
   - 从 token 取 uid（不信任请求体里的 uid）
3. service：
   - 业务校验
   - 幂等键处理
   - 事务编排
   - 发事件
4. repository：
   - INSERT recharge_order（唯一键 req_id）
   - UPDATE account balance = balance + ?
   - INSERT recharge_flow
5. 首次成功返回 201；幂等命中返回 200；响应含 uid/balance/duplicated
```

### 幂等为什么必须在最前面

重试请求可能在第一次处理尚未结束时就到达，因此幂等判断不能只放在业务末尾。正确顺序是先校验用户和参数，再用数据库唯一键占位；若同键参数一致，返回首次结果；参数不同返回冲突；处理中返回明确状态。只靠“查一下有没有”挡不住两个请求同时查到不存在的竞态。

客户端超时重试是常态：

```
客户端 → 服务端（已扣款，但响应丢了）→ 客户端超时 → 重试
```

没有幂等键，就加两次钱。正确做法：

```sql
INSERT INTO recharge_order(req_id, uid, amount_cent, channel, status)
VALUES (?, ?, ?, ?, 'pending')
```

`req_id` 唯一索引，冲突时：

- 参数一致：返回原订单；
- 参数不一致：返回 409 `REQ_ID_CONFLICT`。

**不要用“先 SELECT 再 INSERT”**，并发下两个请求都查不到，然后都插入。唯一索引才是原子判重。

---

## 2.4 错误码设计

错误码区分“协议错误、业务冲突、依赖失败和系统故障”，前端据此决定是否重试、提示什么、是否需要用户操作。HTTP 状态码表达协议层语义，业务 `code` 表达稳定原因，两者应成对映射；不要把数据库原文、堆栈或随意的中文文案直接暴露为 code。

```json
{
  "code": "INVALID_AMOUNT",
  "message": "amount_cent must be between 1 and 100000000",
  "request_id": "req-1"
}
```

- HTTP 状态码表达协议层语义；
- 业务 `code` 表达具体原因；
- `request_id` 用于串联日志；
- 不要把 SQL/堆栈返回给客户端。

| HTTP | 场景 |
|---|---|
| 400 | 参数格式/校验失败 |
| 401 | 未认证 |
| 403 | 无权限 |
| 404 | 资源不存在 |
| 409 | 冲突（幂等参数不一致、库存不足） |
| 429 | 限流 |
| 500 | 内部错误 |
| 502/503 | 下游错误/不可用 |

错误映射集中在一个函数，避免每个 handler 各写一套：

```go
func respondServiceError(w http.ResponseWriter, err error) {
    switch {
    case errors.Is(err, service.ErrInvalidAmount):
        respondError(w, 400, "INVALID_AMOUNT")
    case errors.Is(err, service.ErrConflict):
        respondError(w, 409, "REQ_ID_CONFLICT")
    default:
        respondError(w, 500, "INTERNAL_ERROR")
    }
}
```

---

## 2.5 配置

配置是把同一份代码部署到不同环境的边界。启动时应一次性加载、校验并冻结配置，运行中缺少数据库地址或端口非法要立即失败，而不是等第一个请求才 panic。敏感项只从环境变量或密钥管理注入，日志打印配置时必须脱敏。

```yaml
http:
  addr: ":8080"
mysql:
  dsn: "demo:demo@tcp(127.0.0.1:3306)/minicoin?parseTime=true"
  max_open_conns: 50
redis:
  addr: "127.0.0.1:6379"
```

原则：

- 代码一份，配置按环境注入；
- 优先级：命令行 > 环境变量 > 配置文件 > 默认值；
- 密码放环境变量/密钥管理；
- 启动时校验，缺配置直接失败；
- 打印配置时脱敏。

---

## 2.6 一个服务该拆多细

拆分单位应该是“能独立演进、独立扩容、独立故障隔离的业务能力”，不是按 Controller、Service、DAO 各起一个服务。先问团队是否有人维护、调用是否需要网络、事务是否能跨服务、故障是否会级联；如果答案不支持，单体模块通常更可靠。

先单体，后拆分。拆分信号：

- 多个团队改同一个仓库频繁冲突；
- 某模块需要独立扩容；
- 某模块故障需要隔离；
- 发布节奏差异大；
- 有明确业务边界和独立数据。

不要拆的理由：

- 团队小，一个人维护 10 个服务；
- 没有 CI/CD、监控、链路追踪；
- 事务边界不清，拆完到处分布式事务；
- 只是为了“微服务先进”。

拆分后要付出：网络延迟、超时重试、幂等、分布式事务、更多告警和部署。**收益大于成本再拆。**

---

## 2.7 常见架构反模式

反模式往往不是一眼就能看出的错误，而是短期能跑、长期让变更变慢的结构。读表时重点看“为什么会发生”和“什么时候会反噬”，不要只记名字。

| 反模式 | 后果 |
|---|---|
| handler 直接写 SQL | 不可测试、事务混乱 |
| service 依赖 `*http.Request` | 业务和协议耦合 |
| repository 写业务判断 | 换存储要重写规则 |
| 全局单例满天飞 | 测试互相污染 |
| 在事务里调远程服务 | 长事务、锁等待 |
| 所有错误都 500 | 客户端无法处理 |
| 日志没有 request_id | 无法排查 |
| 配置硬编码 | 多环境部署困难 |

---

## 2.8 项目目录示例

目录结构要体现依赖方向：`cmd` 负责组装，`httpapi` 只处理协议，`service` 持有业务规则，`repository` 隐藏存储实现，`model` 不反向依赖外层。目录名不能代替架构约束，评审时要检查 import 关系；如果 service 里出现 HTTP 状态码或 handler 里出现 SQL，即使目录正确也仍然是耦合。

```
minicoin/
├── cmd/server/main.go
├── internal/
│   ├── config/
│   ├── httpapi/        # 路由、handler、中间件
│   ├── service/        # 业务逻辑
│   ├── repository/     # MySQL/Redis 实现
│   ├── model/          # 领域结构
│   └── apperr/         # 错误码
├── api/                # proto/openapi
├── migrations/         # SQL 迁移
├── configs/
└── go.mod
```

`internal` 限制外部 import；`cmd` 只做启动；业务代码不 import HTTP 框架。

---

## 习题

### 基础题

1. handler、service、repository 各负责什么？
2. 为什么幂等要在数据库层用唯一键？
3. 为什么 uid 要从 token 取，不能从请求体取？
4. 依赖注入有什么好处？
5. 优雅退出要做什么？为什么滚动发布需要它？
6. HTTP 状态码和业务 code 分别表达什么？
7. 什么信号说明该拆微服务？什么情况不该拆？
8. 在事务里调用远程服务有什么问题？

### 动手题

9. 画出 `POST /v1/recharges` 从网关到数据库的完整链路。
10. 写一个 handler，把 service 错误映射成 400/409/500。
11. 写 `main` 的启动顺序：配置、DB、Redis、service、HTTP、优雅退出。
12. 给一个项目设计目录结构，要求 handler 不依赖具体数据库。

### 综合题

13. 一个 handler 里同时做了参数校验、查数据库、加余额、发 MQ，代码 300 行。写出你的重构步骤和每步的验收标准。

---

## 参考答案

<details>
<summary>展开</summary>

1. handler 负责 HTTP/gRPC 协议、解析、格式校验、鉴权和错误映射；service 负责业务规则、事务边界、幂等和依赖编排；repository 负责 SQL、索引、连接和存储错误转换。model 只表达领域数据。

2. 先查再插是两个独立动作，两个请求可能同时查到不存在，再同时插入。正确做法是用数据库唯一索引或原子插入判重，应用层查询只用于返回友好结果，不能作为最终并发保证。

3. 防止 IDOR 越权。用户只能操作 token 对应的 uid；查询条件应直接带 `uid = ?`，不要把别人的资源查出来后再判断，否则容易漏掉分支并泄漏资源是否存在。

4. 依赖注入让 service 依赖小接口，测试可以传 fake，生产传 MySQL/Redis 实现；依赖关系显式，替换存储或协议时不需要改业务规则。

5. 收到退出信号后停止接收新请求，等待在途请求在超时内完成，再关闭数据库、Redis、MQ 客户端并 flush 日志。滚动发布时 readiness 应先摘流量，再退出，避免直接切断请求。

6. HTTP 状态码表达协议层结果，例如 400/401/409/500；业务 code 表达稳定的领域原因，例如 `INVALID_AMOUNT`、`REQ_ID_CONFLICT`。两者应集中映射，不能每个 handler 各写一套。

7. 需要独立扩容、故障隔离、团队边界和发布节奏明显不同时才拆微服务；团队小、事务边界强、共享表、缺少可观测性和发布能力时，单体通常更可靠。

8. 事务持有数据库锁，远程调用可能超时或不可控，会把锁等待放大；远程调用失败后数据库回滚也不能撤销已成功的外部副作用。应把远程调用放到事务外，或用 Outbox/补偿和幂等。

9. 完整链路：

```text
浏览器/客户端
  -> DNS/负载均衡
  -> Nginx/网关：TLS、路由、限流、生成 request_id
  -> Go HTTP middleware：恢复 panic、日志、鉴权、注入 request_id/uid
  -> handler：解析 JSON、校验格式、映射错误
  -> service：金额/渠道/账户状态、幂等规则、事务编排
  -> repository：INSERT recharge_order (uk_req_id)
  -> MySQL transaction：
       INSERT order
       SELECT account ... FOR UPDATE
       UPDATE account balance = balance + ?
       INSERT recharge_flow
       UPDATE order status = success
       COMMIT
  -> 删除余额缓存（失败记录指标，TTL 兜底）
  -> 事务后发事件/Outbox
  -> 返回 201/200 + code + request_id + data
```

每一步都要能失败：网络层失败不给业务副作用；鉴权失败在 service 前结束；数据库超时按“结果未知”用 req_id 查询；缓存删除失败不能让已提交事务回滚；MQ 失败不能直接丢掉事件。

10.

```go
func (h *Handler) Recharge(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UIDFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "authentication is required")
		return
	}

	var body RechargeBody // 只含 req_id / amount_cent / channel，不含 uid
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON", "invalid request body")
		return
	}
	result, err := h.svc.Recharge(r.Context(), service.Request{
		ReqID: body.ReqID, UID: uid, AmountCent: body.AmountCent, Channel: body.Channel,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidAmount):
			writeError(w, r, http.StatusBadRequest, "INVALID_AMOUNT", "amount_cent must be between 1 and 100000000")
		case errors.Is(err, service.ErrInvalidReqID):
			writeError(w, r, http.StatusBadRequest, "INVALID_REQ_ID", "req_id must be 1 to 64 characters")
		case errors.Is(err, service.ErrInvalidChannel):
			writeError(w, r, http.StatusBadRequest, "INVALID_CHANNEL", "channel must be manual or coupon")
		case errors.Is(err, service.ErrMissingReqID):
			writeError(w, r, http.StatusBadRequest, "MISSING_REQ_ID", "req_id is required")
		case errors.Is(err, service.ErrMissingChannel):
			writeError(w, r, http.StatusBadRequest, "MISSING_CHANNEL", "channel is required")
		case errors.Is(err, service.ErrReqIDConflict):
			writeError(w, r, http.StatusConflict, "REQ_ID_CONFLICT", "req_id was reused with different parameters")
		case errors.Is(err, service.ErrAccountNotFound):
			writeError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "account not found")
		case errors.Is(err, service.ErrFrozen):
			writeError(w, r, http.StatusConflict, "ACCOUNT_FROZEN", "account is frozen")
		default:
			// 内部错误只写日志，响应不返回 SQL/堆栈。
			writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
		}
		return
	}
	status := http.StatusCreated
	if result.Duplicated { status = http.StatusOK }
	writeJSON(w, status, map[string]any{
		"code": "OK", "message": "success",
		"request_id": RequestIDFrom(r.Context()),
		"data": result,
	})
}
```

验收要覆盖正常 201、重复 200、参数错误 400、同键不同参数 409、内部错误 500 且响应不含底层错误文本。`writeError` 也必须从同一个 request_id 来源读取，不能 body 和响应头各生成一个。

11. 启动和退出顺序：

```text
读取配置并校验
-> 初始化 logger、tracing
-> 连接 MySQL/Redis/MQ
-> health check 内部依赖
-> 构造 repository/cache/publisher
-> 构造 service/handler
-> 启动 HTTP server
-> 监听 SIGTERM/SIGINT
-> readiness=false，停止接新流量
-> srv.Shutdown(ctx)
-> 关闭 worker、publisher、Redis、DB
-> 记录退出原因并退出
```

关键失败行为：配置非法立即退出并打印字段名；数据库连不上启动失败，不能带病接流量；shutdown 超时时记录卡住的组件，再按预案退出。验证方式是启动后发请求，发送 SIGTERM，观察在途请求完成、监听端口关闭、日志有完整退出顺序。

12.

```text
cmd/server/main.go           组装依赖，不写业务
internal/httpapi/            HTTP 协议适配
internal/service/            业务规则，只依赖接口
internal/repository/         MySQL 实现
internal/cache/              Redis 实现
internal/mq/                 Kafka/RabbitMQ 实现
internal/model/              领域结构
migrations/                  SQL 迁移
```

依赖方向只能是 `httpapi -> service -> repository interface`，handler 不 import MySQL 包。验收：把 repository 替换成内存 fake 后，service 和 handler 测试仍能运行。

13. 先写行为测试，再分四步重构：

1. 抽出 repository：把 SQL 查询和写入移出 handler，参数和错误语义保持不变；验收是原有用例全绿，并新增 repo 集成测试验证唯一键和事务。
2. 抽出 service：移动金额校验、账户状态、幂等和事务编排；验收是 handler 不再依赖 SQL，service 单测可用 fake 覆盖成功/冲突/失败。
3. 收紧 handler：只解析、鉴权、错误映射和响应；验收是非法 JSON 不调用 service，内部错误不泄漏。
4. 引入缓存/MQ/Outbox：缓存删除失败只记指标；MQ 只在事务提交后投递，失败可重试且消费端幂等。每步单独提交、跑 `go test ./... -count=1`，最后补并发同 req_id 测试和回归证据。

</details>
