# 第 24 章 接口文档与自测：让前后端不再靠猜

> 接口文档不是“写完代码后抄一份 URL”，而是开发前就要冻结的行为契约。
> 本章用虚构 `mini-coin` 接口示范 OpenAPI、curl、Postman、自动化测试和 Mock，所有示例都可直接修改后运行。

---

## 24.1 接口契约包含什么

一个能指导开发的接口必须写清楚：

| 类别 | 必须写 | 例子 |
|---|---|---|
| 路径 | 方法、路径、版本 | `POST /v1/recharges` |
| 请求 | 字段、类型、必填、范围 | `amount_cent: int64 > 0` |
| 响应 | 成功结构、失败结构 | `code/data` |
| 状态码 | 协议层结果 | 201 首次 / 200 幂等 / 400 / 409 / 500 |
| 业务码 | 稳定领域结果 | `ACCOUNT_FROZEN` |
| 幂等 | 重试是否安全 | `req_id` |
| 鉴权 | 谁可以调用 | Bearer token |
| 超时 | 客户端最多等多久 | 3s |
| 错误 | 所有可预期错误 | 8 个错误码 |
| 示例 | 可直接复制运行 | curl + JSON |
| 变更 | 兼容性策略 | 只加字段不删字段 |

缺少任意一项，前后端就会在联调时靠口头补。

这份表不是让人“填满就行”。接口契约的目标是让调用方在不看后端代码的情况下，知道该怎么发请求、怎么判断成功、失败后能不能重试、超时后去哪里查结果。例如只有 `POST /v1/recharges` 和成功 JSON，前端仍然不知道断网重试会不会重复扣钱；只有错误码没有“客户端动作”，前端仍然会把 409 当成 500 无限重试。

看一份契约是否合格，可以假装自己是前端，回答下面五道题：

1. 请求体每个字段的单位、范围、默认值和示例是什么？
2. 成功时 HTTP 状态和业务 `code` 分别是什么？
3. 参数错误、未登录、无权限、资源不存在、业务冲突、依赖故障分别返回什么？
4. 用户点击重试时，哪些字段必须原样复用？
5. 收到 500 或超时，前端如何查询“这笔请求到底有没有成功”？

五道题都能从文档找到答案，才叫可联调。剩下的实现细节可以变更，但以上行为属于对外契约。

一个完整契约通常包含四份内容：OpenAPI/Machine-readable schema、带业务语义的文字说明、可复制执行的请求示例、自动化契约测试。只有 YAML 没有文字，读者不知道 `req_id` 的重试规则；只有文字没有 schema，生成不了客户端；有文档没有契约测试，代码改了字段名也没人发现。

---

## 24.2 设计接口的顺序

先定业务动作，再定 HTTP 表达，最后才写字段。顺序反了时，代码会围绕数据库表设计 URL，前端只能看到一个“万能 POST”，错误处理也没有稳定语义。下面的顺序中，幂等、权限和错误码必须与请求体同时评审，不能等接口写完再补。

```text
业务动作
  -> 资源和动作
  -> 请求/响应
  -> 错误码
  -> 幂等和重试
  -> 权限
  -> 示例
  -> 契约测试
```

### 先确认动作语义

URL 要让调用方一眼看出资源是什么、动作是创建还是查询。创建订单是 `POST`，查询订单是 `GET`，不能因为“查询参数多”就把查询改成 `POST`；也不要让 GET 产生余额变化。资源 ID 应放在路径，过滤和分页放在 query。

```text
POST /v1/recharges          创建一笔充值
GET  /v1/recharges/{id}     查询某一笔充值
GET  /v1/accounts/{uid}/balance  查询账户余额
```

不要把“充值和查询”塞进同一个 POST，也不要用 GET 修改余额。

### 版本策略

对外接口使用 `/v1/`。新增可选字段通常兼容；删除字段、修改字段类型、改变错误语义是破坏性变更，需要新版本或明确迁移期。

---

## 24.3 完整 OpenAPI 示例

```yaml
openapi: 3.0.3
info:
  title: Mini Coin API
  version: 1.0.0
  license:
    name: MIT
    url: https://opensource.org/license/MIT
  description: 虚构充值服务，用于后端教学；不包含任何真实公司或业务信息。
servers:
  - url: http://127.0.0.1:8081
paths:
  /v1/recharges:
    post:
      summary: 创建充值订单
      operationId: createRecharge
      description: |
        首次创建一个 req_id 返回 201 Created；同一个 req_id 且请求参数一致时，
        返回首次结果并标记 duplicated=true，HTTP 状态为 200 OK。
      security:
        - bearerAuth: []
      parameters:
        - in: header
          name: X-Request-Id
          required: false
          description: 客户端追踪 ID；缺失或超过 64 字符时由服务端生成。
          schema:
            type: string
            maxLength: 64
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/RechargeRequest'
            example:
              req_id: demo-1
              amount_cent: 10
              channel: manual
      responses:
        '201':
          description: 首次创建成功
          headers:
            X-Request-Id:
              schema: {type: string}
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/RechargeResponse'
              example:
                code: OK
                message: success
                request_id: trace-demo-001
                data:
                  uid: u-1001
                  balance: 110
                  duplicated: false
        '200':
          description: 幂等命中，请求参数与首次请求一致
          headers:
            X-Request-Id:
              schema: {type: string}
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/RechargeResponse'
              example:
                code: OK
                message: success
                request_id: trace-demo-002
                data:
                  uid: u-1001
                  balance: 110
                  duplicated: true
        '202':
          description: 相同 req_id 的首次请求仍在处理中
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '400':
          description: JSON、金额、req_id 或 channel 不合法
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '401':
          description: 未提供 Bearer token
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '405':
          description: HTTP 方法不是 POST
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '409':
          description: 幂等键冲突、账户冻结或原请求已失败
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '500':
          description: 内部错误；响应不会泄漏 SQL、堆栈或依赖地址
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT
  schemas:
    RechargeRequest:
      type: object
      additionalProperties: false
      required: [req_id, amount_cent, channel]
      properties:
        req_id:
          type: string
          minLength: 1
          maxLength: 64
          description: 客户端生成，并在重试时原样复用。
        amount_cent:
          type: integer
          format: int64
          minimum: 1
          maximum: 100000000
          description: 充值金额，单位是分；字段名自带单位。
        channel:
          type: string
          enum: [manual, coupon]
    RechargeResponse:
      type: object
      required: [code, message, request_id, data]
      properties:
        code:
          type: string
          example: OK
        message:
          type: string
          example: success
        request_id:
          type: string
        data:
          type: object
          required: [uid, balance, duplicated]
          properties:
            uid:
              type: string
              example: u-1001
            balance:
              type: integer
              format: int64
              description: 充值后的可用余额，单位是分。
            duplicated:
              type: boolean
    Error:
      type: object
      required: [code, message, request_id]
      properties:
        code:
          type: string
          enum:
            - UNAUTHORIZED
            - INVALID_JSON
            - INVALID_AMOUNT
            - INVALID_REQ_ID
            - INVALID_CHANNEL
            - MISSING_REQ_ID
            - MISSING_CHANNEL
            - REQ_ID_CONFLICT
            - PROCESSING
            - PREVIOUSLY_FAILED
            - ACCOUNT_NOT_FOUND
            - ACCOUNT_FROZEN
            - METHOD_NOT_ALLOWED
            - INTERNAL_ERROR
        message:
          type: string
          description: 给人阅读的稳定说明，客户端逻辑不能依赖具体文案。
        request_id:
          type: string
```

### 文档中必须补充的说明

OpenAPI 只能描述结构，不能替代以下文字：

```text
1. req_id 在客户端重试时必须复用；
2. 余额单位为分；
3. channel 当前只接受 manual/coupon；
4. 请求超时建议 3 秒；
5. 409 只表示业务状态冲突，不代表客户端应无限重试；
6. 查询余额缓存最多可能延迟 30 秒；
7. 所有金额字段都用 int64 表达。
```

逐段读这份 OpenAPI 时，要注意它为什么这样写：

| 定义 | 作用 | 如果写错会怎样 |
|---|---|---|
| `required: [req_id, amount_cent, channel]` | 告诉生成器和测试哪些字段必填 | 前端漏传 `req_id`，后端幂等失效 |
| `minimum: 1` | 表示金额最小单位 | 前端传 0 或负数仍被放行 |
| `example: OK` | 给文档和 Mock 一个成功样例 | Mock 返回的数据与真实结构不一致 |
| `409` 响应 | 统一描述业务冲突 | 前端把“冻结账户”误判为服务端故障 |
| `request_id` | 用于日志和跨端追踪 | 出错时无法唯一关联请求 |
| `additionalProperties` | 控制是否允许未知字段 | 拼错字段名被静默忽略，产生难查 bug |

严格一点的系统会禁止未知字段，本示例用 `additionalProperties: false` 并在 handler 里 `DisallowUnknownFields()`。`amount_cent` 使用 `int64` 也有边界：OpenAPI 的 `integer` 在某些生成器里可能映射成 JS `number`，超过 `Number.MAX_SAFE_INTEGER` 会丢精度。教学金额设了业务上限，生产若金额更大，应约定以字符串传输或前后端都使用 bigint。

响应结构最好区分“成功 data”和“错误 error”，不要把错误信息塞进成功 data。业务码要放在稳定位置，`message` 只用于人类阅读，前端逻辑不能依赖它的具体文案。后续把 “account is frozen” 改成 “账户已冻结” 时，不应导致前端判断失败。

---

## 24.4 错误码设计

### 错误响应

错误响应要同时给出协议层状态和稳定业务码：网关、监控和通用客户端先看 HTTP，业务代码再按 `code` 分支，`message` 只用于人阅读。响应必须带 `request_id`，否则用户截图里只有一个 500，服务端无法把请求和日志对应起来。内部堆栈、SQL 和依赖地址只能进受控日志。

```json
{
  "code": "ACCOUNT_FROZEN",
  "message": "account is frozen",
  "request_id": "trace-demo-001"
}
```

### 错误码表

| HTTP | code | 含义 | 客户端动作 |
|---|---|---|---|
| 400 | `INVALID_AMOUNT` | 金额不是正整数分 | 修参数 |
| 400 | `INVALID_REQ_ID` | `req_id` 超过 64 字符 | 生成更短的键 |
| 400 | `INVALID_CHANNEL` | 渠道不在 `manual/coupon` 中 | 修参数 |
| 400 | `MISSING_REQ_ID` | 缺幂等键 | 修参数 |
| 404 | `ACCOUNT_NOT_FOUND` | 账户不存在 | 提示用户或创建账户 |
| 409 | `ACCOUNT_FROZEN` | 账户冻结 | 不重试，走客服 |
| 409 | `REQ_ID_CONFLICT` | 同一 req_id 参数不一致 | 不重试，查请求记录 |
| 500 | `INTERNAL_ERROR` | 服务端未知错误 | 用同一 req_id 查询或有限重试 |
| 503 | `SERVICE_UNAVAILABLE` | 依赖不可用/熔断 | 退避重试 |

### 错误码规则

错误码是长期契约，不能随着某次文案优化而改名。建议在代码里集中定义常量，由 service 返回领域错误，handler 只做一次映射；这样同名业务错误不会在不同接口出现两个拼写。新增错误码要同步文档、客户端动作和契约测试。

- 对外稳定，不随日志文案变化；
- 同一错误在不同服务保持一致；
- 不把 SQL、堆栈、内部 IP 返回给客户端；
- 错误响应也带 `request_id`；
- 文档列出错误码，不让前端猜。

实现时建议把错误码定义成常量或枚举，而不是在每个 handler 手写字符串。service 返回带类型的领域错误，HTTP 层集中映射一次。例如 `ErrInvalidAmount -> 400 INVALID_AMOUNT`、`ErrReqIDConflict -> 409 REQ_ID_CONFLICT`、`ErrDependencyUnavailable -> 503 SERVICE_UNAVAILABLE`。这样新增接口时不会出现同一个业务错误在这个接口叫 `FROZEN`、另一个接口叫 `ACCOUNT_DISABLED`。

错误信息分三层：HTTP 状态给网关和通用客户端使用；业务码给业务代码分支使用；`message` 给人看。日志里还要有内部错误详情，但不要返回给客户端。比如数据库死锁可以内部记录 `deadlock found`，对外统一返回 503 或可重试错误，避免泄漏表结构。

可重试性也是契约的一部分。前端需要知道 429 看 `Retry-After`、503 可以指数退避、409 不应自动重试、400 应该修参数。文档没有写清楚，前端只能凭 HTTP 状态猜，最后就会把幂等冲突重试成更多请求。

---

## 24.5 curl 自测

### 成功请求

一次成功的 curl 自测要分三层验证：HTTP 层看状态码和 `Content-Type`，业务层看 `code` 和字段，数据层重新查询余额/订单确认副作用。执行前先记录余额，执行后保存响应和查询结果；不要把“终端有 JSON”当成功。

```bash
curl -i -X POST http://127.0.0.1:8081/v1/recharges \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -H 'X-Request-Id: trace-demo-001' \
  -d '{
    "req_id": "r-20261101-001",
    "amount_cent": 50,
    "channel": "manual"
  }'
```

首次创建预期 `201 Created`；如果这个 `req_id` 已经成功过，则返回 `200 OK`。两种响应都必须带同一个 `X-Request-Id`。

```json
{
  "code": "OK",
  "message": "success",
  "request_id": "trace-demo-001",
  "data": {
    "uid": "u-1001",
    "balance": 150,
    "duplicated": false
  }
}
```

### 幂等验证

幂等测试必须复用同一个 `req_id`，同时记录第二次响应和最终余额。第一次请求可能因为客户端超时其实已经成功，所以第二次返回首次结果并不奇怪；真正错误是第二次又加了一次钱。再补一个“同 req_id、不同金额”的用例，确保服务返回 409 而不是悄悄复用旧结果。

```bash
for i in 1 2; do
  curl -sS -o "/tmp/idem-$i.json" -w "attempt=$i status=%{http_code}\n" \
    -X POST http://127.0.0.1:8081/v1/recharges \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer demo-token' \
    -H "X-Request-Id: trace-idem-$i" \
    -d '{
      "req_id": "r-idem-001",
      "amount_cent": 10,
      "channel": "manual"
    }'
done
jq -e '.code=="OK" and .data.duplicated==false' /tmp/idem-1.json
jq -e '.code=="OK" and .data.duplicated==true' /tmp/idem-2.json
```

验收要同时看响应和最终余额：

```text
第一次 status=201，duplicated=false，余额增加 10
第二次 status=200，duplicated=true，余额不再增加
订单表只有一行 req_id=r-idem-001，流水也只有一条
```

如果第二次又增加 10，先查唯一索引是否真的存在、插入冲突后是否返回首次结果，以及余额更新是否在独立事务里重复执行。
只断言 `duplicated=true` 不够，因为响应字段可能正确而数据库已经重复记账。

### 错误输入

错误输入要逐个验证“返回正确”和“状态未改变”。例如非法金额应返回 400 `INVALID_AMOUNT`，查询数据库确认没有新增订单；非 JSON 应返回 400 而不是泄露解析器堆栈；方法错误应返回 405/404 且不进入写事务。只检查状态码会漏掉“先写了一半再报错”的实现。

```bash
AUTH='Authorization: Bearer demo-token'
URL=http://127.0.0.1:8081/v1/recharges

# 非法金额：400 INVALID_AMOUNT
curl -sS -w '\nstatus=%{http_code}\n' -X POST "$URL" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"req_id":"r-bad","amount_cent":0,"channel":"manual"}'

# 缺 req_id：400 MISSING_REQ_ID
curl -sS -w '\nstatus=%{http_code}\n' -X POST "$URL" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"amount_cent":10,"channel":"manual"}'

# 未知字段：400 INVALID_JSON
curl -sS -w '\nstatus=%{http_code}\n' -X POST "$URL" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"req_id":"r-x","uid":"u-1001","amount_cent":10,"channel":"manual"}'

# 未认证：401 UNAUTHORIZED
curl -sS -w '\nstatus=%{http_code}\n' -X POST "$URL" \
  -H 'Content-Type: application/json' \
  -d '{"req_id":"r-noauth","amount_cent":10,"channel":"manual"}'

# 方法错误：405 METHOD_NOT_ALLOWED
curl -sS -w '\nstatus=%{http_code}\n' -X GET "$URL"
```

### 查看耗时

耗时自测要排除测试机、日志和首次连接带来的噪声，至少连续执行几次看范围。`time_total` 是客户端总耗时，包含连接、TLS、服务端处理和响应传输；要定位慢在哪一段，还需要服务端 trace 和数据库慢查询。单次 200ms 不能证明满足 P99 目标。

```bash
curl -sS -o /tmp/resp.json \
  -w 'status=%{http_code} time=%{time_total}\n' \
  -X POST http://127.0.0.1:8081/v1/recharges \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -d '{"req_id":"r-time","amount_cent":1,"channel":"manual"}'
cat /tmp/resp.json
```

每条 curl 都要有“预期结果”，否则只是把请求发出去。成功请求至少验证 HTTP 状态、`code`、`data.uid`、余额和 `request_id`；错误请求验证 HTTP 状态、业务码、没有内部堆栈，并且数据库状态没有变化。只打印响应不叫自测。

执行顺序也有讲究：先测只读接口，再测写接口；写接口记录执行前余额，执行后重新查询确认；幂等测试要连续发送同一个 `req_id`，并比较两次的 `balance_after`。测试之间使用不同 `req_id`，避免前一个用例的状态污染后一个用例。所有示例使用虚构账号和本地域名，不能拿真实环境练手。

---

## 24.6 Postman / Insomnia / Bruno 怎么用

工具不是重点，流程才是。Postman 适合交互式联调和分享 collection，Insomnia 更轻，Bruno 把 collection 以文件形式放进 Git，方便评审。选择哪个取决于团队协作方式，不是后端能力的核心。

推荐每个接口建立：

```text
Environment: local / test
Request: method, URL, headers, body
Pre-request: 生成 request_id、时间戳
Tests: 状态码、业务码、字段类型、幂等断言
Collection: 按业务链路组织
```

环境变量只放非敏感配置，例如 `baseUrl`、`uid`。token 不要提交到 collection 文件；使用运行时 secret 或本地环境变量，并确保导出、截图、日志不会带出凭证。Mock 域名和真实域名分环境，避免误点一次就打到生产。

Postman 的 Tests 是 JavaScript，和前端的断言习惯接近；Go 测试更适合 CI、并发和精确控制数据库状态。两者最好共用同一份契约和同一组场景名，避免 Postman 里叫“success”，Go 里叫“happy path”，最后没人知道覆盖是否一致。

### Postman Tests 示例

下面的断言覆盖状态码、业务码、字段类型和 request_id 一致性。它适合联调时快速发现契约漂移，但不能替代并发与数据库断言；涉及金额、幂等和权限的用例仍应在 Go 测试里执行。

```js
const body = pm.response.json();

pm.test('status is 201 on first create or 200 on idempotent replay', () => {
  pm.expect([200, 201]).to.include(pm.response.code);
});

pm.test('success code', () => {
  pm.expect(body.code).to.eql('OK');
});

pm.test('balance is integer cents', () => {
  pm.expect(Number.isInteger(body.data.balance)).to.be.true;
});

pm.test('request id is returned in header and body', () => {
  pm.expect(body.request_id).to.eql(pm.response.headers.get('X-Request-Id'));
});
```

### 变量

环境变量把域名、账号和请求 ID 与请求定义分开，方便本地、测试、预发切换。`token` 只应来自本地 secret 或运行时注入，不能写进 Collection 并提交到 Git；示例中的 `requestId` 每次运行重新生成，重试场景则要刻意为同一业务请求复用同一个值。

```text
baseUrl = http://127.0.0.1:8081
uid = u-1001
requestId = {{$guid}}
```

请求体：

```json
{
  "req_id": "{{requestId}}",
  "amount_cent": 10,
  "channel": "manual"
}
```

### 自测集合至少包含

集合要覆盖正常、边界、错误、权限、并发和下游故障，而不是只收集几个快乐路径。每个请求的 Tests 都应断言状态码、业务码和关键字段；写请求还要在后续请求或脚本中核对余额/订单数。集合在本地通过后，再到测试环境验证网关、鉴权和真实依赖。

- 正常充值；
- 重复 req_id；
- 参数错误；
- 未授权；
- 不存在资源；
- 下游超时；
- 限流；
- 响应字段存在；
- 错误响应不泄漏内部信息。

Postman 适合交互式验证和协作；CI 回归更适合 Go 测试、HTTP 文件或脚本。两者不是二选一。

---

## 24.7 用 Go 写接口测试

### httptest

```go
func TestRechargeHandler(t *testing.T) {
    svc := newTestService()
    h := httpapi.New(svc).Routes()

    req := httptest.NewRequest(http.MethodPost, "/v1/recharges",
        strings.NewReader(`{"req_id":"r-1","amount_cent":10,"channel":"manual"}`))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer demo-token")
    req.Header.Set("X-Request-Id", "trace-test-1")

    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)

    if rec.Code != http.StatusCreated {
        t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
    }
    if got := rec.Header().Get("X-Request-Id"); got != "trace-test-1" {
        t.Fatalf("request id=%q", got)
    }
}
```

### 表驱动接口测试

```go
tests := []struct {
    name       string
    body       string
    wantStatus int
    wantCode   string
}{
    {"ok", `{"req_id":"r-1","amount_cent":10,"channel":"manual"}`, 201, "OK"},
    {"zero", `{"req_id":"r-2","amount_cent":0,"channel":"manual"}`, 400, "INVALID_AMOUNT"},
    {"missing", `{"amount_cent":10,"channel":"manual"}`, 400, "MISSING_REQ_ID"},
    {"unknown field", `{"req_id":"r-3","uid":"u-1","amount_cent":10,"channel":"manual"}`, 400, "INVALID_JSON"},
}
```

### 契约测试

契约测试检查实现有没有偏离文档：

- 必填字段是否存在；
- 类型是否一致；
- 枚举是否一致；
- 错误码是否在文档列表；
- 响应是否包含 request_id；
- 成功和失败示例能否解析。

可以使用 OpenAPI 校验工具，也可以写轻量 Go 测试。关键是 CI 不能只跑“服务能启动”。

---

## 24.8 Mock 和前后端并行

在后端未完成时，前端需要可用的 Mock。Mock 要与 OpenAPI 共用同一份契约，避免自己手写一份过期 JSON。

### Mock 服务原则

- 返回和真实接口相同的结构；
- 能模拟成功、参数错误、冲突、超时；
- 不返回真实业务数据；
- 不同场景可以通过请求头切换；
- Mock 不是测试通过的依据。

Mock 的价值是让前端先开始写页面，同时迫使后端把契约想清楚。它不能只返回一个“万能成功 JSON”。例如余额接口要展示加载中、空数据、权限不足、服务暂不可用；充值接口要展示参数错误、幂等成功、账户冻结和超时。前端在这些分支上写得出来，联调时才发现契约缺字段，成本会低很多。

Mock 也不要模拟真实业务规则。比如账户冻结到底返回 409 还是 200+业务码，由契约决定；Mock 只是按契约返回。若 Mock 自己发明一套错误格式，前端接完后再改，就失去了并行开发的意义。

联调时先切到真实本地服务跑成功路径，再逐项触发错误。错误路径可以用请求头、特殊 `uid` 或测试数据构造，但不要在生产环境留下可被真实用户触发的后门。

### Mock 场景：每个场景都要有请求和预期响应

只列 `success/frozen/timeout` 三个词，前端仍然不知道页面该渲染什么。Mock 场景应和契约中的错误码一一对应，并能通过固定请求头切换。下面是虚构示例：

```http
POST /v1/recharges
Content-Type: application/json
X-Request-Id: mock-success-001
X-Mock-Scenario: success

{"req_id":"mock-r-001","amount_cent":10,"channel":"manual"}
```

预期：

```json
{
  "code": "OK",
  "message": "success",
  "request_id": "mock-success-001",
  "data": {"uid":"u-1001","balance":110,"duplicated":false}
}
```

```http
X-Mock-Scenario: frozen
```

预期 HTTP 409，body 为 `{"code":"ACCOUNT_FROZEN","message":"account is frozen","request_id":"mock-frozen-001"}`，并且不返回余额变化。

```http
X-Mock-Scenario: timeout
```

预期：连接在 2 秒后断开或返回契约定义的 `504/503`，而不是 HTTP 200 加一句“超时”。前端必须能进入超时态，并用同一个 `req_id` 重试；重试命中幂等后应展示成功或“处理中”，不能重复提交新订单。

Mock 要能自动验证响应结构：字段缺失、类型错误或业务码不在枚举中时，前端契约测试应失败。`X-Mock-Scenario` 是测试专用头，服务端只应在受控的 Mock 进程或测试环境识别它，生产环境必须禁用，防止外部用户构造任意故障场景。

### 联调顺序

```text
契约评审 -> Mock -> 前端接入 -> 后端实现 -> 契约测试
-> 集成联调 -> 真实错误路径 -> 发布前回归
```

---

## 24.9 用真实接口做自测时怎么脱敏

> 不展示、不保存、不传播任何真实 URL、Cookie、token、公司账号、业务字段或数据。下面的流程只使用虚构值。

### 只提取接口知识

从真实文档中提取：

- HTTP 方法和路径形状；
- 请求字段类型；
- 响应结构；
- 鉴权方式；
- 错误码分类；
- 基线和边界。

然后改写成：

```text
路径：/v1/demo-resources
字段：req_id, amount_cent, channel；uid 从 Bearer token 解析
域名：api.example.com
账号：demo
token：<runtime-secret>
数据：u-1001, r-001
```

### 不进入讲义的内容

- 真实内网域名和 IP；
- 真实 Cookie、Authorization、SSO token；
- 真实用户 ID、订单号、金额和状态；
- 真实业务名称和字段含义；
- 内部接口平台、内部文档平台和带单点登录的链接；
- 截图中的账号、密码和二维码。

### 自测前的安全检查

```bash
# 检查脚本和文档里是否有疑似 token
rg -n '(token|secret|password|cookie|authorization)' .
# 检查是否误写内网地址
rg -n 'https?://' .
# 检查示例数据是否来自真实业务
rg -n 'demo-real|production|prod-' .
```

发现真实凭证时：立即停止使用、从文件中删除、报告负责人并按安全流程轮换。不要只把字符串改成 `***` 后继续传播原文件。

---

## 24.10 Bug 出现后的接口排查

### 先保存证据

bug 排查最怕“日志滚走了，请求参数也忘了”。在重试或重启前先保存原始请求（脱敏）、响应、时间、版本、request_id 和用户操作步骤；这些是后续重放和对比的唯一依据。保存不是把生产数据原样贴到聊天工具，敏感字段必须遮罩并放在受控工单中。

```text
请求方法、路径、请求头（脱敏）
请求体（脱敏）
响应状态、响应体
request_id、trace_id
发生时间、版本、环境
用户操作步骤
```

### 判断是哪一层

先根据状态码和响应体判断是客户端、网关还是服务端，再决定看哪类证据。不要看到 500 就直接翻业务代码：500 可能来自参数解析后的空指针、数据库连接池耗尽、依赖超时未映射，也可能是网关把上游异常包装成 500。每一层都要有能排除或确认的证据。

| 现象 | 优先检查 |
|---|---|
| 400 | JSON、字段、类型、必填、长度 |
| 401 | token 过期、签名、时钟 |
| 403 | 权限、资源归属 |
| 404 | 路径、资源是否真的不存在 |
| 409 | 状态冲突、幂等键冲突 |
| 429 | 限流 |
| 500 | 服务端 panic、DB、空指针、未处理错误 |
| 502/504 | 网关、上游超时、服务重启 |

### 最小复现

把生产请求改造成脱敏 curl：

```bash
curl -i -X POST http://127.0.0.1:8081/v1/recharges \
  -H 'Content-Type: application/json' \
  -H 'X-Request-Id: reproduce-001' \
  -H 'Authorization: Bearer demo-token' \
  -d '{"req_id":"repro-001","amount_cent":10,"channel":"manual"}'
```

能稳定复现后，再用测试固定行为：

```go
func TestBugRepro(t *testing.T) {
    // 先写失败断言，再修实现
}
```

### 修复完成标准

修好不是“这次请求 200 了”。要有能复现旧问题的失败测试，修复后在目标环境通过，并确认相邻边界、并发和错误路径没有回归。若涉及契约或网关，还要用前端实际请求验证；若涉及线上数据，要核对副作用而不是只看接口响应。

- 有失败后通过的测试；
- 原始错误路径已覆盖；
- 相邻参数和边界没有回归；
- 日志和错误码没有泄漏；
- 发布后指标恢复；
- 复盘记录根因和防线。

---

## 24.11 自动生成文档和客户端

Go 常见方案：

- 先写 OpenAPI，生成服务端接口骨架和 TypeScript 客户端；
- 代码注释生成 Swagger，适合已有项目；
- protobuf 生成 gRPC 文档和客户端；
- 文档生成后仍要人工补业务语义、错误码和示例。

### 生成不等于正确

生成工具只能保证结构一致，不能保证：

- 幂等语义正确；
- HTTP 状态码合理；
- 错误码稳定；
- 权限没有遗漏；
- 示例数据安全；
- 文档和真实行为一致。

必须增加契约测试和真实联调。

---

## 24.12 发布前接口验收清单

```text
[ ] OpenAPI/文档与实现一致
[ ] 所有必填字段已验证
[ ] 金额、长度、枚举有边界测试
[ ] 重复请求不会重复产生副作用
[ ] 401/403/404/409/429/500/503 至少验证适用项
[ ] 响应带 request_id
[ ] 日志脱敏
[ ] 超时和重试策略明确
[ ] Mock 场景覆盖成功和失败
[ ] curl 示例可复制运行
[ ] 前后端使用同一份契约
[ ] 破坏性变更有版本和迁移说明
```

这份清单要当门禁执行，不能当愿望。建议分三步：第一步在本地启动服务，按 curl 集合跑成功、幂等和主要错误路径；第二步在测试环境用 Postman/自动化脚本跑一遍真实网络、鉴权、网关和数据库；第三步在预发环境验证超时、限流、回滚和监控告警。任何一步失败都不进入下一步。

验收时要保留证据：命令版本、测试环境、时间、请求 ID、响应摘要、数据库前后状态。对不可重复的人工步骤，写成脚本或 Postman test；对必须人工观察的页面，至少记录截图或录屏并脱敏。发布后还要用相同请求做 smoke test，确认线上错误码和文档一致，避免“测试环境能跑，网关改写了响应”。

如果接口有破坏性改动，清单还要增加兼容性验证：旧客户端调用旧字段仍然可用，新客户端调用新字段也正常，迁移期内的双写/双读有监控，旧字段什么时候删除写在计划里。不能今天发版明天删字段，让前端靠运气。

---

## 24.13 习题

1. 一个接口文档只写了成功 JSON，没有错误码，会带来什么联调问题？
2. `POST /v1/recharges` 的幂等键应该由谁生成？为什么不能由服务端自增 ID 代替？
3. 写出 `amount_cent=0`、缺 `req_id`、账户冻结三种错误响应的 HTTP 和业务码。
4. 用 curl 写一个重复请求测试，要求验证最终余额。
5. Postman 测试与 Go 自动化测试分别适合什么场景？
6. 为什么 Mock 服务不能直接复制真实生产响应？
7. 如何把真实接口改写成安全的讲义示例？
8. 一个接口返回 500，但日志里没有 request_id，排障会卡在哪一步？
9. 设计一个至少包含 8 个请求的接口回归集合。
10. 破坏性接口变更如何迁移，不能直接改字段类型？
11. 用同一个 `req_id` 分别发送金额 10 和金额 20，设计预期响应、数据库断言和 curl 证据。

## 参考答案

<details>
<summary>展开参考答案</summary>

### 1. 只写成功 JSON、不写错误码

前端无法区分“参数错”“没权限”“业务冲突”“服务故障”，最常见的后果是：

- 把 409 冻结账户当成 500 无限重试；
- 把 400 参数错误也做指数退避；
- 超时后不知道用哪个键查询结果；
- 测试只能覆盖 happy path。

一个可用的错误响应必须同时有 HTTP 状态、稳定业务码、`message`、`request_id`：

```json
{
  "code": "REQ_ID_CONFLICT",
  "message": "req_id was reused with different parameters",
  "request_id": "trace-demo-001"
}
```

**验证**：让前端只依据文档写分支，不看后端代码；把每个错误码映射到“修参数 / 重新登录 / 走客服 / 退避重试 / 查结果”之一。映射不出来就说明文档不合格。

---

### 2. 幂等键由谁生成

由**客户端**生成，并在重试时原样复用。服务端自增 ID 每次请求都不同，无法识别“这是同一笔业务请求”。

```http
POST /v1/recharges
Authorization: Bearer demo-token
Content-Type: application/json

{"req_id":"client-7f3a-001","amount_cent":100,"channel":"manual"}
```

规则：

- `req_id` 在客户端一次业务动作开始时生成，重试不重新生成；
- 服务端以 `UNIQUE KEY uk_req_id` 兜底；
- 同 `req_id` 不同参数返回 409 `REQ_ID_CONFLICT`；
- 服务端不能因为客户端没传就悄悄生成一个，否则重试必然重复。

**验证**：用同一个 `req_id` 连发两次，第二次 `duplicated=true` 且余额只增加一次；换金额后仍用同一 `req_id`，必须 409。

---

### 3. 三种错误响应

| 场景 | HTTP | code | 客户端动作 |
|---|---|---|---|
| `amount_cent=0` | 400 | `INVALID_AMOUNT` | 修参数 |
| 缺 `req_id` | 400 | `MISSING_REQ_ID` | 补幂等键 |
| 账户冻结 | 409 | `ACCOUNT_FROZEN` | 不重试，走客服 |

```json
{"code":"INVALID_AMOUNT","message":"amount_cent must be a positive integer","request_id":"trace-1"}
```

```json
{"code":"MISSING_REQ_ID","message":"req_id is required","request_id":"trace-2"}
```

```json
{"code":"ACCOUNT_FROZEN","message":"account is frozen","request_id":"trace-3"}
```

**验证**：三类请求执行前后都查订单数和余额，必须完全不变；响应体不能出现 SQL、表名或堆栈。

---

### 4. curl 重复请求测试

```bash
set -euo pipefail
URL=http://127.0.0.1:8081/v1/recharges
AUTH='Authorization: Bearer demo-token'

before=$(mysql -N -e "SELECT balance FROM mini_coin.account WHERE uid='u-1001'")

for i in 1 2; do
  code=$(curl -sS -o "/tmp/recharge-$i.json" -w '%{http_code}' \
    -X POST "$URL" \
    -H "$AUTH" \
    -H 'Content-Type: application/json' \
    -H "X-Request-Id: trace-idem-$i" \
    -d '{"req_id":"r-idem-001","amount_cent":10,"channel":"manual"}')
  echo "attempt=$i status=$code body=$(cat /tmp/recharge-$i.json)"
done

after=$(mysql -N -e "SELECT balance FROM mini_coin.account WHERE uid='u-1001'")

[ "$code" = "200" ]
[ "$((after - before))" -eq 10 ]
jq -e '.data.duplicated == true' /tmp/recharge-2.json
echo "PASS: balance +10 only once"
```

预期：第一次 201、第二次 200；差值正好 10；第二次 `duplicated=true`。

**失败排查**：差值 20 → 唯一键缺失或幂等逻辑未生效；第二次 201 → 冲突后没有回查；第二次 409 → 参数比较把 `uid` 或 `channel` 比错。

---

### 5. Postman 与 Go 测试

| 维度 | Postman/Bruno | Go 测试 |
|---|---|---|
| 人工联调 | 强 | 弱 |
| 环境变量与分享 | 强 | 需自己写 |
| CI 门禁 | 可跑 Newman | 原生 |
| 并发/竞态 | 弱 | 强 |
| 数据库断言 | 需脚本 | 直接 |
| 精确控制依赖故障 | 弱 | 强 |

推荐组合：

- Postman 保留可导出的 smoke collection，给前端和测试同学用；
- Go `httptest` + 集成测试承担 CI 门禁；
- 两者引用同一份 `api/openapi.yaml` 和同一组场景名；
- Postman 的 token 从环境变量注入，不写进仓库。

**验证**：故意把 handler 的 `amount_cent` 改成 `amount`，Postman 契约断言和 Go 契约测试都应该失败；只修一处不算通过。

---

### 6. Mock 为什么不能复制真实生产响应

真实响应可能包含：

- 用户 ID、手机号、订单号、余额；
- 内部错误文本、SQL、内网地址；
- 真实业务字段和枚举；
- 可被反推出真实业务规模的 ID。

正确做法是基于 OpenAPI 构造虚构数据：

```yaml
examples:
  success:
    value:
      code: OK
      message: success
      request_id: trace-demo-001
      data:
        uid: u-1001
        balance: 110
        duplicated: false
```

Mock 还要覆盖 400/401/409/500，而不是只返回成功。**验证**：对 mock 响应跑敏感词、内网 IP、真实域名扫描；用 schema 校验器确认结构与 OpenAPI 一致。

---

### 7. 如何把真实接口脱敏成讲义示例

按“保留结构、替换内容”的原则：

| 保留 | 替换 |
|---|---|
| HTTP 方法、路径形状 | 域名、版本前缀、业务路径名 |
| 字段类型、单位、枚举、错误分类 | 字段业务名、用户 ID、订单号、金额 |
| 分页/幂等/鉴权模式 | token、cookie、内网地址 |
| 状态码与业务码的对应关系 | 具体文案、内部错误信息 |

检查步骤：

```bash
rg -n -i '公司域名|公司名|内网域名|内部平台名' .
rg -n -i 'password|secret|token|authorization|cookie' .
rg -n '([0-9]{1,3}\.){3}[0-9]{1,3}' .
```

再人工通读一遍，确认没有可反推出真实业务的信息。**验证**：把示例交给不了解原业务的人看，他只能看到一个通用充值服务，不能说出原公司、部门或真实用户。

---

### 8. 500 没有 request_id 会卡在哪

会卡在“无法把用户看到的错误和后端日志对应起来”。只能靠时间、IP、用户 ID 猜，多个并发请求时几乎无法定位。

正确链路：

```go
// 入口中间件生成/透传
reqID := r.Header.Get("X-Request-Id")
if reqID == "" || len(reqID) > 64 {
    reqID = newRequestID()
}
w.Header().Set("X-Request-Id", reqID)
ctx := context.WithValue(r.Context(), requestIDKey{}, reqID)
next.ServeHTTP(w, r.WithContext(ctx))
```

```go
// 响应和日志使用同一个值
writeError(w, r, 500, "INTERNAL_ERROR")
log.Printf("request_id=%s err=%v", RequestIDFrom(r.Context()), err)
```

**验证**：故意触发 500，复制响应头里的 `X-Request-Id`，执行：

```bash
jq -c 'select(.request_id=="trace-500")' /var/log/minicoin/app.jsonl
```

必须能查到同一次请求的日志；查不到就说明中间件、日志字段或响应头有一处没串上。

---

### 9. 8 个以上请求的回归集合

```text
1. 正常首次充值            -> 201, OK, 余额 +amount_cent
2. 相同 req_id 相同参数     -> 200, duplicated=true, 余额不变
3. 相同 req_id 不同金额     -> 409, REQ_ID_CONFLICT
4. amount_cent=0           -> 400, INVALID_AMOUNT
5. amount_cent=-1          -> 400, INVALID_AMOUNT
6. 缺 req_id               -> 400, MISSING_REQ_ID
7. 未知字段 uid            -> 400, INVALID_JSON
8. 非 JSON                 -> 400, INVALID_JSON
9. 无 Authorization        -> 401, UNAUTHORIZED
10. GET /v1/recharges      -> 405, METHOD_NOT_ALLOWED
11. 20 并发同 req_id       -> 只加一次钱
12. 数据库超时             -> 500, INTERNAL_ERROR, 不泄漏 SQL
```

每个用例都要标注：请求、预期 HTTP、预期业务码、数据库副作用、证据文件。

**验证**：先让旧实现跑这套集合，至少应看到幂等或未知字段用例失败；修复后全绿。测试没有先失败过，就不能证明它真的能发现问题。

---

### 10. 破坏性接口变更怎么迁移

不能直接改字段类型。用扩展-收缩：

```text
1. 新增 amount_cent，旧 amount 保留
2. 服务端双写，响应同时返回两个字段
3. 客户端逐步切到 amount_cent
4. 回填历史数据，监控新旧字段差异
5. 观察一个发布周期，确认旧客户端占比为 0
6. 下线 amount，删除字段前保留回滚窗口
```

每一步都要兼容旧版本实例和旧客户端。**验证**：在灰度期间同时用旧客户端和新客户端请求，旧客户端仍能成功；新旧字段对比任务差异为 0。直接删除字段会让回滚版本启动失败。

---

### 11. 同 req_id 不同金额的完整证据

```bash
URL=http://127.0.0.1:8081/v1/recharges
AUTH='Authorization: Bearer demo-token'

curl -sS -o /tmp/first.json -w 'first=%{http_code}\n' -X POST "$URL" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"req_id":"r-conflict-1","amount_cent":10,"channel":"manual"}'

curl -sS -o /tmp/second.json -w 'second=%{http_code}\n' -X POST "$URL" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"req_id":"r-conflict-1","amount_cent":20,"channel":"manual"}'

jq -e '.code=="OK" and .data.duplicated==false' /tmp/first.json
jq -e '.code=="REQ_ID_CONFLICT"' /tmp/second.json

mysql -e "SELECT req_id, amount_cent, status FROM mini_coin.recharge_order WHERE req_id='r-conflict-1'"
mysql -e "SELECT balance FROM mini_coin.account WHERE uid='u-1001'"
```

预期：

- 第一次 201，订单金额 10；
- 第二次 409 `REQ_ID_CONFLICT`；
- 订单仍只有一行且金额为 10；
- 余额只增加 10；
- 两个响应都带 `request_id`。

**失败场景**：第二次返回 200 且 `duplicated=true` → 服务端只比较了 `req_id`，没比较金额；第二次 201 且余额 +20 → 唯一键没生效；第二次 500 → 冲突后在同一事务里继续查询时数据库语义处理错误。

</details>
