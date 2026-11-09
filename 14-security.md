# 第 14 章 后端安全：认证、授权、注入与密钥

> 目标：你能识别并修复最常见的后端漏洞：越权、SQL 注入、命令注入、SSRF、敏感信息泄漏、重放攻击；知道密码和密钥该怎么存。
>
> 安全不是上线前扫一次漏洞，而是每个接口、每条日志、每次依赖升级都要考虑。

---

## 14.1 认证 vs 授权

- **认证（Authentication）**：你是谁？
- **授权（Authorization）**：你能做什么？

```go
// 认证：从 token 解析出 uid
uid, ok := auth.FromContext(ctx)
if !ok {
    return ErrUnauthenticated
}

// 授权：确认这个订单属于当前用户
order, err := repo.Get(ctx, orderID)
if err != nil { return err }
if order.UID != uid {
    return ErrNotFound // 不要返回 403，避免泄漏订单是否存在
}
```

### 常见越权

```http
GET /v1/orders/123
```

如果直接按 `order_id` 查，不校验 `uid`，任何登录用户改一下 ID 就能看别人的订单（IDOR，越权访问）。

正确做法：

```sql
SELECT * FROM recharge_order WHERE id = ? AND uid = ?
```

把 `uid` 作为查询条件，而不是查出来再判断（后者容易漏）。

### 角色权限

```go
type Permission string

const (
    PermReadOrder  Permission = "order:read"
    PermRefund     Permission = "order:refund"
)

func Require(perm Permission) Middleware {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if !auth.HasPermission(r.Context(), perm) {
                respondError(w, http.StatusForbidden, "FORBIDDEN")
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

权限判断放在服务端，不要相信前端隐藏按钮。

---

## 14.2 密码与密钥

### 密码：bcrypt / Argon2id，绝不明文

```go
import "golang.org/x/crypto/bcrypt"

hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
if err != nil { return err }

// 校验
if err := bcrypt.CompareHashAndPassword(hash, []byte(input)); err != nil {
    return ErrInvalidCredentials
}
```

- 不要用 MD5/SHA1/SHA256 直接存密码；
- 每个用户独立 salt（bcrypt 自带）；
- 成本参数按机器调整，登录延迟 100～300ms 可接受；
- 密码错误统一返回“用户名或密码错误”，不透露哪个错。

### 密钥管理

密钥管理要回答四个问题：密钥存在哪里、谁能读、泄露后怎么轮换、轮换时旧请求是否还能工作。

- 密钥放密钥管理服务、KMS 或平台注入的 Secret，不写进 Git、镜像、构建日志；
- 不同环境使用不同密钥，测试密钥不能访问生产资源；
- 定期轮换，并保留短期双密钥窗口完成平滑切换；
- 日志只记录密钥版本或指纹，不记录明文；
- 服务账号最小权限，只拿自己需要的密钥和命名空间。

```go
dsn := os.Getenv("MYSQL_DSN")
if dsn == "" {
    log.Fatal("MYSQL_DSN is required")
}
```

**执行过程。** 本地用 `.env.example` 放占位名，真实值只通过运行环境注入；启动日志打印“配置已加载，版本 v3”，不打印 DSN。验证方式是 `git grep` 搜不到真实值、镜像层中搜不到、无权限账号读取 Secret 应失败、轮换后新旧版本在预定窗口内都能验证。**失败场景**：把密钥写进 CI 变量后仍打进构建上下文；轮换时只替换数据库密码却没更新连接池，导致服务全部连接失败；日志打印完整 DSN，等于把主机、用户名和密码一起泄露。

---

## 14.3 注入攻击

### SQL 注入

错误写法：

```go
query := "SELECT * FROM users WHERE name = '" + name + "'"
db.Query(query)
```

输入 `' OR '1'='1` 就能绕过条件。

正确：参数化查询。

```go
row := db.QueryRowContext(ctx,
    "SELECT id, name FROM users WHERE name = ?", name)
```

`?` 由驱动转义，不是字符串拼接。

**动态表名/列名不能参数化**，必须用白名单：

```go
allowedSort := map[string]string{
    "created_at": "created_at",
    "amount_cent": "amount_cent",
}
col, ok := allowedSort[sortBy]
if !ok { return ErrInvalidSort }
query := "SELECT ... ORDER BY " + col + " LIMIT ?"
```

### 命令注入

错误：

```go
exec.Command("sh", "-c", "convert "+filename+" out.png")
```

文件名 `a; rm -rf /` 会执行任意命令。

正确：不经过 shell，参数分开传。

```go
exec.Command("convert", filename, "out.png")
```

还要校验路径，防止 `../../etc/passwd`。

### SSRF

服务端根据用户提供的 URL 发请求，攻击者可以访问内网：

```
POST /v1/fetch {"url":"http://169.254.169.254/latest/meta-data/"}
```

防护：

```go
func validateURL(raw string) (*url.URL, error) {
    u, err := url.Parse(raw)
    if err != nil { return nil, err }
    if u.Scheme != "http" && u.Scheme != "https" {
        return nil, errors.New("invalid scheme")
    }
    host := u.Hostname()
    ips, err := net.LookupIP(host)
    if err != nil { return nil, err }
    for _, ip := range ips {
        if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
            return nil, errors.New("private address not allowed")
        }
    }
    return u, nil
}
```

注意 DNS rebinding：校验后解析的 IP 可能变。更稳的是自定义 `DialContext`，在连接时校验实际 IP。

### 路径穿越

```go
// 错
filepath.Join("/data", userPath)
// 用户传 ../../etc/passwd
```

正确：`filepath.Clean` + 确认结果在根目录内：

```go
base := "/data"
full := filepath.Join(base, filepath.Clean("/"+userPath))
if !strings.HasPrefix(full, base+string(os.PathSeparator)) {
    return errors.New("invalid path")
}
```

---

## 14.4 XSS / CSRF

后端返回 JSON 时，XSS 主要在前端；但后端仍要：

- 不把用户输入直接拼进 HTML；
- 设置 `Content-Type: application/json`；
- 对富文本做白名单清洗（如 bluemonday）；
- 设置安全响应头。

```go
w.Header().Set("Content-Type", "application/json; charset=utf-8")
w.Header().Set("X-Content-Type-Options", "nosniff")
w.Header().Set("Content-Security-Policy", "default-src 'self'")
```

CSRF 防护：

- 使用 `SameSite=Lax/Strict` cookie；
- 写操作校验 CSRF token；
- 或用 Bearer token（不放 cookie 自动携带）。

---

## 14.5 敏感数据

### 传输

- 外部入口全站 HTTPS，HTTP 自动跳转，启用 HSTS；
- 内部服务尽量使用 mTLS 或网格提供的身份认证，不能假设“内网可信”；
- 不要通过 URL query 传 token/密码，因为代理、浏览器历史和访问日志都会记录；
- 敏感接口设置合理缓存头，避免共享代理缓存响应。

验证时用 `curl -v` 看 TLS 版本、证书链和跳转，再检查响应头是否包含 `Strict-Transport-Security`。失败场景是入口 HTTPS 了，但应用回源仍走明文，或者健康检查接口把内部依赖地址写在响应里；加密只覆盖了第一段链路，不等于端到端安全。

### 存储

- 密码只存抗暴力破解哈希，不存可逆密文；
- 身份证、银行卡等敏感字段加密存储，密钥与数据库分离；
- 日志和埋点不记录完整手机号、证件号、token、Cookie；
- 备份同样要加密，并限制恢复权限；
- 明确数据保留期，过期删除而不是无限堆积。

验证要查三层：数据库字段是可识别哈希/密文而不是明文；备份文件未授权不可读；用测试账号触发日志，确认日志中只出现脱敏值。失败场景是把加密密钥和密文放在同一台机器、同一份备份里，攻击者拿到备份即可解密。

### 返回

```json
{
  "uid": "u-1001",
  "phone": "138****1234",
  "id_card": "110***********1234"
}
```

按最小必要原则返回，不要 `SELECT *` 然后整个对象序列化。

### 日志

```go
// 错
log.Info("login", "password", password, "token", token)
// 对
log.Info("login", "uid", uid, "phone", maskPhone(phone))
```

日志是排障证据，不是请求转储。每新增一个字段都要问：线上值班人真的需要它吗？需要完整值还是哈希/尾号？请求体、响应体、SQL 参数和 header 都可能含敏感数据，默认不应整包打印。

**验证方法。** 用固定测试账号发一次登录、支付和失败请求，搜索日志平台中的完整手机号、证件号、密码、token；都应为零。再检查错误日志是否带堆栈和 SQL 原文。**失败场景**：开发环境为了调试打印完整 body，发布时忘记关闭；把异常对象直接序列化，里面带上了认证 header；日志平台权限过宽，任何员工都能搜真实用户数据。

---

## 14.6 签名与重放

开放 API 常用签名：

```
sign = HMAC-SHA256(secret, method + path + timestamp + nonce + body)
```

服务端校验：

1. timestamp 在 5 分钟窗口内；
2. nonce 未使用（Redis SETNX，TTL 10 分钟）；
3. 签名一致（常量时间比较）；
4. body 未被篡改。

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write([]byte(signingString))
expected := hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(sign)) {
    return ErrInvalidSign
}
```

**用 `hmac.Equal`，不要用 `==`**，避免时序攻击。

---

## 14.7 依赖与供应链

依赖风险不只是“有没有 CVE”，还包括包被投毒、维护者账号被盗、构建机被污染和镜像 tag 漂移。防护目标是让每次构建都能回答：用了哪个版本、从哪里来、谁批准、能否复现。

- 锁定依赖版本（`go.sum`），升级走评审而不是构建时自动拉最新；
- 定期 `govulncheck`，对可利用漏洞排期修复；
- 不引入来源不明的包，检查维护活跃度、许可证和发布历史；
- 构建使用可信基础镜像，生产镜像优先固定 digest；
- CI 产物签名/校验，限制谁能修改发布流水线；
- 最小镜像，减少 shell、编译器和无关工具。

```bash
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...
```

**执行过程。** 在 CI 的独立阶段执行扫描；有漏洞时输出包、版本、调用路径和修复版本。先在一个测试 MR 中故意引入已知漏洞依赖，确认门禁会失败；升级后重新扫描通过，再构建镜像并记录 digest。**失败场景**：只扫 `go.mod` 不扫实际调用路径，报了很多不可利用问题后团队直接忽略；使用 `latest` 镜像导致今天能构建、明天构建出完全不同的运行环境；只修应用依赖，不修基础镜像里的系统库。

---

## 14.8 其他常见问题

| 风险 | 防护 |
|---|---|
| 暴力破解 | 登录限流、失败次数锁定、验证码 |
| 文件上传 | 限制类型/大小、重命名、存对象存储、病毒扫描 |
| 反序列化 | 不反序列化不可信数据到危险类型 |
| 越权 | 每个查询带 uid/tenant_id |
| 信息泄漏 | 错误不返回堆栈/SQL |
| CORS 过宽 | 白名单 origin，不配 `*` + credentials |
| 默认账号 | 禁止默认密码，首次登录强制修改 |

---

## 14.9 上线前安全检查

这张表不是让开发自己打勾就结束。每一项都要有**可复现证据**，否则安全门禁只是形式。

| 检查项 | 怎么验证 | 失败时先做什么 |
|---|---|---|
| 认证与授权 | 用无 token、他人 uid、低权限账号分别请求接口 | 拒绝发布；补授权测试和查询条件 |
| SQL 与动态列 | 搜字符串拼接；用注入 payload 打测试环境 | 改参数化查询/白名单 |
| 命令与路径 | 用 `;`、`..`、绝对路径输入做负向测试 | 去掉 shell，规范化并限制根目录 |
| SSRF | 请求回环、私网、云元数据地址 | 在连接阶段拦截实际 IP |
| 密码与日志 | 查库中无明文；搜日志无密码/token | 轮换已泄露凭据并修复日志 |
| HTTPS 与 Cookie | 检查跳转、HSTS、Secure/HttpOnly/SameSite | 修入口配置，禁止明文回源 |
| 依赖漏洞 | `govulncheck`、镜像扫描、锁定 digest | 评估可利用性并排期升级 |
| 上传 | 改扩展名、超大小、伪造 Content-Type、路径穿越 | 白名单校验并隔离存储 |
| 错误响应 | 触发数据库/下游错误，检查是否泄漏 SQL/堆栈 | 统一错误映射，保留内部日志 |
| 限流与重放 | 重放旧 timestamp/nonce，发送高频请求 | 加时间窗、nonce 和业务幂等 |

上线后仍要监控认证失败率、403/429、异常下载、敏感接口访问和依赖漏洞告警。安全检查通过只代表已知风险被覆盖，不代表系统永久安全。

---

## 习题

### 基础题

1. 认证和授权有什么区别？
2. 为什么查询订单要带 `uid` 条件，而不是查出来再判断？
3. 为什么密码不能用 MD5/SHA256 直接存？
4. SQL 注入如何防护？动态表名怎么办？
5. 命令注入如何防护？
6. 什么是 SSRF？如何防护？
7. 签名校验为什么要用 `hmac.Equal`？
8. CSRF 如何防护？

### 动手题

9. 修复一段拼接 SQL 的代码，改成参数化查询。
10. 写一个 URL 校验函数，禁止内网 IP 和非 http/https。
11. 设计登录接口：密码校验、失败限流、错误信息、日志脱敏。
12. 写一个文件上传校验：类型、大小、文件名、存储路径。

### 综合题

13. 一个开放 API 被攻击者重放请求，导致重复下单。设计完整的防重放方案，包含时间戳、nonce、签名和业务幂等。

---

## 参考答案

<details>
<summary>展开</summary>

1. 认证回答“你是谁”，通常由 token/session/签名证明；授权回答“你能做什么”，由角色、资源和租户边界决定。先认证再授权，不能相信前端隐藏按钮。

2. 防止 IDOR 越权。把 `uid = ?` 放进 SQL 查询条件，能保证用户只能读到自己的资源；查出来再判断容易漏分支，还会泄漏资源是否存在。

3. MD5/SHA256 直接存密码速度快、无 salt、容易被彩虹表和 GPU 暴力破解。使用 bcrypt/Argon2id，每个用户独立 salt，成本参数按登录延迟和安全要求调整。

4. SQL 值用参数化查询，不能拼字符串；动态表名/列名不能参数化，必须用固定白名单映射。用户输入只作为值，不作为 SQL 结构。

5. 不经过 shell，使用 `exec.Command` 并把参数分开传；如果必须经过外部程序，还要校验路径、参数、超时和资源限制。

6. SSRF 是服务端请求用户提供的 URL，攻击者借此访问回环、私网、云元数据或内部服务。要在解析 URL、DNS 解析和实际连接阶段校验 scheme、IP 和重定向，禁止私网/回环/链路本地地址。

7. `==` 可能在第一个不同字节就返回，攻击者可通过响应时间推测签名；`hmac.Equal` 使用常量时间比较。

8. CSRF 防护包括 SameSite Cookie、CSRF token、检查 Origin/Referer，或使用不依赖自动携带 Cookie 的 Bearer token；不能只靠前端隐藏按钮。

9. 修复：

```go
// 错
query := "SELECT id, uid FROM orders WHERE id = '" + orderID + "'"
// 对
row := db.QueryRowContext(ctx, "SELECT id, uid FROM orders WHERE id = ? AND uid = ?", orderID, uid)
```

执行后用 SQL 注入 payload、他人订单 ID 和无 token 分别测试；期望均被拒绝且响应不泄漏数据库错误。

10. URL 校验示例：

```go
func validateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil { return nil, err }
	if u.Scheme != "http" && u.Scheme != "https" { return nil, errors.New("invalid scheme") }
	if u.Hostname() == "" { return nil, errors.New("missing host") }

	ips, err := net.LookupIP(u.Hostname())
	if err != nil { return nil, err }
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return nil, errors.New("private address is not allowed")
		}
	}
	return u, nil
}
```

还必须自定义 HTTP client 的 `DialContext`，在连接时再次校验实际 IP，并限制重定向，避免 DNS rebinding 和 302 绕过。

11. 登录接口：

```text
POST /v1/login {username, password}
-> 读取用户密码 hash，bcrypt.CompareHashAndPassword
-> 失败则统一返回“用户名或密码错误”
-> 按 IP + 用户维度做失败限流/退避
-> 成功签发短期 access token，记录 token 版本
-> 日志只记录 user_id、request_id、结果、IP 的脱敏值
```

验收：错误密码、不存在用户、被锁定用户返回同样外层提示；连续失败触发 429；日志里搜不到密码、token、完整身份证。成功登录也要绑定风控和设备信息，不能只靠密码。

12. 文件上传校验：

```go
const maxUpload = 5 << 20

func validateUpload(w http.ResponseWriter, r *http.Request) (io.Reader, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil { return nil, err }
	mime := http.DetectContentType(data)
	if mime != "image/png" && mime != "image/jpeg" { return nil, errors.New("unsupported type") }

	name := filepath.Base(r.Header.Get("X-Filename"))
	if name == "." || name == string(filepath.Separator) { return nil, errors.New("invalid name") }
	// 随机生成存储名，保存到固定根目录之外不可访问的位置。
	return bytes.NewReader(data), nil
}
```

生产还要防 polyglot 文件、病毒扫描、图片解码炸弹、路径穿越和对象存储权限；验证时上传伪造扩展名、超大文件、`../` 文件名和错误 MIME。

13. 防重放方案：

```text
请求头：
  X-Timestamp: 1760000000
  X-Nonce: 随机32字节
  X-Key-Id: 客户端密钥版本
  X-Signature: HMAC-SHA256(secret, method + path + timestamp + nonce + body_hash)

服务端：
1. 时间戳必须在 ±5 分钟内
2. Redis SET nonce:{key_id}:{nonce} 1 EX 600 NX，重复直接拒绝
3. 用对应 key 重新计算签名，hmac.Equal 比较
4. 校验 body hash 和 Content-Type
5. 业务请求还必须带 req_id，数据库唯一键兜底
6. 记录审计日志和异常指标
```

验收：同一请求重放第二次返回 409/401；过期时间戳拒绝；换 body 但复用签名拒绝；nonce key 在 TTL 后可按策略清理。只加时间戳不加 nonce 仍可在窗口内重放，只用 Redis 不加业务唯一键则 Redis 故障时会重复下单。

</details>
