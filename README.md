# Go 后端开发全景讲义

**从会 JS，到能独立开发、测试、上线、排障一个 Go 后端服务。**

> 本讲义完全使用虚构项目 `mini-coin` 和公开技术栈（Go / MySQL / PostgreSQL / Redis / Kafka / RabbitMQ / gRPC / Prometheus / Docker / Kubernetes）。
> 不包含任何真实组织、内网地址、账号、密码、真实接口或业务数据。所有域名一律为 `example.com`。
>
> CDN 阅读入口：<https://cdn.jsdelivr.net/npm/go-backend-handbook@1.2.1/README.md>

---

## JS 类比怎么用（先读这一节）

你会 JS，这是优势，也是陷阱。本讲义在**两者真有相似概念**时会用 JS 打比方，帮你快速建立直觉：

| 后端概念 | 可以类比的 JS / 前端概念 | 必须记住的差异 |
|---|---|---|
| 包 / 模块 | npm 包、ES Module | Go 按 import path 引用，首字母大写才导出 |
| struct | 固定形状的对象 | 有编译期类型和零值，不是任意 JSON |
| slice | 数组 | 带 len/capacity，可能共享底层数组 |
| map | `Map` / 对象 | 遍历顺序不保证 |
| error 返回值 | `throw` / `catch` | Go 把预期错误作为返回值显式处理 |
| `defer` | `try/finally` | 函数退出时执行，多个按 LIFO |
| interface | TypeScript interface / 鸭子类型 | Go 是结构化满足，接口通常很小 |
| goroutine | `Promise` / async 任务 | 真并行，不是单线程事件循环 |
| context | `AbortController` / `AbortSignal` | 显式沿调用链传递 |
| 拦截器 | Express / Koa middleware | gRPC 的响应和错误语义不同 |
| 链路追踪 | Network waterfall | 跨服务、跨进程传播 |
| `go test` | Vitest / Jest | 编译执行、表驱动、`-race` |
| k6 压测脚本 | 就是 JS 脚本 | 脚本语言不代表被测服务是 JS |
| proto / OpenAPI | TypeScript 类型 + 接口契约 | 同时生成服务端接口和客户端 SDK |

**JS 没有对应物的，不硬套**：指针、数据竞争、互斥锁、SQL 事务隔离级别、Docker、Kubernetes、集群、容量规划、数据库迁移的锁表风险。这些章节直接讲后端模型，类比只用来降低入门门槛，**不能替代对语义的理解**。

---

## 这套讲义解决什么问题

前端转后端最常见的断层不是语法，而是**系统观**：

- 一个请求从进入到落库经过哪些层，每层该负责什么；
- 数据怎么建模，索引怎么建，表大了怎么办，改字段怎么不锁表；
- 并发下怎么保证"不超卖、不重复扣款"；
- 服务挂了怎么在 5 分钟内定位，而不是靠猜；
- 上线前要过哪些门禁，上线后看哪些指标。

本讲义按"一个请求的一生"组织，每章都能在 `code/mini-coin` 里找到对应代码。

---

## 目录

| 章节 | 主题 | 关键产出 |
|---|---|---|
| [00](00-linux.md) | 开发机与 Linux | 能远程开发、看资源、排连接问题 |
| [01](01-go-basics.md) | Go 语言基础（JS 对照） | 能读懂并写业务函数 |
| [02](02-service-architecture.md) | 服务架构与请求生命周期 | 能从 main 追到 DB |
| [03](03-concurrency.md) | 并发：goroutine / channel / race | 能写正确的并发代码 |
| [04](04-mysql.md) | MySQL 实战 | 建表、索引、迁移、刷数据、容量 |
| [05](05-database-selection.md) | 数据库选型与其它数据库 | 知道什么时候不该用 MySQL |
| [06](06-redis.md) | Redis 实战 | 缓存、锁、大 key、热 key |
| [07](07-message-queue.md) | 消息队列 | 解耦、削峰、幂等消费 |
| [08](08-api-and-rpc.md) | 接口设计与 gRPC | 设计一个稳定的接口 |
| [09](09-observability.md) | 配置、日志、链路追踪 | 出问题能查 |
| [10](10-resilience.md) | 限流、熔断、降级、QPS | 不被流量打死 |
| [11](11-monitoring.md) | Prometheus + Grafana 监控告警 | 有看板有报警 |
| [12](12-testing.md) | 测试体系 | 单元/集成/E2E 全链路 |
| [13](13-performance-debug.md) | 压测与调试排障 | 找到瓶颈和 bug |
| [14](14-security.md) | 安全 | 鉴权、越权、注入、脱敏 |
| [15](15-development-process.md) | 完整开发流程 | 需求到上线的标准动作 |
| [16](16-api-docs.md) | 接口文档与自测 | 文档、curl、Postman、Mock |
| [17](17-deploy-ops.md) | 部署与运维 | Docker/K8s/Nginx/云/发布回滚 |
| [18](18-ai-assisted.md) | AI 辅助后端开发 | 用 AI 提效但不挖坑 |
| [19](19-appendix.md) | 附录：容量估算与速查表 | 面试/评审能用的数字 |
| [20](20-production-patterns.md) | 从生产代码提取可复用模式（脱敏版） | 服务启动、拦截器、批量并发、快照、分批处理、SafeGo |

习题与答案：每章末尾有「习题」与「参考答案」。完整代码见 [`code/mini-coin`](code/mini-coin)。

---

## 深度实战卷：从“看懂”到“能独立做”

00~20 章是知识地图和速查框架；下面五章是完整深挖版，不能再只读标题，必须跟着敲、跑测试、做故障实验。每章都有完整代码、边界条件、排障命令、习题与参考答案。

| 章节 | 深度内容 | 学完应产出 |
|---|---|---|
| [21](21-complete-project.md) | 从需求、OpenAPI、建表、事务、幂等、缓存、MQ 到上线 | 一个可测试、可部署的充值服务 |
| [22](22-database-deep-dive.md) | 字段、索引、事务、EXPLAIN、加字段、改类型、迁移、刷数据、容量、备份对账 | 一套安全的数据库变更方案 |
| [23](23-debugging-and-performance.md) | 日志/指标/trace、pprof、race、锁等待、MQ 积压、QPS 压测、故障演练 | 一份有证据的性能与故障报告 |
| [24](24-api-contract-and-testing.md) | OpenAPI、错误码、curl、Postman、httptest、Mock、Bug 复现和接口回归 | 一份可联调、可自动验收的接口契约 |
| [25](25-production-operations.md) | Linux、Docker、Kubernetes、Nginx、云、微服务、灰度发布和回滚 | 一份能执行的上线运行手册 |

### 推荐学习顺序

```text
第 1 周：01 -> 21.1~21.8 -> 写事务和幂等测试
第 2 周：22 -> 21.9~21.10 -> 做索引、迁移、刷数据实验
第 3 周：24 -> 23.1~23.7 -> 写契约测试并制造一次并发 bug
第 4 周：23.8~23.15 -> 压测、pprof、告警和故障复盘
第 5 周：25 -> Docker 构建、灰度、回滚演练
第 6 周：从头独立重写 mini-coin，不看答案完成综合大作业
```

### 每章的学习闭环

```text
先读原理
-> 手敲最小例子
-> 故意制造错误
-> 用命令观察证据
-> 写测试固定行为
-> 做章末习题
-> 对照答案，但先自己写
```

**看不懂代码时不要只读。** 先运行，再改一个参数或断言，观察输出变化；后端知识必须通过“运行—观察—证明”形成。

---

## 怎么学

**第一遍（3~5 天）速通主线**：00 → 01 → 02 → 04 → 08，把 `mini-coin` 跑起来，能充值、能查余额。

```bash
cd code/mini-coin
go test ./...
go run ./cmd/server
curl -i -X POST http://127.0.0.1:8081/v1/recharges \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -H 'X-Request-Id: demo-1' \
  -d '{"req_id":"demo-1","amount_cent":10,"channel":"manual"}'
```

**第二遍（每轮加一个维度）**：

1. 正确性：03 并发、12 测试；
2. 性能：06 Redis、10 限流、13 压测；
3. 可观测：09 日志链路、11 监控告警；
4. 交付：14 安全、15 流程、16 文档、17 部署。

**第三遍**：拿一个自己的小需求，从 Intent 写到上线，全程用本讲义对照。

---

## 前置环境

```bash
go version      # 1.21+
docker version
mysql --version # 或使用 docker 启动
redis-cli --version
```

没有 Docker 也能学：`mini-coin` 默认用内存实现，跑测试不需要外部依赖。

---

## 约定

- 所有示例域名：`api.example.com`、`registry.example.com`、`grafana.example.com`。
- 所有示例账号：`demo` / `u-1001`，仅用于本地。
- 所有金额单位：**分（int64）**，禁止用浮点数表示钱。
- 所有时间：服务端统一 UTC 存储，展示层再转时区。

---

## 许可

MIT。可自由用于学习、教学和培训。
