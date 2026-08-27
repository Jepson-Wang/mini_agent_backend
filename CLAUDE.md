# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

代码注释、错误文案、提交信息一律用中文，保持与现有代码一致。

## 常用命令

```bash
make run        # go run ./cmd/server
make build      # 产物在 bin/server
make test       # 不带 DB(自带 ALLOW_SKIP_DB=1),需要数据库的用例会跳过
make test-db    # 带 DB,真跑仓库层和 service 层用例
make test-ci    # 不给逃生口:没有 TEST_DSN 直接红
make vet        # go vet ./...
make fmt        # gofmt -l -w .
```

**裸 `go test ./...` 在没有 `TEST_DSN` 时会让 `repository` 和 `service` 两个包直接失败**，这是刻意的。原因是 `go test` 不带 `-v` 时既不打印 skip、也不打印通过的包的任何输出 —— "52 个用例全被跳过"和"全部通过"输出完全一样，都是一行 `ok`，而被跳掉的里面有下面那份安全不变量清单里的五条。想在没有库的情况下跑，就显式说出来：`make test`（它替你设 `ALLOW_SKIP_DB=1`）。

跑单个测试（仓库层和 service 层必须给 `TEST_DSN`）：

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
- **gRPC 的错误原文绝不出 `agentclient`**：`Unavailable` 的 `status.Message()` 里是 `dial tcp 10.0.0.7:50051: ...`，而这个 error 经 `OnStreamError` 一路走到 `tasks.error_msg`，那一列**会跟着任务详情回显给用户**。`translateGRPCError` 只保留 op 和状态码名。同理 `/health` 探测失败的原因只进日志，不进响应体（那个接口通常还不鉴权）。
- **回收器只写 `failed`，不写 `canceled`**：和上面 Agent 回调那条同一个理由 —— 用户看到是自己取消的，不会去查。
- **`/internal` 是没有用户身份的路径**：`middleware.InternalAuth` 只能证明"调用方是我们自己的服务"，证明不了它代表谁。所以那个组下面只能有"按资源 ID 推进状态"的接口，**绝不能有拿入参当身份的接口**。密钥用 `subtle.ConstantTimeCompare` 比，漏配时全拒（500），不放行。

- **配额链路断在任何一环,表现都是 500**:`TaskQuota` → `ErrTaskQuotaExceeded` → `errRules` → 42900。掉进"没登记 → 50000"的默认分支时,客户端会把它当成"服务坏了"去重试,恰好是超配额时最不该做的事。`TestFailFromError_QuotaExceeded` 盯着这条。

对应的测试删了就会出问题（**打 ★ 的五条没有 `TEST_DSN` 就不会跑**，所以别把 `ALLOW_SKIP_DB=1` 当默认姿势）：★`TestLogin_FailuresAreIndistinguishable`、★`TestLogin_DisabledUser`、`TestUserResp_NoEmbeddedStruct`、`TestRegister_ValidationLeaksNothing`、`TestAuth_NilServiceFailsClosed`、★`TestCancel_OtherUsersTaskLooksMissing`、★`TestCancel_LateCallbackCannotRevive`、★`TestHandleCallback_RejectsCanceled`、`TestCallback_RequiresSecret`、`TestCallback_NoSecretConfiguredFailsClosed`、`TestTranslateGRPCError_DropsRawMessage`、`TestHealth_ProbeErrorNotEchoed`、`TestReconcile_NeverWritesCanceled`、`TestFailFromError_QuotaExceeded`、`TestFailFromError_UnknownErrorLeaksNothing`、`TestLoad_WireSecretsMustBeASCII`、`TestOnEvent_ErrorCarriesOnlyErrorMsg`。

## 任务的最终答复

`tasks.output` 存 Agent 跑完给出的正文,只有 `succeeded` 的任务才有。

在此之前这个值是**被丢掉的**:`finished` 事件的 payload 里带着它,而
`TaskEventHandler` 只取了状态。结果是"任务成功了"能查到、"它到底答了什么"查不到 ——
任何对话界面都没法做。现在 `outputFromPayload` 从 payload 里解 `final_response`
(那个 JSON 由 `grpcserver/server.py` 的 `json.dumps(out)` 拼出来),
经 `CallbackResult.Output` 落库,再由 `dto.TaskResp.Output` 出去。

三条约定:

- **解不出来不让事件失败**。状态是"成功"这件事比正文更要紧,一起丢掉的话任务会
  永远停在 `running`,那比"答案没显示出来"糟糕得多。解析失败记一条 Warn。
- **不拿原始 payload 当答案兜底**。那样用户会看到一坨 JSON。
- **`output` 和 `error_msg` 互斥**。成功的任务不带错误信息,失败的任务不带正文。
  混了的话用户会把报错当答案读 —— 这也是这两个值合进 `CallbackResult` 而不是
  继续往参数表上加 `*string` 的原因:两个相邻的可空字符串参数是典型的
  "传反了编译器也不吭声"的形状。

`maxEventOutput` 是 64KiB,比 `error_msg` 的 2000 宽得多(这是正文,截短就没法用了),
但仍然必须有 —— `text` 列在数据库那边没有上限,而这一列会原样回显给用户。
HTTP 回调那条路由 `dto.TaskCallbackReq.Output` 的 `max` 挡着,两条汇报通道保持等价。

对应的测试:`TestOnEvent_FinishedCarriesFinalResponse`、
`TestOnEvent_FinishedWithBadPayloadStillSucceeds`、`TestOnEvent_ErrorCarriesOnlyErrorMsg`。

## 任务配额

`service.TaskQuota` 限制单个用户在一个滑动窗口内能建多少任务(默认 200 / 24h,`TASK_QUOTA_MAX=0` 关掉)。**默认是开着的**:默认关掉的话这个开关就成了"知道的人才会去打开"的摆设,而不知道的人正是最需要它的人 —— 每个任务都会驱动一整轮 LLM 循环,不限额等于注册一个号就拿到一张无限额度的卡。Agent 侧的 `IterationBudget` 只挡住单个任务失控,挡不住"一秒建一千个任务"。

三个约定:

- **只在建任务时查**(`CreateWithFirstTask` / `CreateTaskWithSessionID`),取消和查询不查 —— 花钱的是建。
- **归属校验之后再查配额**。反过来的话,拿别人的 `session_id` 撞一下,就能从"配额超限"和"会话不存在"的差别里判断出这个 ID 真实存在。
- **数不出来(库报错)就放行**。配额是软限制,而这条路径上的库故障会让紧接着的 `INSERT` 自己失败;在这儿拦下来只会把"建任务失败"变成一个指向错误方向的"配额超限"。

它不是精确锁:两个并发请求可能都读到"还差一个",于是超一个。要精确就得把计数和插入放进同一个事务再锁用户行,为一个防滥用的粗粒度阈值不值得。

`TaskQuota` 收的是 `taskCounter` 接口而不是 `*repository.Repository`(同 `reconcile.go` 的 `staleLister`),所以 `quota_test.go` **不需要 `TEST_DSN`**。这一点是有意的:把一条防滥用规则押在"没有库就会被跳过"的用例上,等于没测。

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

**两把要上链路的密钥(`AGENT_CALLBACK_SECRET` / `AGENT_GRPC_TOKEN`)必须是可见 ASCII**(`0x21..0x7E`),由 `config.checkWireSecret` 在启动时挡下。不挡的话失败形态极难查:长度这关轻松通过、服务正常启动、日志一切正常,然后**每一次调用**都在传输层被打回来 —— gRPC 那边是 `Illegal header value` + `INTERNAL`,而错误里没有任何东西指向密钥格式。不含空格是因为空格在头部里合法但会被两端的修剪吃掉。`JWT_SECRET` 不受这条约束:它只用来签名,从不出现在任何头部里。限定 ASCII 顺带也让两侧对"多长算够"有了同一个答案 —— Python 的 `len()` 数字符,Go 的数字节,只有全 ASCII 时两者才恒等。

`AGENT_GRPC_TOKEN` 是**反方向**的那把密钥：`AGENT_CALLBACK_SECRET` 管 Agent 打过来，它管本服务打过去（挂在 gRPC metadata 的 `x-agent-token` 里，Agent 那侧对应 `MINI_AGENT_GRPC_TOKEN`，两边必须一致）。同样不是启动必填 —— Agent 绑在回环地址上时它自己就不鉴权。但填了就必须 ≥32 字节，且**不能和 `JWT_SECRET` 或 `AGENT_CALLBACK_SECRET` 相同**：三把钥匙的暴露面完全不同（JWT 发给每个用户，另外两把只在服务之间走），共用一把等于任何一条路径泄露就全丢。凭证挂在 `PerRPCCredentials` 上而不是自己写 interceptor，是因为后者要给 unary 和 stream 各写一个，漏掉 stream 的话最危险的 `RunTask` 恰好没带凭证，而那要到部署时才暴露。

`AGENT_ADDR`（默认 `localhost:50051`）和 `AGENT_TASK_TIMEOUT`（默认 `30m`）都有默认值，但都会过 `Validate`：空地址、非正的超时一律启动失败。

熔断和回收器的配置项（都有默认值，都过 `Validate`）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `AGENT_BREAKER_THRESHOLD` | 5 | 连续多少次「下游有病」就熔断。**0 = 关掉** |
| `AGENT_BREAKER_COOLDOWN` | 30s | 熔断后多久放一个探针。阈值 >0 时必须 >0 |
| `RECONCILE_INTERVAL` | 1m | 多久扫一次卡住的任务。**≤0 = 关掉回收器** |
| `RECONCILE_STALE_AFTER` | 15m | 多久没动静算卡住。必须 ≥ `RECONCILE_INTERVAL` |
| `RECONCILE_BATCH` | 100 | 一轮最多收几个 |

`stale_after < interval` 会被 `Validate` 拒掉：那几乎一定是两个值写反了，放过去的话刚建出来还没跑的任务下一轮就被判死，而且是**所有**任务。

`agentclient` 包（目录 `internal/agentclient`）是 Agent 那侧 gRPC 服务的客户端，**全仓库唯一允许 import `gen/agent/v1` 的地方，没有例外**。

进出这个包的一律是本地类型：`StartTaskReq` / `CancelTaskReq` / `Event`（含 `EventType` 那组字符串常量和 `IsTerminal`）。proto 类型能走到的最远处是 `toEvent` 和 `CancelTask` 里那次翻译。**别把 wire 类型顶到业务层** —— 对面改一次字段名、加一个枚举值，handler、service 和两边的测试就会一起红，而那本该是实现细节。

这条规矩由 `internal/agentclient/imports_test.go` 的 `TestOnlyAgentClientImportsProto` 守着：它遍历整个仓库解析 import。配套的 `TestProtoPkgIsActuallyImportedHere` 防的是常量过期后前者一直绿着却什么也没查。要加东西就在 `agentclient` 里加一个本地类型，照 `Event` / `CancelTaskReq` 的样子来。

未知的事件类型翻成 `EventUnknown` 后**照样往下发**（payload 还在），只是多记一条 Warn —— 直接丢弃的话，「对面升级了、我们没跟上」这件事没人会发现。

`var _ Client = (*GRPCClient)(nil)` 那行别删 —— 少了它接口和实现漂移了编译器不会吭声。

`StartTask` 起的消费协程要活过发起它的那次 HTTP 请求，所以 ctx 必须先 `context.WithoutCancel` 再配自己的超时（`AGENT_TASK_TIMEOUT`，默认 30 分钟）。直接从请求 ctx 派生的话，handler 一 return 整条事件流就断了。

#### 错误翻译

**gRPC 的 `error` 一步都不许走出 `agentclient`。** `translateGRPCError`（`errors.go`）按状态码归到四个本地哨兵：`ErrUnavailable`（Unavailable / ResourceExhausted）、`ErrTimeout`（DeadlineExceeded）、`ErrBadRequest`（InvalidArgument / NotFound / FailedPrecondition / Unimplemented …）、`ErrRemoteInternal`（Internal / Unknown / DataLoss / 其余）。

翻译结果里**只留 op 和状态码名，绝不带 `status.Message()`**。Unavailable 的 message 长这样：`dial tcp 10.0.0.7:50051: connect: connection refused` —— 里面是内网地址，而这个 error 会经 `OnStreamError` 一路走到 `tasks.error_msg`，那一列是跟着任务详情**回显给用户**的。`TestTranslateGRPCError_DropsRawMessage` 盯着这条。丢掉原文不影响排查：状态码说明了故障类别，Agent 地址启动日志里有一份。

`service.translateAgentErr` 再把哨兵翻成 `model.ErrAgentUnavailable` / `ErrAgentTimeout`，`response.errRules` 映射到 `50300` / `50400`。**别让 `response` 直接 import `agentclient`** —— 那会把响应外壳挂到 gRPC 基础设施上。

注意：这两个业务码目前**没有 HTTP 生产者**。派发失败走的是「标 `failed`、创建照样 200」（见下面的任务派发），取消通知是 best-effort，所以 Agent 挂掉不会有任何接口回 503。规则先登记好，等哪天有接口要把 Agent 错误直接抛给客户端时就是对的。

#### 熔断

`internal/pkg/breaker` 是个不依赖 gRPC 的三态熔断器（closed → open → half_open）。`GRPCClient` 在 `StartTask` / `CancelTask` 前 `Allow()`，拿到结果 `Record()`。

**什么错误算「下游有病」由 `countsAsUnhealthy` 决定，只有 `ErrUnavailable` / `ErrTimeout` / `ErrRemoteInternal` 算。** 把 `InvalidArgument` 这类算进去的话，前端一阵坏请求就能把一个完全健康的 Agent 关在门外。

**`StartTask` 那条路的 `Record` 在 `consume` 里，不在 `StartTask` 里。** `RunTask` 是 server-streaming，`c.cli.RunTask(...)` 不等对面回话就返回 —— Agent 进程没了它照样返回 nil，错误要到第一次 `Recv()` 才浮出来。按它的返回值记账的话，熔断器永远认为下游是好的，装了等于没装，而且没有任何迹象。`consume` 里那个 `recorded` 标志保证一次 `Allow` 只对一次 `Record`：多记会搞乱半开期计数，少记会让探针名额漏掉、熔断器再也回不到 closed。`TestConsume_StreamFailureOpensBreaker` 和 `TestConsume_RecordsExactlyOnce` 盯着这两条。

#### Health

`Client.Health` 探一次 Agent，**刻意不走熔断器**：不 `Allow`（熔断打开时更该让探活发出去，否则再也看不到「对面回来了」；半开期那唯一的探针名额也不该被监控轮询吃掉），不 `Record`（`/health` 被按秒轮询，让它驱动熔断器等于把状态机交给采集频率）。熔断状态由 `BreakerState()` 单独报。

`/health` 因此有两个独立字段：`agent`（现在探得通吗）和 `breaker`（业务流量放不放行）。`agent=up` + `breaker=open` 是合理组合，意思是「对面恢复了，再过一个冷却期业务也会恢复」。

**Agent 挂了 `/health` 回 200 + `status=degraded`，不回 500。** 数据库挂了才 500。差别在失败半径：数据库没了几乎每个接口都废，该让编排系统摘掉这个实例；Agent 没了只影响「新建任务」，回 500 会让 k8s 把所有实例一起重启，把局部故障放大成整站不可用——而重启并不能让 Agent 回来。`TestHealth_AgentDownIsNot500` 盯着这条。

### 任务派发

`SessionService` 建完任务立刻调 `dispatch` 把它发给 Agent（`StartTask`）。两个入口都要发：`CreateWithFirstTask` 和 `CreateTaskWithSessionID`。

**`dispatch` 必须在事务提交之后调，不能塞进 `Transaction` 的闭包里。** Agent 是同步的，可能几毫秒内就把 `started` 事件推回来，而那条事件按 `task_uid` 去 UPDATE —— 事务没提交的话那一行对别的连接不可见，UPDATE 匹配不到，`cas` 返回 `false`（在它眼里只是「迟到的回调」，不是错误），任务就永远停在 `pending`。这种 bug 只在 Agent 快、库慢时出现，本地几乎复现不出来。`TestCreate_DispatchesAfterCommit` 盯着它——那条用例**不能**用 `withTx`（要验的正是跨连接可见性，而 `withTx` 的事务永不提交，断言会恒为假），所以它直连 `testDB` 并自己 `t.Cleanup`。

**派发失败就地标 `failed`，不留在 `pending`**：同步 Agent 只有这一次交接机会，交不出去就再没人推动它，留在 `pending` 就是前端一个永远转圈、还没人能解释的任务。写进 `error_msg` 的原因不带 gRPC 原文（里面是内网地址和端口）。派发失败**不让创建失败**——任务确实建出来了。

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

### 心跳与回收器

上面那张表有个漏洞：**它只覆盖「流还活着」的情况**。进程重启、Pod 被驱逐、Agent 被 OOM kill 时，`OnStreamError` 根本没机会跑，任务就永远停在 `pending` / `running`。`service.Reconciler`（`reconcile.go`）是这条链路的兜底。

一轮回收 = `TaskRepo.ListStale(before, batch)` 捞出「中间态 + `updated_at < now - stale_after`」的任务（这是 `idx_tasks_status_updated` 的第一个消费者，`status` 等值在前、`updated_at` 范围在后，正好是这个索引的形状），逐个走 `HandleCallback` 标 `failed`。

几条不能改的：

- **只写 `failed`，绝不写 `canceled`**。写 `canceled` 用户会以为是自己取消的，一个该被排查的故障就这么藏起来了。
- **走 `HandleCallback`，不直接调仓库层**。`cas` 在那儿——一个在我们捞出来之后才完成的任务不会被盖成 failed。
- **单个失败不中断整轮**（`continue`，不 `return`），一条坏记录不该让后面几十个都收不掉。
- **只收尾，不重跑**。跑了一半的 LLM 任务重跑可能重复扣费、重复调外部工具。要不要重来是用户的决定。
- `ListStale` 的 `limit <= 0` 直接返回空，不是全表 —— GORM 的 `Limit(0)` 是不写 LIMIT 子句，而这跑在后台，真捞了全表也没人会看见。

配套的心跳解决「长任务被误杀」：`TaskEventHandler.OnEvent` 收到进度事件（`thinking` / `tool_call` / `tool_result` / `unknown`）时调 `TaskService.Heartbeat` → `TaskRepo.Touch`，刷一次 `updated_at`。所以 **`stale_after` 要对比的是「Agent 两次事件之间的最长间隔」，不是任务总时长**，一个跑两小时但一直在推进度的任务不会被收掉。改状态的事件不用额外心跳 —— `cas` 那条 UPDATE 本来就刷了 `updated_at`。

**`Touch` 的节流在 SQL 的 `WHERE updated_at < ?` 里，不在内存里。** 内存里记「上次 touch 时间」要按 `task_uid` 存一张表还得清理，而且多副本部署时每个副本各记各的、节流直接失效。`Touch` 只续 `running`：`pending` 意味着还没交接成功，那正是回收器该来看一眼的状态，给它续命等于把交接失败永远藏起来。

### main.go 的构造环

```
TaskService ──要──> agentclient.Client ──要──> EventHandler ──要──> TaskService
```

拆点是「事件处理器只用 `HandleCallback` / `Heartbeat`，而那两条路径根本不碰 Agent 客户端」：先拿一个不带客户端的 `TaskService` 把处理器建起来，客户端有了之后再给 HTTP 那侧建一个带客户端的。两个都只是仓库之上的无状态包装，不共享东西。别改成 setter —— 那会让「客户端什么时候可用」变成运行时才知道的事。

回收器复用那个**不带客户端**的 `statusWriter`：它只写状态，从不联系 Agent —— 一个连不上的 Agent 正是它要处理的情况。它跑在自己的 `context.WithCancel` 上（不是请求 ctx），退出时先 `stopReconcile()` 再 `srv.Shutdown()`：它是纯后台循环，没有在途请求要照顾，让它在关闭过程中再扫一轮只会拖长关闭时间。

已知未做：Agent 挂掉时**没有任何 HTTP 接口回 503**（`50300` / `50400` 的映射已经登记好，但派发失败走「标 failed + 200」、取消通知是 best-effort，两条路都不往上抛）；回收器**多副本会重复扫**（每个副本各扫各的，靠 `cas` 兜底不会写错，只是白烧几次查询，要治得加 advisory lock 或选主）；登录没有限流（`handler/auth.go` 有 TODO，bcrypt 的耗时拦不住撞库）；JWT 没有吊销机制（`jti` 已签进去，是将来做黑名单的钩子）；`Login` 里"用户不存在"分支比"口令错"少跑一次 bcrypt，存在约 160ms 的时间侧信道（代码注释里写明了）；`/internal` 目前和业务接口共用一个监听端口，生产上应该单独端口或由网关按前缀挡在公网外。
