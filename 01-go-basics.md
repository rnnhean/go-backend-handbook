# 第 1 章 Go 语言基础（JS 对照版）

> 目标：你写惯了 JS，能快速读懂 Go 业务代码，并避开那些“语法看懂了、运行时却踩坑”的地方。
>
> 本章所有例子都能单独运行。建议边看边建一个 `hello` 目录，用 `go run .` 验证。

---

## 1.1 先建立正确的心智模型

| JS | Go | 关键差异 |
|---|---|---|
| 动态类型，运行时才发现类型错 | 静态类型，编译期检查 | 类型错了根本编译不过 |
| 单线程 + 事件循环 | goroutine + 调度器，多核并行 | 共享内存真的会同时被改 |
| `class` + 原型 | `struct` + 方法 | 没有继承，用组合 |
| `throw` / `try/catch` | 返回值 `error` | 错误是普通值，必须显式处理 |
| `null` / `undefined` | 零值 | `var n int` 是 0，不是 undefined |
| `npm` | `go mod` | 依赖版本写在 `go.mod/go.sum` |
| `node app.js` | `go run .` / `go build` | 上线用编译产物 |
| Promise/async-await | goroutine + channel/context | 并发模型不同，不能直接类比 |

### 最重要的三个差异

**1. Go 没有异常作为正常控制流。** 函数返回 `(result, error)`，调用方必须判断。

**2. Go 的并发是真的并行。** JS 的 `await` 是单线程事件循环；Go 的两个 goroutine 可能同时读写同一个变量，必须加锁或用 channel。

**3. Go 的值语义很常见。** 赋值、传参默认是拷贝；切片/map/channel 内部有引用，行为要单独理解。

---

## 1.2 第一个程序：`go run` 与 `go build`

建目录：

```bash
mkdir -p hello && cd hello
go mod init example.com/hello
```

`main.go`：

```go
package main

import "fmt"

func main() {
    fmt.Println("Hello, 后端")
}
```

运行：

```bash
go run .
# 输出：Hello, 后端

go build -o app .
./app
# 输出：Hello, 后端
```

### `go run` 和 `go build` 的区别

| | `go run .` | `go build -o app .` |
|---|---|---|
| 做什么 | 编译到临时目录并立即执行 | 编译成当前目录的 `app` |
| 产物 | 通常看不到，执行完临时文件可能被清理 | 明确的二进制文件 |
| 用途 | 本地开发、快速验证 | CI、镜像、上线 |
| 需要 Go 工具链 | 是 | 运行时不需要 |
| 改代码后 | 必须重新执行 | 必须重新 build |

**上线永远用 `go build` 的产物。** `go run` 不会热更新，也不是“解释执行”。

交叉编译：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -trimpath -ldflags="-s -w" -o app .
```

- `GOOS/GOARCH`：目标系统和架构；
- `CGO_ENABLED=0`：静态链接，方便放进 alpine/scratch；
- `-trimpath`：去掉本机路径；
- `-s -w`：去掉符号表和调试信息，减小体积。

---

## 1.3 变量、常量、零值

```go
var a int = 1
var b = 2          // 类型推断
c := 3             // 只能在函数内用
const MaxRetry = 3

var s string       // ""
var n int          // 0
var ok bool        // false
var p *int         // nil
var m map[string]int // nil
```

### 零值不是 undefined

```go
var n int
fmt.Println(n + 1) // 1，不会 NaN

var s string
fmt.Println(len(s)) // 0

var m map[string]int
fmt.Println(m["a"]) // 0（读 nil map 安全）
m["a"] = 1          // panic: assignment to entry in nil map
```

**nil map 可以读，不能写。** 要写必须先 `make`：

```go
m := make(map[string]int)
m["a"] = 1
```

### 常量

```go
const (
    StatusPending = iota // 0
    StatusPaid           // 1
    StatusFailed         // 2
)
```

业务枚举建议用有意义的字符串对外返回，内部可用 int 节省空间。

---

## 1.4 基础类型与金额

```go
bool
string
int int8 int16 int32 int64
uint uint8 ...
float32 float64
byte  // uint8 的别名
rune  // int32 的别名，表示 Unicode 码点
```

### 金额绝对不要用 float

金额的核心要求是“表示和累加都精确”，不是“看起来像小数”。`float64` 用二进制表示十进制小数，`0.1`、`0.2` 无法精确存储，经过多次计算后会产生尾差；用最小货币单位 `int64` 存分，所有运算都是整数，边界清楚。若业务确实需要高精度小数，应使用数据库 `DECIMAL` 和明确的舍入规则，并在 API 字段名里写清单位。

```go
// 错
price := 0.1 + 0.2
fmt.Println(price) // 0.30000000000000004

// 对：用 int64 存分
type Money int64

func (m Money) String() string {
    return fmt.Sprintf("%d.%02d", int64(m)/100, int64(m)%100)
}

func main() {
    m := Money(1234)
    fmt.Println(m.String()) // 12.34
}
```

### 字符串是字节序列

```go
s := "你好"
fmt.Println(len(s))                    // 6（UTF-8 字节数）
fmt.Println(utf8.RuneCountInString(s)) // 2（字符数）
fmt.Println(s[0])                      // 228，字节值
```

遍历字符用 `for _, r := range s`，不要用下标当字符。

---

## 1.5 数组、切片、map

### 数组 vs 切片

数组长度是类型的一部分，赋值时整个数组拷贝；切片是一个包含指针、长度和容量的描述符，日常业务几乎都用切片。切片变量本身按值传递，但它指向的底层数组共享，所以函数修改元素可能影响调用方。理解 `len` 和 `cap` 是排查“为什么 append 后原数组变了”的关键。

```go
arr := [3]int{1, 2, 3} // 长度是类型的一部分
s := []int{1, 2, 3}    // 切片，最常用
s = append(s, 4)
fmt.Println(len(s), cap(s)) // 4 6（容量可能更大）
```

### 切片共享底层数组（高频坑）

```go
a := []int{1, 2, 3, 4}
b := a[1:3]
b[0] = 99
fmt.Println(a) // [1 99 3 4]
```

`b` 和 `a` 指向同一块数组。要独立副本：

```go
c := append([]int(nil), b...)
// 或 Go 1.21+
c := slices.Clone(b)
```

### append 可能触发扩容

`append` 在容量足够时复用底层数组，容量不足时分配新数组并复制旧元素。调用方手里的切片长度不会自动变化，因此函数内部的 append 结果必须返回；同时，复用底层数组可能让两个切片互相影响。生产代码不要依赖具体扩容倍数，需要独立数据时显式复制。

```go
a := make([]int, 2, 4)
a[0], a[1] = 1, 2
b := append(a, 3)  // 容量够，b 和 a 共享
c := append(a, 4, 5, 6) // 容量不够，c 是新数组
```

不要依赖 `append` 后两个切片是否共享，除非你非常清楚容量。需要独立就 clone。

### nil 切片 vs 空切片

```go
var a []int   // nil，JSON -> null
b := []int{}  // 非 nil，JSON -> []
```

API 返回列表时通常用 `b`，避免前端拿到 `null` 还要判空。

### map

Go 的 map 是哈希表，平均 O(1) 查找，但遍历顺序不保证，不能把它当有序集合。读取不存在的 key 返回 value 类型零值，若零值本身有业务含义，必须用 `v, ok := m[k]` 区分“不存在”和“存了零值”；nil map 可以读但不能写。

```go
m := map[string]int{"a": 1}
m["b"] = 2

v, ok := m["c"]
if !ok {
    fmt.Println("not found") // 输出
}

delete(m, "a")

for k, v := range m {
    fmt.Println(k, v)
}
```

map 遍历顺序随机。需要稳定输出先排序 key。

### 并发读写 map 会 panic

Go 运行时会对并发读写 map 做检测，一旦发现会直接触发 fatal error，而不是返回一个普通错误；这类崩溃通常无法靠业务层 recover 恢复。需要并发访问时，用 `sync.RWMutex` 保护普通 map，或选 `sync.Map`；选择前先确认访问模式，不能因为“并发”就无脑换 `sync.Map`。

```go
m := map[string]int{}
go func() { m["a"] = 1 }()
go func() { _ = m["a"] }()
// fatal error: concurrent map read and map write
```

并发场景用 `sync.Map`（读多写少）或 `sync.RWMutex` 包一层。

---

## 1.6 结构体与方法

```go
type Account struct {
    UID     string
    Balance int64 // 分
}

func (a *Account) Deposit(amount int64) error {
    if amount <= 0 {
        return fmt.Errorf("amount must be positive: %d", amount)
    }
    a.Balance += amount
    return nil
}

func main() {
    a := &Account{UID: "u-1001"}
    _ = a.Deposit(100)
    fmt.Println(a.Balance) // 100
}
```

- **值接收者** `func (a Account)`：拿到副本，改不到原对象；
- **指针接收者** `func (a *Account)`：能修改原对象。

经验：结构体方法统一用指针接收者，除非是不可变的小值类型。

### 构造函数

Go 没有构造器语法，`NewXxx` 是团队约定。它的价值不是“风格”，而是集中检查不变量：必填字段是否为空、默认值是否合法、依赖是否 nil。若类型允许零值直接使用，也可以不写构造函数，但必须在文档和测试里证明零值安全。

Go 没有构造函数，用 `NewXxx`：

```go
func NewAccount(uid string) (*Account, error) {
    if uid == "" {
        return nil, errors.New("uid required")
    }
    return &Account{UID: uid}, nil
}
```

### 组合代替继承

```go
type Base struct{ ID int64 }
func (b Base) Describe() string { return fmt.Sprintf("id=%d", b.ID) }

type User struct {
    Base
    Name string
}

u := User{Base: Base{ID: 1}, Name: "demo"}
fmt.Println(u.Describe()) // id=1
```

没有 `extends`。嵌入是“把字段/方法提升”，不是继承，也不支持多态覆盖。

---

## 1.7 接口：隐式实现

```go
type Reader interface {
    Read(p []byte) (int, error)
}

type MyReader struct{}

func (MyReader) Read(p []byte) (int, error) {
    return 0, io.EOF
}

var r Reader = MyReader{} // 不需要 implements
```

### 接口的价值：依赖倒置

```go
type AccountRepo interface {
    Get(ctx context.Context, uid string) (*Account, error)
    AddBalance(ctx context.Context, uid string, amount int64) error
}

type RechargeService struct {
    repo AccountRepo
}

func NewRechargeService(repo AccountRepo) *RechargeService {
    return &RechargeService{repo: repo}
}
```

测试传 fake，生产传 MySQL 实现。业务层不 import 数据库包。

### 空接口与 `any`

`any` 能装任意类型，代价是编译期类型信息消失，使用前必须做类型断言或 `type switch`。它适合日志字段、通用容器边界等真的需要异构数据的场景；业务结构不要用 `map[string]any` 逃避建模，否则字段拼错、类型漂移和空值问题只能到运行时才发现。

```go
var x any = 1
n, ok := x.(int)
if !ok { ... }
```

`any` 能装任何值，但用之前要类型断言，滥用会丢失类型安全。业务接口尽量用具体类型/小接口。

---

## 1.8 错误处理：Go 的核心习惯

```go
func Load(id string) (*Account, error) {
    if id == "" {
        return nil, errors.New("empty id")
    }
    a, err := repo.Get(id)
    if err != nil {
        return nil, fmt.Errorf("load account %s: %w", id, err)
    }
    return a, nil
}
```

### `%w` 是什么

```go
var ErrNotFound = errors.New("not found")

err := fmt.Errorf("load account: %w", ErrNotFound)

fmt.Println(errors.Is(err, ErrNotFound)) // true
```

`%w` 保留错误链，调用方可以用 `errors.Is` 判断，而不是比较字符串。

### 哨兵错误 vs 自定义错误

哨兵错误适合调用方只需要判断“哪一类失败”的场景，例如 `ErrNotFound`；自定义错误适合需要携带字段、重试建议或上下文的场景。无论哪种，都应由调用方用 `errors.Is` / `errors.As` 判断，而不是比较错误字符串。错误一旦成为对外契约，就不能随意改文案。

```go
var ErrInvalidAmount = errors.New("invalid amount")

type BizError struct {
    Code string
    Msg  string
}

func (e *BizError) Error() string { return e.Code + ": " + e.Msg }

var be *BizError
if errors.As(err, &be) {
    fmt.Println(be.Code)
}
```

- `errors.Is` 判断“是不是某个错误”；
- `errors.As` 判断“是不是某种类型”。

### 不要用 panic 处理业务错误

```go
// 错
if amount <= 0 {
    panic("invalid amount")
}

// 对
if amount <= 0 {
    return ErrInvalidAmount
}
```

panic 只用于“程序无法继续”的编程错误。业务错误是可预期的，应该返回给调用方。

### 错误信息要有上下文，但不要泄漏敏感信息

包装错误时补的是“哪一步失败”，不是把整个请求体倒出来。推荐写成 `load account u-1001: %w` 这种可定位的信息；密码、token、Cookie、完整身份证和支付凭证不能进入错误或日志。对外响应只返回稳定错误码和安全提示，详细错误留在受控日志里。

```go
return fmt.Errorf("query order %s: %w", orderID, err)
// 不要带密码、token、完整身份证
```

---

## 1.9 defer / panic / recover

`defer` 适合释放资源、解锁和记录退出路径，因为无论函数从哪条 return 离开都会执行。但 defer 不是 try/catch：它不能让已经发生的业务错误消失，也不应该用 recover 把所有 panic 都吞掉。真正需要 recover 的位置是 goroutine 入口和 HTTP/gRPC 中间件等进程边界。

```go
func ReadFile(path string) error {
    f, err := os.Open(path)
    if err != nil {
        return err
    }
    defer f.Close()
    // ...
    return nil
}
```

- `defer` 在函数返回前执行，后进先出；
- 参数在 `defer` 语句执行时求值；
- 循环里大量 `defer` 会堆积，应该把循环体抽成函数。

```go
for _, path := range paths {
    func() {
        f, _ := os.Open(path)
        defer f.Close()
        // ...
    }()
}
```

### goroutine 里的 panic 会导致整个进程退出

```go
func SafeGo(fn func()) {
    go func() {
        defer func() {
            if r := recover(); r != nil {
                log.Printf("panic: %v", r)
            }
        }()
        fn()
    }()
}
```

不要在每个函数里到处 `recover`；在 goroutine 入口、HTTP 中间件等边界统一处理。

---

## 1.10 包与模块

```bash
go mod init example.com/minicoin
go get github.com/redis/go-redis/v9
go mod tidy
```

目录：

```
cmd/server/main.go      # 可执行入口
internal/service/       # 只能本模块 import
internal/repository/    # 数据访问
internal/httpapi/       # HTTP 接入
api/                    # proto/openapi
migrations/             # SQL 迁移
```

`internal` 是 Go 的强制访问控制：模块外的代码不能 import。

包名用短小写单词，不要 `util`、`common` 这种大杂烩。

---

## 1.11 代码格式与静态检查

`gofmt` 解决格式一致性，`go vet` 找可疑代码，`staticcheck`/`golangci-lint` 进一步发现未使用返回值、低效写法和常见 bug。三者不能互相替代，也不能替代测试：静态检查通过只说明“没发现某些模式”，不证明业务行为正确。CI 应固定工具版本，避免本地和流水线结论不同。

```bash
gofmt -w .
go vet ./...
staticcheck ./...      # 可选
golangci-lint run      # 团队常用
```

CI 里检查格式：

```bash
test -z "$(gofmt -l .)"
```

**`gofmt` 不是风格偏好，是 Go 的统一标准。** 不要手写对齐。

---

## 1.12 一个完整业务例子

这个例子故意把账户、金额校验和错误返回放在同一个小程序里，目的是让你沿着“输入 → 校验 → 状态变化 → 输出”读完一条完整路径。运行后先观察成功输出，再把金额改成 `-1`，确认错误分支不会修改余额；最后把 `errors.Is` 改成字符串比较，思考为什么那会成为脆弱的代码。

```go
package main

import (
    "errors"
    "fmt"
)

var ErrInvalidAmount = errors.New("invalid amount")

type Account struct {
    UID     string
    Balance int64
}

func (a *Account) Deposit(amount int64) error {
    if amount <= 0 {
        return ErrInvalidAmount
    }
    a.Balance += amount
    return nil
}

func main() {
    a := &Account{UID: "u-1001"}

    if err := a.Deposit(100); err != nil {
        fmt.Println("deposit failed:", err)
        return
    }
    fmt.Printf("uid=%s balance=%d\n", a.UID, a.Balance)

    if err := a.Deposit(-1); errors.Is(err, ErrInvalidAmount) {
        fmt.Println("invalid amount rejected")
    }
}
```

输出：

```
uid=u-1001 balance=100
invalid amount rejected
```

---

## 习题

### 基础题

1. `var s []int` 和 `s := []int{}` 有什么区别？JSON 序列化结果分别是什么？
2. 下面输出什么？为什么？
```go
a := []int{1,2,3,4}
b := a[:2]
b = append(b, 99)
fmt.Println(a)
```
3. nil map 读和写分别会发生什么？
4. 值接收者和指针接收者有什么区别？
5. Go 的接口是显式实现还是隐式实现？
6. `errors.New` 和 `fmt.Errorf("%w")` 的区别？
7. 为什么业务错误不该 panic？
8. `defer` 在循环里写 10 万次会怎样？
9. `go run .` 和 `go build .` 的区别？上线用哪个？
10. `internal` 目录有什么作用？

### 动手题

11. 写一个 `Money` 类型，`String()` 返回 `"12.34"`，输入 1234。
12. 写 `Divide(a, b int) (int, error)`，b 为 0 返回错误。
13. 写一个接口 `AccountRepo` 和内存 fake，测试充值 service。
14. 写一个程序，演示切片共享底层数组，并给出两种复制方法。

### 综合题

15. 设计一个 `RechargeService`：依赖 `AccountRepo` 接口，校验金额、幂等键，余额不足返回哨兵错误。写出接口、实现和表驱动测试。

---

## 参考答案

<details>
<summary>展开</summary>

1. `var s []int` 是 nil 切片，JSON 编码为 `null`；`s := []int{}` 是非 nil 空切片，编码为 `[]`。返回列表通常用空切片，避免前端额外处理 null。

2. 输出 `[1 2 99 4]`。`b := a[:2]` 与 `a` 共享底层数组，容量仍足够，`append(b, 99)` 写到 `a[2]`。

3. nil map 读取安全，返回 value 零值；写入会 panic，例如 `assignment to entry in nil map`。写入前用 `make` 初始化。

4. 值接收者拿到副本，修改字段不会影响原对象；指针接收者可以修改原对象。需要修改状态或结构体较大时用指针接收者，方法集要统一。

5. Go 接口是隐式实现，只要类型方法集满足接口即可，不需要写 `implements`。静态检查在编译期完成。

6. `errors.New` 适合创建哨兵错误；`fmt.Errorf("%w", err)` 包装错误并保留错误链。调用方用 `errors.Is/As` 判断，而不是比较字符串。

7. 业务错误是可预期结果，例如金额非法、余额不足、资源不存在；应返回 `error` 让调用方映射 HTTP 状态。panic 会中断当前流程，未在边界 recover 时可能导致进程退出。

8. 每次 `defer` 都会注册一个延迟调用，10 万次会累计到函数返回，增加内存并延迟资源释放。把循环体抽成小函数，让每次迭代结束就执行 defer。

9. `go run .` 会编译到临时目录后立即执行，适合开发验证；`go build -o app .` 生成可复用二进制，运行时不需要 Go 工具链。上线、镜像和 CI 用 `go build` 产物。

10. `internal` 限制模块外代码 import 该目录，用来保护内部实现。它是编译期访问控制，不是运行时权限系统。

11.

```go
package main

import (
	"fmt"
	"strconv"
)

type Money int64

func (m Money) String() string {
	cents := int64(m)
	if cents < 0 {
		return "-" + Money(-cents).String()
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func main() {
	fmt.Println(Money(1234).String())
	fmt.Println(Money(-5).String())
}
```

预期输出：

```text
12.34
-0.05
```

12.

```go
package main

import (
	"errors"
	"fmt"
)

var ErrDivisionByZero = errors.New("division by zero")

func Divide(a, b int) (int, error) {
	if b == 0 {
		return 0, ErrDivisionByZero
	}
	return a / b, nil
}

func main() {
	fmt.Println(Divide(10, 2))
	fmt.Println(Divide(10, 0))
}
```

13. fake 应该实现业务需要的最小接口，并保存可观察状态：

```go
type AccountRepo interface {
	Get(ctx context.Context, uid string) (Account, error)
	Add(ctx context.Context, uid string, amount int64) error
}

type fakeRepo struct {
	accounts map[string]Account
	err      error
}

func (f *fakeRepo) Get(_ context.Context, uid string) (Account, error) {
	if f.err != nil { return Account{}, f.err }
	a, ok := f.accounts[uid]
	if !ok { return Account{}, ErrNotFound }
	return a, nil
}

func (f *fakeRepo) Add(_ context.Context, uid string, amount int64) error {
	if f.err != nil { return f.err }
	a := f.accounts[uid]
	a.Balance += amount
	f.accounts[uid] = a
	return nil
}
```

service 只用接口，测试注入 fake；生产再注入 MySQL 实现。断言余额，不要只断言 `err == nil`。

14. 演示共享与复制：

```go
package main

import "fmt"

func main() {
	a := []int{1, 2, 3, 4}
	b := a[:2]
	b[0] = 99
	fmt.Println("shared:", a)

	copy1 := append([]int(nil), a...)
	copy1[0] = 7
	fmt.Println("copy after append:", a, copy1)

	copy2 := make([]int, len(a))
	copy(copy2, a)
	copy2[1] = 8
	fmt.Println("copy after copy:", a, copy2)
}
```

预期 `a[0]` 因共享变成 99，而两种复制方式都不会再修改 `a`。Go 1.21 也可用 `slices.Clone(a)`。

15. 一个可测试的完整轮廓：

```go
var (
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrReqIDReused         = errors.New("req_id reused with different request")
)

type Account struct {
	UID     string
	Balance int64
}

type RechargeRequest struct {
	ReqID  string
	UID    string
	Amount int64
}

type AccountRepo interface {
	Recharge(ctx context.Context, req RechargeRequest) (Account, bool, error)
}

type RechargeService struct {
	repo AccountRepo
}

func (s RechargeService) Recharge(ctx context.Context, req RechargeRequest) (Account, error) {
	if req.ReqID == "" { return Account{}, errors.New("req_id is required") }
	if req.UID == "" { return Account{}, errors.New("uid is required") }
	if req.Amount <= 0 { return Account{}, errors.New("amount must be positive") }
	acc, _, err := s.repo.Recharge(ctx, req)
	return acc, err
}
```

repository 的职责是用 `req_id` 唯一约束原子判重，在一个事务中完成“插入订单 + 更新余额”。表驱动测试至少包含：

| 场景 | 输入 | 期望 |
|---|---|---|
| 正常 | 金额 10，余额 100 | 余额 110，无错误 |
| 金额为 0 | 金额 0 | 参数错误，余额不变 |
| 金额为负 | 金额 -1 | 参数错误，余额不变 |
| 缺 req_id | 空字符串 | 参数错误，余额不变 |
| 同 req_id 同参数 | 再发一次 | 余额仍只加一次，`duplicated=true` |
| 同 req_id 不同金额 | 金额改变 | `ErrReqIDReused`，余额不变 |
| 余额不足 | 允许扣减的场景 | `ErrInsufficientBalance`，事务回滚 |

失败时先看测试是否只测了同一请求两次；必须补同键不同参数和 repo 错误。并发场景用 50 个 goroutine 同 req_id，最后断言余额只增加一次。

</details>
