# mini-coin

虚构的充值练习项目，与任何公司代码无关。

```bash
go test ./...
go run ./cmd/server
curl -i -X POST http://127.0.0.1:8081/v1/recharges \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -H 'X-Request-Id: demo-1' \
  -d '{"req_id":"demo-1","amount_cent":10,"channel":"manual"}'
```

首次创建返回 `201`；同一 `req_id` 再请求返回 `200`，`data.duplicated=true`，余额不会再增加。

目录：

- `cmd/server`：核心服务入口
- `cmd/gateway`：网关入口（练习鉴权/限流时扩展）
- `internal/httpapi`：HTTP 契约、身份、request_id 和错误映射
- `internal/service`：业务层
- `internal/repository`：数据访问层
- `internal/cache`：缓存抽象
- `internal/mq`：消息发送抽象
- `migrations`：MySQL 建表/变更 SQL
- `api/coin.proto`：gRPC 接口定义
- `docs/changes`：需求变更包

`Bearer demo-token` 只用于本地教学，固定映射到虚构用户 `u-1001`。生产必须校验真实 token 的签名、过期时间、issuer 和 audience，不能照抄这段教学鉴权。
