# 第 16 章 接口文档与自测：拿一条真实接口走完整流程

> 目标：给你一个接口需求，你能写出前端照着就能联调、测试照着就能验收的文档，并用 curl / Postman / Go 测试把它真正跑通。
>
> 本章用一个虚构接口 `POST /v1/recharges` 做完整示范。所有域名、账号、订单号都是 `example.com` / `demo` / `u-1001`。

---

## 16.1 接口文档是什么，不是什么

接口文档不是“把字段列出来”。它同时是：

- **前端联调说明书**：请求怎么发、响应长什么样、错误怎么处理；
- **测试验收依据**：每个错误码怎么触发；
- **后端实现契约**：字段类型、必填、边界、幂等；
- **长期兼容承诺**：老客户端还能不能用。

一份差文档：

```text
POST /recharge
参数：amount, channel
返回：成功/失败
```

前端会问：金额单位是什么？channel 有哪些值？失败 HTTP 状态码是多少？重复请求怎么办？超时了能不能重试？

---

## 16.2 一份合格接口文档的结构

```markdown
1. 基本信息：名称、路径、方法、负责人、状态
2. 鉴权：需要什么 token、权限
3. 请求头：Content-Type、认证头、trace；幂等键放 header 还是 body 要明确
4. 路径/Query 参数
5. 请求体：字段表 + JSON 示例
6. 成功响应：状态码 + 字段表 + 示例
7. 错误码：HTTP 状态 + code + 触发条件 + 客户端动作
8. 幂等/重试/超时
9. curl 示例
10. 变更记录
```

下面逐项写完整。

---

## 16.3 完整示例：创建充值单

本节不是另写一份“示意契约”。下面的字段、状态码和响应体必须与仓库里的
[`code/mini-coin/api/openapi.yaml`](code/mini-coin/api/openapi.yaml) 以及
`internal/httpapi/handler.go` 保持一致。文档、OpenAPI、代码三处任何一处改了，另外两处都要同步。

### 基本信息

| 项 | 值 |
|---|---|
| 名称 | 创建充值单 |
| 方法 | `POST` |
| 路径 | `/v1/recharges` |
| 鉴权 | `Authorization: Bearer <token>` |
| 幂等 | 请求体 `req_id` 必填；同一业务请求重试时必须原样复用 |
| 本地地址 | `http://127.0.0.1:8081` |
| 负责人 | `backend-demo` |
| 状态 | 教学版 v1 |

### 请求头

| Header | 必填 | 说明 |
|---|---|---|
| `Authorization` | 是 | `Bearer <access_token>`；当前教学服务只校验非空并映射到虚构用户 `u-1001` |
| `Content-Type` | 是 | 必须是 `application/json` |
| `X-Request-Id` | 否 | 链路追踪 ID，最长 64 字符；不传或超长时由服务端生成 |

### 请求体

```json
{
  "req_id": "demo-1",
  "amount_cent": 10,
  "channel": "manual"
}
```

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `req_id` | string | 是 | 1～64 字符 | 客户端生成并在重试时原样复用 |
| `amount_cent` | integer(int64) | 是 | 1～100000000 | 金额，单位分 |
| `channel` | string | 是 | `manual` / `coupon` | 充值渠道 |

`uid` 不在请求体里：服务端从认证信息解析。请求体出现未知字段（例如客户端自作聪明传
`"uid":"u-1001"`）时，handler 的 `DisallowUnknownFields()` 会返回 400，而不是悄悄忽略。

### 首次成功响应 `201 Created`

```http
HTTP/1.1 201 Created
Content-Type: application/json
X-Request-Id: trace-1
```

```json
{
  "code": "OK",
  "message": "success",
  "request_id": "trace-1",
  "data": {
    "uid": "u-1001",
    "balance": 110,
    "duplicated": false
  }
}
```

### 幂等命中响应 `200 OK`

用同一个 `req_id`、相同参数再次请求：

```json
{
  "code": "OK",
  "message": "success",
  "request_id": "trace-2",
  "data": {
    "uid": "u-1001",
    "balance": 110,
    "duplicated": true
  }
}
```

`duplicated=true` 表示这次没有再加钱，返回的是首次请求的结果。HTTP 200 与首次创建的 201 不同，客户端和监控都要能区分。

### 错误响应

统一错误体：

```json
{
  "code": "INVALID_AMOUNT",
  "message": "amount_cent must be between 1 and 100000000",
  "request_id": "trace-3"
}
```

| HTTP | `code` | 触发条件 | 客户端动作 |
|---|---|---|---|
| 400 | `INVALID_JSON` | JSON 格式错、多个 JSON 值、未知字段 | 修正请求体，不重试 |
| 400 | `INVALID_AMOUNT` | `amount_cent <= 0` 或超过 100000000 | 修正金额 |
| 400 | `INVALID_REQ_ID` | `req_id` 超过 64 字符 | 生成更短的键 |
| 400 | `INVALID_CHANNEL` | `channel` 不在 `manual/coupon` 中 | 改用合法渠道 |
| 400 | `MISSING_REQ_ID` | `req_id` 为空 | 生成幂等键 |
| 400 | `MISSING_CHANNEL` | `channel` 为空 | 补渠道 |
| 401 | `UNAUTHORIZED` | 缺失/格式错误的 Bearer token | 重新登录或修 token |
| 404 | `ACCOUNT_NOT_FOUND` | 账户不存在 | 提示用户或创建账户 |
| 405 | `METHOD_NOT_ALLOWED` | 方法不是 POST | 检查请求方法 |
| 409 | `REQ_ID_CONFLICT` | 同一 `req_id` 使用了不同 `uid/amount/channel` | 不自动重试，提示并排查 |
| 409 | `ACCOUNT_FROZEN` | 账户冻结 | 提示用户联系客服 |
| 409 | `PREVIOUSLY_FAILED` | 该幂等键首次执行已失败 | 不自动复用，走人工/新请求流程 |
| 202 | `PROCESSING` | 相同 `req_id` 的首次请求还在处理中 | 退避后查询或用同一键重试 |
| 500 | `INTERNAL_ERROR` | 未预期的服务端错误 | 用同一 `req_id` 重试并带 `request_id` 报障 |

`message` 是给人看的说明，客户端逻辑只能依赖稳定的 `code`，不能匹配中文或英文文案。响应头
`X-Request-Id` 与响应体 `request_id` 应一致。

### 幂等规则

- 同一个 `req_id` + 相同 `uid/amount_cent/channel`：首次 201；重试 200 + `duplicated=true`；
- 同一个 `req_id` + 任一业务参数不同：409 `REQ_ID_CONFLICT`；
- `recharge_order.req_id` 必须有唯一索引，原子冲突判断不能靠“先 SELECT 再 INSERT”；
- 交易记录至少保留一个覆盖客户端最大重试窗口的周期，教学版按 24 小时设计；
- 客户端超时后不能换新 `req_id` 盲目重试，否则会变成两笔合法请求。

### 用 curl 做四组最小验收

先启动服务：

```bash
cd code/mini-coin
go run ./cmd/server
# 另开终端；服务监听 127.0.0.1:8081
```

第一组，首次成功：

```bash
curl -sS -D /tmp/h1 -o /tmp/b1 -w 'status=%{http_code}\n' \
  -X POST 'http://127.0.0.1:8081/v1/recharges' \
  -H 'Authorization: Bearer demo-token' \
  -H 'Content-Type: application/json' \
  -H 'X-Request-Id: trace-demo-1' \
  -d '{"req_id":"doc-demo-1","amount_cent":10,"channel":"manual"}'
jq -e '.code == "OK" and .request_id == "trace-demo-1" and .data.balance == 110 and .data.duplicated == false' /tmp/b1
```

预期 `status=201`；`jq -e` 退出 0，且初始余额 100 加 10 后为 110。

第二组，同键同参数重试：

```bash
curl -sS -o /tmp/b2 -w 'status=%{http_code}\n' \
  -X POST 'http://127.0.0.1:8081/v1/recharges' \
  -H 'Authorization: Bearer demo-token' \
  -H 'Content-Type: application/json' \
  -d '{"req_id":"doc-demo-1","amount_cent":10,"channel":"manual"}'
jq -e '.data.duplicated == true and .data.balance == 110' /tmp/b2
```

预期 `status=200`，余额仍然是 110，没有重复加钱。

第三组，同键不同金额：

```bash
curl -sS -o /tmp/b3 -w 'status=%{http_code}\n' \
  -X POST 'http://127.0.0.1:8081/v1/recharges' \
  -H 'Authorization: Bearer demo-token' \
  -H 'Content-Type: application/json' \
  -d '{"req_id":"doc-demo-1","amount_cent":20,"channel":"manual"}'
jq -e '.code == "REQ_ID_CONFLICT"' /tmp/b3
```

预期 `status=409`，`/tmp/b3` 的 `code` 为 `REQ_ID_CONFLICT`。再传一次未知字段 `uid`，预期 400
`INVALID_JSON`；不带 token，预期 401 `UNAUTHORIZED`。四组都通过，才算“文档示例可执行”。

---

## 16.4 用 OpenAPI 描述

完整、可被工具读取的契约在
[`code/mini-coin/api/openapi.yaml`](code/mini-coin/api/openapi.yaml)。完整文件已经包含请求字段、成功响应、统一错误体、鉴权方式和每个状态码；这里不再维护一份会漂移的“关键片段”。打开
[`api/openapi.yaml`](code/mini-coin/api/openapi.yaml)，按下面顺序读：

1. `paths./v1/recharges.post`：方法、鉴权、请求体和 201/200/202/400/401/404/405/409/500；
2. `RechargeRequest`：`req_id`、`amount_cent`、`channel` 的类型、必填和边界；
3. `RechargeResponse`：成功响应固定为 `code/message/request_id/data`；
4. `Error`：错误响应固定为 `code/message/request_id`，并列出所有稳定业务码；
5. `securitySchemes.bearerAuth`：告诉工具和前端怎么注入 Bearer token。

这些定义不是装饰：生成 SDK、生成 Mock、校验响应和跑契约测试都读同一份 YAML。例如 `additionalProperties: false` 对应
handler 的 `DisallowUnknownFields()`；`amount_cent.maximum` 对应 service 的金额上限；`201/200` 对应首次创建与幂等命中。

要验证“文档片段没有漂移”时，不要人眼比对，直接运行：

```bash
cd code/mini-coin
npx --yes @redocly/cli@2.72.1 lint api/openapi.yaml
python3 - <<'PY'
import yaml
spec = yaml.safe_load(open('api/openapi.yaml'))
responses = spec['paths']['/v1/recharges']['post']['responses']
assert {'200', '201', '202', '400', '401', '404', '405', '409', '500'} <= set(responses)
schema = spec['components']['schemas']
props = schema['RechargeRequest']['properties']
assert props['amount_cent']['minimum'] == 1
assert props['amount_cent']['maximum'] == 100000000
codes = schema['Error']['properties']['code']['enum']
assert {'INVALID_REQ_ID', 'INVALID_CHANNEL', 'ACCOUNT_NOT_FOUND'} <= set(codes)
print('OpenAPI contract checks: OK')
PY
```

第一段检查 YAML、引用和 OpenAPI 规则，第二段把本章最容易写错的边界、状态码和错误码变成断言。预期两段都退出 0；
若 `lint` 报引用错误或 Python `assert` 失败，先修契约，不要让前端或测试继续消费坏契约。


OpenAPI 的价值不是“画一个漂亮页面”，而是让文档、Mock、客户端生成、契约测试和 CI 共用同一份机器可读契约：

- `additionalProperties: false` 明确未知字段要拒绝；
- `amount_cent` 的单位写在字段名和描述里，避免“元/分”混淆；
- `201` 和 `200` 都写清楚，客户端才能正确处理幂等命中；
- `required` 和枚举能生成准确 SDK，也能在测试中构造反例；
- schema 变更可以进入代码评审，避免只改页面不留下版本记录。

## 16.5 在接口管理平台里怎么填

接口管理平台只是承载契约的工具，真正要做的是让前端、后端、测试看到同一份可执行说明。录入前先把第 16.3 节的内容写成草稿；录入后再用平台的“调试/运行”发一次真实请求。页面能保存不代表契约正确，必须拿响应和文档逐字段对照。

1. **基本信息**：选择 `POST`，路径填 `/v1/recharges`，写负责人、状态和版本；
2. **分类**：放到“充值”目录，方便按业务查找；
3. **环境**：至少配置 `local` 和 `test`，域名使用 `http://127.0.0.1:8081` 或 `https://api.example.com`；
4. **请求 Headers**：逐个添加 `Authorization`、`Content-Type` 和可选的 `X-Request-Id`；幂等键 `req_id` 在请求体里；
5. **请求 Body**：选择 JSON，贴一个能执行的示例，再给每个字段标类型、必填、范围、单位和示例值；
6. **成功响应**：贴最终格式，不要贴内部 DTO 或数据库模型；字段名、类型和可空性必须与真实响应一致；
7. **错误响应**：本接口至少录入 400、401、404、405、409、500 和 202；如果网关还会返回 429，也必须单独录入并写 `Retry-After`。不要为了“看起来完整”添加服务端根本不会返回的状态码；
8. **语义说明**：写幂等键复用规则、超时、重试、金额单位、权限来源和变更策略；
9. **Mock**：从同一份 schema 生成成功、参数错误、冲突、超时场景，不要手写另一套 JSON；
10. **发布前自测**：在平台“运行”一次，再用 curl 和 Go `httptest` 各验证一次。

平台“运行”本质上也是发 HTTP 请求，适合快速确认路径、鉴权和字段，但它不会自动证明并发幂等、数据库状态和错误码映射。一次完整录入的完成标准是：前端只打开文档就能构造请求；测试能按文档触发每个错误；后端 CI 能用同一份 schema 做契约检查。

### 一次真实录入：每个框到底填什么

不要对着空页面从零想。先在编辑器里准备下面这份“录入源稿”，平台里的字段、OpenAPI 和代码测试都从这里抄同一份值：

| 平台字段 | 填入内容 | 验收方式 |
|---|---|---|
| 接口名称 | `创建充值单` | 前端能按名称搜索到 |
| 方法/路径 | `POST /v1/recharges` | 请求行与后端路由完全一致 |
| 目录 | `交易 / 充值` | 新同学能从业务目录找到 |
| 负责人 | `backend-demo` | 出问题能找到 owner |
| 状态 | `已发布 v1` | 未实现接口不能标已发布 |
| 请求 Header | `Authorization`、`Content-Type`；可选 `X-Request-Id` | 不带 Authorization 应返回 401 |
| 请求 body | `{"req_id":"platform-demo-1","amount_cent":100,"channel":"manual"}` | 平台示例可直接发送并返回 201，不出现真实账号 |
| 成功响应 | 201 + `code/message/request_id/data{uid,balance,duplicated}` | 与真实响应逐字段一致 |
| 错误响应 | 400/401/409/429/500 + 稳定业务码 | 测试能按文档逐项触发 |
| 幂等说明 | 同 key 同参数返回原单；不同参数 409 | 并发重放后余额只加一次 |
| 超时/重试 | 客户端超时只复用原 key 查询或重试 | 不产生第二笔订单 |
| 变更记录 | 日期、版本、兼容性 | 老客户端仍能调用 |

录入后按下面的顺序验收：

1. 在平台选择测试环境，填一个测试 token，发送成功示例，检查 HTTP 状态、响应字段和 `request_id`；
2. 故意删掉 `req_id`，确认返回文档声明的 400 `MISSING_REQ_ID`，而不是 500；
3. 把同一个 `req_id` 的金额从 10 改成 20，确认返回 409 `REQ_ID_CONFLICT`；
4. 用文档中的 curl 再发一次，确认平台和 curl 的结果一致；
5. 打开 OpenAPI/导出文件，执行 lint 或导入 Postman，确认没有丢字段；
6. 把真实响应脱敏后保存到测试记录，不要只截一张看不出状态码的页面。

如果平台要求单点登录，登录只是获取访问权限，不是技术验证。**不要把真实域名、Cookie、token、账号、二维码或业务字段复制到讲义、截图和提交记录里。**公开示例统一使用 `example.com`、`demo`、`u-1001` 和运行时占位凭证。

---

## 16.6 自测工具怎么选

| 工具 | 适合 | 不适合 |
|---|---|---|
| curl | 快速验证、CI、脚本 | 复杂环境管理 |
| Postman | 手工联调、集合、环境变量 | 大规模自动化 |
| Insomnia | 类似 Postman | 团队统一性 |
| Go httptest | 接口自动化测试 | 手工探索 |
| k6 | 压测 | 功能断言 |
| grpcurl | gRPC 调试 | HTTP JSON |

### curl 常用

curl 最适合验证“真实 HTTP 行为”：状态码、响应头、JSON、耗时和重定向。调试时先加 `-i` 看响应头，再用 `-o` 保存响应体，最后用 `jq` 校验字段。只看到终端打印了一段 JSON，不知道状态码和耗时，不算完成自测。

```bash
# 看状态码 + 耗时
curl -sS -o /tmp/body.json -w '%{http_code} %{time_total}\n' \
  -X POST 'http://127.0.0.1:8081/v1/recharges' \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -H 'X-Request-Id: req-1' \
  -d '{"req_id":"curl-demo-1","amount_cent":100,"channel":"manual"}'

cat /tmp/body.json | jq

# 失败时看详细过程
curl -v ...
```

### Postman

Postman 的优势是环境变量、Collection、Tests 和团队共享，适合前后端联调与人工探索。它不应成为唯一回归手段：Collection 文件可能漂移，复杂并发和数据库断言不适合放在 GUI 里。把 Postman 当“能重复运行的场景集合”，关键场景再固化成 Go 测试。

1. 新建 Collection `MiniCoin`，再建 `成功`、`参数错误`、`未认证`、`幂等冲突` 四个请求；
2. 新建 Environment `local`，只放非生产值：`base_url=http://127.0.0.1:8081`、`token=demo-token`、`req_id=req-postman-001`；
3. URL 使用 `{{base_url}}/v1/recharges`，Headers 设置 `Content-Type: application/json`、`Authorization: Bearer {{token}}`；请求体把 `req_id` 设为 `{{req_id}}`；
4. 成功请求的 Tests 不只断言 201，还要断言响应结构：

```js
pm.test('status 201', () => pm.response.to.have.status(201));
pm.test('response contract', () => {
  const body = pm.response.json();
  pm.expect(body.code).to.eql('OK');
  pm.expect(body.request_id).to.be.a('string');
  pm.expect(body.data.uid).to.be.a('string');
  pm.expect(body.data.balance).to.be.a('number');
  pm.expect(body.data.duplicated).to.eql(false);
});
pm.test('request id matches header', () => {
  const body = pm.response.json();
  pm.expect(body.request_id).to.equal(pm.response.headers.get('X-Request-Id'));
});
```

5. 参数错误请求把金额改成 `0`，Tests 断言 `status 400`、`code` 为 `INVALID_AMOUNT`，并确认响应体没有堆栈和 SQL；
6. 未认证请求删掉 Authorization，断言 `401 UNAUTHORIZED`；
7. 幂等冲突请求复用同一个 `{{req_id}}`、把金额改成 `20`，断言 `409 REQ_ID_CONFLICT`；
8. 用 Collection Runner 按“成功 → 冲突”顺序跑一遍，再导出 Collection 供团队使用；
9. 环境变量不要把生产 token 提交到仓库。Postman 的 GUI 结果只是手工证据，关键路径仍要有 Go `httptest` 自动回归。

### grpcurl

```bash
# 列出服务（服务端开启反射）
grpcurl -plaintext 127.0.0.1:9090 list

# 调方法
grpcurl -plaintext -H 'authorization: Bearer demo-token' -d '{"req_id":"req-1","amount_cent":100}' \
  127.0.0.1:9090 recharge.v1.RechargeService/CreateRecharge
```

---

## 16.7 自测清单：正常、边界、异常、并发

### 正常

正常路径至少要证明“响应正确”和“副作用正确”两件事。HTTP 200 只说明请求被处理，余额有没有增加、订单有没有落库、消息有没有发出，需要再查数据库和下游。成功样本应使用固定测试账号，记录执行前状态，执行后通过查询接口和数据库各验证一次。

- 200/201，字段完整；
- 金额、余额计算正确；
- 数据库真的有记录；
- 缓存/消息/下游行为正确。

### 边界

边界测试围绕“刚好合法、刚好非法、长度极限、类型边界”设计。金额不仅要测 1 和上限，还要测 0、负数、上限加一和超过 JS 安全整数的值；字符串要测空串、最大长度和超长；分页要测 0、1、上限和上限加一。每个用例都写清期望 HTTP、业务码和数据库是否变化。

- `amount_cent = 1`、最大值、最大值 + 1；
- 空字符串、超长字符串；
- 不存在的 channel；
- 分页 limit=0、limit=上限 + 1；
- 时间边界。

### 异常

异常路径要验证服务不会“失败一半”：鉴权失败不写库，参数错误不扣余额，下游超时后能查询订单而不是让用户盲目重试。可以用故障注入、Mock 或测试配置制造依赖超时，但不要在生产环境通过特殊账号开后门。

- 缺 token、过期 token；
- 越权访问别人的订单；
- 数据库断开；
- Redis 断开；
- 下游超时；
- 重复请求、并发重复请求。

### 并发

并发测试不是把请求发出去看有没有 500，而是验证业务不变量：同一个幂等键只产生一笔订单，余额只增加一次，重复请求返回同一结果或明确冲突。脚本要保存每个请求的状态码和响应体，结束后查订单数、余额和流水条数；只看终端最后一行会漏掉部分失败。

```bash
rm -f /tmp/recharge-*.json /tmp/recharge-*.status
seq 1 20 | xargs -P20 -I{} sh -c '
  code=$(curl -sS -o /tmp/recharge-{}.json -w "%{http_code}" \
    -X POST "http://127.0.0.1:8081/v1/recharges" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer demo-token" \
    -H "X-Request-Id: race-{}" \
    -d "{\"req_id\":\"race-same-key\",\"amount_cent\":100,\"channel\":\"manual\"}")
  printf "%s\n" "$code" > /tmp/recharge-{}.status
'
cat /tmp/recharge-*.status | sort | uniq -c
# 期望：1 个 201，19 个 200；不应出现 409/500
grep -l '"duplicated":true' /tmp/recharge-*.json | wc -l  # 期望 19
```

预期：只有第一个请求真正加钱，其余 19 个返回 200 且 `duplicated=true`，最终余额只增加 100。若出现多个 201
或出现 409 `REQ_ID_CONFLICT`，先查请求体里的 `req_id/amount_cent/channel` 是否被并发脚本拼错，再查 repository
的唯一键和事务；不要通过重跑脚本掩盖竞态。

### 用 Go 做自动化接口测试

```go
func TestCreateRecharge_HTTP(t *testing.T) {
    srv := httptest.NewServer(NewRouter(fakeService{}))
    defer srv.Close()

    body := `{"req_id":"http-req-1","amount_cent":100,"channel":"manual"}`
    req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/recharges", strings.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer demo-token")
    req.Header.Set("X-Request-Id", "trace-test-1")

    resp, err := http.DefaultClient.Do(req)
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusCreated {
        raw, _ := io.ReadAll(resp.Body)
        t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
    }
    var got struct {
        Code string `json:"code"`
        Data struct{ Duplicated bool `json:"duplicated"` } `json:"data"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&got); err != nil { t.Fatal(err) }
    if got.Code != "OK" || got.Data.Duplicated {
        t.Fatalf("unexpected body: %+v", got)
    }
    if got := resp.Header.Get("X-Request-Id"); got != "trace-test-1" {
        t.Fatalf("request id=%q", got)
    }
}
```

---

## 16.8 契约测试：文档和代码不能各说各话

后端改了字段名，文档没改，前端联调就炸。契约测试把 OpenAPI 当成断言，流程必须能重复执行：

```bash
# 1. 先校验 OpenAPI 本身没有语法和引用错误
cd code/mini-coin
npx @redocly/cli lint api/openapi.yaml

# 2. 启动隔离实例。教学代码用内存仓库；接真实 DB 时必须用独立测试库。
MINICOIN_CONFIG=config/dev.json go run ./cmd/server > /tmp/recharge.log 2>&1 &
SERVER_PID=$!
trap 'kill "$SERVER_PID"' EXIT
for i in $(seq 1 20); do
  code=$(curl -s -o /dev/null -w '%{http_code}' \
    -X POST http://127.0.0.1:8081/v1/recharges \
    -H 'Content-Type: application/json' \
    -d '{"req_id":"ready-check","amount_cent":1,"channel":"manual"}')
  [ "$code" = "401" ] && break
  sleep 0.2
done
[ "$code" = "401" ] || { echo "server not ready"; cat /tmp/recharge.log; exit 1; }

# 3. 用 schema 生成请求并校验实际响应；认证由 --header 注入
schemathesis run api/openapi.yaml --base-url http://127.0.0.1:8081 \
  --header 'Authorization: Bearer demo-token' \
  --checks all --max-examples 50
```

在 CI 里固定为四步：启动隔离服务 → 等健康检查成功 → 根据 OpenAPI 生成/重放请求并校验状态码和响应 schema → 失败时上传服务日志和请求样本并阻断合并。预期失败信息要能指出具体 path、字段和期望类型。不要用生产环境跑生成式测试，也不要只校验 HTTP 200；业务错误体中的 `code`、`message`、`request_id` 也要属于 schema。

如果契约测试发现文档和实现不一致，先确认已发布的兼容承诺：破坏老客户端的实现不能直接改文档；只有确认没有消费者依赖后，才按版本策略新增字段或废弃旧字段。

---

## 16.9 文档坏味道

| 坏味道 | 后果 | 修正 |
|---|---|---|
| 只写“成功/失败” | 前端不知道怎么处理 | 列状态码和错误码 |
| 字段没有类型/单位 | 金额传成元 | 写 `amount_cent` + integer |
| 不写必填 | 联调反复试 | 每个字段标必填 |
| 不写幂等 | 重试重复扣款 | 写 `req_id` 幂等键和冲突规则 |
| 示例和实际不一致 | 联调失败 | 用 curl 实测后粘贴 |
| 没有变更记录 | 老客户端突然坏 | 记录版本和兼容性 |
| 错误返回堆栈 | 泄漏内部信息 | 统一错误体 |
| 文档里放真实 token | 泄漏 | 用占位符 |

---

## 16.10 出现 bug 后怎么用文档排查

```markdown
1. 复现：拿到请求 URL、方法、headers、body、时间、request_id
2. 对比文档：路径/字段/类型/必填是否符合
3. 看响应：HTTP 状态码 + code + request_id
4. 查日志：按 request_id 找服务端日志
5. 查依赖：DB/Redis/MQ 是否报错
6. 最小复现：用 curl 重放同一请求
7. 修复：代码/文档/配置
8. 回归：把该场景加入自动化测试
```

如果文档和实现不一致，先确认哪边是契约：已发布的接口应以**兼容老客户端**为准，不能为了改代码随便改契约。排查时保留原始请求、响应和 request_id，用 curl 重放，再对照文档的字段、类型、必填和状态码。若请求本身不符合文档，修客户端或补校验提示；若实现偏离文档，修服务并加契约测试；若文档过期，先确认没有老客户端依赖，再按版本兼容流程更新。修复完成后把该场景加入自动回归，避免同类问题再次出现。

---

## 习题

### 基础题

1. 接口文档至少应包含哪些部分？
2. 为什么金额字段要写单位和类型？
3. 为什么 `uid` 不能从请求体取？
4. 幂等键的作用是什么？重复请求参数不一致返回什么？
5. OpenAPI 有什么价值？
6. curl、Postman、Go httptest 各适合什么？
7. 契约测试解决什么问题？
8. 文档里为什么不能放真实 token？

### 动手题

9. 给“查询订单详情”写完整文档：路径、鉴权、响应、错误码、curl。
10. 把第 9 题的接口写成 OpenAPI YAML。
11. 用 Postman 建一个集合，包含成功、参数错误、未认证三个请求和断言。
12. 写一个并发自测脚本，验证同一幂等键只加一次钱。

### 综合题

13. 前端说“接口返回 500”，测试说“文档写的是 400”。你如何定位是代码问题、文档问题还是请求问题？写出完整排查步骤和修复原则。

---

## 参考答案

<details>
<summary>展开</summary>

1. 基本信息和负责人、鉴权、请求头、路径/方法、参数与 body、字段类型/单位/必填、成功响应、所有可预期错误码、幂等和重试规则、curl 示例、版本和变更记录。

2. 金额必须写清整数或小数、单位、范围和精度。推荐 `amount_cent: int64/minimum 1`；否则前端可能把 10 元发成 10 分，或把 int64 大整数转成 JS 浮点丢失精度。

3. `uid` 必须由服务端从认证 token 解析，不能相信请求体；否则用户改一个 uid 就能给他人充值或查询他人订单。

4. 幂等键防止客户端超时重试重复创建/扣款。同键同参数返回首次结果；同键不同参数返回 409 `REQ_ID_CONFLICT`，不能把另一笔请求结果冒充成功。

5. OpenAPI 能生成文档、Mock、客户端和契约测试，统一字段类型、必填和状态码，减少前后端口头约定。

6. curl 适合快速复现和 CI smoke；Postman 适合手工联调、环境变量和集合断言；Go `httptest` 适合自动化单测和回归，不需要启动真实网络。

7. 契约测试在代码变更时验证请求/响应仍符合 OpenAPI 或旧客户端预期，能提前发现字段删除、类型改变和状态码漂移。

8. 文档会进入 Git、截图、聊天和公开页面；真实 token 会泄漏并可能被滥用。示例统一用 `<token>` 或环境变量。

9. 查询订单详情文档：

```http
GET /v1/orders/{order_id}
Authorization: Bearer <token>
X-Request-Id: req-demo-001
```

| 项 | 规则 |
|---|---|
| `order_id` | 路径参数，string，非空 |
| 鉴权 | 必须登录，只能查 token 对应用户的订单 |
| 成功 | 200，`code=OK`，返回订单和金额分 |
| 参数错误 | 400 `INVALID_ORDER_ID` |
| 未认证 | 401 `UNAUTHORIZED` |
| 不存在/越权 | 404 `ORDER_NOT_FOUND`，不泄漏是否存在 |
| 内部错误 | 500 `INTERNAL_ERROR`，不返回 SQL/堆栈 |

成功响应：

```json
{
  "code": "OK",
  "message": "success",
  "request_id": "req-demo-001",
  "data": {
    "order_id": "90001",
    "uid": "u-1001",
    "amount_cent": 1000,
    "status": "SUCCEEDED",
    "created_at": "2026-11-01T10:00:00+08:00"
  }
}
```

curl：

```bash
curl -sS -D /tmp/headers -o /tmp/body -w '%{http_code}\n' \
  -H 'Authorization: Bearer <token>' \
  -H 'X-Request-Id: req-demo-001' \
  http://127.0.0.1:8081/v1/orders/90001
jq -e '.code == "OK" and .data.amount_cent == 1000' /tmp/body
```

再用他人订单和不存在订单各测一次，均应为 404；用无 token 测一次，应为 401。

10. OpenAPI YAML：

```yaml
openapi: 3.0.3
info:
  title: Mini Coin Orders API
  version: 1.0.0
paths:
  /v1/orders/{order_id}:
    get:
      operationId: getOrder
      parameters:
        - in: path
          name: order_id
          required: true
          schema: {type: string, minLength: 1}
        - in: header
          name: X-Request-Id
          required: false
          schema: {type: string, maxLength: 64}
      responses:
        '200':
          description: order found
          content:
            application/json:
              schema: {$ref: '#/components/schemas/OrderResponse'}
        '400': {$ref: '#/components/responses/BadRequest'}
        '401': {$ref: '#/components/responses/Unauthorized'}
        '404': {$ref: '#/components/responses/NotFound'}
        '500': {$ref: '#/components/responses/InternalError'}
      security: [{bearerAuth: []}]
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
  schemas:
    Order:
      type: object
      required: [order_id, uid, amount_cent, status, created_at]
      properties:
        order_id: {type: string}
        uid: {type: string}
        amount_cent: {type: integer, format: int64, minimum: 1}
        status: {type: string, enum: [SUCCEEDED, FAILED, PROCESSING]}
        created_at: {type: string, format: date-time}
    OrderResponse:
      type: object
      required: [code, message, request_id, data]
      properties:
        code: {type: string, example: OK}
        message: {type: string}
        request_id: {type: string}
        data: {$ref: '#/components/schemas/Order'}
    Error:
      type: object
      required: [code, message, request_id]
      properties:
        code: {type: string}
        message: {type: string}
        request_id: {type: string}
  responses:
    BadRequest:
      description: invalid request
      content: {application/json: {schema: {$ref: '#/components/schemas/Error'}}}
    Unauthorized:
      description: unauthenticated
      content: {application/json: {schema: {$ref: '#/components/schemas/Error'}}}
    NotFound:
      description: order not found
      content: {application/json: {schema: {$ref: '#/components/schemas/Error'}}}
    InternalError:
      description: internal error
      content: {application/json: {schema: {$ref: '#/components/schemas/Error'}}}
```

验收：`openapi-generator validate` 和 `schemathesis run openapi.yaml --base-url http://127.0.0.1:8081` 通过；真实响应字段与 schema 一致。

11. Postman Collection（可直接导入，三个请求）：

```json
{
  "info": {"name": "mini-coin recharge checks", "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},
  "variable": [{"key": "baseUrl", "value": "http://127.0.0.1:8081"}],
  "item": [
    {
      "name": "success",
      "request": {
        "method": "POST",
        "header": [
          {"key": "Content-Type", "value": "application/json"},
          {"key": "Authorization", "value": "Bearer {{token}}"},
          {"key": "X-Request-Id", "value": "postman-success-1"}
        ],
        "body": {"mode": "raw", "raw": "{\"req_id\":\"pm-1\",\"amount_cent\":100,\"channel\":\"manual\"}"},
        "url": {"raw": "{{baseUrl}}/v1/recharges", "host": ["{{baseUrl}}"], "path": ["v1", "recharges"]}
      },
      "event": [{"listen": "test", "script": {"exec": [
        "pm.test('status 201', () => pm.response.to.have.status(201));",
        "const body = pm.response.json();",
        "pm.test('contract', () => { pm.expect(body.code).to.eql('OK'); pm.expect(body.data.duplicated).to.eql(false); pm.expect(body.request_id).to.eql(pm.response.headers.get('X-Request-Id')); });"
      ]}}]
    },
    {
      "name": "bad request",
      "request": {
        "method": "POST",
        "header": [
          {"key": "Content-Type", "value": "application/json"},
          {"key": "Authorization", "value": "Bearer {{token}}"},
          {"key": "X-Request-Id", "value": "postman-bad-1"}
        ],
        "body": {"mode": "raw", "raw": "{\"req_id\":\"pm-bad\",\"amount_cent\":0,\"channel\":\"manual\"}"},
        "url": {"raw": "{{baseUrl}}/v1/recharges", "host": ["{{baseUrl}}"], "path": ["v1", "recharges"]}
      },
      "event": [{"listen": "test", "script": {"exec": [
        "pm.test('status 400', () => pm.response.to.have.status(400));",
        "const body = pm.response.json();",
        "pm.test('stable code', () => pm.expect(body.code).to.eql('INVALID_AMOUNT'));",
        "pm.test('request id present', () => pm.expect(body.request_id).to.be.a('string'));"
      ]}}]
    },
    {
      "name": "unauthenticated",
      "request": {
        "method": "POST",
        "header": [
          {"key": "Content-Type", "value": "application/json"},
          {"key": "X-Request-Id", "value": "postman-noauth-1"}
        ],
        "body": {"mode": "raw", "raw": "{\"req_id\":\"pm-noauth\",\"amount_cent\":100,\"channel\":\"manual\"}"},
        "url": {"raw": "{{baseUrl}}/v1/recharges", "host": ["{{baseUrl}}"], "path": ["v1", "recharges"]}
      },
      "event": [{"listen": "test", "script": {"exec": [
        "pm.test('status 401', () => pm.response.to.have.status(401));",
        "pm.test('no internal details', () => pm.expect(pm.response.text()).to.not.match(/sql|panic|stack/i));"
      ]}}]
    }
  ]
}
```

在 Postman Environment 设置 `baseUrl` 和测试 token；运行 Collection 后三个请求都应通过。Postman 通过不代表数据库状态正确，所以还要查余额没有因错误请求变化。

12. 并发同幂等键自测：

```bash
set -euo pipefail
base=http://127.0.0.1:8081
before=$(curl -sS "$base/v1/accounts/u-1001/balance" | jq -r '.data.balance')
seq 1 20 | xargs -P20 -I{} sh -c '
  curl -sS -o /tmp/recharge-{}.json -w "%{http_code}\n" \
    -X POST "$0/v1/recharges" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" \
    -H "X-Request-Id: load-{}" \
    -d "{\"req_id\":\"race-1\",\"amount_cent\":100,\"channel\":\"manual\"}"
' "$base" > /tmp/recharge-status.txt

after=$(curl -sS "$base/v1/accounts/u-1001/balance" | jq -r '.data.balance')
test $((after - before)) -eq 100
grep -q '"duplicated":true' /tmp/recharge-*.json
```

期望：余额只增加 100；第一次 201，其余 200 或明确的处理中响应；`duplicated=true` 至少出现一次；订单表和流水各只有一条。失败时先看状态码分布、request_id 和数据库唯一键冲突，不要先盲目重跑。

13. 排查步骤：

```text
1. 收集 URL、method、headers、body、时间、响应原文和 request_id。
2. 用 curl 原样重放，检查是否真的返回 500；排除代理/客户端缓存。
3. 对照 OpenAPI：路径、字段名、类型、必填、状态码和业务 code。
4. 用无 token、错误金额、正确 token 分别缩小是鉴权、参数还是服务端问题。
5. 按 request_id 查网关日志、应用日志、trace 和数据库指标。
6. 判断属于哪一类：请求不符合文档、实现偏离文档、文档过期、网关/依赖故障。
7. 以已发布契约为准：代码错修代码；文档错先确认没有老客户端依赖，再走兼容变更；网关错修配置。
8. 补契约测试、集成测试和监控，保存复现命令、响应和日志作为证据。
```

修复原则不是“谁声音大听谁”，而是以已发布契约和用户影响为准；任何破坏性字段/类型变化都必须版本化或提供迁移窗口。

</details>
