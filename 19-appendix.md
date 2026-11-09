# 第 19 章 附录：容量估算、速查表与知识地图

> **JS 类比速查**（帮助建立直觉，不表示语义等价；差异列必须看）
>
> | 后端概念 | JS / 前端里的近似物 | 关键差异 |
> |---|---|---|
> | 容量估算 | 前端性能预算 | 后端按 QPS、延迟、存储、增长估算 |
> | 排障决策树 | 浏览器 Problems 面板的思路 | 后端要按指标→日志→trace 逐层缩小 |
> | 知识地图 | 前端技能树 | 后端多了数据、网络、运维、稳定性 |

> **先说明：这是查阅表，不是正文教程。** 第一次学习不要从本章顺着背表，而应先按第 00～18 章建立完整心智模型，再用本章查命令、估容量、排查故障。表里的结论如果没有前文解释，就回到对应章节看“为什么”和完整示例。本章适合复习、面试前自测和值班时快速定位，不适合替代正文。


## 19.1 容量估算模板

```
DAU            = 100 万
人均请求/天     = 50
日请求          = 5000 万
平均 QPS        = 5000万 / 86400 ≈ 580
峰值系数        = 5
峰值 QPS        = 2900
单机能力        = 1500 QPS（压测得出）
冗余系数        = 1.5
需要实例        = 2900 / 1500 × 1.5 ≈ 3
```

存储估算：

```
单条记录大小 = 200B
日增记录     = 500 万
日增数据     = 1GB
保留 1 年    = 365GB（加索引约 ×1.5）
```

## 19.2 常用数字（量级参考，必须实测）

| 操作 | 量级 |
|---|---|
| 内存随机读 | 100ns |
| SSD 随机读 | 100µs |
| 同机房网络 RTT | 0.5ms |
| 跨机房 RTT | 30ms |
| MySQL 主键查询 | 0.5~2ms |
| Redis GET | 0.2~1ms |
| 磁盘顺序读 | 500MB/s |
| 千兆网 | 125MB/s |

## 19.3 Linux 速查

```bash
# 系统
hostnamectl; uname -a; uptime; nproc; free -h; df -h
# 进程
ps aux --sort=-%cpu | head; top -Hp PID; pgrep -f app
# 端口
ss -lntp; lsof -i:8080
# 文件
du -sh * | sort -h; find . -name '*.log' -mtime +7
# 日志
tail -f app.log; grep -n 'ERROR' app.log | tail -50
# 网络
curl -v; nc -vz host port; dig example.com
# 信号
kill -15 PID; kill -9 PID
```

## 19.4 Go 速查

```bash
go run .; go build -o app .; go test ./...; go test -race ./...
go vet ./...; gofmt -w .; go mod tidy; go tool pprof ...
```

| 概念 | 要点 |
|---|---|
| 切片 | 共享底层数组，注意 append 覆盖 |
| map | 并发读写会 panic，需加锁 |
| error | 返回值处理，`%w` 包装 |
| defer | 函数返回前执行，循环里慎用 |
| goroutine | 必须有退出路径 |
| channel | 谁发送谁关闭 |
| context | 传递取消/超时，放首参 |
| 接口 | 隐式实现，面向接口编程 |

## 19.5 MySQL 速查

```sql
EXPLAIN SELECT ...;                       -- 看执行计划
SHOW PROCESSLIST;                         -- 当前连接
SHOW ENGINE INNODB STATUS;                -- 锁/事务
SHOW INDEX FROM t;                        -- 索引
SELECT * FROM information_schema.processlist WHERE time > 5;
```

| 问题 | 处理 |
|---|---|
| 慢查询 | EXPLAIN → 加索引/改 SQL |
| 深分页 | 游标法 |
| 锁等待 | 查 `innodb_trx`、缩短事务 |
| 主从延迟 | 减少大事务、并行复制 |
| 连接满 | 查泄漏、调池、限流 |

## 19.6 Redis 速查

```bash
redis-cli --bigkeys
redis-cli --hotkeys
redis-cli info memory
redis-cli info stats
redis-cli slowlog get 10
SCAN 0 MATCH prefix:* COUNT 100
```

## 19.7 HTTP 状态码

| 码 | 含义 |
|---|---|
| 200/201/204 | 成功 |
| 301/302/304 | 重定向/缓存 |
| 400 | 参数错 |
| 401/403 | 未认证/无权限 |
| 404 | 不存在 |
| 409 | 冲突 |
| 422 | 语义错 |
| 429 | 限流 |
| 500/502/503/504 | 服务错/网关错/不可用/超时 |

## 19.8 排障决策树

```
接口报错？
├─ 4xx → 客户端问题：参数、鉴权、路径、限流
└─ 5xx
   ├─ 503 → 过载/依赖不可用 → 看限流、熔断、实例健康
   ├─ 504 → 超时 → 看 trace 慢 span
   └─ 500 → 看日志堆栈 + request_id
变慢？
├─ CPU 高 → pprof CPU
├─ CPU 低 → 等 IO/锁/下游 → trace
├─ 只有 P99 高 → GC、长尾、慢 SQL、抖动
└─ 全站慢 → 依赖（DB/Redis/DNS/网络）
```

## 19.9 术语表

| 术语 | 含义 |
|---|---|
| QPS | 每秒查询数 |
| P99 | 99% 请求的延迟上界 |
| SLA | 服务等级协议 |
| SLO | 服务等级目标 |
| SLI | 服务等级指标 |
| RTO | 故障恢复时间 |
| RPO | 数据丢失窗口 |
| 幂等 | 同一请求重复执行结果一致 |
| 最终一致 | 短时不一致，最终收敛 |
| 熔断 | 下游故障时快速失败 |
| 降级 | 牺牲非核心功能保核心 |
| 灰度 | 小比例流量验证新版本 |
| Outbox | 本地消息表保证消息必达 |
| CDC | 变更数据捕获（如 binlog） |

## 19.10 后端知识地图

```
语言基础 ── 并发 ── 内存/GC
    │
    ├─ 数据：MySQL / PostgreSQL / MongoDB / Redis / ES / ClickHouse
    ├─ 通信：HTTP / gRPC / MQ / WebSocket
    ├─ 架构：分层 / DDD / 微服务 / 事件驱动
    ├─ 质量：测试 / 压测 / 代码评审 / 静态扫描
    ├─ 稳定：限流 / 熔断 / 降级 / 隔离 / 容量
    ├─ 观测：日志 / 指标 / 链路 / 告警
    ├─ 安全：认证 / 授权 / 注入 / 脱敏 / 签名
    └─ 交付：Git / CI / Docker / K8s / 云 / 发布回滚
```

## 19.11 继续深入的方向

| 方向 | 学什么 |
|---|---|
| 数据库 | 索引原理、事务隔离、复制、分库分表 |
| 分布式 | 一致性、共识、分布式事务、幂等 |
| 高并发 | 缓存、异步、池化、无锁、批处理 |
| 可观测 | OpenTelemetry、Prometheus、日志平台 |
| 云原生 | K8s、Service Mesh、Serverless |
| 安全 | OWASP、渗透测试、合规 |

## 19.12 自测：你出师了吗

能独立完成下面这些，就算能干活了：

- [ ] 从零跑起一个 Go 服务，连 MySQL/Redis
- [ ] 设计表、索引、迁移，处理过一次加字段
- [ ] 写并发安全、幂等的写接口
- [ ] 写单元/集成/E2E 测试，CI 全绿
- [ ] 用 trace 定位一次慢请求
- [ ] 配一个 Prometheus 告警
- [ ] 用 k6 压出服务拐点
- [ ] 用 Docker/K8s 部署并回滚
- [ ] 写一份前端不用追问的接口文档
- [ ] 处理一次线上故障并复盘
