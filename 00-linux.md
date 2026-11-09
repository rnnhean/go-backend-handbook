# 第 0 章 开发机与 Linux 入门

> 目标：能连上远程开发机，看懂机器状态，磁盘/内存/CPU 出问题时知道先做什么，不把“机器卡了”当成玄学。
>
> 本章用 `devbox.example.com`、`demo` 这类占位符。真实地址、账号、密钥不要写进公开文档。

---

## 0.1 为什么后端要有一台开发机

前端本地起一个 dev server 就能开发，因为依赖主要是 Node 和浏览器。后端通常不行：

- MySQL/Redis/Kafka 在机房或容器平台，本地连不上；
- 服务要访问测试环境、注册中心、配置中心；
- 编译、压测、抓包、看日志需要一台长期在线的 Linux；
- 本地 Mac/Windows 和线上 Linux 的环境差异会制造 bug。

所以团队一般给每人一台远程 Linux 开发机，代码、编译、联调都在上面做。

**“本地写代码、开发机编译”** 的意思是：

```
你的电脑：编辑器（VS Code / GoLand）通过 SSH/Remote 编辑远程文件
开发机：真正执行 go build / go test / 启动服务 / 连数据库
```

不是把代码拷来拷去，而是编辑器直接操作远程工作区。

---

## 0.2 怎么连：SSH 四层排查

```bash
cat ~/.ssh/config
ssh devbox
```

`~/.ssh/config` 示例：

```
Host devbox
  HostName devbox.example.com
  Port 22
  User demo
  IdentityFile ~/.ssh/id_ed25519_demo
  IdentitiesOnly yes
  ServerAliveInterval 30
  ServerAliveCountMax 3
```

`ServerAliveInterval 30` 每 30 秒发心跳，防止长时间不操作被防火墙断开。

### 连不上，按四层排查

排查 SSH 不能靠反复重试。先判断失败发生在哪一层：DNS 是否能解析、目标端口是否可达、服务端是否接受认证、登录后 shell 是否正常。每层只保留一个能给出确定结论的命令，并把报错原文记录下来；否则很容易把“代理没开”误判成“密钥坏了”。下面四条命令分别验证网络、端口、认证和登录后环境，执行顺序不要跳。

```
网络通不通 → 端口开不开 → 认证过不过 → 登录后环境对不对
```

| 现象 | 命令 | 常见原因 |
|---|---|---|
| `Connection timed out` | `ping host`、`nc -vz host 22` | 不在内网/VPN、防火墙、安全组 |
| `Connection refused` | `nc -vz host 22` | sshd 没启动、端口错 |
| `Permission denied (publickey)` | `ssh -vvv devbox` | 公钥没放对、权限不对 |
| `Host key verification failed` | `ssh-keygen -R host` | 机器重装，指纹变了 |
| 连上就断 | `ssh -vvv`，服务端 `/var/log/auth.log` | 账号被锁、磁盘满、sshd 限制 |
| 卡在 Connecting | `curl -v telnet://host:22` | 代理/跳板机问题 |

一次性确认：

```bash
ping -c 2 devbox.example.com
nc -vz devbox.example.com 22
ssh -o BatchMode=yes devbox 'echo ok'
ssh devbox 'hostname; whoami; uptime'
```

### 公钥权限

```bash
chmod 700 ~/.ssh
chmod 600 ~/.ssh/id_ed25519_demo
chmod 644 ~/.ssh/id_ed25519_demo.pub
chmod 600 ~/.ssh/authorized_keys   # 在开发机上
```

权限太宽，sshd 会拒绝使用密钥。

---

## 0.3 登录后先看什么

```bash
hostnamectl          # 主机名、系统版本、内核
whoami; id           # 我是谁、在哪些组
pwd; df -h           # 当前目录、磁盘
free -h              # 内存
nproc                # CPU 核数
uptime               # 负载
top -b -n1 | head -20
```

### 负载怎么读

`uptime` 输出：

```
load average: 1.20, 0.80, 0.50
```

三个数分别是 1/5/15 分钟平均。经验：

- `load < 核数`：通常正常；
- `load > 核数`：有排队；
- `load > 2 × 核数`：明显过载。

但 load 包含不可中断 IO 等待，磁盘慢时 load 也会高。结合 `top` 的 `%wa` 和 `iostat` 看。

### CPU 怎么读

`%CPU` 是某个进程消耗的 CPU 时间比例，单核跑满大约是 100%，多核进程可能超过 100%；`load average` 表示处于可运行和不可中断等待状态的任务数，不等于 CPU 使用率。判断时要把两者和三段时间窗口一起看：1 分钟高、15 分钟低通常只是短突发；三个值一起高且 `nproc` 只有 2 时，机器已经持续过载；CPU 不高但 load 很高，常见原因是大量 IO 等待。

```bash
top
# 看 %us（用户态）、%sy（内核态）、%wa（IO 等待）、%id（空闲）
```

- `%us` 高：业务计算多，查 CPU profile；
- `%sy` 高：系统调用/上下文切换多；
- `%wa` 高：磁盘 IO 慢；
- `%id` 低但 load 高：可能在等 IO。

---

## 0.4 磁盘满了怎么办

### 先定位

定位磁盘问题要同时看“文件系统使用率”和“文件归属”。`df -h` 只告诉你哪个挂载点满了，`du` 找出目录占用，`lsof +L1` 处理“文件已删除但进程仍持有句柄”的特殊情况。生产机不要立刻递归扫描整个 `/`，先限定到日志、容器、临时目录和 core dump，控制命令本身的 IO。

```bash
df -h                                  # 哪个分区满
du -sh /var/* 2>/dev/null | sort -h    # 一级目录谁大
du -sh /var/log/* | sort -h | tail     # 日志大户
lsof +L1                               # 已删除但被进程占用的文件
```

`df` 显示 100% 但 `du` 找不到大文件，通常是**文件被删除但进程还开着**，用 `lsof +L1` 找。

### 处理顺序

先止血再找根因。生产上优先确认影响范围、是否有副本可承接、最近是否发布，再决定扩容、摘流量或回滚；不要一上来 `kill -9`，否则线程栈、buffer 和未完成请求都会丢失。下面的顺序可以当作一次最小应急演练。

1. **止血**：确认不是审计/事故证据后，清理日志或临时文件；
2. **找根因**：日志没轮转？core dump？临时文件没清？
3. **加防线**：logrotate、磁盘告警 80%/90%、容器日志大小限制。

```bash
# 安全清空正在写的日志（rm 后空间可能不释放）
truncate -s 0 /var/log/app/app.log

# 找 7 天前的临时文件，先看再删
find /tmp -type f -mtime +7 -ls | head
```

**不要直接 `rm -rf /var/log/*`**：可能删掉正在写的关键日志，空间还不释放。

### 磁盘满会导致什么

磁盘满不只影响写日志。Go 进程可能无法创建临时文件、MySQL 无法写 redo/binlog、容器运行时无法拉镜像，甚至 DNS 和日志采集也会异常。因此“接口 500”可能只是磁盘问题，排查时要主动看 `df -i` 和 `df -h`。

- 日志写失败；
- 数据库无法写入/WAL 卡住；
- 容器被驱逐；
- SSH 登录失败（无法写 session）；
- 服务启动失败。

磁盘告警必须早于 100%。

---

## 0.5 内存满了怎么办

```bash
free -h
ps aux --sort=-%mem | head
cat /proc/meminfo | head
```

Linux 会把空闲内存用作 page cache，`free` 的 `available` 才是真正可用。如果 `available` 很低且 swap 在涨，就是真紧张。

### 处理

内存问题先分清 `free`、`available`、缓存和 RSS。Linux 会把空闲内存用于 page cache，`free` 很小不一定危险；`available` 才是新进程大约还能拿到的内存。若 `available` 持续下降、swap 大量换入换出，或内核日志出现 OOM kill，才说明已经影响业务。处理时优先摘流量、回滚、限制并发和保留现场，不要用清理 page cache 伪装恢复。

先找到增长最快的目录和文件，确认它是否能安全清理，再处理进程仍持有的已删除文件。生产清理必须保留审计日志和数据库文件，不能直接 `rm -rf /` 或删除正在写入的 binlog。

1. 找最大进程：`ps aux --sort=-%mem | head`；
2. 看是否持续上涨（内存泄漏）；
3. Go 服务用 pprof heap 分析；
4. 容器设置合理内存 limit 和 `GOMEMLIMIT`；
5. swap 只是缓冲，不是解决方案。

OOM 时内核会杀进程：

```bash
dmesg -T | grep -i 'killed process'
```

---

## 0.6 环境变量与 shell

```bash
export APP_ENV=dev
echo $APP_ENV
env | grep APP_
```

临时生效只影响当前 shell。永久生效写到 `~/.bashrc` 或 `~/.zshrc`，但**不要把密钥写进 shell 历史**。

查看命令来源：

```bash
which go
type go
go version
```

PATH 配置：

```bash
export PATH=$PATH:/usr/local/go/bin
```

### 常用的管道组合

管道把上一条命令的 stdout 交给下一条命令的 stdin，`grep` 过滤、`sort` 排序、`uniq -c` 计数、`awk` 取列。排障时要意识到每一段都可能改变结果：日志格式变化会让列号失效，`tail -f` 后接过滤会缓冲，统计大文件也会争抢磁盘。先在样本文件上验证输出，再对线上大日志执行。

```bash
# 统计访问日志里各状态码数量
awk '{print $9}' access.log | sort | uniq -c | sort -nr

# 找监听 8080 的进程
ss -lntp | grep 8080

# 实时看错误日志
tail -f app.log | grep -i error

# 查文件里某个字符串
grep -rn 'panic' /var/log/app/
```

---

## 0.7 必会命令

```bash
# 文件
ls -lah
cd /path
pwd
cp -r src dst
mv old new
mkdir -p /data/app
find . -name '*.go'
du -sh *

# 文本
cat file
less file          # /搜索，q退出
head -n 50 file
tail -f file
grep -rn 'keyword' .
sed -n '10,20p' file

# 进程
ps aux | grep app
top
kill -15 PID       # 优雅退出
kill -9 PID        # 强杀，最后手段

# 端口
ss -lntp | grep 8080
lsof -i:8080

# 网络
curl -v https://example.com
curl -sS -o /dev/null -w '%{http_code} %{time_total}\n' https://example.com
nc -vz host 443

# 压缩
tar -czf app.tar.gz app/
tar -xzf app.tar.gz
```

### `kill -15` 和 `kill -9`

信号不是“关闭按钮”，而是通知进程采取动作。`SIGTERM` 给服务执行优雅退出的机会：停止接收新请求、等待在途请求、关闭连接池并 flush 日志；`SIGKILL` 由内核立即终止，进程没有清理机会。正确顺序是先发 TERM，观察进程是否在发布窗口内退出；只有确认卡死且已评估影响后才发 KILL。

- `SIGTERM(15)`：请求进程优雅退出，服务可以处理完在途请求、关闭连接；
- `SIGKILL(9)`：内核直接杀，进程没有机会清理。

先 15，等几秒不退再 9。

---

## 0.8 systemd：让进程常驻

systemd 负责拉起进程、收集日志和管理开机启动，但它不是业务健康检查器。一个 unit 至少要写清运行用户、工作目录、启动命令、重启策略、环境变量和文件描述符上限；配置错误时先 `systemctl daemon-reload`，再用 `systemctl status` 与 `journalctl -u` 看真实退出原因。`Restart=on-failure` 只能恢复偶发崩溃，如果进程因为配置错误反复退出，自动重启会制造日志风暴。

```ini
# /etc/systemd/system/minicoin.service
[Unit]
Description=minicoin
After=network.target

[Service]
User=app
WorkingDirectory=/opt/minicoin
ExecStart=/opt/minicoin/server
Restart=on-failure
RestartSec=3
Environment=APP_ENV=prod
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl start minicoin
sudo systemctl status minicoin
sudo systemctl restart minicoin
journalctl -u minicoin -f
```

`Restart=on-failure` 让进程崩溃后自动拉起，但**不要用它掩盖反复崩溃**，要看日志找根因。

---

## 0.9 开发机常见问题

这张表用于建立第一反应：先区分网络、认证、DNS、端口和本机环境，不要看到“连不上”就重装客户端。每次排查都记录失败发生在哪一层，后续才能复用命令。

| 现象 | 可能原因 | 处理 |
|---|---|---|
| 连不上 | VPN/防火墙/sshd | 按四层排查 |
| 登录后很卡 | CPU/内存/磁盘满 | top/free/df |
| `go: command not found` | PATH 没配 | `which go`、配 PATH |
| 拉代码失败 | 凭证/网络 | `ssh -T git@host`、检查代理 |
| 端口被占 | 旧进程没退 | `ss -lntp`、kill |
| 磁盘满 | 日志/core dump | df/du/lsof |
| 服务起不来 | 配置/端口/依赖 | 看日志、`journalctl` |

---

## 习题

### 基础题

1. 为什么后端需要远程开发机？
2. SSH 连不上的四层排查顺序是什么？
3. `load average` 三个数分别代表什么？load 高一定是 CPU 忙吗？
4. `df` 显示磁盘满但 `du` 找不到大文件，可能是什么原因？
5. `free` 的 `available` 和 `free` 有什么区别？
6. `kill -15` 和 `kill -9` 的区别？
7. systemd 的 `Restart=on-failure` 有什么作用和风险？
8. 为什么不能把密钥写进 `~/.bashrc` 或命令历史？

### 动手题

9. 写一组命令：查看主机名、CPU 核数、内存、磁盘、负载。
10. 写命令找出 `/var/log` 下最大的 10 个文件。
11. 写命令找出监听 8080 的进程并优雅停止。
12. 写一个 systemd unit，让服务崩溃后 3 秒重启。

### 综合题

13. 开发机突然很卡，SSH 勉强能连上。写出你的前 10 个排查命令和判断逻辑。

---

## 参考答案

<details>
<summary>展开</summary>

1. 后端依赖数据库、缓存、消息队列和内网服务，还需要一台长期在线、与线上 Linux 接近的机器做编译、联调、压测、抓包和看日志。前端本地 dev server 能替代一部分工作，但不能替代后端依赖环境。

2. 按“DNS → TCP 端口 → SSH 认证 → 登录后 shell”排查。DNS 用 `getent hosts devbox.example.com`；TCP 用 `nc -vz devbox.example.com 22`；认证用 `ssh -vvv devbox`；登录后用 `hostname; whoami; uptime`。每层成功后再进下一层，不要把网络超时误判成密钥错误。

3. 三个数分别是最近 1、5、15 分钟的可运行任务和不可中断等待任务的平均数量。load 高不一定是 CPU 忙：磁盘 IO 阻塞也会把 load 拉高，要同时看 `top` 的 `%wa`、`iostat` 和进程状态。

4. 常见原因是文件已经被删除，但进程仍持有打开句柄，inode 空间没有释放。执行：

```bash
df -h
sudo lsof +L1 | head -30
```

确认是哪个进程后，重启或让进程重新打开文件；不要直接删除不认识的文件。

5. `free` 是完全没有被使用的内存；`available` 是估算后可分配给新进程的内存，包含可回收的 page cache。判断是否快 OOM 主要看 `available`、swap 和进程 RSS。

6. `SIGTERM(15)` 请求进程优雅退出，服务可以停止接新请求、等待在途请求并关闭连接池；`SIGKILL(9)` 由内核立即终止。先发 15，等待发布窗口仍不退出并评估影响后才发 9。

7. `Restart=on-failure` 能在进程异常退出后自动拉起，减少短时故障；风险是配置错误、端口冲突或持续 panic 时反复重启，制造日志风暴并掩盖根因。必须同时看 `systemctl status` 和 `journalctl -u`。

8. 写进 `.bashrc`、命令历史或进程命令行会进入文件、shell history、备份、截图和 `ps` 输出，泄露后很难彻底清理。使用密钥管理或平台注入的环境变量，并确保日志脱敏。

9.

```bash
hostnamectl
nproc
free -h
df -h
uptime
```

预期输出依次包含主机信息、CPU 核数、内存、磁盘和三个 load 值。命令缺失时先查 `command -v`，不要用一个命令的输出推断全部资源状态。

10.

```bash
sudo du -ah /var/log 2>/dev/null | sort -rh | head -10
```

这是一次性排查，不要在生产高峰对整个 `/` 扫。用 `sudo` 是为了读取受限日志；如果 `du` 很慢，先缩小到具体业务日志目录。

11.

```bash
pid=$(ss -lntp 'sport = :8080' | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1)
test -n "$pid"
kill -TERM "$pid"
```

先确认 PID 对应的是预期进程，再发 SIGTERM。若进程 10 秒后仍不退出，先用 `ps -o pid,ppid,stat,etime,cmd -p "$pid"` 和日志判断是否卡在关闭流程；只有确认必须强杀时才用 `kill -KILL`。

12. 完整的 unit 文件：

```ini
[Unit]
Description=minicoin
After=network.target

[Service]
User=app
WorkingDirectory=/opt/minicoin
ExecStart=/opt/minicoin/server
Restart=on-failure
RestartSec=3
Environment=APP_ENV=prod
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

启用并验证：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now minicoin
sudo systemctl status minicoin
journalctl -u minicoin -n 50 --no-pager
```

预期进程处于 `active (running)`，日志中没有配置或端口错误。配置错时先修 unit 再 reload；端口占用时先查监听者，不要靠反复 restart。

13. 先保留 SSH 会话，按下面顺序取证据：

```bash
uptime
nproc
free -h
df -h
top -b -n1 | head -30
ps -eo pid,ppid,stat,%cpu,%mem,rss,etime,cmd --sort=-%cpu | head -20
ps -eo pid,ppid,stat,%cpu,%mem,rss,etime,cmd --sort=-%mem | head -20
iostat -xz 1 3
ss -s
dmesg -T | tail -50
journalctl -n 100 --no-pager
```

判断逻辑：`load` 高且 `%us` 高，查 CPU 消耗进程；`%wa` 高且 `iostat` 的 `%util`/await 高，查磁盘 IO；`available` 低且 RSS 高，查内存泄漏或被回收的缓存；`df` 满，查日志和已删除但未释放的文件；`dmesg` 出现 OOM killer，先保留证据再考虑扩容或重启。命令权限不足时记录 `sudo` 限制，不能把“没权限”误写成“机器正常”。

</details>
