# 第 12 章 测试体系：单测、集成、E2E 与压测

> 目标：你能为一个接口写出“能抓住真实 bug”的测试，而不是为了覆盖率写一堆永远为真的断言；知道哪一层测什么、怎么造数据、怎么接 CI。
>
> 测试的价值不是证明代码能跑，而是**在改动时告诉你哪里坏了**。

---

## 12.1 测试金字塔

```
        E2E / 手工验收      少量、慢、贵
       ───────────────
      集成测试（DB/Redis）  中等数量
     ───────────────────
    单元测试（纯逻辑）      大量、快、便宜
```

| 层 | 测什么 | 依赖 | 速度 | 例子 |
|---|---|---|---|---|
| 单元 | 业务规则、边界、错误分支 | 无/假实现 | 毫秒 | 金额校验、状态机 |
| 集成 | SQL、事务、缓存、序列化 | 真实 MySQL/Redis | 秒 | 幂等插入、行锁 |
| 契约 | 接口请求/响应兼容 | HTTP/proto | 秒 | OpenAPI 校验 |
| E2E | 用户完整路径 | 全套环境 | 分钟 | 充值 → 到账 |

反模式：

- 所有测试都连真实数据库：慢、不稳定；
- 只写单测不写集成：SQL 写错测不出来；
- 只靠手工点页面：回归成本高；
- 追求 100% 覆盖率：大量无意义断言。

---

## 12.2 表驱动测试

Go 最常见的测试写法：

```go
func TestValidateAmount(t *testing.T) {
    tests := []struct {
        name    string
        amount  int64
        wantErr error
    }{
        {"normal", 100, nil},
        {"zero", 0, ErrInvalidAmount},
        {"negative", -1, ErrInvalidAmount},
        {"max", math.MaxInt64, nil},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := ValidateAmount(tt.amount)
            if !errors.Is(err, tt.wantErr) {
                t.Fatalf("got %v, want %v", err, tt.wantErr)
            }
        })
    }
}
```

要点：

- `t.Run` 给每个用例命名，失败时知道是哪个；
- 用 `errors.Is` 比较哨兵错误；
- 不要只比较 `err != nil`，要确认错误类型；
- 用例名描述场景，不要用 `case1`。

---

## 12.3 用接口做 fake，不一定要 mock 框架

```go
type AccountRepo interface {
    Get(ctx context.Context, uid string) (*Account, error)
    AddBalance(ctx context.Context, uid string, amount int64) error
}

type fakeRepo struct {
    accounts map[string]*Account
    err      error
}

func (f *fakeRepo) Get(ctx context.Context, uid string) (*Account, error) {
    if f.err != nil { return nil, f.err }
    a, ok := f.accounts[uid]
    if !ok { return nil, ErrNotFound }
    return a, nil
}

func (f *fakeRepo) AddBalance(ctx context.Context, uid string, amount int64) error {
    if f.err != nil { return f.err }
    f.accounts[uid].Balance += amount
    return nil
}
```

测试：

```go
func TestRecharge(t *testing.T) {
    repo := &fakeRepo{accounts: map[string]*Account{
        "u-1001": {UID: "u-1001", Balance: 0},
    }}
    svc := NewRechargeService(repo)

    got, err := svc.Recharge(context.Background(), RechargeRequest{
        ReqID: "req-1", UID: "u-1001", Amount: 100,
    })
    if err != nil { t.Fatal(err) }
    if got.Balance != 100 { t.Fatalf("balance=%d", got.Balance) }
}
```

**fake 只实现你需要的接口**，不要让测试依赖整个数据库。

### 什么时候用 mock 框架

- 需要验证调用次数/参数；
- 第三方接口复杂；
- 但不要滥用：过度 mock 会让测试和实现细节耦合，重构就坏。

---

## 12.4 集成测试：SQL 和事务必须用真数据库

单测的 fake 无法验证：

- SQL 语法；
- 索引是否命中；
- 唯一键冲突；
- 事务回滚；
- 行锁/并发行为。

### 用 testcontainer 或本地测试库

集成测试连真实数据库，但**不能连开发共享库或生产库**。推荐每个 CI Job 启动一次性 MySQL 容器，或给每个测试进程分配独立 schema。测试启动时先执行与生产相同的迁移，再用 `TRUNCATE` 或事务回滚清理数据。下面的 `TEST_MYSQL_DSN` 是占位配置，缺少时应直接 skip，而不是悄悄连接到默认数据库：

```go
func TestRechargeRepo(t *testing.T) {
    if testing.Short() {
        t.Skip("skip integration in -short")
    }
    db, err := sql.Open("mysql", os.Getenv("TEST_MYSQL_DSN"))
    if err != nil { t.Fatal(err) }
    defer db.Close()

    // 每个测试用独立 schema 或事务回滚
    if _, err := db.Exec("TRUNCATE TABLE recharge_order"); err != nil {
        t.Fatal(err)
    }

    repo := NewRechargeRepo(db)
    order, err := repo.CreateIfAbsent(ctx, Order{ReqID: "req-1", UID: "u-1", Amount: 100})
    if err != nil { t.Fatal(err) }

    // 重复插入应返回已存在
    _, err = repo.CreateIfAbsent(ctx, Order{ReqID: "req-1", UID: "u-1", Amount: 100})
    if !errors.Is(err, ErrDuplicate) { t.Fatalf("got %v", err) }
}
```

**执行过程。** 先执行 `go test ./... -short`，集成测试应被跳过；再启动一个本地 MySQL，设置 `TEST_MYSQL_DSN`，执行 `go test ./internal/repository -run TestRechargeRepo -count=1 -v`。预期第一次插入成功，第二次返回 `ErrDuplicate`，表中只有一条订单。可以故意删掉唯一索引再跑，测试必须失败，证明它确实在验证数据库约束，而不是只验证 fake 的行为。

**失败场景。** 多个测试共用一张表却不清理，导致结果依赖执行顺序；只测“插入成功”，不测重复键，迁移时删掉唯一约束也发现不了；测试连接串写死成本地 root，换 CI 后连到错误数据库。集成测试的隔离性和它测的业务同样重要。

### 并发测试

```go
func TestConcurrentDuplicateRequest(t *testing.T) {
    const n = 20
    var wg sync.WaitGroup
    errs := make(chan error, n)

    for i := 0; i < n; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            _, err := svc.Recharge(ctx, RechargeRequest{ReqID: "same", UID: "u-1", Amount: 100})
            errs <- err
        }()
    }
    wg.Wait()
    close(errs)

    // 只允许一个成功，余额只加一次
}
```

并发测试要断言**最终状态**，不是只看有没有报错。上例还缺少三个关键断言：

```go
success := 0
for err := range errs {
    if err == nil { success++ }
}
if success != 1 {
    t.Fatalf("success=%d, want 1", success)
}

balance, err := repo.GetBalance(ctx, "u-1")
if err != nil { t.Fatal(err) }
if balance != 100 {
    t.Fatalf("balance=%d, want 100", balance)
}
```

这段测试必须跑在真实 MySQL 或具备同等原子语义的 repository 上；用不加锁的内存 fake 跑 20 个 goroutine，很容易把测试自身写成数据竞争。CI 中至少运行一次 `go test -race ./...`，并重复执行并发测试多轮，例如 `go test ./internal/service -run TestConcurrentDuplicateRequest -count=20`。如果只断言 `err != nil`，重复加钱、重复订单和上下文取消都可能被漏掉。

---

## 12.5 HTTP 测试：httptest

```go
func TestCreateRecharge(t *testing.T) {
    h := NewHandler(fakeService{})
    req := httptest.NewRequest(http.MethodPost, "/v1/recharges",
        strings.NewReader(`{"req_id":"req-1","amount_cent":100,"channel":"manual"}`))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer demo-token")
    req.Header.Set("X-Request-Id", "trace-test-1")

    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)

    if rec.Code != http.StatusCreated {
        t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
    }
}
```

覆盖：

- 首次成功 201、幂等命中 200；断言状态码、`Content-Type`、响应业务码和响应体关键字段；
- 参数错误 400；发送非法 JSON、缺少必填字段、金额为负，确保 handler 不调用 service；
- 未认证 401；验证中间件拒绝请求；
- 幂等冲突 409；让 fake service 返回 `ErrReqIDConflict`，断言稳定错误码，而不是数据库原文；
- 服务错误 500；让 fake 返回内部错误，断言响应不泄漏 SQL、堆栈和下游地址；
- 响应头与响应体中的 `request_id` 一致；这能预警链路追踪断链。

`httptest` 测的是 HTTP 边界，不替代真实数据库集成测试。测试里的 fake service 最好记录调用次数，例如参数错误时断言 `calls == 0`；否则 handler 返回 400 但后台仍执行了扣款，单看状态码发现不了。

### 契约测试

用 OpenAPI 校验响应：

```bash
go test ./... -run TestContract
```

或 CI 里用 Schemathesis/Dredd 对真实服务跑。契约测试检查的是“请求/响应结构和文档是否一致”，不证明业务结果正确。例如文档声明 `balance` 是整数，实际返回字符串，契约测试应失败；但余额算错仍要靠业务测试发现。

**执行过程。** 先从接口文档导出 `openapi.yaml`，在 CI 启动服务，用固定请求调用测试环境，再用校验器比对状态码、Content-Type 和 JSON Schema。把一次真实失败写进回归用例：把 `amount_cent` 从整数改成字符串，预期契约测试失败；恢复后通过。这样证明门禁不是“打印一句通过”。

**边界。** 契约测试不应对生产环境写数据，也不应用真实用户凭证；测试账号、数据和请求 ID 都必须是虚构的。接口文档与实现冲突时，要停下来确认哪一边是产品契约，不能直接改测试让它通过。

---

## 12.6 E2E：少量、关键路径

E2E 跑完整链路：

```bash
#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8081}"

# 1. 创建充值
resp=$(curl -sS -X POST "$BASE_URL/v1/recharges" \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -H 'X-Request-Id: e2e-001' \
  -d '{"req_id":"e2e-req-1","amount_cent":10000,"channel":"manual"}')

echo "$resp" | jq -e '.code == "OK" and .request_id == "e2e-001"'
echo "$resp" | jq -e '.data.balance == 10000 and .data.duplicated == false'

# 2. 原样重试同一个 req_id：验证幂等命中，而不是又加一次钱
resp2=$(curl -sS -X POST "$BASE_URL/v1/recharges" \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -d '{"req_id":"e2e-req-1","amount_cent":10000,"channel":"manual"}')
echo "$resp2" | jq -e '.code == "OK" and .data.duplicated == true and .data.balance == 10000'
```

E2E 只覆盖 1～3 条最关键路径，不要把所有边界都放 E2E。

---

## 12.7 覆盖率：看关键分支，不要刷数字

```bash
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
go tool cover -html=coverage.out
```

覆盖率告诉你“哪些代码没被执行”，不告诉你“断言是否正确”。

应该关注：

- 核心业务分支是否覆盖；
- 错误路径是否覆盖；
- 并发/幂等是否覆盖；
- 不要为了 100% 给 getter 写测试。

---

## 12.8 race / benchmark / fuzz

### race

```bash
go test -race ./...
```

检测运行时数据竞争。注意：只检测实际执行到的路径，测试没覆盖的竞态发现不了。

### benchmark

```go
func BenchmarkEncode(b *testing.B) {
    for i := 0; i < b.N; i++ {
        _ = encode(data)
    }
}
```

```bash
go test -bench=. -benchmem
```

看 `ns/op`、`B/op`、`allocs/op`。优化前后用同一 benchmark 对比。

### fuzz

```go
func FuzzParseAmount(f *testing.F) {
    f.Add("100")
    f.Fuzz(func(t *testing.T, s string) {
        _, _ = ParseAmount(s) // 不能 panic
    })
}
```

```bash
go test -fuzz=FuzzParseAmount -fuzztime=30s
```

适合解析器、协议、输入校验。

---

## 12.9 测试数据管理

- 每个测试自己造数据，不依赖执行顺序；
- 用固定 ID 前先清理；
- 集成测试用独立 schema；
- 不要用生产数据（隐私/合规）；
- 时间、随机数、UUID 要可注入，测试才能稳定；
- 外部服务用 fake，不发真实请求。

```go
type Clock interface{ Now() time.Time }
```

测试传固定时间，避免 `time.Now()` 导致断言不稳定。

---

## 12.10 CI 门禁

```yaml
stages: [lint, test, integration, build]

lint:
  script:
    - test -z "$(gofmt -l .)"
    - go vet ./...

test:
  script:
    - go test ./... -count=1 -race -short

integration:
  script:
    - go test ./... -count=1 -run Integration
  services:
    - mysql:8.0
    - redis:7

build:
  script:
    - CGO_ENABLED=0 go build -trimpath -o app ./cmd/server
```

原则：

- 合并前必须全绿；
- 慢测试分层；
- 失败日志可下载；
- 测试要稳定，flaky 测试要修，不要重跑掩盖。

---

## 12.11 常见测试坏味道

| 坏味道 | 问题 |
|---|---|
| 只断言 `err == nil` | 没验证结果 |
| 测试依赖执行顺序 | 单独跑就失败 |
| 大量 `time.Sleep` | 慢且不稳定 |
| mock 所有东西 | 测试实现细节，重构就坏 |
| 用生产数据 | 隐私/不可重复 |
| 不测错误路径 | 线上错误没覆盖 |
| 覆盖率 100% 但断言空洞 | 假安全 |

---

## 习题

### 基础题

1. 测试金字塔各层测什么？为什么不能全靠 E2E？
2. 表驱动测试有什么好处？
3. fake 和 mock 的区别？什么时候用哪个？
4. 为什么 SQL/事务必须用集成测试？
5. 覆盖率能证明什么，不能证明什么？
6. `-race` 能发现所有数据竞争吗？
7. 什么是 flaky 测试？怎么处理？
8. 为什么测试里要注入时间/随机数？

### 动手题

9. 给 `ValidateAmount` 写表驱动测试，覆盖 0、负数、正常、最大值。
10. 给充值 service 写 fake repo，测试重复 req_id 只加一次。
11. 用 `httptest` 测试创建订单接口的 200/400/409。
12. 写一个并发测试：20 个 goroutine 同时用同一 req_id 充值，断言只成功一次。

### 综合题

13. 设计一个充值功能的测试计划：单测、集成、契约、E2E 各覆盖什么，CI 怎么分层，如何判断可以上线。

---

## 参考答案

<details>
<summary>展开</summary>

1. 单测测纯业务规则和错误分支；集成测真实 SQL、事务、唯一键、缓存和序列化；契约测请求响应兼容；E2E 测少量关键用户路径。E2E 慢、贵且容易受环境影响，不能覆盖全部边界。

2. 表驱动把输入、期望和用例名集中，新增场景只需加一行，失败时 `t.Run` 能直接指出场景；适合金额校验、权限矩阵和状态机。

3. fake 是可工作的简化实现，验证结果；mock 验证调用次数、参数和顺序。优先 fake，只有确实需要验证交互或第三方边界复杂时才用 mock。

4. fake 无法证明 SQL 语法、索引、唯一键、事务回滚和锁行为；这些必须在真实 MySQL 或一次性容器里测试。

5. 覆盖率证明哪些代码被执行过，不证明断言正确、边界完整、并发正确或线上行为正确。核心业务分支比一个漂亮的百分比重要。

6. 不能。race detector 只检测测试实际执行到的并发路径，未触发的读写不会被发现。

7. flaky 测试是偶发成功/失败的测试，常见原因是时间、执行顺序、共享状态、网络和并发。修复根因或隔离依赖，不要用重跑掩盖。

8. 注入时间和随机数能让测试可重复，避免凌晨、跨日、时区和随机 ID 导致断言不稳定。副作用和外部服务也应可注入或隔离。

9. 表驱动测试：

```go
func TestValidateAmount(t *testing.T) {
	tests := []struct {
		name    string
		amount  int64
		wantErr bool
	}{
		{"normal", 100, false},
		{"zero", 0, true},
		{"negative", -1, true},
		{"max", math.MaxInt64, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAmount(tt.amount)
			if (err != nil) != tt.wantErr {
				t.Fatalf("amount=%d err=%v wantErr=%v", tt.amount, err, tt.wantErr)
			}
		})
	}
}
```

运行 `go test ./... -run TestValidateAmount -count=1`，四个子用例都应通过。

10. fake repo 必须保存幂等键和参数：

```go
type fakeRepo struct {
	mu       sync.Mutex
	orders   map[string]RechargeRequest
	balances map[string]int64
}

func (f *fakeRepo) Recharge(_ context.Context, req RechargeRequest) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.orders[req.ReqID]; ok {
		if old.AmountCent != req.AmountCent || old.UID != req.UID || old.Channel != req.Channel {
			return Result{}, ErrReqIDConflict
		}
		return Result{UID: req.UID, Balance: f.balances[req.UID], Duplicated: true}, nil
	}
	f.orders[req.ReqID] = req
	f.balances[req.UID] += req.AmountCent
	return Result{UID: req.UID, Balance: f.balances[req.UID]}, nil
}
```

测试同 req_id 调两次，断言第二次 `duplicated=true` 且余额只增加一次；再改金额，断言 409 冲突且余额不变。

11. `httptest` 接口测试：

```go
func TestCreateRechargeStatus(t *testing.T) {
	h := handlerWithService(fakeService{})
	tests := []struct {
		name string
		body string
		want int
	}{
		{"created", `{"req_id":"r-1","amount_cent":100,"channel":"manual"}`, http.StatusCreated},
		{"bad json", `{`, http.StatusBadRequest},
		{"conflict", `{"req_id":"conflict","amount_cent":100,"channel":"manual"}`, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/recharges", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer demo-token")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}
```

还要断言响应 `code/request_id/data`，以及内部错误不包含 SQL 或堆栈。

12. 并发测试：

```go
func TestConcurrentSameReqID(t *testing.T) {
	repo := repository.NewMemory()
	svc := service.NewRecharge(repo, cache.NewMemory(), mq.NewMemoryProducer())
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	results := make(chan service.RechargeResult, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.Recharge(context.Background(), service.RechargeRequest{
				ReqID: "req-race", UID: "u-1001", Amount: 10, Channel: "manual",
			})
			errs <- err
			results <- result
		}()
	}
	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		if err != nil { t.Fatal(err) }
	}
	acc, err := repo.GetAccount(context.Background(), "u-1001")
	if err != nil { t.Fatal(err) }
	if acc.Balance != 110 { t.Fatalf("balance=%d want=110", acc.Balance) }
}
```

至少一个结果 `duplicated=false`，其余应为 `true`；最终余额只增加一次。用 `go test -race ./... -count=1` 同时发现共享状态竞争。

13. 充值测试计划：

| 层 | 覆盖内容 | 失败证据 |
|---|---|---|
| 单测 | 金额校验、缺字段、冻结账户、同键不同参数、repo 错误 | 具体用例名、输入和余额断言 |
| 集成 | 唯一键、事务回滚、行锁、流水和订单一致、50 并发同 req_id | SQL 错误、事务前后余额和订单数 |
| 契约 | 201/200、400/401/409/500、字段类型、request_id 一致 | OpenAPI 校验报告和响应样本 |
| E2E | 登录 → 充值 → 查余额 → 查流水 | 请求链、request_id、最终数据库结果 |

CI 分层：提交时跑格式、vet、单测和 race；合并前跑一次性 MySQL 集成测试和契约测试；部署预发后跑 smoke/E2E。上线条件：全部门禁通过、无 flaky、错误率和 P99 在基线内、监控告警已配置、回滚演练通过。

</details>
