# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

代码注释、错误文案、提交信息一律用中文，保持与现有代码一致。

## 常用命令

```bash
make run        # go run ./cmd/server
make build      # 产物在 bin/server
make test       # 不带 DB,需要数据库的用例会跳过
make test-db    # 带 DB,真跑仓库层和 service 层用例
make vet        # go vet ./...
make fmt        # gofmt -l -w .
```

跑单个测试（仓库层和 service 层必须给 `TEST_DSN`，否则静默跳过）：

```bash
TEST_DSN="host=localhost user=postgres password=123456 dbname=mini_agent_test port=5432 sslmode=disable" \
  go test -count=1 -run 'TestLogin_FailuresAreIndistinguishable' ./internal/service/
```

`-count=1` 是必需的，否则拿到的是缓存结果。`go test` 的缓存粒度是**包**，改任何一个 `_test.go` 整包重跑。

`TEST_DSN` 必须指向单独的测试库（现在是 `mini_agent_test`），**不能指向开发库 `mini_agent_backend`** —— `TestMain` 里会跑 `AutoMigrate` 改表结构。

## 启动契约

配置四层优先级，后盖前：内置默认值 < `configs/config.yaml` < `.env` < 真实环境变量。

- `JWT_SECRET` **只从环境变量读**，`config.JWT.Secret` 上打了 `yaml:"-"`，写进 YAML 也不会被加载。必须 ≥ 32 字节，缺了直接启动失败，不给随机生成的兜底。
- `config.LoadEnvFile` 不覆盖已存在的环境变量，所以线上误留一份 `.env` 盖不掉编排系统注入的密钥。它必须在任何 `os.Getenv` 之前调用。
- 配置校验失败一律 `os.Exit(1)`，没有"能跑但不安全"的降级路径。
- `/debug/panic` 只在 `APP_ENV != production` 时注册。

**表结构的真实来源是 `main.go` 里的 `db.AutoMigrate`**，不是 `migrations/`。`migrations/000001_init.{up,down}.sql` 目前只有一行注释，是空占位，`make migrate-up` 什么都不会做。

## 分层与跨层契约

```
handler   HTTP 适配:绑定/格式校验 → 调 service → 错误映射 → 响应塑形
service   业务规则、事务边界
repository 数据访问,负责把 gorm/驱动错误翻译成 model.Err*
```

判断某段逻辑归哪层：**只看请求本身就能判的归 handler，需要查库或需要业务知识的归 service。**（"email 格式对不对"是 handler；"email 是否已注册"是 service。）handler 里出现 `if x.UserID != userID` 就是层错了 —— 归属校验在 service（`SessionService.mustOwn`）。

### 错误处理

四个动作，按"谁消费这个错误"选：

| 动作 | 用在 |
|---|---|
| `return err` | 这一层加不了新信息（仓库层已翻译过的错误直接透传） |
| `fmt.Errorf("...: %w", err)` | 你手里有上层不知道的事实（如 `user %d`） |
| `return model.ErrXxx` | 跨包边界翻译，或**故意**切断细节（登录失败合并） |
| `errors.New(...)` | 压根没有 cause（接线错误、参数非法） |

**跨包边界翻译，包内包装。** 仓库层把 `gorm.ErrDuplicatedKey` 翻成 `model.ErrEmailOccupied` 而不是 `%w` 包装 —— 包装会让 GORM 的错误类型变成对上层的 API 契约。

`errRules`（`internal/pkg/response/response.go`）用切片配 `errors.Is` 而不是 `map[error]`，因为业务错误经常被 `%w` 包一层，map 直接查会漏。**没登记的错误一律 `CodeInternal`，对外只说"内部错误"** —— 数据库报错原文可能带表名、SQL、连接串。

一个错误只上报一次：要么 `return err`，要么 `slog` 后自己消化，别两个都做。handler 里 `response.FailFromError` 会 `c.Error(err)` 把原文交给 Logger 中间件带 `trace_id` 打出来，handler 自己不写日志。

### 响应外壳

所有出口（含 404/405/panic 兜底）都走 `response.OK` / `Fail` / `FailWithData`，客户端永远解析同一个结构。

**业务码前三位对齐 HTTP 状态码**，`httpStatus()` 直接 `code / 100`，不维护映射表。加新码时在对应区间往后排（`40001` = 参数类的某个细分）。`Fail` 会 `c.Abort()`。

### 中间件顺序

`TraceID → Recovery → Logger`，私有组再加 `Auth`。TraceID 必须最外层（后两个都要用 trace_id），Recovery 在 Logger 外面（Logger 自己 panic 也能兜住）。

### 接口定义在使用方

- `handler.AuthService` 定在 handler 包 —— handler 测试塞个假的就能跑，不连库不签 JWT
- `service.TokenIssuer` 定在 service 包 —— service 测试不需要真密钥
- `middleware.TokenVerifier` 定在 middleware 包

**依赖为 nil 时一律 fail closed 返回 500，不放行、不 panic、也不退化成 404。** 见 `middleware.Auth`、`AuthHandler.ready`。路由照常注册是刻意的：「接口不存在(404)」和「接口没接好(500)」混在一起会让排查走错方向。

### 事务

`Repository.Transaction(ctx, fn)`，`fn` 拿到的是绑定在该事务上的 `*Repository` 副本，里面每个仓库方法走同一条连接。**Go 没有 `@Transactional`，多次写入的原子性必须手动开事务**，漏了不会有编译错误。

嵌套调用时 GORM 开的是 SAVEPOINT 而不是嵌套事务 —— 测试正是靠这一点把整个用例包在外层事务里再回滚。

## 安全不变量（改动前先读）

这些不是风格偏好，改动会引入实际漏洞，且都有测试锁着：

- **账号枚举**：`Login` 里"用户不存在"和"口令错误"必须归到同一个 `model.ErrInvalidCredentials`，同一个码、同一句文案。`ErrEmailOccupied` 的文案也刻意含糊。
- **校验顺序**：`Login` 的账号状态检查必须在口令校验**之后**。放前面 = 不知道口令也能问出邮箱是否注册。`ErrUserDisabled` 能和 `ErrInvalidCredentials` 分开，全靠这个顺序。
- **`Register` 先散列再建记录**：反过来的话散列失败会在库里留一条没有口令的用户，唯一索引让他再也注册不上。
- **不回显原始 error**：`err.Error()` 绝不进响应体。绑定失败走 `handler.bindJSON` → `validate.Translate`，只输出预置文案；`validate` 包通篇不碰 `fe.Value()`，口令不会跟着校验错误漏出去。
- **`UserResp` 手写字段，不内嵌 `model.User`**：内嵌的话以后往模型加敏感列会自动出现在响应里。`dto_test.go` 用反射锁住"不许有匿名字段"。
- **口令长度用自定义 `bmax` 而不是 `max`**：validator 的 `max` 数的是**字符数**，bcrypt 在 **72 字节**处截断。`max=72` 会放 72 个汉字（216 字节）进去，只有前 24 个进散列。
- **`email` 必须在 Register 和 Login 两侧都过 `normalizeEmail`**：只做一边比两边都不做更糟 —— 能注册进去，登录时找不到。
- **归属校验一律返回 `NotFound`，不返回 `Forbidden`**：`SessionService.mustOwn` / `TaskService.mustOwn` 把"不存在"和"不是你的"归成同一个错误。分开的话，403 就等于在说"这个 ID 真实存在"，接口变成 ID 探测器。**这个检查必须在 service，handler 里出现 `if x.UserID != userID` 就是层错了。**
- **Agent 回调不许写 `canceled`**：`dto.TaskCallbackReq` 的 `oneof` 只放行 `running/succeeded/failed`，`TaskService.HandleCallback` 的 `switch` 再挡一次。放开的话，一个被攻破的 Agent 就能把任务标成"用户取消"悄悄吞掉 —— 用户看到是自己取消的，不会去查。
- **取消是「先落库、再通知 Agent」**：反过来的话，Agent 可能在我们写库之前就回调 `succeeded`，于是把一个已完成的任务盖成 `canceled`。先写 `canceled`，迟到的回调撞上 `cas` 的 `from=[pending,running]` 自然落空。
- **`/internal` 是没有用户身份的路径**：`middleware.InternalAuth` 只能证明"调用方是我们自己的服务"，证明不了它代表谁。所以那个组下面只能有"按资源 ID 推进状态"的接口，**绝不能有拿入参当身份的接口**。密钥用 `subtle.ConstantTimeCompare` 比，漏配时全拒（500），不放行。

对应的测试删了就会出问题：`TestLogin_FailuresAreIndistinguishable`、`TestLogin_DisabledUser`、`TestUserResp_NoEmbeddedStruct`、`TestRegister_ValidationLeaksNothing`、`TestAuth_NilServiceFailsClosed`、`TestCancel_OtherUsersTaskLooksMissing`、`TestCancel_LateCallbackCannotRevive`、`TestHandleCallback_RejectsCanceled`、`TestCallback_RequiresSecret`、`TestCallback_NoSecretConfiguredFailsClosed`。

## 测试约定

- 需要 DB 的用例统一走 `withTx`：每个用例一个事务、结束回滚，天然隔离。**绕开 `withTx` 直接用包级 `testDB` 会在 `t.Parallel()` 下偶发失败。**
- `TestMain` 里 `hash.SetCost(bcrypt.MinCost)` 是**进程级**副作用，所有测试共享（否则每次登录 ~160ms）。
- `TestMain` 全包只能有一个，内部包和外部包各放一个也会报 `multiple definitions of TestMain`。拆多个测试文件时，共享的 `TestMain` / `testDB` / helper 集中放 `main_test.go`。
- **Postgres 25P02 坑**：一次约束冲突会让整个外层事务进入 aborted 状态，后续语句全失败。所以**期望失败的插入必须是该用例的最后一条语句**（见 `TestRegister_EmailIsCaseInsensitive`）。
- handler 测试用 `package handler_test` + 假 service，走完整 router round-trip，不连库。
- **`AutoMigrate` 必须包在 `migrateTestDB` 的 advisory 锁里**（`repository` 和 `service` 各有一份，锁常量要一致）。`go test ./...` 并行跑包，两个 `TestMain` 同时 `ADD COLUMN` 会有一个拿到 `42701 column already exists`，整包 `log.Fatal`。平时迁移是空操作看不出来，**只在加了新列之后的第一次跑挂**，第二次就好了 —— 很容易被当成偶发放过去。

## 任务状态机

```
pending ──► running ──┐
   │                  ├──► succeeded / failed   （Agent 回调）
   └──────────────────┘
   └──────────────────────► canceled            （用户主动取消）
```

终态（`succeeded` / `failed` / `canceled`）不再变。这条规则**只在 `repository/task.go` 的 `cas` 里兑现** —— 一条带 `status IN (...)` 的 UPDATE，判断和写入在同一条语句里，靠行锁仲裁并发。返回 `false` 表示没匹配到行（任务不存在，或已是终态），**这不是错误**。

状态字符串一律用 `model.TaskPending` 这组常量，别写字面量。`model.IsTerminal` 对未知状态返回 `false`（宁可让它还能被推动，也不要卡死）。

## 当前进度

已接线的路由：

| 方法 | 路径 | 鉴权 |
|---|---|---|
| GET | `/health` | 无 |
| POST | `/api/v1/auth/register` | 无 |
| POST | `/api/v1/auth/login` | 无 |
| POST | `/api/v1/sessions` | `middleware.Auth`（用户 JWT） |
| GET | `/api/v1/sessions` | 同上 |
| GET | `/api/v1/sessions/:id` | 同上 |
| PATCH | `/api/v1/sessions/:id` | 同上 |
| DELETE | `/api/v1/sessions/:id` | 同上 |
| POST | `/api/v1/sessions/:id/tasks` | 同上 |
| GET | `/api/v1/sessions/:id/tasks` | 同上 |
| GET | `/api/v1/tasks/:uid` | 同上 |
| POST | `/api/v1/tasks/:uid/cancel` | 同上 |
| POST | `/internal/tasks/:uid/callback` | `middleware.InternalAuth`（共享密钥，无用户身份） |
| GET | `/debug/panic` | 仅 debug 模式 |

**gin 的坑：同一层级上的路径参数必须同名。** `/tasks/:uid` 和 `/tasks/:id` 混用会在 `NewRouter` 时直接 panic —— 是启动崩，不是运行时报错。

写新接口时：`ulidParam` 那种纯格式校验放 handler，归属校验放 service（`mustOwn`）；分页参数走 `dto.PageQuery` + `bindQuery`，**`limit=-1` 必须在入口拦下**（GORM 的 `Limit(负数)` 是不写 LIMIT 子句，也就是全表）。

**列表响应统一用 `dto.ListResp[T]`**，`NewListResp` 里那句 `items = []T{}` 是必须的 —— nil 切片序列化成 `null`，客户端一个 `items.map` 就炸，而且只在"这个用户一条数据都没有"时炸，本地复现不出来。

**`PATCH /sessions/:id` 的 `title` 是 `*string` + `required`**：`{}` 和 `{"title":null}` 在 Go 这边都是 nil，分不出"没提这个字段"和"要求清空"，所以两个都拒。清空的写法是 `{"title":""}`。

`AGENT_CALLBACK_SECRET` **不是启动必填**（和 `JWT_SECRET` 不同）：缺了只是 `/internal` 那组接口一律 500，服务其余部分照常。但填了就必须 ≥32 字节，且不能和 `JWT_SECRET` 相同。

`AGENT_ADDR`（默认 `localhost:50051`）和 `AGENT_TASK_TIMEOUT`（默认 `30m`）都有默认值，但都会过 `Validate`：空地址、非正的超时一律启动失败。

`agentclient` 包（目录 `internal/agentclient`）是 Agent 那侧 gRPC 服务的客户端，**全仓库唯一允许 import `gen/agent/v1` 的地方，没有例外**。

进出这个包的一律是本地类型：`StartTaskReq` / `CancelTaskReq` / `Event`（含 `EventType` 那组字符串常量和 `IsTerminal`）。proto 类型能走到的最远处是 `toEvent` 和 `CancelTask` 里那次翻译。**别把 wire 类型顶到业务层** —— 对面改一次字段名、加一个枚举值，handler、service 和两边的测试就会一起红，而那本该是实现细节。

这条规矩由 `internal/agentclient/imports_test.go` 的 `TestOnlyAgentClientImportsProto` 守着：它遍历整个仓库解析 import。配套的 `TestProtoPkgIsActuallyImportedHere` 防的是常量过期后前者一直绿着却什么也没查。要加东西就在 `agentclient` 里加一个本地类型，照 `Event` / `CancelTaskReq` 的样子来。

未知的事件类型翻成 `EventUnknown` 后**照样往下发**（payload 还在），只是多记一条 Warn —— 直接丢弃的话，「对面升级了、我们没跟上」这件事没人会发现。

`var _ Client = (*GRPCClient)(nil)` 那行别删 —— 少了它接口和实现漂移了编译器不会吭声。

`StartTask` 起的消费协程要活过发起它的那次 HTTP 请求，所以 ctx 必须先 `context.WithoutCancel` 再配自己的超时（`AGENT_TASK_TIMEOUT`，默认 30 分钟）。直接从请求 ctx 派生的话，handler 一 return 整条事件流就断了。

### 任务状态是谁写的

**Agent 是同步的**：一次 `RunTask` 调用从头跑到尾，事件流就是任务结果的权威来源。`service.TaskEventHandler` 把事件翻成状态：

| 事件 | 动作 |
|---|---|
| `started` | → `running` |
| `finished` | → `succeeded` |
| `error` | → `failed`，payload 当失败原因 |
| `thinking` / `tool_call` / `tool_result` / `unknown` | 只记日志，**不写库**（是进度不是状态） |
| 流中断（`OnStreamError`） | → `failed` |

**流断了必须给任务收尾**，不然它永远停在 `running`。这不会误伤已完成的任务：事件在同一个协程里按序处理，走到 `OnStreamError` 时 `finished` 要么早处理完了（`cas` 落空），要么根本没来过。

所有状态写入都过 `TaskService.HandleCallback`，**别绕开它直接调仓库层** —— `canceled` 白名单、`cas`、迟到回调的日志三件事都在那儿。事件里的失败原因要用 `truncate` 截到 `maxEventErrMsg`（和 `dto.TaskCallbackReq` 的 `max=2000` 对齐）：gRPC 这条路没有 binding tag 挡着，`tasks.error_msg` 又是 text，不截的话对面一条事件就能塞几 MB 进来，而且会跟着任务详情回显给用户。同理，流中断写进 `error_msg` 的文案不能带 gRPC 原文（里面有内网地址和端口）。

`/internal` 回调**保留但同步 Agent 用不上**，Python 侧不必实现它。两条路都过 `cas`，不会打架。

### main.go 的构造环

```
TaskService ──要──> agentclient.Client ──要──> EventHandler ──要──> TaskService
```

拆点是「事件处理器只用 `HandleCallback`，而那条路径根本不碰 Agent 客户端」：先拿一个不带客户端的 `TaskService` 把处理器建起来，客户端有了之后再给 HTTP 那侧建一个带客户端的。两个都只是仓库之上的无状态包装，不共享东西。别改成 setter —— 那会让「客户端什么时候可用」变成运行时才知道的事。

已知未做：登录没有限流（`handler/auth.go` 有 TODO，bcrypt 的耗时拦不住撞库）；JWT 没有吊销机制（`jti` 已签进去，是将来做黑名单的钩子）；`Login` 里"用户不存在"分支比"口令错"少跑一次 bcrypt，存在约 160ms 的时间侧信道（代码注释里写明了）；`/internal` 目前和业务接口共用一个监听端口，生产上应该单独端口或由网关按前缀挡在公网外。
