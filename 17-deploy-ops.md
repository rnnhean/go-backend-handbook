# 第 17 章 部署与运维：Docker / Kubernetes / Nginx / 云

> 目标：你能把一个 Go 服务打成镜像，部署到 Kubernetes，通过 Nginx 暴露，发布新版本，出问题时回滚，并看懂 CPU/内存/副本数/探针这些配置为什么存在。
>
> 本章的配置都能直接跑（用 `demo` 镜像名和 `example.com` 域名）。每一段配置后面都会解释“这行不写会怎样”。

---

## 17.1 一次发布要解决什么

把代码放到服务器上只是第一步。一次可用的发布要回答：

1. **构建可复现**：同样的 commit 在任何机器构建出同样产物；
2. **运行环境一致**：不依赖“这台机器刚好装了某个库”；
3. **能接流量**：端口、域名、TLS、负载均衡配置正确；
4. **能健康检查**：进程活着但数据库断了，算不算健康？
5. **能滚动更新**：旧实例退出前，新实例已经准备好；
6. **能回滚**：上一个版本镜像还在，命令一条；
7. **能观察**：日志、指标、追踪能定位到具体实例和版本。

---

## 17.2 Docker：把“能运行”打包

### 为什么 Go 适合容器

Go 编译成静态二进制，运行时不需要 JVM/Node。镜像可以只有几 MB，启动快，资源占用低。但“适合”不等于“自动正确”：最终镜像必须包含时区证书等运行依赖，必须注入版本和配置，必须以非 root 运行，并且不能用构建机的本地路径。容器化解决的是交付一致性，不解决程序自身的超时、内存和优雅退出问题。

### 标准多阶段 Dockerfile

```dockerfile
# ---------- 构建阶段 ----------
FROM golang:1.23-alpine AS builder

WORKDIR /src
# 先拷贝依赖文件，利用 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/server ./cmd/server

# ---------- 运行阶段 ----------
FROM alpine:3.20

RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder /out/server /app/server
COPY --from=builder /src/configs /app/configs

USER app
EXPOSE 8080
ENTRYPOINT ["/app/server"]
```

逐行解释：

| 配置 | 作用 | 不写会怎样 |
|---|---|---|
| 多阶段构建 | 构建工具不进入最终镜像 | 镜像几百 MB，攻击面大 |
| 先 COPY go.mod | 依赖不变时复用缓存 | 每次改代码都重新下载依赖 |
| `CGO_ENABLED=0` | 静态链接，能在 alpine/scratch 跑 | 依赖 glibc，换基础镜像就报错 |
| `-trimpath` | 去掉本机路径 | 镜像泄漏构建目录，构建不可复现 |
| `-ldflags="-s -w"` | 去掉符号表/调试信息 | 镜像更大 |
| `-X main.version` | 注入版本号 | 线上不知道跑的是哪个版本 |
| 非 root 用户 | 降低容器逃逸风险 | 被攻破后权限过大 |
| 固定基础镜像版本 | 构建可复现 | `latest` 某天变了，构建结果不同 |

### 构建和运行

构建后先在本地以和生产相同的方式运行，验证健康检查、配置读取和优雅退出。下面的命令只把必要端口映射出来，配置通过环境变量注入；真实密码不能写在命令历史、镜像 tag 或讲义里。

```bash
docker build -t registry.example.com/demo/recharge:v1.2.3 .
docker run --rm -p 8080:8080 \
  -e APP_ENV=dev \
  registry.example.com/demo/recharge:v1.2.3

curl -sS http://127.0.0.1:8080/healthz
# {"status":"ok"}
```

判断启动成功不能只看 `docker run` 没有立刻退出。预期是容器保持运行、日志出现“配置加载完成/开始监听”，`docker ps` 能看到端口映射，`/healthz` 返回 200。若容器立即退出，先执行 `docker ps -a` 看退出码，再执行 `docker logs CONTAINER`；常见原因是配置 key 缺失、数据库地址不可达、监听地址只写了 `127.0.0.1`、镜像架构不匹配或内存 limit 太小。修完后重新构建并重复这组验证，不能只因为“命令返回了容器 ID”就认为发布成功。

### `.dockerignore`

```
.git
*.log
tmp/
coverage.out
.env
```

不写会把 `.git` 和本地密钥打进构建上下文，构建慢且有泄漏风险。

### 镜像仓库

```bash
docker tag recharge:v1.2.3 registry.example.com/demo/recharge:v1.2.3
docker push registry.example.com/demo/recharge:v1.2.3
```

镜像 tag 用**不可变版本**（git sha 或语义版本），不要只用 `latest`。回滚需要明确的旧版本。

### 容器里不该做什么

容器应该是无状态的运行单元，重启后能靠镜像和外部配置恢复。把配置、密钥、唯一数据或启动时迁移塞进容器，会让多副本发布出现不可控的竞态。下面这些规则不是“最佳实践口号”，每一条都对应真实故障：密钥泄漏、日志丢失、迁移重复执行、磁盘写满。

- 不把配置和密钥打进镜像；
- 不以 root 运行；
- 不把日志只写容器文件系统（重启就没了），输出到 stdout/stderr；
- 不在容器启动时跑数据库迁移（多副本会并发执行）；
- 不把临时文件写满容器磁盘。

---

## 17.3 Kubernetes：声明“我要几个什么样的实例”

Kubernetes 的核心是**声明式**：你写“我要 3 个副本、每个 0.5 核、健康检查这个路径”，控制器负责让现实收敛到期望。

### Deployment

Deployment 管理无状态 Pod 的副本和滚动更新。它保证“期望副本数”收敛，但不保证业务一定健康；Pod 是否接流量由 readiness 决定，进程是否该重启由 liveness 决定。配置里的副本数、资源、探针和发布策略必须一起评审。

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: recharge
  labels: { app: recharge }
spec:
  replicas: 3
  revisionHistoryLimit: 5
  selector:
    matchLabels: { app: recharge }
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  template:
    metadata:
      labels: { app: recharge, version: v1.2.3 }
    spec:
      terminationGracePeriodSeconds: 30
      containers:
        - name: recharge
          image: registry.example.com/demo/recharge:v1.2.3
          ports:
            - containerPort: 8080
          env:
            - name: APP_ENV
              value: prod
          envFrom:
            - secretRef: { name: recharge-secrets }
          resources:
            requests: { cpu: "200m", memory: "128Mi" }
            limits:   { cpu: "1",    memory: "512Mi" }
          readinessProbe:
            httpGet: { path: /readyz, port: 8080 }
            initialDelaySeconds: 3
            periodSeconds: 5
            failureThreshold: 3
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
            initialDelaySeconds: 10
            periodSeconds: 10
            failureThreshold: 3
          lifecycle:
            preStop:
              exec: { command: ["sleep", "5"] }
```

这份 YAML 不是贴上去就结束。先执行 `kubectl apply --dry-run=server -f deployment.yaml` 检查 API、字段和权限，再 apply，随后用下面命令确认控制器真的收敛：

```bash
kubectl get deployment recharge
kubectl get pods -l app=recharge -o wide
kubectl rollout status deployment/recharge --timeout=5m
kubectl get endpointslices -l kubernetes.io/service-name=recharge
```

预期依次是 Deployment 的 `READY` 达到 `3/3`、三个 Pod 分布在可接受节点且 `STATUS=Running`、rollout 返回成功、Service 的 Endpoint 至少包含三个就绪地址。`READY 2/3` 或 Endpoint 少一个，都说明有 Pod 没通过 readiness，不能把它当成“大致成功”。

逐项解释：

- `replicas: 3`：三个实例，滚动发布时至少有一个可用；
- `maxUnavailable: 0`：更新过程中不允许可用副本低于期望值；
- `maxSurge: 1`：先多起一个，再逐个替换；
- `terminationGracePeriodSeconds: 30`：给进程处理完在途请求的时间；
- `requests`：调度依据，K8s 按这个找节点；
- `limits`：上限，超内存会被 OOMKill；
- `readinessProbe`：没通过就不接流量；
- `livenessProbe`：连续失败会重启容器；
- `preStop sleep 5`：让 Service 先把该实例摘掉，再退出进程。

### 探针不能随便写

很多事故来自探针配错：

| 错误 | 后果 |
|---|---|
| liveness 检查数据库 | 数据库抖动导致所有 Pod 一起重启，雪崩 |
| readiness 永远返回 200 | 进程没初始化完就接流量 |
| initialDelay 太短 | 启动慢的服务被反复重启 |
| 超时时间太长 | 坏实例长时间留在负载均衡里 |

正确做法：

- `/healthz`：只检查进程是否活着，不查外部依赖；
- `/readyz`：检查配置加载、连接池初始化、必要的本地状态；如果数据库短暂不可用时服务仍可降级接流量，就不要把数据库健康塞进 readiness；
- 依赖健康放到业务指标/告警；如果必须摘流量，要明确摘除后用户会看到什么、多久恢复。

验证方式不是在 YAML 里读一遍。进入 Pod 请求两个端点，确认状态码和响应体；然后人为让依赖不可用：readiness 应把实例从 Endpoint 摘掉但不重启，恢复后重新加入；liveness 不应因为同一个依赖抖动而反复重启。若观察到所有 Pod 同时重启，先检查 liveness 是否错误地依赖了数据库。

### Service

Service 给一组 Pod 提供稳定虚拟 IP 和 DNS 名称。Pod 会重建、IP 会变化，调用方不应直接写 Pod IP。Selector 必须和 Pod 标签完全匹配；一个拼错的标签不会让 Service 报错，只会得到“没有后端”的 503/超时，因此排障时先看 Endpoint。

```yaml
apiVersion: v1
kind: Service
metadata:
  name: recharge
spec:
  selector: { app: recharge }
  ports:
    - port: 80
      targetPort: 8080
  type: ClusterIP
```

`ClusterIP` 只集群内可访问；对外通常由 Ingress/Nginx/云负载均衡暴露。Service 创建成功不代表流量能到 Pod，必须检查 Endpoint：

```bash
kubectl get endpointslices -l kubernetes.io/service-name=recharge
kubectl run curl-test --rm -it --image=curlimages/curl --restart=Never --   curl -v http://recharge/healthz
```

如果 Endpoint 为空，优先查 selector 与 Pod label、Pod readiness；如果 Endpoint 有地址但请求失败，查 `targetPort`、网络策略和容器监听地址。

### Ingress

Ingress 负责把域名和路径路由到 Service。配置后先看 Ingress 是否拿到地址、控制器是否接受规则，再用真实 Host 请求验证：

```bash
kubectl get ingress recharge
kubectl describe ingress recharge
curl -v -H 'Host: api.example.com' http://INGRESS_ADDRESS/healthz
```

如果返回 404，检查 `pathType`、路径是否被 rewrite、Service 名称/端口以及控制器日志；如果返回 503，通常是 Service 没有就绪 Endpoint。

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: recharge
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "30"
spec:
  ingressClassName: nginx
  rules:
    - host: api.example.com
      http:
        paths:
          - path: /v1/recharges
            pathType: Prefix
            backend:
              service:
                name: recharge
                port: { number: 80 }
```

### 配置与密钥

ConfigMap 适合普通配置，Secret 适合凭证，但 Secret 默认只是 Kubernetes 内部对象，不是自动加密的保险箱。生产要启用静态加密、最小 RBAC、审计和外部密钥管理；配置变更也要版本化并能在出问题时回退。不要把 DSN、token 或私钥写进镜像层和 Git。

```yaml
apiVersion: v1
kind: ConfigMap
metadata: { name: recharge-config }
data:
  app.yaml: |
    address: ":8080"
    log_level: info
---
apiVersion: v1
kind: Secret
metadata: { name: recharge-secrets }
type: Opaque
stringData:
  MYSQL_DSN: "demo:change-me@tcp(mysql:3306)/minicoin?parseTime=true"
```

Secret 默认只是 base64 编码，不是加密。生产要用云 KMS/密钥管理，并限制 RBAC。验证配置不能只执行 `kubectl get secret`；应启动一个临时排查 Pod，用应用相同的挂载/环境读取配置，确认 key 名、大小写、换行和权限正确；再用故意缺失的 key 做一次启动失败测试，确保应用明确报错且不打印敏感值。

### 发布与回滚

发布命令只负责把新镜像交给控制器，真正的完成标准是新 Pod Ready、旧 Pod 正常退出、入口健康检查通过、业务指标没有恶化。`rollout status` 超时并不一定自动回滚，值班人必须根据事件、日志和指标决定继续等待、暂停还是执行回滚。

```bash
# 发布新版本
kubectl set image deployment/recharge recharge=registry.example.com/demo/recharge:v1.2.4
kubectl rollout status deployment/recharge --timeout=5m

# 看历史
kubectl rollout history deployment/recharge

# 回滚到上一版
kubectl rollout undo deployment/recharge

# 回滚到指定版本
kubectl rollout undo deployment/recharge --to-revision=3
```

`rollout status` 会卡住直到新副本可用；如果超时，先看：

```bash
kubectl get pods -l app=recharge
kubectl describe pod <pod>
kubectl logs <pod> --previous
kubectl get events --sort-by=.lastTimestamp
```

常见原因：镜像拉取失败、配置缺失、探针失败、资源不足、启动即 panic。按顺序先看 Pod 状态和事件，再看 `logs --previous` 的退出现场，最后对照 Deployment 的镜像、环境变量和探针。不要在生产容器里随意安装工具或改文件；确需进入时只做只读取证。

一次可验证的回滚演练至少包含：

1. 发布一个故意失败但不会写数据的镜像，确认告警和 `rollout status` 发现问题；
2. 执行 `kubectl rollout undo`，观察旧 ReplicaSet 恢复；
3. 用 smoke test 验证健康检查、核心接口和业务查询；
4. 检查发布期间没有重复写入、消息积压或缓存脏数据；
5. 把耗时、命令和问题写进运行手册。

### 资源限制怎么定

`requests` 决定调度器按多少资源预留，`limits` 决定容器最多能用多少；两者设错的结果不同。requests 过低会让节点超卖，limits 过低会让 Go 进程 OOMKill 或被 CPU 节流。先用压测观察 RSS、heap、GC 和 CPU，再留出启动、突发和 runtime 开销，不能抄一个网上模板。

```yaml
resources:
  requests: { cpu: "200m", memory: "128Mi" }
  limits:   { cpu: "1",    memory: "512Mi" }
```

- `requests` 太小：节点超卖，实际运行时抢不到 CPU；
- `limits` 太小：Go 进程被 OOMKill，`kubectl describe` 会显示 `OOMKilled`；
- CPU limit 太低：GC 和业务线程被节流，延迟抖动；
- 内存 limit 要大于 Go 堆峰值 + runtime 开销，通常留 20%～30%。

Go 服务在容器里要设置 `GOMEMLIMIT`（如 400MiB）帮助 GC 提前工作，但不能替代合理 limit。

### HPA

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: { name: recharge }
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: recharge
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target: { type: Utilization, averageUtilization: 70 }
```

HPA 根据 CPU/内存/自定义指标扩缩容。注意扩容有延迟（拉镜像、启动、预热），突发流量要配合入口限流和预留副本。部署后执行 `kubectl get hpa recharge`，预期 `TARGETS` 有数值、`MINPODS/MAXPODS` 正确、`REPLICAS` 在当前范围内；如果显示 `<unknown>`，检查 metrics-server/自定义指标和 Pod requests。压测抬升 CPU 后验证副本增加，停止压力后观察缩容不会引起错误率上升。

---

## 17.4 Nginx：入口的交通警察

### 反向代理

Nginx 接在客户端和应用之间，负责 TLS、连接复用、超时和基础限流。代理超时必须和后端预算协调：如果 Nginx 等待 10 秒、应用却允许 30 秒，客户端会先收到 504，后端仍继续执行写操作。下面用 DNS 名称代替真实后端 IP，实际部署时由服务发现或负载均衡提供地址。

```nginx
upstream recharge_backend {
    least_conn;
    server app-1.example.com:8080 max_fails=3 fail_timeout=10s;
    server app-2.example.com:8080 max_fails=3 fail_timeout=10s;
    keepalive 64;
}

server {
    listen 80;
    server_name api.example.com;

    client_max_body_size 2m;

    location /v1/ {
        proxy_pass http://recharge_backend;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Request-ID $request_id;

        proxy_connect_timeout 2s;
        proxy_send_timeout 10s;
        proxy_read_timeout 10s;
    }
}
```

解释：

- `least_conn`：把请求发给当前连接最少的后端；
- `max_fails/fail_timeout`：后端连续失败后暂时摘除；
- `keepalive`：复用后端连接，降低握手开销；
- `proxy_read_timeout`：后端超过 10s 不响应就断开，避免连接堆积；
- `X-Request-ID`：把请求 ID 传给后端，日志能串起来。

### 限流

入口限流用来保护后面的应用和数据库，不是为了让所有请求都“排完队”。`burst` 给短时突发留余量，`nodelay` 决定突发是否立即处理；阈值要按真实容量和业务优先级设置，并区分登录、查询、写操作。超过阈值应返回 429 和 `Retry-After`，同时记录被限流的路由，避免误伤正常用户。

```nginx
limit_req_zone $binary_remote_addr zone=api:10m rate=10r/s;

location /v1/ {
    limit_req zone=api burst=20 nodelay;
    limit_req_status 429;
    proxy_pass http://recharge_backend;
}
```

`rate=10r/s` 是平均速率，`burst=20` 允许 20 个突发，`nodelay` 表示突发不排队直接处理。超过返回 429。

### 常见 Nginx 问题

Nginx 的状态码描述的是代理这一层看到的结果，不等于后端根因。看到 502 先确认 upstream 是否可达，看到 504 再分后端处理慢、连接池排队或代理超时。排查时同时看 `nginx -t`、错误日志、upstream 状态和应用日志；只改超时时间往往只是把问题藏得更久。

| 现象 | 原因 | 排查 |
|---|---|---|
| 502 Bad Gateway | 后端没启动/端口错/崩溃 | `curl` 后端、看 upstream 日志 |
| 504 Gateway Timeout | 后端超过 `proxy_read_timeout` | 看后端延迟和慢查询 |
| 413 Request Entity Too Large | `client_max_body_size` 太小 | 调整或让客户端压缩 |
| 499 | 客户端主动断开 | 后端太慢或客户端超时 |
| 真实 IP 不对 | 没传 `X-Forwarded-For` | 配置 trusted proxy |

---

## 17.5 云上常见形态

| 组件 | 作用 | 自建 vs 云服务 |
|---|---|---|
| 负载均衡（SLB/ALB） | 流量分发、TLS、健康检查 | 云服务省运维 |
| 对象存储（S3/OSS） | 图片、备份、静态文件 | 几乎都用云服务 |
| 托管数据库（RDS） | MySQL/PG 备份、主从、监控 | 小团队优先托管 |
| 托管 Redis | 缓存高可用 | 优先托管 |
| 托管 Kafka/MQ | 消息队列 | 看规模和成本 |
| KMS | 密钥管理 | 合规场景必备 |
| 日志/监控服务 | 集中日志、指标、告警 | 云服务或自建 Prometheus/Grafana |

云不是“不用运维”，而是把**底层硬件和部分数据库运维**交给云；应用配置、容量、发布、告警仍然要你负责。

---

## 17.6 集群与高可用

高可用的基本思路：**消除单点 + 故障自动转移 + 容量冗余**。副本数不等于高可用：三个实例如果同机、同可用区、连同一个数据库，仍然会一起失败。先画请求路径和依赖，再逐层检查实例、负载均衡、数据库、缓存、消息和配置是否都有单点。

高可用还要定义 RTO（恢复要多久）和 RPO（允许丢多少数据），并通过故障演练验证，而不是只在架构图上画双副本。

- 应用至少 2 个副本，跨可用区；
- 数据库主从 + 自动切换，定期演练；
- Redis 主从 + Sentinel/Cluster；
- 负载均衡健康检查；
- 监控和告警独立于业务集群；
- 备份异地保存，定期恢复演练。

高可用不是“不会坏”，而是“坏了能在可接受时间内恢复”。要定义 RTO（恢复时间）和 RPO（可丢多少数据）。

---

## 17.7 微服务：什么时候拆，怎么拆

### 拆分信号

拆微服务应该有可量化的收益，例如团队发布互相阻塞、某模块需要独立扩缩容、故障隔离或数据边界已经清楚。仅凭“代码多”“想用 RPC”不是拆分理由。下面列的是信号，不是自动触发条件，仍要估算网络、事务、部署和排障成本。

- 多个团队改同一个仓库频繁冲突；
- 某模块需要独立扩容（搜索 vs 交易）；
- 某模块故障需要隔离；
- 发布节奏差异大；
- 有明确业务边界和独立数据。

### 不要拆的理由

团队规模和基础设施能力往往比业务边界更早成为瓶颈。拆完后每个服务都需要日志、指标、trace、发布、回滚、备份和值班；如果这些还没有统一，拆分会把本地函数调用变成跨网络故障。下面这些情况应先做模块化单体。

- 团队小，一个人维护 10 个服务；
- 没有 CI/CD、监控、链路追踪；
- 事务边界不清，拆完到处分布式事务；
- 只是为了“技术先进”。

### 拆分后的代价

拆分不是免费的架构升级，而是把复杂度从代码里移到网络上。一次本地调用变成 RPC 后，会出现超时、重试、重复消费、部分成功、版本不兼容和链路追踪问题；原先一个数据库事务也可能变成 Saga 和补偿。把这些成本写进方案，再判断是否值得。

- 网络调用替代本地调用：延迟、超时、重试、幂等；
- 数据一致性：Outbox、Saga、对账；
- 运维：更多服务、更多告警、更多部署；
- 排障：需要 trace id 串起多个服务。

**先单体，后拆分。**单体内部保持清晰模块边界，等拆分收益大于成本再动手。

---

## 17.8 运维要不要学

要学到“能独立上线和止损”的程度，不等于成为专职运维。后端至少要能判断故障在应用、容器、代理、网络还是数据库，能执行发布和回滚，能把证据交给对应负责人。只会写代码但不知道服务怎么运行，线上出问题时无法保护用户。

- 会看 CPU/内存/磁盘/网络；
- 会看容器日志和 K8s 事件；
- 会配置探针、资源限制、副本数；
- 会发布、回滚、扩容；
- 会看 Nginx 502/504；
- 会配置基础告警。

不需要一开始就精通内核调优、K8s 源码、网络协议。遇到问题知道去查什么、找谁，比背命令重要。

---

## 17.9 上线检查清单

清单不能只是在发布前看一眼。每一项都应有命令或证据：镜像 digest 在哪里、探针路径实际返回什么、回滚命令有没有演练、数据库迁移是否能被旧版本读取。无法证明的项应标为未验证，而不是默认打勾。

```markdown
- [ ] 镜像 tag 是 commit sha 或语义版本，不是 latest
- [ ] 容器非 root，配置和密钥不在镜像里
- [ ] readiness/liveness 分离，liveness 不查外部依赖
- [ ] requests/limits 已按压测设置
- [ ] 至少 2 副本，跨可用区
- [ ] 滚动发布 maxUnavailable=0
- [ ] preStop + 优雅退出时间匹配
- [ ] Nginx/Ingress 超时大于后端 P99
- [ ] 日志输出 stdout，带 request_id/version
- [ ] 监控看板、告警、值班人就位
- [ ] 回滚命令已验证
```

---

## 习题

### 基础题

1. 多阶段构建解决了什么问题？怎样验证最终镜像里没有编译器和源码？
2. 为什么 Go 服务常用 `CGO_ENABLED=0`？
3. `requests` 和 `limits` 的区别是什么？内存 limit 太小会发生什么？
4. readiness 和 liveness 探针分别应该检查什么？为什么 liveness 不能查数据库？
5. `maxUnavailable: 0` 和 `maxSurge: 1` 的滚动发布过程是怎样的？
6. Nginx 502 和 504 分别通常是什么原因？
7. 为什么镜像 tag 不能用 `latest`？
8. 微服务拆分后，原来本地事务会变成什么问题？

### 动手题

9. 给一个 Go 服务写 Dockerfile，要求：多阶段、非 root、静态编译、版本注入。
10. 写一个 Deployment，要求 3 副本、滚动更新不减少可用副本、readiness/liveness 分离、CPU/内存限制。
11. 写一个 Nginx 配置，把 `/v1/` 代理到两个后端，设置 10s 读超时和每 IP 10r/s 限流。
12. 模拟发布失败：镜像 tag 不存在，写出 `kubectl` 排查命令和回滚命令。

### 综合题

13. 一个 Go 服务上线后 Pod 反复重启，`kubectl describe` 显示 `OOMKilled`。写出排查步骤：是内存泄漏、limit 太小，还是流量突增？如何验证和修复？

---

## 参考答案

<details>
<summary>展开参考答案</summary>

### 1. 多阶段构建

运行镜像只需要编译后的二进制，不需要 Go 工具链、源码和构建缓存。多阶段构建把“构建环境”和“运行环境”分开，镜像更小、攻击面更小。

```dockerfile
# syntax=docker/dockerfile:1.7
FROM golang:1.24-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /out/server /app/server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/server"]
```

验证：

```bash
docker build --build-arg VERSION=v1.2.3 -t minicoin:v1.2.3 .
docker run --rm --entrypoint /app/server minicoin:v1.2.3 --version
docker run --rm --entrypoint sh minicoin:v1.2.3 -c 'command -v go' ; echo "exit=$?"
```

`command -v go` 找不到 Go（退出码非 0）才对。用 `docker history --no-trunc` 检查没有把 `.git`、测试文件或构建缓存带进最终层。

---

### 2. 为什么 `CGO_ENABLED=0`

Go 默认可能链接 libc。`CGO_ENABLED=0` 生成静态二进制，可以放进 `scratch`/distroless 这类没有 glibc 的镜像，也避免运行机器 glibc 版本不一致。

验证：

```bash
CGO_ENABLED=0 go build -o /tmp/server ./cmd/server
file /tmp/server          # statically linked
ldd /tmp/server           # not a dynamic executable
```

例外：用了需要 C 库的驱动（如某些 Oracle/SQLite 实现）时不能盲目关闭，要么改用纯 Go 驱动，要么在运行镜像里保留对应动态库。

---

### 3. `requests` 和 `limits`

- `requests`：调度器保证的最小资源，用来决定 Pod 放到哪个节点；
- `limits`：容器能使用的上限。CPU 超限会被节流，内存超限会被 OOMKill。

```yaml
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi
```

内存 limit 太小的表现：Pod 在压测或流量高峰反复 `OOMKilled`，RSS 曲线贴着 limit。

```bash
kubectl describe pod POD | grep -A6 'Limits\|Requests\|Last State'
kubectl top pod POD
```

修复不是直接把 limit 调大：先看 heap profile、缓存条目数和流量曲线，区分泄漏、缓存无上限和正常峰值。若确认是峰值，按“实测峰值 + runtime/GC 余量”设置，并同步检查节点容量。

---

### 4. readiness 和 liveness

| 探针 | 回答什么 | 失败动作 |
|---|---|---|
| startup | 启动完成了吗 | 暂不执行其他探针 |
| readiness | 现在能接流量吗 | 从 Service Endpoint 摘除 |
| liveness | 进程还活着吗 | 重启容器 |

liveness **不能查数据库**：数据库抖动会让所有 Pod 同时重启，反而扩大故障。

```yaml
readinessProbe:
  httpGet: {path: /readyz, port: 8080}
  periodSeconds: 5
  failureThreshold: 3
livenessProbe:
  httpGet: {path: /healthz, port: 8080}
  periodSeconds: 10
  failureThreshold: 6
```

`/readyz` 可以检查配置加载、迁移完成、必要依赖是否可用；`/healthz` 只检查进程内部状态（事件循环、关键 goroutine）。

验证：在测试环境停掉 MySQL，readiness 失败且 Endpoint 被摘除，但 Pod 不重启；恢复后自动重新接流量。

---

### 5. `maxUnavailable: 0` + `maxSurge: 1`

期望 3 副本时：

1. 新建第 4 个 Pod（surge）；
2. 等它 Ready；
3. 删除 1 个旧 Pod；
4. 再新建一个，重复直到 3 个新 Pod。

整个过程可用副本数不低于 3。

```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 0
    maxSurge: 1
```

验证：

```bash
kubectl get pods -w
kubectl rollout status deployment/minicoin --timeout=5m
kubectl get endpointslices -l kubernetes.io/service-name=minicoin
```

`-w` 输出里应看到新 Pod 先 `Ready`，旧 Pod 才 `Terminating`；`rollout status` 最终输出 `successfully rolled out`。

---

### 6. Nginx 502 和 504

| 状态 | 含义 | 优先查 |
|---|---|---|
| 502 Bad Gateway | 连不上上游，或上游提前断开 | 进程、端口、监听地址、连接被拒 |
| 504 Gateway Timeout | 上游连上了但超时没响应 | 应用 P99、下游依赖、代理超时 |

```bash
# 502：直连上游，区分“应用没起”还是“Nginx 配错”
curl -v http://127.0.0.1:8081/healthz

# 504：看应用和 Nginx 的耗时
kubectl logs deploy/minicoin --since=10m | jq 'select(.duration_ms > 1000)'
tail -f /var/log/nginx/error.log | grep upstream
```

502 常见原因：后端只监听 `127.0.0.1`、端口写错、容器崩溃、连接池满导致 accept 失败。504 常见原因：慢 SQL、锁等待、下游超时、Nginx `proxy_read_timeout` 小于应用处理时间。

修复后验证：经 Nginx 请求核心接口，状态码、响应体和 `request_id` 与直连一致；再压测 5 分钟观察 502/504 为 0。

---

### 7. 为什么镜像 tag 不能用 `latest`

`latest` 会漂移：同一份 Deployment 在不同时间拉到的可能是不同代码，无法从镜像反查 commit，也无法稳定回滚。

```text
minicoin:latest                 # 错
minicoin:git-9f3a1c2            # 可追溯
minicoin:v1.2.3@sha256:...      # 最稳，tag + digest
```

验证：

```bash
docker image inspect minicoin:v1.2.3 --format '{{index .RepoDigests 0}}'
kubectl get deploy minicoin -o jsonpath='{.spec.template.spec.containers[0].image}'
```

发布系统应记录 `commit sha`、镜像 digest 和发布人。回滚时用 digest，不用 `latest`。

---

### 8. 微服务拆分后的本地事务

原来一个数据库事务能保证“扣款 + 写流水”原子完成；拆成两个服务后，变成跨网络调用，可能出现：

- 扣款成功、写流水失败；
- 调用超时但对方实际成功；
- 重试导致重复扣款；
- 两个服务发布版本不兼容。

解决手段：

- Outbox：本地事务写业务表和待发消息，提交后异步投递；
- Saga：每一步有补偿动作；
- 幂等键 + 唯一约束：处理重试和重复消费；
- 对账任务：最终发现不一致；
- 版本兼容：先扩展、再迁移、最后收缩。

验证：注入“第二步超时”，确认系统最终通过重试/补偿达到一致，并且重复消息不会重复扣款。

---

### 9. Go 服务 Dockerfile

同第 1 题。关键要求逐项核对：

```text
□ 多阶段：builder + runtime
□ 只复制二进制和必要配置
□ CGO_ENABLED=0（或明确保留动态库）
□ 非 root USER
□ 版本通过 -ldflags -X 注入
□ .dockerignore 排除 .git、测试数据、密钥
□ 有 HEALTHCHECK 或由编排系统探针检查
```

构建并验证：

```bash
docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t minicoin:test .
docker run -d --name mc -p 18080:8080 minicoin:test
curl -fsS http://127.0.0.1:18080/healthz
docker exec mc id                 # uid 非 0
docker stop -t 15 mc             # 观察优雅退出日志
```

---

### 10. Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minicoin
spec:
  replicas: 3
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  selector:
    matchLabels: {app: minicoin}
  template:
    metadata:
      labels: {app: minicoin}
    spec:
      terminationGracePeriodSeconds: 30
      containers:
        - name: app
          image: registry.example.com/minicoin:v1.2.3
          ports:
            - containerPort: 8080
          readinessProbe:
            httpGet: {path: /readyz, port: 8080}
            periodSeconds: 5
          livenessProbe:
            httpGet: {path: /healthz, port: 8080}
            periodSeconds: 10
          resources:
            requests: {cpu: 100m, memory: 128Mi}
            limits: {cpu: 500m, memory: 512Mi}
          securityContext:
            runAsNonRoot: true
            allowPrivilegeEscalation: false
```

验证：

```bash
kubectl apply --dry-run=server -f deploy.yaml
kubectl apply -f deploy.yaml
kubectl rollout status deployment/minicoin --timeout=5m
kubectl get pods -l app=minicoin -o wide
kubectl get endpointslices -l kubernetes.io/service-name=minicoin
```

发布时用 `kubectl get pods -w` 确认新 Pod Ready 后才删除旧 Pod；`terminationGracePeriodSeconds` 必须大于应用 shutdown 超时。

---

### 11. Nginx 反向代理 + 限流

```nginx
limit_req_zone $binary_remote_addr zone=perip:10m rate=10r/s;

upstream minicoin {
    server 10.0.0.11:8080;
    server 10.0.0.12:8080;
    keepalive 32;
}

server {
    listen 80;
    server_name api.example.com;

    location /v1/ {
        limit_req zone=perip burst=20 nodelay;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Request-ID $request_id;
        proxy_connect_timeout 2s;
        proxy_send_timeout 10s;
        proxy_read_timeout 10s;
        proxy_pass http://minicoin;
    }
}
```

验证：

```bash
nginx -t
ab -n 200 -c 20 http://127.0.0.1/v1/healthz
# 超过 10r/s 的请求应出现 429
curl -i http://127.0.0.1/v1/recharges -d '{}' -H 'Content-Type: application/json'
```

再直连后端对比状态码和 `X-Request-ID`，确认代理没有改写业务错误。限流阈值要按真实容量压测得出，不能照抄 10r/s。

---

### 12. 镜像 tag 不存在导致发布失败

```bash
kubectl get pods
kubectl describe pod POD
kubectl get events --sort-by=.lastTimestamp
kubectl rollout status deployment/minicoin --timeout=60s
```

典型输出：

```text
Warning  Failed  ...  Failed to pull image "registry.example.com/minicoin:v9.9.9": not found
```

排查顺序：

1. 确认镜像 tag 是否真的推送成功：`docker manifest inspect registry.example.com/minicoin:v9.9.9`；
2. 检查 imagePullSecret 和仓库权限；
3. 检查 Deployment 里的 image 是否被 CI 变量替换错；
4. 回滚：

```bash
kubectl rollout undo deployment/minicoin
kubectl rollout status deployment/minicoin --timeout=5m
kubectl get pods -l app=minicoin -o wide
```

回滚后请求 `/healthz` 和核心接口，确认旧版本 digest 生效。最后补 CI 门禁：镜像不存在时禁止更新 Deployment。

---

### 13. Pod 反复 OOMKilled

```bash
kubectl describe pod POD | grep -E 'OOMKilled|Limits|Requests|Last State|Restart Count'
kubectl top pod POD
kubectl logs POD --previous
kubectl get pod POD -o yaml | grep -A10 resources
```

三种原因与验证：

| 原因 | 证据 | 修复 |
|---|---|---|
| 内存泄漏 | RSS 持续上涨，GC 后不回落，profile 显示对象增长 | 修代码，加内存回归测试 |
| limit 太小 | 压测时 RSS 峰值超过 limit，但稳定后正常 | 按峰值 + 余量调整，并核对节点容量 |
| 流量突增 | QPS 与 RSS 同步上涨，扩容后缓解 | 扩容 + 入口限流 + 缓存优化 |

Go 服务抓 heap profile：

```bash
curl -s "http://POD:8080/debug/pprof/heap" > heap.pprof
go tool pprof -top heap.pprof
```

修复后不要只看“Pod 不重启了”，要在相同负载下观察 30 分钟：RSS 曲线平稳、GC 周期正常、P99 和错误率回到基线。

</details>
