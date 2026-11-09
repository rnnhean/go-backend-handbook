# 第 8 章 接口设计与 gRPC

> 目标：设计出前端愿意对接、后端好维护、出错能定位、升级不破坏老客户端的接口；知道什么时候用 HTTP/JSON，什么时候用 gRPC。
>
> 接口一旦发出去就是契约。改字段、改错误码、改分页方式都可能让老客户端崩溃。设计时先想“半年后怎么改”。

---

## 8.1 HTTP 接口设计：资源、方法、状态码

### URL 用名词，动作用方法

```http
GET    /v1/recharges?limit=20
POST   /v1/recharges
GET    /v1/recharges/{id}
POST   /v1/recharges/{id}/refunds
DELETE /v1/recharges/{id}
```

不要：

```http
POST /v1/getRecharge
POST /v1/createRecharge
POST /v1/recharge/do
```

URL 表示“资源”，HTTP 方法表示“动作”。不是所有业务都能完美映射 REST，但至少保持一致。

### 状态码表达协议层语义

HTTP 状态码描述请求在协议层的处理结果，业务错误码描述业务规则。两者不能混成“永远 200 + code”。例如参数格式错误用 400，未认证用 401，无权限用 403，不存在用 404，冲突用 409，限流用 429，服务端故障用 5xx；客户端和网关才能正确重试、告警和缓存。

| 状态码 | 含义 | 例子 |
|---|---|---|
| 200 | 成功 | 查询、更新成功 |
| 201 | 已创建 | POST 创建订单 |
| 204 | 成功无响应体 | DELETE |
| 400 | 请求格式/参数错误 | JSON 解析失败、金额为负 |
| 401 | 未认证 | token 缺失/过期 |
| 403 | 已认证但无权限 | 普通用户访问管理接口 |
| 404 | 资源不存在 | 订单不存在 |
| 409 | 冲突 | 幂等键参数不一致、库存不足 |
| 422 | 语义校验失败 | 字段格式对但业务不合法 |
| 429 | 限流 | 请求过多 |
| 500 | 服务内部错误 | 未捕获异常 |
| 502 | 上游错误 | 支付渠道返回异常 |
| 503 | 服务不可用 | 依赖不可用、熔断 |
| 504 | 超时 | 上游超时 |

**不要所有错误都返回 200 + `success:false`。** 网关、监控、客户端重试策略都依赖状态码。

### 统一错误体

统一错误体让客户端能用稳定 code 分支，而不是解析人类文案。`code` 是契约，`message` 是展示辅助，`request_id` 用于查日志；`details` 只放安全、可解释的字段。服务端错误不要返回 SQL、堆栈或依赖地址。

```json
{
  "code": "INVALID_AMOUNT",
  "message": "amount_cent must be positive",
  "request_id": "req-01H...",
  "details": {
    "field": "amount_cent",
    "value": -100
  }
}
```

- `code` 稳定，供客户端分支；
- `message` 可读，可国际化；
- `request_id` 用于查日志；
- `details` 可选，不要放敏感信息；
- 永远不要把数据库错误、堆栈返回给客户端。

### 分页

**Offset 分页**：

```http
GET /v1/recharges?limit=20&offset=40
```

响应：

```json
{
  "items": [],
  "total": 1000,
  "limit": 20,
  "offset": 40
}
```

问题：数据变化时可能重复/漏项，深分页慢。

**游标分页**：

```http
GET /v1/recharges?limit=20&cursor=eyJjcmVhdGVkX2F0IjoiLi4uIiwiaWQiOjEyM30
```

响应：

```json
{
  "items": [],
  "next_cursor": "eyJjcmVhdGVkX2F0IjoiLi4uIiwiaWQiOjE1MH0=",
  "has_more": true
}
```

游标是服务端编码的位置，客户端不要解析。适合信息流、订单列表等大数据量场景。

**限制 `limit` 上限**（如 100），否则一次查 10 万条会打垮数据库。

### 幂等

创建类接口必须有一个稳定幂等键。本项目把 `req_id` 放在请求体里，并写进 `recharge_order.req_id` 唯一索引：

```http
POST /v1/recharges
Authorization: Bearer <token>
Content-Type: application/json

{"req_id":"req-20261101-0001","amount_cent":100,"channel":"manual"}
```

服务端：

1. 从 token 解析 uid，校验金额和渠道；
2. 用 `req_id` 唯一索引原子占位，不能先查再插；
3. 同 `req_id` 且 `uid/amount_cent/channel` 一致：首次返回 201，重试返回 200 且 `duplicated=true`；
4. 同 `req_id` 但参数不一致：返回 409 `REQ_ID_CONFLICT`。

也有系统把幂等键放在 `Idempotency-Key` 请求头。两种都行，但同一份契约只能选一种并写进 OpenAPI；本讲义后续代码统一使用请求体 `req_id`，避免文档和实现各说各话。GET/PUT/DELETE 在语义上天然幂等，POST 必须依赖业务键或幂等表。

### 时间、金额、ID

时间统一用带时区的 RFC3339 或明确的 UTC 时间戳，金额用最小货币单位整数，ID 用不会丢精度的字符串或 64 位整数。不要让前端自己猜单位，也不要在 JSON 里用浮点数表示钱。

- 时间用 RFC3339 带时区：`2026-11-01T10:00:00.123+08:00`；
- 金额用整数分，字段名带单位：`amount_cent`；
- ID 用字符串返回，避免前端 JS 大整数精度丢失；
- 枚举用字符串，不要用魔法数字。

---

## 8.2 版本与兼容

### 版本放哪里

```http
/v1/recharges
```

URL 版本简单直观，适合大版本。小改动通过**向后兼容**解决，不要每个字段都升版本。

### 兼容规则

兼容的核心是“旧客户端继续工作”。新增可选字段、增加枚举值通常兼容；删除字段、改字段类型、改变默认值、改变错误码语义通常不兼容。发布前要用旧版本客户端和新版本服务做契约测试，而不是只看新文档。

| 变更 | 是否兼容 | 做法 |
|---|---|---|
| 新增可选字段 | 是 | 客户端忽略未知字段 |
| 新增枚举值 | 通常是 | 客户端要有 default 分支 |
| 删除字段 | 否 | 先废弃、观察、再删 |
| 改字段类型 | 否 | 发新字段/新版本 |
| 改错误码含义 | 否 | 新增错误码 |
| 改默认分页大小 | 可能 | 明确文档，评估影响 |

### 废弃流程

废弃接口要经历公告、观测、双版本、最后下线。先统计旧版本调用方和调用量，提供迁移文档和新接口，设置明确的截止日期；没有观测就直接删除，最常见的结果是某个不知名的客户端线上失败。

1. 文档标注 `deprecated`；
2. 响应加 `Deprecation`/`Sunset` 头；
3. 监控老字段/老版本调用量；
4. 通知客户端；
5. 调用量归零后删除。

---

## 8.3 gRPC 与 protobuf

### 什么时候用 gRPC

gRPC 适合服务间高频、强类型、低延迟的调用，尤其是内部服务和多语言团队。它基于 HTTP/2 和 protobuf，能生成客户端和服务端代码，并支持流式传输；代价是浏览器直连、调试和网关支持不如普通 HTTP 方便。

- 服务间内部调用，追求性能/强类型；
- 需要流式（server streaming、client streaming、双向流）；
- 多语言、契约优先；
- 需要代码生成客户端。

### 什么时候不用

对外公开 API、需要简单 curl 调试、主要给浏览器调用、团队没有 proto 工具链时，普通 HTTP/JSON 往往更合适。不要为了“性能更好”让所有调用方都背一套 gRPC 基础设施。

- 浏览器直接调用（gRPC-Web 需要代理，复杂）；
- 对外公开 API，客户端多样；
- 团队不熟悉 protobuf/代码生成；
- 简单 CRUD，HTTP/JSON 足够。

### proto 示例

```proto
syntax = "proto3";

package recharge.v1;

option go_package = "example.com/minicoin/gen/recharge/v1;rechargev1";

service RechargeService {
  rpc CreateRecharge(CreateRechargeRequest) returns (CreateRechargeResponse);
  rpc GetRecharge(GetRechargeRequest) returns (GetRechargeResponse);
  rpc WatchRecharge(WatchRechargeRequest) returns (stream RechargeEvent);
}

message CreateRechargeRequest {
  string req_id = 1;
  string uid = 2;
  int64 amount_cent = 3;
  string channel = 4;
}

message CreateRechargeResponse {
  string order_id = 1;
  string status = 2;
  int64 balance_cent = 3;
}
```

生成：

```bash
protoc --go_out=. --go-grpc_out=. api/recharge/v1/recharge.proto
```

### proto 演进规则

protobuf 字段编号是线上契约，不能复用已删除的编号。新增字段用新编号并保持可选，删除字段要保留编号和名称，修改类型要新增字段并做双写/迁移。客户端应对未知字段和未知枚举值保持兼容。

- 字段编号一旦使用不能复用；
- 删除字段用 `reserved 3;` 保留编号；
- 不要改字段类型（int32 → int64 也可能不兼容）；
- 新增字段用新编号，客户端忽略未知字段；
- 枚举加 `UNSPECIFIED = 0`；
- 包名/服务名变更等于新 API。

```proto
message User {
  reserved 4, 7 to 9;
  reserved "old_name";
  string id = 1;
  string name = 2;
}
```

### gRPC 超时与取消

gRPC 的 deadline 会沿 context 传到服务端，服务端应把同一个 ctx 继续传给数据库和下游，不能新建一个没有截止时间的 background context。客户端取消后，服务端要尽快停止无用工作；长流式 RPC 还要处理半关闭、背压和连接中断。

```go
ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
defer cancel()

resp, err := client.CreateRecharge(ctx, req)
if status.Code(err) == codes.DeadlineExceeded {
    // 超时，按幂等键重试或查询结果
}
```

gRPC 的错误用 status code：

| code | 含义 |
|---|---|
| `InvalidArgument` | 参数错误 |
| `Unauthenticated` | 未认证 |
| `PermissionDenied` | 无权限 |
| `NotFound` | 不存在 |
| `AlreadyExists` | 冲突 |
| `ResourceExhausted` | 限流/配额 |
| `Unavailable` | 依赖不可用 |
| `DeadlineExceeded` | 超时 |
| `Internal` | 内部错误 |

### 拦截器

```go
func AuthInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
    md, ok := metadata.FromIncomingContext(ctx)
    if !ok || len(md.Get("authorization")) == 0 {
        return nil, status.Error(codes.Unauthenticated, "missing token")
    }
    return handler(ctx, req)
}
```

拦截器适合做鉴权、日志、指标、恢复 panic、超时。顺序要固定并测试。

---

## 8.4 超时、重试、熔断

### 超时必须逐层传递

```
客户端 1s → 网关 800ms → 服务 500ms → 数据库 300ms
```

下游超时应小于上游，否则上游已经放弃，下游还在做无用功。

### 重试规则

重试只适用于“可能短暂恢复且重复执行安全”的错误。4xx 参数错误通常不重试，429 按 `Retry-After` 退避，连接失败/超时可以有限重试；支付、扣库存等操作必须携带幂等键，并给重试设置总预算，不能让每层都各重试三次形成指数放大。

- 只重试**幂等**操作；
- 只重试**可恢复**错误（超时、503、连接失败）；
- 指数退避 + 抖动；
- 限制总次数和总预算；
- 熔断打开时不重试。

```go
backoff := 50 * time.Millisecond
for i := 0; i < 3; i++ {
    resp, err := call(ctx)
    if err == nil { return resp, nil }
    if !isRetryable(err) { return nil, err }
    select {
    case <-ctx.Done(): return nil, ctx.Err()
    case <-time.After(backoff):
    }
    backoff *= 2
}
```

### 熔断

见第 10 章。接口设计时要明确：下游不可用时返回 503 还是降级数据？客户端怎么处理？

---

## 8.5 接口设计检查清单

```markdown
- [ ] URL 用名词，方法语义正确
- [ ] 状态码和业务 code 都有，且稳定
- [ ] 错误体包含 request_id
- [ ] 分页有 limit 上限和稳定排序
- [ ] 创建接口有幂等键
- [ ] 时间/金额/ID 格式明确
- [ ] 鉴权和越权检查
- [ ] 超时、重试、限流、熔断策略
- [ ] 版本/兼容/废弃策略
- [ ] 文档有 curl 示例和错误码
- [ ] 有契约测试
```

---

## 习题

### 基础题

1. 为什么 URL 用名词、方法表达动作？
2. 400、401、403、404、409、429、503 分别什么时候用？
3. 为什么不能所有错误都返回 200？
4. Offset 分页和游标分页各适合什么场景？
5. 创建接口为什么需要幂等键？参数不一致时返回什么？
6. proto 字段编号为什么不能复用？删除字段怎么做？
7. gRPC 的 `DeadlineExceeded` 和 `Unavailable` 有什么区别？
8. 什么情况下不该用 gRPC？

### 动手题

9. 设计“查询用户订单列表”接口：URL、参数、分页、响应、错误码。
10. 给“创建充值单”写 proto 定义，包含幂等键和金额单位。
11. 写一个 gRPC 拦截器，统一记录耗时和错误码。
12. 设计一个兼容方案：把响应里的 `status` 从数字改成字符串，老客户端不能崩。

### 综合题

13. 一个对外 API 要被 3 个客户端（Web、iOS、Android）使用，版本升级时如何保证不破坏老客户端？写出兼容、灰度、废弃的完整策略。

---

## 参考答案

<details>
<summary>展开</summary>

1. URL 标识资源，HTTP 方法表达动作，语义清晰，网关、缓存、监控和客户端都能按统一规则处理。不要用 `/getOrder`、`/updateOrder` 这类 RPC 风格路径混在 REST API 中。

2. 400 请求参数或格式错误；401 未认证；403 已认证但无权限；404 资源不存在；409 业务冲突或幂等参数不一致；429 限流；503 依赖不可用或熔断。状态码和业务 code 要成对映射。

3. 网关、监控、客户端重试和告警都依赖 HTTP 状态码；全部返回 200 会把参数错误、权限错误和服务故障混在一起，客户端只能解析字符串猜测。

4. Offset 适合后台管理、数据量小、可以接受数据变化导致页漂移；游标适合信息流和大数据量，编码稳定位置并限制 limit，避免深分页扫描。

5. 创建接口需要幂等键防止客户端超时重试重复创建或扣款。同键同参数返回首次结果，同键不同参数返回 409 `REQ_ID_CONFLICT`。

6. protobuf 字段编号是线上契约，复用编号会让旧数据按新类型解析。删除字段要 `reserved` 编号和名称，新增字段用新编号，不能原地改类型。

7. `DeadlineExceeded` 是调用超过 deadline；`Unavailable` 是连接失败、服务暂时不可用或过载。超时通常不能立即重试，未知结果要先按幂等键查询；不可用可以按退避和熔断策略重试。

8. 浏览器直连、公开 API、调用方多样、团队不熟 protobuf/代码生成、简单 CRUD 且 HTTP/JSON 足够时，不用 gRPC。

9. 查询订单列表契约：

```http
GET /v1/orders?limit=20&cursor=eyJpZCI6OTAwMDF9
Authorization: Bearer <token>
```

| 参数 | 类型 | 规则 |
|---|---|---|
| `limit` | integer | 默认 20，必须 1..100 |
| `cursor` | string | 可选，服务端不透明游标 |

成功：

```json
{
  "code": "OK",
  "message": "success",
  "request_id": "req-demo-001",
  "data": {
    "items": [
      {"order_id": "90001", "amount_cent": 1000, "status": "SUCCEEDED", "created_at": "2026-11-01T10:00:00+08:00"}
    ],
    "next_cursor": "eyJpZCI6OTAwMDF9",
    "has_more": true
  }
}
```

错误：400 `INVALID_CURSOR`/`INVALID_LIMIT`，401 `UNAUTHENTICATED`，500 `INTERNAL`。服务端只按 token 的 uid 查询，不能用 query 参数传 uid。

10. proto 定义：

```proto
syntax = "proto3";

package coin.v1;

option go_package = "example.com/minicoin/gen/coin/v1;coinv1";

service CoinService {
  rpc CreateRecharge(CreateRechargeRequest) returns (CreateRechargeResponse);
  rpc GetRecharge(GetRechargeRequest) returns (GetRechargeResponse);
}

message CreateRechargeRequest {
  string req_id = 1;       // 幂等键
  int64 amount_cent = 2;   // 单位：分，必须 > 0
  string channel = 3;      // 白名单
  string request_id = 4;   // 链路追踪，可选
}

message CreateRechargeResponse {
  string order_id = 1;
  string uid = 2;
  int64 balance_cent = 3;
  bool duplicated = 4;
}

message GetRechargeRequest {
  string order_id = 1;
}
message GetRechargeResponse {
  string order_id = 1;
  string status = 2;
  int64 amount_cent = 3;
}
```

uid 从认证上下文取，不放进请求业务字段。生成后用 `buf lint`、`buf breaking` 和契约测试检查兼容性。

11. unary 拦截器：

```go
func UnaryMetrics(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err).String()
		logger.Info("grpc_request",
			"method", info.FullMethod,
			"code", code,
			"duration_ms", time.Since(start).Milliseconds(),
			"trace_id", traceIDFromContext(ctx),
		)
		return resp, err
	}
}
```

注册：

```go
srv := grpc.NewServer(grpc.UnaryInterceptor(UnaryMetrics(logger)))
```

验证：成功请求记录 `OK`，下游超时记录 `DeadlineExceeded`；日志不能打印 token、完整请求体或敏感字段。

12. 兼容方案不是直接改 JSON 类型。新增字符串字段：

```json
{
  "status": 1,
  "status_text": "SUCCEEDED"
}
```

服务端双写一段时间；新客户端读 `status_text`，老客户端继续读 `status`；监控 `status` 的调用方。新大版本再让 `status` 变成字符串，或新建 v2 响应。发布前用老客户端契约测试，确认旧字段和旧枚举仍可用。

13. 升级策略：

1. 先冻结兼容规则：只加字段、不删字段、不改类型、不改错误码语义。
2. 在 v1 增加可选字段，Web/iOS/Android 分客户端版本灰度读取。
3. 文档标注 deprecated，响应加 `Deprecation`/`Sunset`，给迁移截止时间。
4. 监控每个客户端版本、旧字段调用量和错误码，发现异常立即暂停灰度。
5. 至少保留一个兼容窗口；确认调用量归零后再删除旧字段。
6. 破坏性变化发 `/v2`，不要原地改 `/v1`。

验证证据包括旧版本客户端的契约测试、灰度期间错误率/P99、旧字段调用曲线和回滚演练。成功标准不是“新客户端能用”，而是旧客户端在整个迁移窗口内继续工作。

</details>
