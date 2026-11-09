# mini-coin 架构

- `cmd/server`: 核心服务, 负责充值业务。
- `cmd/gateway`: 对外 HTTP 网关, 负责鉴权、限流、协议转换。
- `internal/service`: 业务编排, 单元测试主要覆盖这里。
- `internal/repository`: 数据访问, 可在内存实现和 MySQL 实现之间替换。
- `internal/cache`: 缓存抽象。
- `internal/mq`: 领域事件发布, 可在内存实现和 Kafka/RabbitMQ 之间替换。

拆分服务的三个判断:
1. 是否能独立扩容(例如充值读写流量差异大)。
2. 是否能独立隔离故障(例如通知服务挂掉不影响充值)。
3. 是否有独立团队/发布节奏。
不要为了"看起来像微服务"而拆; 单体模块化往往更简单。
