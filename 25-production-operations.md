# 第 25 章 生产运行手册：Linux、Docker、Kubernetes、Nginx 与云

> 这一章解决的不是“会背几个命令”，而是：服务发布后出了问题，你能沿着请求路径找到第一故障点，做最小止损，并用证据验证恢复。
>
> 本章命令全部可以在一台练习机或本地容器环境执行；域名、镜像仓库、账号和路径都是虚构值。没有授权时，不要在生产环境执行写操作、杀进程、改配置或重启服务。

## 25.1 开发机是什么，为什么需要

开发机是一台长期在线、环境接近测试/生产的 Linux 机器。前端通常只需要本地 Node 进程就能开发；后端经常还要同时连接 MySQL、Redis、消息队列、注册中心，甚至要访问只在受控网络里开放的测试依赖。把这些东西全部搬进个人电脑，常见结果是环境不一致、凭证散落、压测影响别人，或者本地系统与线上内核/网络行为不同。

典型分工是：

- 本地编辑器写代码，格式化、单测、断点调试尽量在本地完成；
- 远程开发机负责 Linux 构建、集成测试、联调和压测；
- CI 用干净环境重新构建，证明“不是只有你的机器能跑”；
- 测试/预发环境验证真实部署形态；
- 生产只接受通过门禁的不可变产物。

### 连接开发机：先建立可重复的连接

```bash
ssh devbox
```

不要每次手敲地址、用户名和密钥路径。把它们写入 `~/.ssh/config`：

```sshconfig
Host devbox
  HostName devbox.example.com
  User demo
  IdentityFile ~/.ssh/id_ed25519_demo
  IdentitiesOnly yes
  ServerAliveInterval 30
  ServerAliveCountMax 3
```

`IdentitiesOnly yes` 防止 SSH 把本机所有密钥依次试一遍；`ServerAliveInterval` 每 30 秒探测一次连接，网络短暂空闲时不容易被中间设备断开。

连接成功后不要立刻开始改代码，先确认自己落在哪台机器、哪个目录、什么资源状态：

```bash
hostname
whoami
pwd
uptime
df -h
free -h
nproc
```

怎么读：

- `hostname` 必须和预期机器一致，避免误在生产机器执行命令；
- `pwd` 确认当前目录，后续相对路径都基于它；
- `uptime` 的 load average 要和 CPU 核数一起看，不能单独说“load 高”；
- `df -h` 看磁盘使用率和挂载点，尤其注意 `/`、`/var`、数据盘是否分开；
- `free -h` 同时看 available 和 swap，不要只看 used；
- `nproc` 是可用逻辑核数，是判断负载和并行度的基准。

### 连不上排查：按网络层逐层缩小范围

不要一上来重装 SSH。按下面顺序检查，前一层没通，后面的认证和目录问题都无从谈起：

```bash
# 1. DNS 能否解析，解析结果是不是预期地址
getent hosts devbox.example.com

# 2. TCP 22 是否可达；超时和 connection refused 含义不同
nc -vz devbox.example.com 22

# 3. 看 SSH 在哪一步失败
ssh -vvv devbox
```

判读方法：

- DNS 无结果：检查域名、VPN、DNS 配置；
- `Connection timed out`：通常是网络不通、安全组/防火墙丢包；
- `Connection refused`：地址到了，但目标端口没有服务或拒绝连接；
- `Permission denied (publickey)`：网络已通，问题在用户名、密钥、权限或服务端授权；
- 登录成功后立刻断开：检查磁盘满、内存耗尽、登录 shell 配置、账号限制和服务端日志。

`ssh -vvv` 可能包含用户名、主机名、密钥路径和服务器指纹。发群前先删除这些信息，只保留错误发生的阶段。

### 本地写代码、开发机编译是什么意思

意思不是每次保存都在远程跑。推荐流程是：

1. 本地编辑器通过 SSH/远程工作区修改远程仓库，或本地 Git 提交后推送到远端；
2. 在开发机拉取同一 commit，执行 `gofmt`、`go test`、`go build`；
3. 需要真实依赖时在开发机启动服务或集成测试；
4. 结果、日志和失败命令回到本地继续修改。

关键是保证本地和开发机使用**同一个 Git commit、同一份 `go.mod`、同一套环境变量约定**。如果本地改了文件却没同步，远程测试证明的不是你眼前的代码。

## 25.2 Linux 进程、端口和资源

线上排障的第一步通常不是看代码，而是确认进程是否存在、端口是否在听、资源是否已经耗尽。

### 进程：先确认“谁在跑、跑得怎么样”

```bash
ps aux | grep mini-coin
pgrep -af mini-coin
systemctl status mini-coin
journalctl -u mini-coin -n 200 --no-pager
```

`ps aux` 适合看 PID、CPU、内存、启动命令；`pgrep -af` 直接按名字找进程并显示完整参数；`systemctl status` 告诉你服务是否 active、最近一次退出码和重启时间；`journalctl` 看服务日志。

预期结果不是“有输出”就算正常，而是：

- 只有一个预期 PID，或有明确的多 worker 设计；
- 启动参数和部署版本一致；
- `Active: active (running)`，没有频繁 restart；
- 日志没有启动即 panic、配置错误、端口占用或数据库连接失败。

失败场景：`ps` 能看到进程但端口不通，可能是进程启动后卡在初始化；`systemctl` 反复 restart，要看 `journalctl` 的退出原因；有多个旧 PID，说明发布脚本没有正确停止旧版本，可能造成端口争抢或双版本同时处理请求。

### 端口：确认监听地址和占用者

```bash
ss -lntp
ss -lntp | grep 8081
lsof -iTCP:8081 -sTCP:LISTEN
```

要同时看三件事：

- 监听的是 `127.0.0.1:8081` 还是 `0.0.0.0:8081`；前者只能本机访问，容器/负载均衡可能连不上；
- 监听进程是不是当前部署的实例；
- 端口是否被旧进程或其他服务占用。

如果没有输出，依次检查进程是否启动、配置端口是否被环境变量覆盖、容器端口是否正确映射。不要直接 `kill -9` 占用者，先确认它是不是数据库代理、sidecar 或另一个合法实例。

### 信号：区分优雅退出和强杀

```bash
kill -TERM PID   # 请求优雅退出，生产默认
kill -KILL PID   # 强制杀死，最后手段
```

Go 服务应监听 `SIGTERM`：停止接收新请求，等待在途请求完成，在超时上限内关闭数据库、Redis、消息队列客户端，最后退出。`SIGKILL` 不给进程清理机会，可能造成正在处理的消息未确认、临时文件残留或连接被上游认为仍存活。

验证优雅退出不能只看“kill 后进程没了”。执行：

```bash
kill -TERM PID
ss -lntp | grep 8081        # 应逐步停止监听
journalctl -u mini-coin -n 50 --no-pager
```

日志里应能看到停止接收、等待在途请求、关闭依赖和最终退出；如果超过预设时间仍未退出，说明 shutdown 逻辑被阻塞，需要查 context、锁或下游调用。

### 资源：CPU、内存、磁盘、文件描述符一起看

```bash
top -Hp PID
free -h
df -h
du -sh /var/log/* | sort -h
iostat -xz 1
ss -s
ulimit -n
```

- `top -Hp PID` 按线程看 CPU，能发现某个 GC 线程、加密线程或自旋线程异常；
- `free -h` 重点看 `available`；`available` 很低且 swap 持续使用，说明内存压力已经影响系统；
- `df -h` 看容量，`df -i` 还要看 inode；大量小文件可能容量没满但 inode 先耗尽；
- `du` 找大目录，但会扫盘，生产上先限定目录并注意 I/O；
- `iostat -xz 1` 看 `%util`、await 和队列，判断磁盘是否成为瓶颈；
- `ss -s` 看连接总量和 TIME_WAIT；
- `ulimit -n` 看文件描述符上限，连接数高时上限过低会出现 `too many open files`。

### 磁盘满：先止损，再找增长源

```bash
df -h
df -i
du -xhd1 /var | sort -h
lsof +L1
find /var/log -type f -size +1G -ls
```

`lsof +L1` 找“文件已删除但进程还打开着”的文件；这类文件仍占空间，单纯 `rm` 不会立刻释放。处理顺序：

1. 先确认日志/临时文件不是事故证据，必要时复制到安全位置；
2. 对持续写入的日志使用 `truncate`，让文件句柄保持有效；
3. 检查日志轮转、容器日志上限和临时文件清理任务；
4. 加磁盘容量/inode 水位告警；
5. 修复增长源后再扩容，否则只是延后再次写满。

```bash
truncate -s 0 /var/log/mini-coin/app.log
```

不要直接删除正在被进程打开的大日志文件；也不要为了腾空间删除数据库数据文件、WAL 或备份。

### 内存高：区分 Go heap、RSS 和系统缓存

```bash
free -h
ps aux --sort=-%mem | head
pmap -x PID | tail
cat /proc/PID/status
```

Go 进程内存高时，要区分：

- heap 正在使用的对象；
- 已分配但等待 GC 回收的对象；
- goroutine 栈和泄漏；
- 大对象/大缓存的临时分配；
- 进程 RSS 与 Go heap 不一致，例如运行时保留但未归还操作系统的页。

`free` 的 buff/cache 不是“应用泄漏”；`ps` 的 RSS 也不能直接等于 Go heap。用 pprof 看 heap、goroutine 和分配栈，再结合 GC 指标判断。修复后要在相同负载、相同时间窗口比较 RSS、heap inuse、GC 次数和 P99，而不是重启后看一眼数字。

## 25.3 Nginx：入口层要解决什么

Nginx 通常位于入口，负责 TLS、反向代理、静态文件、请求头透传和基础限流。它不是“配一下就能上”的黑盒；每个超时、缓冲和请求头都影响故障表现。

```nginx
upstream mini_coin_backend {
    server 127.0.0.1:8081;
    keepalive 32;
}

server {
    listen 443 ssl;
    server_name api.example.com;

    ssl_certificate     /etc/nginx/tls/fullchain.pem;
    ssl_certificate_key /etc/nginx/tls/privkey.pem;

    location /v1/ {
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Request-Id $request_id;
        proxy_connect_timeout 1s;
        proxy_send_timeout 5s;
        proxy_read_timeout 5s;
        proxy_pass http://mini_coin_backend;
    }
}
```

解释几个容易配错的点：

- `proxy_http_version 1.1` 与 `keepalive` 配合，复用上游连接；
- `X-Forwarded-For` 记录链路，但要只在可信入口信任该头，不能让公网客户端随意伪造；
- `X-Request-Id` 由入口生成时，应用要优先沿用，保证前端、Nginx、应用日志能串起来；
- `proxy_connect_timeout` 是连接上游的超时，不等于上游处理超时；
- `proxy_read_timeout` 是等待上游响应的超时，设置过短会误杀正常慢请求，过长会占住连接；
- `proxy_pass` 的尾部斜杠和 location 会改变路径，必须用真实请求验证。

改配置后先做语法检查，再平滑重载：

```bash
nginx -t
nginx -s reload
curl -sS -D - https://api.example.com/healthz
tail -n 100 /var/log/nginx/error.log
```

`nginx -t` 成功只说明语法和证书路径可读，不代表上游可用；`curl -D -` 同时看状态码和响应头；错误日志要带时间窗口和上游地址。不要在生产用 `nginx -s stop` 做常规发布。

### 常见错误：先分清是谁失败了

| 现象 | 更像哪一层的问题 | 第一轮怎么查 |
|---|---|---|
| 502 Bad Gateway | Nginx 连到了上游但上游拒绝/断开/返回非法响应 | `ss` 看后端端口，`curl` 直连后端，查 Nginx `error.log` 的 upstream 信息 |
| 504 Gateway Timeout | Nginx 等上游超过 `proxy_read_timeout` | 对比 Nginx 日志、应用 P99、下游慢查询和线程/协程栈 |
| 413 Payload Too Large | 请求体超过 Nginx/网关限制 | 看 `client_max_body_size`、实际 Content-Length、是否走了不同入口 |
| 499 | 客户端提前断开 | 查客户端超时、Nginx 上游耗时、用户网络；不能只当成后端错误 |
| 404 | 路由、location、路径重写或上游前缀不对 | 用相同 Host/Path 做 `curl`，对比直连后端的路径和访问日志 |
| 401/403 | 认证或入口/WAF 权限策略 | 对比直连与经过入口的请求头、token、IP 策略 |

一个典型误区是看到 504 就重启应用。先确认请求是否真的到达应用：如果应用访问日志里没有这个 request_id，问题可能在入口路由或连接建立；如果应用有日志且一直等数据库，才进入数据库和锁的排查。

### 请求 ID：排障的串联键

入口生成 `$request_id` 后，应用应把它写入响应头和结构化日志，并在内部 RPC 中透传。否则前端看到错误、Nginx 只知道 504、应用日志只有自己的 ID，三方无法对应同一次请求。

验证方法：

```bash
curl -sS -D /tmp/headers.txt https://api.example.com/v1/recharges \
  -H 'Authorization: Bearer demo-token' \
  -H 'Content-Type: application/json' \
  -d '{"req_id":"trace-demo-1","amount_cent":100,"channel":"manual"}'

grep -i 'x-request-id' /tmp/headers.txt
```

然后用这个 ID 同时搜索 Nginx access log、应用日志和下游日志。如果任何一层缺失，说明链路追踪还没有闭环。

## 25.4 Docker：把构建和运行固定下来

### 镜像、容器、卷、网络

```text
镜像 = 不可变的文件系统模板 + 启动元数据
容器 = 镜像的一次运行实例，有自己的进程、网络命名空间和可写临时层
卷 = 独立于容器生命周期的持久化存储
网络 = 容器之间以及容器与外部通信的虚拟网络
```

容器删除后，临时层通常一起删除；数据库、上传文件、队列数据不能只放在临时层。镜像 tag 也可能被覆盖，生产发布应固定 digest。

### Go 多阶段构建：为什么这样写

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
ENTRYPOINT ["/server", "-config=/etc/mini-coin/config.json"]
```

- 先复制 `go.mod/go.sum` 再下载依赖，源码改动可以复用依赖层缓存；
- `CGO_ENABLED=0` 生成静态二进制，适合极简运行镜像；
- `-trimpath` 去掉构建机绝对路径，`-ldflags="-s -w"` 减小体积；
- 运行镜像不含编译器和 shell，攻击面更小；没有 shell 时通过日志、健康端点和临时调试容器排查；
- 应用以非 root 用户运行；配置和密钥通过挂载/环境注入，不写进镜像。

镜像能构建不等于能运行。至少执行：

```bash
docker build -t mini-coin:local .
docker run --rm -d --name mini-coin \
  -p 8081:8081 \
  -e MINICOIN_MYSQL_DSN='user:password@tcp(local-mysql:3306)/mini_coin' \
  mini-coin:local

docker ps --filter name=mini-coin
docker logs --tail 100 mini-coin
curl -sS http://127.0.0.1:8081/healthz
```

预期的顺序是：容器状态变为 `Up`，日志显示配置加载成功、依赖连接成功、监听 8081，健康检查返回 200。若容器立即退出，先看 `docker logs`；若是 `connection refused`，检查应用监听地址是否为 `0.0.0.0` 而不是只监听 `127.0.0.1`，以及 `-p` 映射是否正确。真实密码只用本地环境变量或 secret，不写进 Dockerfile、tag、命令历史或讲义。

### Docker 排障：从容器状态回到具体原因

```bash
docker ps -a
docker logs --tail 200 CONTAINER
docker inspect CONTAINER
docker stats --no-stream
docker exec -it CONTAINER sh
docker cp CONTAINER:/tmp/dump.txt ./dump.txt
```

`docker ps -a` 先看退出码和状态；`logs` 看应用错误；`inspect` 看挂载、环境变量、网络、重启策略和 OOMKilled；`stats` 看实时资源；`exec` 进入容器只用于阅读，不要在运行容器里临时改代码；`cp` 取诊断文件时注意脱敏。极简镜像没有 shell 时，应使用专门的调试镜像/临时容器，而不是把生产镜像换成带一堆工具的大镜像。

### 持久化：区分“可以重建”和“唯一数据”

缓存通常可以重建，数据库文件、对象存储、消息队列持久化数据不能丢。使用卷/托管服务时仍要确认：

- 数据实际落在哪个存储，容器重建后是否还在；
- 备份是否包含该卷，恢复演练是否成功；
- 单副本存储是否会在节点故障时丢失；
- 权限、加密和快照保留时间是否符合要求。

验证不能只执行 `docker volume ls`。应写入一条测试数据，删除并重建容器，再读出来；然后从备份恢复到一个新环境，核对行数、校验和与业务关键字段。

## 25.5 Kubernetes：最小可用部署与排障

Kubernetes 负责调度、重启、服务发现和滚动更新，不自动替你保证代码正确、数据库高可用或容量足够。先理解 Deployment、Service、探针和事件，再谈 HPA、Ingress 和网格。

### Deployment：副本、资源和启动参数

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mini-coin
spec:
  replicas: 3
  selector:
    matchLabels:
      app: mini-coin
  template:
    metadata:
      labels:
        app: mini-coin
    spec:
      containers:
        - name: server
          image: registry.example.com/mini-coin:1.0.0
          ports:
            - containerPort: 8081
          envFrom:
            - secretRef:
                name: mini-coin-secrets
          resources:
            requests:
              cpu: 200m
              memory: 256Mi
            limits:
              cpu: 1
              memory: 512Mi
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
            initialDelaySeconds: 3
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /livez
              port: 8081
            initialDelaySeconds: 10
            periodSeconds: 10
```

- `replicas` 是期望副本数，不等于当前可用数；滚动更新时可能同时存在新旧版本；
- `requests` 影响调度，`limits` 影响被限流/被杀；CPU limit 过低会导致延迟抖动，内存 limit 过低会 `OOMKilled`；
- `envFrom` 只说明引用了 secret，不保证 key 正确；启动校验必须明确报错；
- 生产镜像应固定 digest 或在流水线中记录不可变版本，避免同名 tag 被覆盖。

### Service：把流量送到正确 Pod

```yaml
apiVersion: v1
kind: Service
metadata:
  name: mini-coin
spec:
  selector:
    app: mini-coin
  ports:
    - port: 80
      targetPort: 8081
```

`selector` 必须匹配 Pod 标签；`port` 是 Service 暴露端口，`targetPort` 是容器端口。配置后验证：

```bash
kubectl get endpointslices -l kubernetes.io/service-name=mini-coin
kubectl get svc mini-coin
kubectl run curl-test --rm -it --image=curlimages/curl --restart=Never -- \
  curl -sS http://mini-coin/healthz
```

如果 Endpoint 为空，先查标签和 readiness；如果能从 Pod 访问但 Service 不通，查端口名/targetPort、网络策略和 DNS。不要在没看 Endpoint 的情况下反复重启应用。

### 滚动发布：观察的是“新版本是否健康”

```bash
kubectl set image deployment/mini-coin server=registry.example.com/mini-coin:1.0.1
kubectl rollout status deployment/mini-coin --timeout=5m
kubectl rollout history deployment/mini-coin
kubectl get pods -l app=mini-coin -w
```

`rollout status` 超时表示发布未在期限内达到期望状态，不等于自动成功；`get pods -w` 观察新 Pod 是否 Ready、是否反复重启。发布前要确认 `maxUnavailable/maxSurge`、就绪探针和优雅退出，否则可能出现新旧版本同时处理请求、旧 Pod 被杀时丢请求。

### startup/readiness/liveness：三个问题不要混

| 探针 | 回答的问题 | 失败后发生什么 | 适合检查什么 |
|---|---|---|---|
| startup | 进程完成启动了吗 | 在成功前暂停 readiness/liveness | 加载大配置、建连接池、迁移/预热 |
| readiness | 现在能接新流量吗 | 从 Service 摘掉，不重启 | 依赖暂时不可用、应用正在过载、准备未完成 |
| liveness | 进程是否已经无法自愈 | 重启容器 | 死锁、永久卡死、内部状态不可恢复 |

常见错误是把数据库可用性直接放进 liveness：数据库抖动时所有实例同时被重启，重启后仍然连不上，故障被放大。更稳妥的做法是 liveness 只检查进程自身是否能响应/推进，readiness 检查关键依赖和是否可接流量；依赖不可用时优先摘流量或降级，并单独告警。

验证探针不能只看 YAML：进入 Pod 执行同样的 HTTP 请求，检查状态码、超时和响应体；人为让依赖不可用，观察 Pod 是否从 Endpoint 摘除但没有被重启；恢复依赖后再确认重新接流量。

### K8s 排障：从事件到容器内事实

```bash
kubectl get pods -o wide
kubectl describe pod POD
kubectl logs POD --tail=200
kubectl logs POD --previous
kubectl exec -it POD -- sh
kubectl get events --sort-by=.lastTimestamp
kubectl top pod
```

判读顺序：

- `get pods` 看 STATUS、RESTARTS、AGE、NODE；
- `describe` 看 Events、探针失败原因、退出码、拉镜像失败和环境变量引用错误；
- 当前日志没有启动信息时用 `--previous` 看上一次崩溃；
- `events` 按时间排序，常能直接看到调度、镜像、探针和 OOM 原因；
- `top` 只能说明当前资源，不能解释历史 OOM 或重启。

常见状态及第一轮动作：

- `ImagePullBackOff`：检查镜像地址、仓库认证、tag/digest 和网络；
- `CrashLoopBackOff`：看退出码、配置、启动依赖、端口和 `--previous` 日志；
- `Pending`：看节点资源、污点、亲和性、PVC 绑定；
- `OOMKilled`：看 limit、heap/profile、缓存上限和大对象，不要只把 limit 调大；
- `Readiness` 失败：检查 `/readyz`、依赖连接和超时，确认是否只是摘流量。

## 25.6 云上后端要理解什么

云把机器、网络、存储、数据库托管化，但请求路径、超时、容量和故障边界仍然由你负责。至少要能画出：DNS → 公网 IP/负载均衡 → WAF/网关 → 容器网络 → 应用 → 数据库/缓存/队列，以及每一段的超时、重试和凭证来源。

上线前要问：

```text
服务部署在哪个可用区？跨可用区流量是否收费/有延迟？
数据库有没有副本、备份和自动切换？恢复要多久？
健康检查超时、负载均衡空闲超时分别是多少？
公网只开放哪些端口？管理端口是否暴露？
凭证由谁注入、如何轮换、日志里会不会打印？
日志保留多久，能否按 request_id 检索？
流量翻倍时哪个资源先饱和？
团队和云厂商各自负责什么？
```

责任边界必须写进运行手册。托管数据库提供备份文件，不代表团队不用做恢复演练；负载均衡做健康检查，不代表应用的 readiness 一定正确；对象存储提供持久性，不代表桶权限不会把数据公开。

### 排障先画请求路径，再看每一段证据

假设客户端在 3 秒超时，而请求路径是：

```text
客户端 3s -> 负载均衡 5s -> 网关 4s -> 应用 3s -> DB 1s
```

客户端 3 秒就断开，可能根本等不到应用和 DB 的 3 秒/1 秒超时；用户重试后可能产生重复写入。上层超时与下层超时必须形成预算关系，核心写请求要有幂等键。排障时按 request_id 逐段确认：请求有没有到入口、有没有到应用、应用是否在等 DB、DB 是否在等锁。

云上故障常见的误判是“应用日志没报错，所以不是应用问题”。如果负载均衡先超时并断开，应用可能仍在处理但没有机会写响应；如果 WAF 直接拦截，应用甚至收不到请求。每段都要有访问日志、错误日志和指标。

## 25.7 微服务和集群

### 什么时候拆微服务

拆分应由边界和运行需求驱动，而不是代码行数或“看起来先进”。适合拆的信号：

- 某个模块需要独立扩容，例如图片处理 CPU 很高；
- 需要独立发布节奏，例如风控策略每天变更；
- 数据所有权清晰，团队可以独立维护自己的表/API；
- 团队协作成本已经明显高于网络调用成本；
- 有统一日志、trace、配置、限流、发布和告警基础。

不该拆的信号：

- 两个模块共享同一张表和同一个事务；
- 一个小团队维护一个业务闭环；
- 拆完只是把本地调用变成 RPC，却没有独立部署价值；
- 没有服务发现、超时/重试/熔断、可观测性和回滚能力；
- 服务之间仍然绕过 API 直接读对方数据库。

判断方法不是开会投票，而是做一次模块化单体：明确包边界、接口、独立测试和数据访问边界。先证明模块能独立测试、独立发布，再评估拆出去是否真的降低协作成本。

### 单体优先不是落后

一个边界清楚的模块化单体，通常比十几个没有治理的微服务更可靠。微服务会把函数调用变成网络调用，把事务变成分布式一致性，把堆栈变成 trace，把编译错误变成运行时契约错误。没有相应基础设施时，拆分只会增加故障面。

### 集群不等于高可用

三个副本如果都在同一台机器、同一个可用区、依赖同一个数据库，仍然会一起挂。高可用要逐层检查：

- 实例是否分散到不同节点/可用区；
- 负载均衡是否正确摘除失败实例；
- 数据库是否有副本、自动切换和恢复演练；
- 缓存挂了能否回源，是否会把数据库打穿；
- 消息是否持久化，重复消费是否幂等；
- 配置中心是否多副本，错误配置能否快速回滚；
- 时钟、证书、DNS 和网络策略是否可靠。

验证高可用要做故障演练：杀一个实例、断一个依赖、切一个可用区，观察流量转移、错误率和恢复时间。没有演练的“多副本”只是配置上的乐观。

## 25.8 发布、灰度和回滚

### 发布前门禁：每一项都要有证据

```text
[ ] 单元、集成、E2E 通过，测试报告可追溯
[ ] gofmt/go vet/race 通过
[ ] 迁移脚本在空库和目标版本演练
[ ] 镜像 digest 固定，构建产物可追溯
[ ] 配置和 secret 已注入，启动校验通过
[ ] 健康检查、资源限制和优雅退出配置
[ ] 监控面板、告警、trace 可用
[ ] 回滚镜像、数据库兼容方案和负责人明确
```

门禁不是勾选游戏。比如“迁移演练”要有空库执行、旧版本数据执行、失败回滚和耗时记录；“健康检查”要真的访问 `/readyz` 和 `/livez`；“回滚”要在预发实际执行一次，不能只写命令。

### 灰度：先定义基线和停止条件

```text
1. 记录当前版本基线：错误率、P99、QPS、业务成功率、资源
2. 发布 1 个实例或 5% 流量
3. 观察 5~15 分钟，和基线比较
4. 满足继续条件才扩到 10%，再观察
5. 扩到 50%，再观察
6. 全量后继续观察一个完整业务周期
```

没有指标就不叫灰度，只是慢慢发布。继续/停止阈值必须提前写死，例如：

```text
10% 流量观察 10 分钟：
- 5xx 错误率 < 0.5%
- P99 < 500ms
- 充值成功率不低于基线 2 个百分点
- 无新增 panic/deadlock
- CPU、内存、连接池没有持续逼近上限

任意一项不满足 -> 停止扩量，保留现场，准备回滚
```

灰度期间不要同时改数据库结构、配置和代码。变量太多时，出问题无法判断是哪一项导致。观察窗口要覆盖一次完整业务周期，不能只看前 10 秒。

### 回滚：先恢复用户，再定位原因

```bash
kubectl rollout undo deployment/mini-coin
kubectl rollout status deployment/mini-coin --timeout=5m
kubectl get pods -l app=mini-coin
curl -sS https://api.example.com/healthz
```

回滚后必须验证业务结果，而不只是 Pod 状态：

- 入口错误率和 P99 是否恢复；
- 订单、流水、余额是否一致；
- 消息是否积压或重复消费；
- 缓存是否包含新版本写入的不兼容数据；
- 是否有重复充值或幂等冲突。

数据库迁移必须向前兼容：先加可空字段/索引，代码双写双读并回填，确认旧版本仍能运行，再收缩。否则应用回滚了，数据库结构却回不去，故障会持续。

## 25.9 监控与报警上线清单

### 必须监控什么

- 请求 QPS、成功率、P50/P95/P99；
- goroutine、GC、内存、CPU、文件描述符；
- 数据库连接池、慢查询、锁等待；
- Redis 延迟、错误和大 key 趋势；
- MQ 生产/消费速率、lag；
- 业务成功率、关键数量；
- 磁盘容量/inode 水位；
- 证书和域名到期。

监控是“随时能看趋势”，告警是“现在必须有人做决定”。CPU 80% 可以放到看板，但如果系统仍满足 SLO，不必半夜叫醒人；真正该告警的是用户影响、容量耗尽风险和依赖持续不可用。

### 告警必须能行动

每条告警附：

```text
影响说明：用户现在会遇到什么
排查链接：看板、日志查询、trace 入口
运行手册：先做什么、何时回滚
负责人：谁处理、何时升级
静默/抑制条件：维护窗口、上游故障时如何避免重复告警
```

无法行动的告警应删除或改为看板，否则会造成告警疲劳。P1 告警必须有负责人、升级路径和运行手册；只有“错误率高”四个字，值班人仍然只能猜。

### 从告警到根因的固定路径

收到“充值接口 5xx 升高”后，不要直接翻代码。按证据走：

1. 看时间范围和影响面：哪些实例、哪些接口、哪些用户/渠道；
2. 看四层指标：入口/应用/依赖/资源，确认错误率和 P99 是同时上升还是只有某一层异常；
3. 用 request_id 找一条失败样本，看应用日志和下游错误；
4. 如果是 DB 慢，查连接池、慢查询和锁等待；如果是 Redis，查延迟、大 key；如果是 MQ，查 lag 和消费错误；
5. 先止损：回滚、切流、限流、降级或扩容，确认用户恢复；
6. 再定位根因，补测试、监控和运行手册。

### 告警测试

人为制造一次可恢复的错误（例如在预发关闭一个依赖），确认告警能触发、通知到正确的人、运行手册可用、恢复后告警能自动关闭。没测试过的告警，和没写的告警差不多。

## 25.10 一次部署故障复盘：从现象到改进

### 现象和证据

K8s 发布后所有 Pod 处于 `CrashLoopBackOff`，入口 503。先保留证据：

```bash
kubectl get pods -o wide
kubectl describe pod POD
kubectl logs POD --previous --tail=200
kubectl get events --sort-by=.lastTimestamp
kubectl rollout history deployment/mini-coin
```

`get pods` 看重启次数和节点，`describe` 看退出码/事件/探针失败，`--previous` 看崩溃前日志，`events` 看调度和镜像，`rollout history` 对比版本。

假设发现：新镜像要求 `MINICOIN_CONFIG`，Deployment 没注入；配置读取 panic；liveness 因配置错误反复重启。直接原因是配置缺失，根因至少包括：启动校验不在 CI 覆盖、发布清单没有检查 secret、liveness 掩盖了启动阶段错误。

### 止损、修复和验证

先回滚或修复配置注入，优先恢复用户：

```bash
kubectl rollout undo deployment/mini-coin
kubectl rollout status deployment/mini-coin --timeout=5m
curl -sS https://api.example.com/healthz
```

修复后不能只看 Pod 变绿。要验证：新版本能读到配置、错误配置会在启动时明确失败且不打印 secret、readiness/liveness 行为正确、发布 smoke test 真正阻断坏镜像、监控在故障时触发并在恢复后关闭。

### 复盘产出不是“检讨”

至少输出：

```text
时间线：何时发布、何时发现、何时止损、何时恢复
影响面：多少用户/请求/金额，是否重复写入
直接原因：哪个配置/代码/操作触发
根因：为什么测试、门禁、监控没有提前发现
改进项：负责人、截止时间、验证证据
运行手册更新：下次如何在 5 分钟内判断和止损
```

改进项要能验收。例如“增加配置解析测试”要说明覆盖哪个配置、在哪个 CI 阶段运行、失败是否阻断发布；“增加 smoke test”要说明请求哪个接口、预期状态码、失败是否阻断发布。没有负责人和验证证据的改进项，通常不会真正落地。

复盘文档要脱敏，不能把真实账号、内网地址、用户数据和密钥写进公开仓库。

## 25.11 习题

1. 开发机 SSH 登录报 `Permission denied (publickey)`，你怎么按层排查？每一步的预期结果是什么？
2. Go 服务收到 SIGTERM 后正确退出流程是什么？如何验证它真的优雅退出？
3. Nginx 502 和 504 分别优先查什么？各给一条能缩小范围的命令。
4. Docker 镜像、容器、卷的区别是什么？为什么数据库唯一数据不能只放在容器临时层？
5. 用 Docker 启动 mini-coin 后容器立刻退出，写出你的排查顺序和三个最可能原因。
6. readiness 和 liveness 有什么区别？依赖数据库时如何设计？如何用故障注入验证？
7. Kubernetes Pod `OOMKilled` 怎么排查？为什么不能只调大 memory limit？
8. 微服务拆分前至少需要哪些基础设施？给出三个“不该拆”的信号。
9. 为什么灰度发布没有指标就不算灰度？设计一个带继续/停止阈值的灰度方案。
10. 发布前数据库迁移如何保证应用可回滚？写出向前兼容的迁移顺序。
11. 收到“充值接口 5xx 升高”的告警，写出从告警到定位根因的证据链。
12. 复盘一个部署事故，写出时间线、直接原因、根因和两个可验收改进项。

## 参考答案

<details>
<summary>展开参考答案</summary>

### 1. SSH `Permission denied (publickey)`

按层排查，每步都有明确判据：

```bash
# 1. DNS
getent hosts dev.example.com
# 预期：输出内网地址；没有输出 -> DNS/Hosts 问题

# 2. TCP
nc -vz dev.example.com 22
# 预期：succeeded；refused -> sshd 未监听/端口错；timed out -> 网络/防火墙

# 3. SSH 详细日志
ssh -vvv -i ~/.ssh/id_ed25519 user@dev.example.com
# 看最后停在哪个阶段：banner、kex、publickey
```

常见原因：

| 现象 | 检查 |
|---|---|
| `Connection refused` | `systemctl status sshd`、监听端口、安全组 |
| `timed out` | VPN、路由、防火墙、跳板机 |
| 走到 `publickey` 后失败 | 用户名、`IdentityFile`、公钥是否在 `authorized_keys` |
| `Permissions 0644 ... too open` | `chmod 600 ~/.ssh/id_ed25519` |
| 服务端拒绝 | `sshd_config` 的 `AllowUsers`、`PubkeyAuthentication` |

```bash
ls -l ~/.ssh/id_ed25519
ssh-keygen -y -f ~/.ssh/id_ed25519   # 私钥是否可读
ssh-add -l
```

**验证**：`ssh -o BatchMode=yes user@host true` 退出码为 0。不要靠反复重启 sshd 碰运气；每层确认后再进入下一层。

---

### 2. SIGTERM 优雅退出

正确顺序：

```text
收到 SIGTERM
  -> 停止接受新连接（http.Server.Shutdown）
  -> 等待在途请求完成（带超时 context）
  -> 关闭 DB / Redis / MQ 客户端
  -> 输出 shutdown 日志并退出 0
```

```go
srv := &http.Server{Addr: ":8080", Handler: mux}
go func() {
    if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
        log.Fatal(err)
    }
}()

ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
defer stop()
<-ctx.Done()

shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()
if err := srv.Shutdown(shutdownCtx); err != nil {
    log.Printf("shutdown: %v", err)
}
db.Close()
```

验证：

```bash
go run ./cmd/server &
pid=$!
curl -sS http://127.0.0.1:8080/healthz
kill -TERM $pid
wait $pid; echo "exit=$?"
```

预期：进程在 15 秒内退出；日志出现 shutdown；发送 SIGTERM 后再请求应连接失败，但在途请求能完成。若超过 grace period 被 SIGKILL，检查是否有阻塞的 DB 调用或未退出的 goroutine。

---

### 3. Nginx 502 / 504

```bash
# 502：先确认上游进程和端口
ss -lntp | grep 8080
curl -v http://127.0.0.1:8080/healthz

# 504：确认上游耗时
curl -sS -o /dev/null -w 'status=%{http_code} time=%{time_total}\n' \
  http://127.0.0.1:8080/v1/recharges
```

| 状态 | 优先看 | 缩小范围的命令 |
|---|---|---|
| 502 | 上游是否监听、容器是否崩溃、端口/地址是否写错 | `ss -lntp`、`docker logs`、`kubectl logs` |
| 504 | 应用处理是否超过 `proxy_read_timeout`、慢 SQL/锁 | 应用 P99、trace、`SHOW PROCESSLIST` |

```bash
nginx -t
tail -n 100 /var/log/nginx/error.log
```

**验证**：临时把上游端口改错，应稳定复现 502；把应用 sleep 到超过 `proxy_read_timeout`，应稳定复现 504。恢复配置后两类请求都返回正常状态。

---

### 4. 镜像、容器、卷

```text
镜像：只读模板（代码 + 依赖 + 启动命令）
容器：镜像的一次运行实例，有临时可写层
卷：独立于容器生命周期的持久化数据
```

```bash
docker image ls
docker ps -a
docker volume ls
docker inspect -f '{{json .Mounts}}' CONTAINER
```

数据库不能用容器临时层保存数据：`docker rm` 后数据就没了。要用命名卷、绑定挂载或托管数据库，并且真的做一次恢复演练。

**验证**：

```bash
docker run -d --name pg -e POSTGRES_PASSWORD=local_only -v pgdata:/var/lib/postgresql/data postgres:16
docker exec pg psql -U postgres -c 'CREATE TABLE t(id int);'
docker rm -f pg
docker run -d --name pg2 -e POSTGRES_PASSWORD=local_only -v pgdata:/var/lib/postgresql/data postgres:16
docker exec pg2 psql -U postgres -c '\dt'
```

表仍在，说明卷生效；只在 `docker ps` 看到 Up 不算验证。

---

### 5. 容器立刻退出

```bash
docker ps -a
docker logs CONTAINER
docker inspect CONTAINER --format '{{.State.ExitCode}} {{.State.Error}}'
docker inspect CONTAINER --format '{{json .Config.Env}}'
docker inspect CONTAINER --format '{{json .Mounts}}'
```

退出码：

| 退出码 | 常见原因 |
|---|---|
| 1 | 配置错误、依赖连接失败、panic |
| 2 | 命令行参数错误 |
| 137 | 被 SIGKILL/OOM |
| 143 | 收到 SIGTERM，正常退出路径 |

三个最常见原因：

1. 配置缺失或格式错误（环境变量、配置文件路径）；
2. 数据库/Redis 地址不可达，启动时直接 `log.Fatal`；
3. 监听地址写成 `127.0.0.1`，容器外访问不到；或端口映射错误。

**验证**：修复后用 `docker run --rm -p 18080:8080 ...` 启动，`curl /healthz` 成功，`docker logs` 没有 error，容器保持运行 1 分钟以上。

---

### 6. readiness / liveness

```text
startup：启动慢时保护，成功前不执行其他探针
readiness：能否接流量；失败只摘 Endpoint
liveness：进程是否卡死；失败会重启容器
```

依赖数据库时：

- liveness 不查数据库，避免数据库抖动导致全部 Pod 重启；
- readiness 可以检查关键依赖，但要有短超时和缓存，避免探针本身压垮依赖；
- 业务是否降级由应用决定，不能把“依赖不可用”简单等同于“进程死亡”。

```yaml
readinessProbe:
  httpGet: {path: /readyz, port: 8080}
  timeoutSeconds: 1
  periodSeconds: 5
livenessProbe:
  httpGet: {path: /healthz, port: 8080}
  timeoutSeconds: 1
  periodSeconds: 10
```

**故障注入验证**：停掉数据库，观察 Pod 仍是 Running、Endpoint 被摘除；恢复数据库后 Endpoint 自动加回。如果 Pod 被重启，说明 liveness 错误地依赖了数据库。

---

### 7. Pod `OOMKilled`

```bash
kubectl describe pod POD | grep -E 'OOMKilled|Limits|Requests|Restart'
kubectl top pod POD
kubectl logs POD --previous
kubectl get pod POD -o jsonpath='{.spec.containers[0].resources}'
```

排查顺序：

1. 看 `Last State: Terminated, Reason: OOMKilled` 和 limit；
2. 看 RSS 曲线是持续上涨（泄漏）还是随流量波动（峰值）；
3. 抓 heap profile：`go tool pprof -top http://POD:8080/debug/pprof/heap`；
4. 检查缓存、大对象、批量查询、日志缓冲是否有上限；
5. 对比发布前后的内存曲线。

不能只调大 limit：如果泄漏，调到 2Gi 只是延后崩溃；如果节点内存不足，还会影响其他 Pod。

**验证**：修复后用相同负载跑 30 分钟，RSS 平稳、GC 正常、重启次数为 0；再逐步提高负载确认余量，而不是只看一次 `kubectl top`。

---

### 8. 微服务拆分前的基础设施

至少要有：

- 服务发现和负载均衡；
- 配置中心与密钥管理；
- 结构化日志、指标、分布式追踪；
- 超时、重试、熔断、限流；
- 灰度发布、回滚、健康检查；
- 数据迁移和兼容策略；
- 告警、值班和运行手册；
- 对账/补偿机制。

三个“不该拆”的信号：

1. 两个模块共享同一张表、同一个事务，拆开就要分布式事务；
2. 团队很小，拆完只是把函数调用变成 RPC，运维成本翻倍；
3. 没有统一可观测性和发布能力，出问题无法定位。

**验证**：先做模块化单体，把边界、接口和测试打清楚；只有当某个模块需要独立扩缩容、独立发布或独立团队时，再拆。

---

### 9. 没有指标的灰度不算灰度

灰度必须有基线、比例、观察窗口、继续/停止阈值：

```text
基线：错误率 0.1%，P99 120ms，成功率 99.8%
1% 5min -> 10% 10min -> 50% 10min -> 100% 30min
继续：错误率 < 0.5%，P99 < 500ms，成功率不低于基线 1 个百分点
停止：错误率 > 2% 持续 5min，或 P99 > 1s 持续 5min，或资金不一致
```

```bash
kubectl set image deployment/minicoin app=registry.example.com/minicoin:v1.2.3
kubectl rollout status deployment/minicoin
kubectl get pods -w
```

每一步都用同一个看板对比新旧版本：按 `version` 标签看错误率、P99、成功率和资源。**验证**：在测试环境故意让新版本返回 10% 的 500，灰度流程应在 1% 阶段停止并回滚，而不是继续扩到 100%。

---

### 10. 数据库迁移保证可回滚

向前兼容顺序：

```text
1. 加可空列/新表/新索引（旧代码可忽略）
2. 发布双写代码
3. 回填历史数据并校验
4. 发布双读代码，比较新旧结果
5. 切读新字段
6. 观察一个发布周期
7. 停旧写，最后删除旧字段
```

```sql
ALTER TABLE recharge_order ADD COLUMN channel VARCHAR(16) NULL;
-- 应用双写 channel
UPDATE recharge_order SET channel='unknown' WHERE id > ? AND channel IS NULL LIMIT 1000;
-- 校验无 NULL 后再收紧
ALTER TABLE recharge_order MODIFY channel VARCHAR(16) NOT NULL DEFAULT 'unknown';
```

**回滚验证**：在第 2～5 步之间，把应用回滚到上一版本，旧版本必须仍能正常读写；数据库不删列、不改旧字段类型。只验证新版本能启动不算可回滚。

---

### 11. “充值 5xx 升高”证据链

```text
T0 告警：/v1/recharges 5xx > 2%，持续 5 分钟
T+1min 确认影响面：QPS、错误率、P99、实例数、最近发布
T+2min 分层：按 route/instance/version 看，是否只影响新版本
T+3min 依赖：MySQL 连接池、慢查询、锁等待、Redis、MQ
T+4min 样本：取一个 request_id，查日志和 trace 的最长 span
T+5min 止损：回滚/切流/限流/降级，先恢复用户
T+10min 根因：对比发布差异、SQL、配置、依赖变更
T+30min 复盘：补测试、告警、发布门禁和运行手册
```

```bash
kubectl rollout history deployment/minicoin
kubectl logs deploy/minicoin --since=15m | jq 'select(.level=="ERROR")' | head
curl -sS "http://jaeger.example.com/api/traces/$TRACE_ID" | jq '.data[0].spans[] | {operationName,duration}'
mysql -e 'SHOW PROCESSLIST'
```

**验证**：恢复标准不是“Pod Running”，而是错误率回到基线、核心充值成功、余额和流水一致、同一请求可重复验证。

---

### 12. 部署事故复盘

```markdown
# 事故复盘：充值接口 5xx 升高

## 时间线
- 10:00 发布 v1.8.0
- 10:03 告警：5xx 从 0.1% 升到 4%
- 10:05 确认只影响新版本
- 10:07 回滚到 v1.7.3
- 10:12 错误率恢复

## 影响面
- 受影响请求数、用户数、金额范围
- 是否有数据不一致

## 直接原因
新版本配置解析把 DB 连接池大小读成 0，连接获取立即失败。

## 根因
配置结构体新增字段后没有默认值；CI 只测了“能启动”，没有请求真实数据库。

## 改进项
1. 配置解析加默认值和单测，负责人 A，11-05 前，CI 失败即阻断；
2. 发布 smoke test 请求充值接口并校验 201，负责人 B，11-08 前，失败自动回滚；
3. 看板增加“连接池使用率/获取失败”面板，负责人 C，11-10 前。
```

改进项必须可验收：有负责人、截止时间、验证命令或指标、失败是否阻断发布。只写“加强测试”不算改进。

</details>
