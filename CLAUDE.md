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

限流要 Redis。本地起一个：

```bash
docker run -d -p 6379:6379 redis:7-alpine
REDIS_ADDR=localhost:6379 make run
```

**不起也能跑** —— `REDIS_ADDR` 为空就不启用限流（启动时会有一条 Warn）。`make test` 里的限流用例走 miniredis，**不需要任何外部服务**，这一点和 `quota_test.go` 同一个取向：把一条防滥用规则押在"没有服务就会被跳过"的用例上，等于没测。

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

- **「执行器满了」不是「执行器坏了」**:Agent 侧的并发护栏超限时用 gRPC 的 `RESOURCE_EXHAUSTED` 拒绝,backend 把它归到**独立的** `agentclient.ErrAgentBusy`(不并进 `ErrUnavailable`),**不得**算进 `countsAsUnhealthy`。熔断器数的是「持续出错」,不是「接不了活」—— 算进去的话,一次流量高峰(5 次拒绝就够 `breaker_threshold`)就能把一个**完全健康**的 Agent 整个隔离掉;而 Agent 恰恰是在满载的时候最忙,把它摘掉只会让剩下的任务更堵,于是更多请求被拒 —— 自己把自己推向更满。用户看到的文案也必须分开:「并发任务数已达上限」而不是「连接中断」。`TestCountsAsUnhealthy_AgentBusyIsNotUnhealthy`、`TestTranslateGRPCError_ResourceExhaustedIsBusy`、`TestOnStreamError_AgentBusyMessage` 盯着这条。

对应的测试删了就会出问题（**打 ★ 的五条没有 `TEST_DSN` 就不会跑**，所以别把 `ALLOW_SKIP_DB=1` 当默认姿势）：★`TestLogin_FailuresAreIndistinguishable`、★`TestLogin_DisabledUser`、`TestUserResp_NoEmbeddedStruct`、`TestRegister_ValidationLeaksNothing`、`TestAuth_NilServiceFailsClosed`、★`TestCancel_OtherUsersTaskLooksMissing`、★`TestCancel_LateCallbackCannotRevive`、★`TestHandleCallback_RejectsCanceled`、`TestCallback_RequiresSecret`、`TestCallback_NoSecretConfiguredFailsClosed`、`TestTranslateGRPCError_DropsRawMessage`、`TestHealth_ProbeErrorNotEchoed`、`TestReconcile_NeverWritesCanceled`、`TestFailFromError_QuotaExceeded`、`TestFailFromError_UnknownErrorLeaksNothing`、`TestLoad_WireSecretsMustBeASCII`、`TestOnEvent_ErrorCarriesOnlyErrorMsg`、`TestAllow_NarrowBucketRejectDoesNotConsumeGlobal`、`TestAllow_FixedWindowBoundaryBurstIsRejected`、`TestAllow_SameMillisecondAttemptsAllCount`、`TestAllow_ZeroLimitBucketIsSkipped`、`TestAllow_RetryAfterIsWindowNotRemaining`、`TestAuthRateLimit_OversizedBodyIsRejected`、`TestAuthRateLimit_UsesRemoteAddrNotXFF`、`TestAuthRateLimit_RejectionDoesNotRevealDimension`、`TestAuthRateLimit_EmailKeyIsHMACNotPlaintext`、`TestChatRateLimit_MissingUserIDFailsClosed`、`TestLogin_RateLimitedResponseIsIdenticalForUnknownEmail`、`TestNew_SetsExplicitTimeouts`、`TestHealth_RedisProbeErrorNotEchoed`、`TestRevokeToken_TTLIncludesVerifyLeeway`、`TestRevokeUser_TokenFromNextSecondSurvives`、`TestRevokeUser_TokenFromSameSecondIsRevoked`、`TestRevokeUser_StoredNotBeforeIsWholeSecondMillis`、`TestAuth_NilClaimsFailsClosed`、`TestRun_NilLockStillScans`、`TestRelease_OnlyOwnLock`、`TestTranslateGRPCError_ResourceExhaustedIsBusy`、`TestCountsAsUnhealthy_AgentBusyIsNotUnhealthy`、`TestRetryable_AgentBusyIsNotRetried`、`TestTranslateAgentErr_AgentBusy`、`TestOnStreamError_AgentBusyMessage`、`TestFailFromError_AgentBusy`。

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

## JWT 吊销

以前登出是**纯前端行为** —— 前端删掉本地 token,那个 token 在服务端仍然有效,直到 `AccessTTL`(默认 2 小时)过期。两个真实后果:用户改密码后攻击者手里的 token 还能用 2 小时;**管理员禁用一个用户之后,他现有的 token 还能用 2 小时**。第二条是实打实的安全洞,而且藏得深 —— 禁用检查只在 `Login` 里,`middleware.Auth` 一次库都不查。

`internal/pkg/revoke` 补上了这一层 —— 但**只补上了第一条**。第二条(禁用账号)
到今天仍然是开着的,因为 `RevokeUser` 没有任何生产调用方,理由见下面那张表下面的 ★。

**`jti` 现在是签发必需项,生成失败要让 `Issue` 失败。** 之前它是「失败就用空串」,因为只用于日志。有了黑名单之后那个理由不成立了:**没有 jti 的 token 吊销不掉** —— 你没有任何东西可以拿去写进黑名单,等于发了一张注销不掉的卡。`Verify` 侧同样拒绝无 jti 的 token,这会让升级前签出的老令牌全部失效,**是有意的部署决策**,发版说明里要写「所有用户需重新登录」。

**两种粒度不能互相替代。**

| 场景 | 吊销什么 | 用哪种 |
|---|---|---|
| 登出 | 只有当前这一个令牌 | 单 token 黑名单(按 jti) | **已接线** |
| 改密码 / 禁用账号 | 该用户全部令牌,包括你不知道存在的 | 用户级 `not_before`(按 user_id) | **只有能力,没接线** |

★ **第二行目前只是"能力齐备",不是"洞补上了"。** `Revoker.RevokeUser` 写好了也测了,
但**全仓库没有任何生产代码调它**(只有 `revoke_test.go`)—— 既没有禁用用户的接口,
也没有改密码的接口。所以「管理员禁用一个用户之后他的 token 还能用 2 小时」这个洞
**和做这一层之前完全一样,仍然开着**。别把这一节读成"禁用已经生效了"。
要补齐得先有那两个接口,然后在它们里面调 `RevokeUser`。

黑名单单独用不够:禁用用户时你手里没有他的 jti —— jti 只在他自己的 token 里,没法枚举「这个用户有哪些令牌在外面」。用户级单独用也不够:它是全有或全无,用它做登出会把该用户所有设备都踢下线。

**黑名单的 TTL 必须是 `exp - now + token.Leeway`,不是 `exp - now`。** 短了吊销静默失效(用户以为登出了,其实没有),长了纯泄内存。那个 `Leeway` 不能省:校验侧带着 `jwt.WithLeeway(token.Leeway)`(30s),一个 token 到 `exp + 30s` 之前都还验得过;TTL 只算到 `exp` 的话,中间那 30 秒里 token 有效、黑名单却已经被 Redis 回收 —— 临近过期时登出完全无效,而且没有任何迹象。同理 `ttl <= 0` 那个短路也要带余量,所以"在 `exp` 之后几秒登出"仍然会写黑名单,那恰恰是最该写的时刻。

**`token.Leeway` 导出是刻意的**:让 `revoke` 从那里取、由 `main.go` 显式传进 `revoke.New`,这个耦合就是看得见的。在 `revoke` 里另抄一个 30s 常量的话,`token` 那边一改这边就静默失效 —— 那正是上面这个 bug 的同一个形态。

这仍然是 Redis 最漂亮的用法:**key 的生命周期和业务对象的生命周期对齐,垃圾回收交给 Redis,不需要任何清理任务。** 换成数据库表就得自己写定时任务删过期行,那套问题这个项目已经在 `Reconciler` 上踩过一遍了。`TestRevokeToken_TTLIncludesVerifyLeeway` 和 `TestRevokeToken_SurvivesDuringLeeway` 盯着这条。

**用户级比较的方向不能反**:`token.iat < not_before` 才算被吊销。写反的话用户改完密码之后新登录的令牌也立刻失效,表现是「登录成功但马上又被踢」的死循环,单用户手工测试时很难第一时间联想到吊销逻辑。`TestRevokeUser_DoesNotAffectNewerTokens` 盯着这条。

**`not_before` 必须向上取整到整秒,而且存储格式保持毫秒。** 这是个容易被漏掉的精度坑:JWT 的 `iat` 只有**秒**精度(golang-jwt 的 `NewNumericDate` 做 `t.Truncate(TimePrecision)`,默认 1 秒),毫秒位恒为 0;而 `not_before` 原来存真毫秒。于是吊销发生在 `12:00:05.200` 时 `nbf = ...5200`,同一秒内重新登录拿到的新令牌 `iat` 被截断成 `...5000` —— `5000 < 5200`,刚签出来的票被判吊销,表现就是上一条警告的那个死循环,只是成因是精度不是方向。

取整方向是**安全决策**,这个精度下没有两全:向下取整的话那一秒内签发的票 `iat` 恰好**等于** `nbf`,而判据是 `<`,于是它永远不算被吊销 —— 代价不是多活一秒,是**多活它自己完整的 `AccessTTL`(2 小时)**。向上取整只是让那一秒内的重新登录白跑一趟。按后果的不对称选向上。所以这个修法不是消除"刚签出来就被判吊销",而是把它从"毫秒位决定、看起来随机"收敛成"同一秒内必然、下一秒必然恢复"。

**格式不许改成秒**:滚动发布期间 Redis 里还躺着旧的毫秒值,按秒解析会得到一个巨大的数字,瞬间把所有人判成已吊销 —— 而那些 key 的 TTL 是 2 小时,窗口足够长。`TestRevokeUser_TokenFromNextSecondSurvives`、`TestRevokeUser_TokenFromSameSecondIsRevoked`、`TestRevokeUser_StoredNotBeforeIsWholeSecondMillis` 盯着这三条。

**`middleware.Auth` 拿到空声明要 fail closed 500。** `verifyClaims` 对一个 `ClaimsVerifier` 返回了 nil 声明且无 error 时会原样返回 `(nil, nil)` —— 那是接口契约允许的返回值(查不到就返回 nil 而忘了带 error 的实现很常见)。不挡的话紧接着的 `c.Set(userIDKey, claims.UserID)` 会**让每个鉴权请求 panic**,被 `Recovery` 兜成 500:看起来像服务故障,实际是接线 bug,排查方向一开始就走偏。回 401 也不行 —— 那是把「代码接错了」伪装成「用户的令牌不对」。`TestAuth_NilClaimsFailsClosed` 盯着这条。

**吊销检查 fail open,但 `Verifier == nil` 仍然 fail closed。** 判据是**「前一道防线还在不在」**:`Verifier` 是身份校验本身,没了就是裸奔(fail closed);吊销是在一个**签名已经验过**的身份上再加一层,它失灵不代表身份可疑(fail open)。两个方向相反,而且必须相反。

**被吊销时的文案和其它失败完全一样**（「登录状态无效或已过期」）。单独说「已吊销」会泄露信息:「这个令牌是被登出的还是被禁用的」就成了一条可观测的信号,和账号枚举是同一类问题。

**`pkg/token` 不许 import `pkg/revoke`。** 组合发生在 `middleware.Auth`。反过来的话,一个能脱离一切依赖单测的签名工具包会变成要 Redis 才能测的包。`middleware.NewClaimsAdapter` 是那层薄适配器 —— 它让 `pkg/token.Manager` 能返回完整声明(jti 和 iat),方向是单向的 `middleware → token`,没有环。

已知缺口:**没有 refresh token**,所以 `AccessTTL` 只能保持在 2h 这个体验可接受的值上,Redis 挂掉时吊销真空期的上界就是 2h。要缩短必须先做 refresh token(存储、轮转、重用检测、撤销链),那是独立的一块 —— 半做一个比不做更糟。

对应的测试:`TestVerify_RejectsTokenWithoutJTI`、`TestRevokeToken_TTLIncludesVerifyLeeway`、`TestRevokeToken_SurvivesDuringLeeway`、`TestRevokeUser_DoesNotAffectNewerTokens`、`TestRevokeUser_TokenFromNextSecondSurvives`、`TestRevokeUser_TokenFromSameSecondIsRevoked`、`TestRevokeUser_StoredNotBeforeIsWholeSecondMillis`、`TestAuth_NilClaimsFailsClosed`、`TestAuth_RevokedTokenRejected`、`TestAuth_RevokedMessageIsGeneric`、`TestAuth_RevokerErrorAllows`、`TestLogout_RevokesCurrentToken`。

## 限流

登录注册和建任务三条路都挂了基于 Redis 的滑动窗口限流（`internal/pkg/ratelimit`）。
**`REDIS_ADDR` 为空 = 这一层不启用**，服务其余部分照常跑，启动时会打一条 Warn。

### 三种控制回答三个不同的问题

| 控制 | 回答的问题 | 在哪 |
|---|---|---|
| `TaskQuota` | **总共**能建多少（200 / 24h） | `service/quota.go` |
| 限流 | **多久来一个** | `middleware/ratelimit.go` |
| 并发信号量 | **同时几个在跑** | **backend 侧还没做**（Agent 侧已有第一层） |

**这三样不能互相替代。** 所以「限流能不能保护 Agent」的诚实答案是：它挡得住突发脚本，
挡不住「稳定 2/s 持续半小时 = 3600 个并发任务」。`agentclient` 那侧至今没有并发上限
（`client.go` 只有一个 `sync.WaitGroup`），每个 `StartTask` 无条件起一个 goroutine
持一条 gRPC 流最长 30 分钟。要管并发得上信号量，那是另一件事。

Agent 侧的并发护栏（同时能跑的任务数满了就用 `RESOURCE_EXHAUSTED` 拒绝）已经把
「接不了活」变成一次**明确的失败**，但它是**事后**的：backend 这边仍然会先把任务
记录建出来。backend 侧的前置信号量（建任务之前查在途计数）还是另一件事，见「已知未做」。

### 五个桶

| key | 默认 | 它在防什么 |
|---|---|---|
| `rl:{login}:global:<action>` | 10 / 1s，login 和 register **各一个** | 过载保护，**不是安全措施** |
| `rl:{login}:ip:<ip>` | 30 / 60s | 拦单机的脑死流量 |
| `rl:{login}:email:<hmac>` | 5 / 60s | 拦撞库 —— 真正起作用的那一层 |
| `rl:{chat}:global` | 5 / 1s | 挡突发脚本 |
| `rl:{chat}:user:<user_id>` | 10 / 60s | 单账号的速率上限 |

几条不能改的：

- **全局闸是过载保护，不是安全措施。** `hash.DefaultCost = 12`，一次 bcrypt 约 160ms，
  所以 50/s × 160ms = 每秒 8 个 CPU 秒 —— 服务器会在远没到 50/s 之前先趴下，那道闸
  永远不触发。定在 10/s 它才真的生效。**代价要说清楚**：攻击者换约 120 个 IP 就能持续
  打满它，让所有人登不进去。但没有这道闸的话同样的流量直接把 bcrypt 打爆，连会话和
  任务接口一起死 —— 有它是「快速 429」，没它是「整站超时」。
  **别写「两层互相保护」，那是过度声称。**
- **IP 的 30/60s 必须比邮箱的 5/60s 高一个数量级。** 两个维度防的不是一回事。拿邮箱的
  阈值去卡 IP，一个公司/学校的 NAT 出口（整栋楼共用一个 IP）会被整体误伤。
- **`register` 和 `login` 各一个全局桶**，不共用。共用的话一轮注册洪水会把已有用户的
  登录一起锁死，而登录的可用性比注册重要得多。代价是 bcrypt 的总负载上限是 20/s 而
  不是 10/s，这是刻意换的。
- **只在「建」的路由上挂 chat 限流**（`POST /sessions`、`POST /sessions/:id/tasks`），
  GET 和 cancel 不挂 —— 同 `TaskQuota`「花钱的是建」。

### 为什么必须是 Lua

一次判定是 `ZREMRANGEBYSCORE` → `ZCARD` → **如果没满**才 `ZADD`。中间那个「如果」
决定了它不能用 `MULTI/EXEC`：事务不能根据前一条命令的结果分支，命令是一次性排队、
一次性执行的。Pipeline 只是把多个往返合成一个，**不保证原子性**，两个并发请求会都
读到「还差一个」。只剩 Lua：Redis 单线程跑完整个脚本，期间插不进别的命令。

对照固定窗口（`INCR` + `EXPIRE`）：边界允许 2 倍突发 —— 第 1 秒的最后 1 毫秒打满，
第 2 秒的第 1 毫秒再打满。`TestAllow_FixedWindowBoundaryBurstIsRejected` 盯着这条，
它是整个设计选滑动窗口的全部理由。

### 脚本里三条不能动的

- **两遍循环：第一遍只判定，第二遍才记账。** 边查边写的话，一个被窄桶拒掉的请求会
  先把全局桶记一账 —— 攻击者维持住速率就能占满全局名额，让所有人被拒，而日志上看
  全是「正常拦截」。限流器自己成了 DoS 放大器。
  `TestAllow_NarrowBucketRejectDoesNotConsumeGlobal` 盯着这条。
- **时钟只有 Redis 的 `TIME`，不从 Go 传。** `ZREMRANGEBYSCORE` 是对共享状态的**删除**，
  由 Go 传时间的话一个快钟副本会删掉其他副本还认为在窗口内的成员，**全集群静默失效**。
  `TIME` 在 Redis 5 之后的脚本里合法（效果复制），不需要 `redis.replicate_commands()`。
  测试侧用 `miniredis.SetTime` 控时钟。
- **成员名必须带唯一后缀。** ZSET 的成员会去重，同一毫秒两个请求用同样的成员名，第二次
  `ZADD` 只是更新 score，`ZCARD` 不变 —— **一次尝试凭空消失**。
  `TestAllow_SameMillisecondAttemptsAllCount` 盯着这条。

### `TRUSTED_PROXIES` —— 不配它，IP 那一层等于没做

`c.ClientIP()` 默认信任 `X-Forwarded-For`，而 **gin 默认信任所有代理**
（`trustedProxies: ["0.0.0.0/0", "::/0"]`）。不配的话攻击者每个请求伪造一个新 IP，
IP 桶永远打不满。`NewRouter` 里 `SetTrustedProxies(d.TrustedProxies)`，**默认 nil =
谁都不信，`ClientIP()` 退回 socket 对端**。

每一项都在 `config.Validate` 里解析校验过，因为 `NewRouter` 里那个 error 没法往外抛 ——
吞掉的话一个拼错的 `TRUSTED_PROXIES` 会静默变成「谁都不信」，放到 nginx 后面时
**所有客户端塌缩成同一个 IP 桶**，限流变成全站共享一个额度，而且没有任何迹象。

顺带：`middleware.Logger` 已经在调 `c.ClientIP()`，所以放到代理后面时访问日志也会
开始记网关 IP —— 配好 `TRUSTED_PROXIES` 两处一起恢复。

### fail-open / fail-closed 在这条链路上是分开的

| 情况 | 行为 | 理由 |
|---|---|---|
| 没配 `REDIS_ADDR` | 中间件压根不挂 | 合法的部署选择，启动 Warn 兜住 |
| Redis 运行时报错 / 超时 | **放行**，计数 | 限流拦滥用，而口令校验那道防线还在。fail-closed 会让一次 Redis 抖动变成全站登不进去 |
| 启动时 Ping 不通 | **重试 3 次后 `os.Exit(1)`** | 启动时拨不通通常是配错，同 `JWT_SECRET` 缺失。重试是为了别让一次抖动变成 CrashLoopBackOff |
| `ChatRateLimit` 取不到 `user_id` | **500，fail closed** | 那只意味着这条路由忘了挂 `Auth`，是接线 bug，不是依赖不可用 |

**后两行方向相反，而且必须相反。** 把「这个部署没启用防护」和「代码接错了」混成同一个
分支，就是把一个 bug 伪装成一个配置。

### 两个和安全直接相关的实现细节

- **邮箱进 key 之前要过 HMAC，不是裸 sha256。** Redis 的 key 会出现在 `SLOWLOG`、
  `MONITOR`、监控面板里，而邮箱是 PII。裸 sha256 **不构成去标识化** —— 邮箱地址空间
  可枚举，拿一本字典就能反查。用 `HMAC-SHA256(subkey, emailx.Normalize(email))`，
  `subkey = HMAC-SHA256(JWT_SECRET, "mini-agent:ratelimit:email")`，标准密钥分离，
  不需要新配置项，也不让 `JWT_SECRET` 本身出现在任何 key 里。
- **超长 body 必须拒绝，不能「跳过邮箱桶然后放过去」。** `AuthRateLimit` 要读 body 才能
  拿到邮箱，而 dto 没开 `DisallowUnknownFields` —— 塞一个 4KB 的垃圾字段照样能绑定成功、
  照样登录。放过去等于攻击者用一个字节数就关掉了唯一一层定向防护。合法的 auth body
  不到 1KB。`TestAuthRateLimit_OversizedBodyIsRejected` 盯着这条。

### 已知取舍

- **统计的是尝试，不是失败。** 「只统计失败的登录」要在 `c.Next()` 之后按状态码回写，
  会把原子的「查+记」拆成两半。代价是**别人可以用你的邮箱把你的桶打满，让你一分钟内
  登不进去**。这是真实存在的，不装作没有。
- **`Retry-After` 回的是固定的窗口长度，不是精确剩余时间。** 精确值会泄露「桶里最早那次
  尝试发生在什么时候」，而邮箱桶是攻击者和账号主人共享的 —— 那等于从一个免鉴权接口
  读出受害者上次登录的时刻。
- **不回 `X-RateLimit-Remaining`**：那是给攻击者一张精确的余额表。
- **拒绝时不透露是哪个桶拦的** —— 同一个码、同一句文案、同一组头部。区分开攻击者就
  知道该换 IP 还是换邮箱。`Scope` 只进日志和指标。

### 配置项

| 变量 | 默认 | 说明 |
|---|---|---|
| `REDIS_ADDR` | 空 | **空 = 不启用限流** |
| `REDIS_PASSWORD` | 空 | 只从环境变量读，`yaml:"-"` |
| `REDIS_DB` | 0 | |
| `RATELIMIT_AUTH_GLOBAL_LIMIT` / `_WINDOW` | 10 / 1s | 0 = 关掉这一层 |
| `RATELIMIT_IP_LIMIT` / `_WINDOW` | 30 / 1m | 同上 |
| `RATELIMIT_EMAIL_LIMIT` / `_WINDOW` | 5 / 1m | 同上 |
| `RATELIMIT_CHAT_GLOBAL_LIMIT` / `_WINDOW` | 5 / 1s | 同上 |
| `RATELIMIT_CHAT_USER_LIMIT` / `_WINDOW` | 10 / 1m | 同上 |
| `TRUSTED_PROXIES` | 空 | 逗号分隔的 IP/CIDR。**空 = 谁都不信** |

**五层全是 0 会被 `Validate` 拒掉**：那是「看起来启用了实际什么都没做」，和
`BreakerThreshold > 0 && BreakerCooldown <= 0` 被拒是同一个理由。

### 不能删的测试（追加到现有清单）

`TestAllow_NarrowBucketRejectDoesNotConsumeGlobal`、
`TestAllow_FixedWindowBoundaryBurstIsRejected`、
`TestAllow_SameMillisecondAttemptsAllCount`、
`TestAllow_ZeroLimitBucketIsSkipped`、
`TestAllow_RetryAfterIsWindowNotRemaining`、
`TestAuthRateLimit_OversizedBodyIsRejected`、
`TestAuthRateLimit_UsesRemoteAddrNotXFF`、
`TestAuthRateLimit_RejectionDoesNotRevealDimension`、
`TestAuthRateLimit_EmailKeyIsHMACNotPlaintext`、
`TestChatRateLimit_MissingUserIDFailsClosed`、
★`TestLogin_RateLimitedResponseIsIdenticalForUnknownEmail`

最后那条打 ★ 是因为它守的是 CLAUDE.md 的第一条不变量（账号枚举）：对一个**已注册**
邮箱和一个**未注册**邮箱各打到 42901，两次的状态码、业务码、文案、`Retry-After`、
响应体字节必须完全一致。设计上天然成立（桶按提交的邮箱做 key，不查库），但**没有
用例锁着就等于没有保证**。

## 实时推送（SSE）

`GET /api/v1/sessions/:id/stream` 把任务的执行过程（思考文本、调了什么工具、工具返回了什么）
实时推给浏览器。**粒度是会话**：一个连接看全这个会话的任务流。

### 地基：事件落库 = 权威，Pub/Sub 只做唤醒信号

事件先写进 `task_events`，Redis Pub/Sub 只发一句「这个会话有更新了」。
收到信号之后去读库里 `id > 游标` 的那几条，**不是「拿这条事件」**。

所以信号丢了不丢数据，只丢实时性（还有兜底轮询）。没配 Redis 时退化成每
`SSE_POLL_INTERVAL` 查一次库 —— **功能不会消失**，只是延迟变成一个轮询周期。
这跟限流/吊销的取舍一致：没配 Redis 是合法部署。

三条不能改的：

- **事件表不存 `user_id`。** 归属靠 `session_id → sessions.user_id` 推。
  少存一列就少一处可能填错的地方。
- **续传游标必须落在本会话一条事件的 id 上。** 判据是「查 `afterID=lastID-1, limit=1`，
  拿回来的 id 必须 == lastID」。不能拿「本会话最早的 id」比 —— 真正会漏发的是
  「游标比本会话还晚」（事件 5、6、7 / 游标 100 时 minID=5 < 100 比出来「没问题」，
  但读 `id > 100` 一条都拿不到）。`TestSSE_ForeignLastIDRestartsFromBeginning` 和
  `TestSSE_ForeignLastIDInGapRestartsFromBeginning` 盯的是这两条分支。
- **归属校验排在连接数上限之前。** 反过来的话，别人的会话在「满了」时回 503 而不是 404，
  等于泄露这个 session_id 真实存在。

### `task_events` 会一直涨，而且保存 Agent 的原始输出

**这两条都要记住，它们不是可以忽略的实现细节：**

- **没有保留策略。** 本轮拍板不做清理，表会一直涨。量级：一次 LLM 任务 10~50 个事件。
- **payload 存的是 Agent 的原文。** 改动前 `thinking` / `tool_call` / `tool_result`
  只进日志（而且日志里连 payload 都没有）；现在它们**原样落库 + 原样推给浏览器**。
  `tool_result.content` 可能是**整个文件的内容**（`read_file` 工具），几百 KB。

第二条的后果要认真对待：Agent 能读到的后端本机文件（`.env` 里的 `JWT_SECRET`、
DB DSN 等）现在会被**长期持久化**，并回显给会话主人。这个项目对密钥泄漏极其敏感
（JWT 吊销、账号枚举都是这个基调），而这条路径绕过了那道防线。

现在的兜底只有 `maxEventPayload = 8KiB`（按字符截断，不劈汉字，在
`service.RecordEvent` 这个**唯一入口**上做）。**这是限长，不是脱敏。**
要做脱敏/摘要/保留期，是独立的一件事 —— 别以为有 8KiB 上限就安全了。

### 连接数是三级闸

每会话 / **单用户** / 全进程，都是**软限制**（先查后订中间有缝，超限一个只是多一个 goroutine）。

**单用户那一级是必须有���，不能只有「每会话」和「全局」：** 一个账号拥有 N 个会话就能开
`N × MaxConnPerSession` 条连接，把全进程的预算吃光，于是**别的用户全部连不上** ——
那等于一个账号替所有人做决定。`Hub.CountUser` 按 user_id 跨会话计数。

「全进程」不是「全集群」：多副本部署时实际是「副本数 × 这个值」。

### 发布信号必须有界 —— ctx 上的超时不算数

**`sse.Bus.Publish` 跑在事件发布路径上**（Agent 的 gRPC 事件流那个 goroutine），
那条链路卡不得。而 go-redis v9 的命令路径是：

```
cn.WithReader(c.context(ctx), c.opt.ReadTimeout, ...)
```

`baseClient.context()`（redis.go:1892）在 `ContextTimeoutEnabled=false`（**默认值**）时
把调用方的 ctx **换成 `context.Background()`** —— 于是 `context.WithTimeout(ctx, 200ms)`
**到不了 socket**。剩下的只有 `opt.ReadTimeout`，而订阅用的那个客户端
（`redisx.NewPubSub`）`ReadTimeout` 是 `-1`，`options.init()` 会把它归一成 `0`
=「没有超时」。

结果：Redis 半开（TCP 建立了但对端不回话）时 PUBLISH **永久阻塞**，整条事件流停摆。

所以 `sse.Bus` 持有**两个**客户端：`sub` 是无超时的（订阅建立之后是长期阻塞读，
超时一到期订阅就断），`pub` 是 `pub.WithTimeout(publishTimeout)` 的**有界视图**
（`baseClient.withTimeout` 只改 `ReadTimeout`/`WriteTimeout`，**共享连接池**，不会多开连接）。
**这条不变量由总线自己兜住**，不靠调用方传对客户端。
`TestBus_PublishIsBoundedEvenOnUnboundedClient` 用一个「连上了但永远不回话」的
TCP 服务端盯着它。

订阅侧的 5s 建订阅超时是**真的**：Pub/Sub 的读写路径直接用调用方的 ctx
（`cn.WithReader(ctx, 0, ...)`），没有 `c.context()` 那一步。

### 单轮推送有上限，超出的不会丢

`SSE_MAX_REPLAY_PER_DRAIN`（默认 1000）限制**一轮**推送下发多少条事件。
一个攒了几万条事件的会话，一次重连补发历史就会把整段历史一口气写出去 ——
那是一次性放大，单次查询的上限（`SSE_REPLAY_LIMIT`）拦不住。

**上限必须在批内也管住**，不能只在批与批之间查：一次查询就能拿回 200 条，
只查批间的话单批就把上限冲过去了。

超出的**不会丢**：游标已经推进到最后一个发出去的事件，剩下的下一轮（兜底轮询或唤醒信号）
继续发。上限按住的是**单轮的突发量**，不是总量。

### 其它几条

- **前端的去重键是 `(task_uid, seq)`，不是 id。** id 是库的全局自增序号，和「哪一条逻辑事件」
  不是一回事：任务流重发时 Agent 从 `seq=1` 重新数，同一条逻辑事件会以**不同的 id**
  再写一行、再推一帧。帧里的 data 同时带 `task_uid` 和 `seq` 就是为了这个。
- **`sse.enabled=false` 回 `50303`，不是 `50300`。** 50300 是「执行器暂时不可用」，
  前端会以为该等执行器恢复而反复重试一条根本不存在的路。50303 文案直说「请改用轮询」。
  `50302` 是连接数超限。**这两个码是同步返回的**（SSE 握手阶段就被挡），前端真的会收到。
- **握手阶段失败走统一 JSON 外壳，流阶段绝不再调 `response.Fail/OK`。** 响应头已发出，
  改不了状态码，混 JSON 会带歪浏览器的解析器。
- **不用 `c.Stream`。** 它依赖 `w.CloseNotify()`，而 `httptest.ResponseRecorder`
  不实现 `http.CloseNotifier`，类型断言直接 panic。改用 `c.Request.Context().Done()`。
- **`X-Accel-Buffering: no` 必带。** nginx 默认缓冲会把事件攒着一起发。
- **关停顺序**：`stopReconcile()` → `close(shutdownCh)`（**必须排在 `srv.Shutdown` 之前**，
  否则 Shutdown 等满 10s）→ `srv.Shutdown()` → `sseBus.Close()` → `ssePubSub.Close()` → 关库。
- **依赖没注入要 fail closed 500，不许 panic。** `Deps.SSEStream` 是接口字段，
  `SSEStream: (*service.TaskEventService)(nil)` 时接口本身不是 nil，`== nil` 判不出来 ——
  `handler.isNilDep` 用反射认这种「有类型无值」，`service.TaskEventService` 的方法
  也各自守了 nil 接收者。

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
| POST | `/api/v1/auth/logout` | `middleware.Auth`（用户 JWT） |
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

**`required` 对 `*string` 只判 nil，不判空串。** validator 的 `hasValue` 撞上指针字段是直接 `return true` 的，所以 `{"input":""}` 能大摇大摆过掉 `required`。`CreateTaskReq.Input` 因此还挂了一个自定义的 `notblank`（`internal/pkg/validate`），把空串和纯空白一起挡在 40000。漏了的话会建出一个 input 为空的任务、原样派给 Agent，驱动一整轮什么都没有的 LLM 循环，还白占一次配额。

`CreateSessionReq.Input` 走的是另一条路：**它可空是刻意的**（先开一个空会话，首轮输入等想好了再发），所以空白输入在 handler 由 `NormalizedInput` 归一成 nil，`CreateWithFirstTask` 那一趟只建会话、不建任务、不派发 —— 不是回 400。把它改成必填之前先想清楚：这是接口契约，不是漏校验。两侧的差别由 `TestCreateTask_RequiresInput` 和 `TestCreateSession_BlankInputCreatesNoTask` 锁着。

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

未知的事件类型翻成 `EventUnknown` 后**照样往下发**（payload 还在），只是多记一条 Warn —— 直接丢弃的话，「对面升级了、我们没跟上」这件事没人会发生。

★ **这条 Warn 的价值在于它是稀有的**，所以别拿它报日常事件。压缩事件以前按 `EVENT_TYPE_UNSPECIFIED` 发，于是**每次压缩**都触发一条"未知类型"的 Warn —— 而这条 Warn 的本意是"对面升级了、我们没跟上"，用它报"对面没跟上"的日常事件，信号就被稀释了：真漏了某个新事件时没人看得见。压缩事件现在有专属枚举（`EVENT_TYPE_COMPACTING` / `_COMPACTED` = 7 / 8，**只追加**、不改动已有编号），Go 侧登记在 `agentclient.eventTypes` 里。

★ **加枚举值是跨仓库改动**，顺序固定：先改 backend 的 `proto/agent/v1/agent.proto`（**权威副本**）→ 复制到 Agent 仓库的 `mini_agent/proto/agent/v1/agent.proto`（两份必须逐字节一致）→ 两边各自重新生成（backend `make proto`；Agent 侧 `grpc_tools.protoc`）→ 更新两边的映射表（Go 的 `eventTypes`、Python 的 `_EVENT_TYPES`）。proto3 的枚举是开放的，所以**先升哪边都不会崩**：对面没认的新值落进 `EventUnknown` 分支，照样往下发、只多一条 Warn —— 不会错认成别的类型。

`var _ Client = (*GRPCClient)(nil)` 那行别删 —— 少了它接口和实现漂移了编译器不会吭声。

`StartTask` 起的消费协程要活过发起它的那次 HTTP 请求，所以 ctx 必须先 `context.WithoutCancel` 再配自己的超时（`AGENT_TASK_TIMEOUT`，默认 30 分钟）。直接从请求 ctx 派生的话，handler 一 return 整条事件流就断了。

#### 错误翻译

**gRPC 的 `error` 一步都不许走出 `agentclient`。** `translateGRPCError`（`errors.go`）按状态码归到本包的哨兵：`ErrUnavailable`（Unavailable）、`ErrAgentBusy`（ResourceExhausted）、`ErrTimeout`（DeadlineExceeded）、`ErrAlreadyRunning`（AlreadyExists）、`ErrBadRequest`（InvalidArgument / NotFound / FailedPrecondition / Unimplemented …）、`ErrRemoteInternal`（Internal / Unknown / DataLoss / 其余）。

**`AlreadyExists` 单拎一个哨兵，别并回 `ErrBadRequest`。** 它在这条链路上只有一个来源：流断了之后原样重发，而 Agent 那边上一次的 worker 还没散场（`self._tasks.register` 挡回来的）。那是「等一下就好」，而 `ErrBadRequest` 的语义是「重试一万次也一样」。混在一起的话，重发在最常见的时序下每次都失败，而且没有任何迹象。它同样**不算下游有病**——对面健康得很。`TestSentinelFor_AlreadyExistsIsRetryable` 盯着这三条。

翻译结果里**只留 op 和状态码名，绝不带 `status.Message()`**。Unavailable 的 message 长这样：`dial tcp 10.0.0.7:50051: connect: connection refused` —— 里面是内网地址，而这个 error 会经 `OnStreamError` 一路走到 `tasks.error_msg`，那一列是跟着任务详情**回显给用户**的。`TestTranslateGRPCError_DropsRawMessage` 盯着这条。丢掉原文不影响排查：状态码说明了故障类别，Agent 地址启动日志里有一份。

`service.translateAgentErr` 再把哨兵翻成 `model.ErrAgentUnavailable` / `ErrAgentBusy` / `ErrAgentTimeout`，`response.errRules` 映射到 `50300` / `50301` / `50400`。**别让 `response` 直接 import `agentclient`** —— 那会把响应外壳挂到 gRPC 基础设施上。

注意：这几个业务码目前**都没有 HTTP 生产者**。派发失败走的是「标 `failed`、创建照样 200」（见下面的任务派发），取消通知是 best-effort，所以 Agent 挂掉不会有任何接口回 503。`50301`（执行器满了）更特别 —— 它是**异步**到达的：任务记录**已经建了**、派发**已经发生了**，错误要等事件流里才浮出来，于是用户看到的是任务详情里的 `error_msg`，**不是** HTTP 响应里的业务码。**别把 `50301` 读成「已经生效了」** —— 今天没有任何接口会返回它；登记它只是为了让将来有同步路径时它不掉进 `CodeInternal`（50000，那会让客户端以为服务坏了），以及让前端能区分「并发满了」和「Agent 挂了」。规则先登记好，等哪天有接口要把 Agent 错误直接抛给客户端时就是对的。

#### 熔断

`internal/pkg/breaker` 是个不依赖 gRPC 的三态熔断器（closed → open → half_open）。`GRPCClient` 在 `StartTask` / `CancelTask` 前 `Allow()`，拿到结果 `Record()`。

**什么错误算「下游有病」由 `countsAsUnhealthy` 决定，只有 `ErrUnavailable` / `ErrTimeout` / `ErrRemoteInternal` 算。** 把 `InvalidArgument` 这类算进去的话，前端一阵坏请求就能把一个完全健康的 Agent 关在门外。

**`ErrAgentBusy`（执行器满了）明确不算。** 熔断器数的是「持续出错」，不是「接不了活」—— 把它算进去，一次流量高峰就能把一个健康的 Agent 隔离掉，而 Agent 恰恰是在满载的时候最忙。`countsAsUnhealthy` 里为此显式挡了一道，别只靠"它没被列在上面"。`TestCountsAsUnhealthy_AgentBusyIsNotUnhealthy` 盯着这条。

**但 busy 被记成「健康」有个副作用：它会把连续失败计数清零。** `consume` 里那句 `record(!countsAsUnhealthy(tErr))` 在 busy 时算出的是 `Record(true)` —— 方向是对的（Agent 回话了，它明确告诉你「我满了」，这证明它活着可及），但 `breaker.Record(true)` 在 closed 状态下做的第一件事就是 `b.failures = 0`（`internal/pkg/breaker/breaker.go`）。于是混流场景下（Agent 一半连不上一半满载）每次 busy 都把计数抹平，熔断器可能因此**永远打不开**。已知取舍：breaker 改成三态（healthy / unhealthy / **neutral**）是独立的一件事，不在本次范围内。

**`StartTask` 那条路的 `Record` 在 `consume` 里，不在 `StartTask` 里。** `RunTask` 是 server-streaming，`c.cli.RunTask(...)` 不等对面回话就返回 —— Agent 进程没了它照样返回 nil，错误要到第一次 `Recv()` 才浮出来。按它的返回值记账的话，熔断器永远认为下游是好的，装了等于没装，而且没有任何迹象。`consume` 里那个 `recorded` 标志保证一次 `Allow` 只对一次 `Record`：多记会搞乱半开期计数，少记会让探针名额漏掉、熔断器再也回不到 closed。`TestConsume_StreamFailureOpensBreaker` 和 `TestConsume_RecordsExactlyOnce` 盯着这两条。

**每次重发也各自拿一次 `Allow`、还一次 `Record`**（`redial`）。让重发白嫖首发那个名额的话，一个已知连不上的 Agent 会被按 `1 + maxStreamRetries` 倍的流量打——熔断器装了等于没装。`TestRunTask_EachRetryGoesThroughBreaker` 盯着这条。

#### 流中断重试

**gRPC 自带的 `retryPolicy`（`WithDefaultServiceConfig` 里那份）在这条路上基本是摆设。** 透明重试在客户端收到**第一条响应消息**的那一刻就 committed 了，而 `RunTask` 的 `started` 几毫秒就到。所以那份配置只覆盖得了「流根本没建起来」，跑到一半断掉它一次都不会重。真正的重发在 `retry.go` 的 `runTask` 里。

**重发是安全的，因为 Agent 侧本来就是照着「调用方会原样重发」写的。** `run_conversation` 起手三步：`ensure_session`（幂等补 sessions 行）、`recover`（从库里重建历史，顺带按 `tool_call_id` 从幂等表回填已执行工具的真结果补桩）、`_turn_in_flight`（上一轮没跑完就不再重复记用户那句话，直接从补桩后的历史续跑）。所以重发的代价被限制在「崩在哪一步就从哪一步接着跑」，不会从头再烧一遍 LLM，也不会重复执行有副作用的工具。

**钥匙是 `session_id` + `input`，不是 `seq`。** `seq` 在 Agent 那侧是 `itertools.count(1)`，每次 `RunTask` 都从 1 重新数 —— 它是流内序号，不是断点游标。事件不是 Agent 的状态，`messages` 表才是。所以 proto 一个字都不用改。

几条不能改的：

- **判据是「之前收到过事件没有」，不是错误类型**（`streamOutcome.gotEvent`）。流跑到一半断掉，在 gRPC 那层和「压根连不上 Agent」是同一个状态码（`Unavailable`）、同一个哨兵，按错误分不开。但收到过事件就证明 Agent 确实接住了任务并且在跑。一条都没收到 = 交接压根没成功，重发只是拿 N 倍流量去撞一个已知连不上的下游，那是熔断器的活。`TestRunTask_NoEventMeansNoRetry` 盯着这条。
- **`consume` 绝不自己调 `OnStreamError`**，它只返回 `streamOutcome`。提前上报的话任务就成了终态，重发跑出来的 `started` / `finished` 会撞上 `cas` 的 `from=[pending,running]` 一条都匹配不上，被当成迟到回调**静静丢掉**——你会看到一个成功跑完却显示 `failed` 的任务，而日志里只有一行 INFO。
- **要不要重发是 service 说的算**（`EventHandler.ShouldRetry` → `TaskEventHandler` → `TaskService.IsRunning`）。`agentclient` 不读库，判断不了任务是不是已经被用户取消了（取消是「先落库、再通知 Agent」，库里已经是 `canceled`），也判断不了回收器是不是先收了尾。查不到或库报错时**不重发**：判错的两个方向代价不对称，不重发只是退回原来的行为（标 `failed`，用户自己重发一次），错误地重发是在用户不知情的情况下再烧一轮 LLM。`TestRunTask_HandlerCanVetoRetry`、`TestShouldRetry_OnlyWhenStillRunning` 盯着这条。
- **`ErrTimeout` 不重发。** 它自己的注释就写着：超时意味着请求可能已经到对面并且正在执行。而这条路上的超时是 `AGENT_TASK_TIMEOUT`（默认 30m），重两次就是 90 分钟。`TestRunTask_TimeoutIsNotRetried`。
- **总窗口必须远小于 `RECONCILE_STALE_AFTER`（默认 15m）。** 重发期间任务在库里是 `running` 且 `updated_at` 不动，超过那个窗口回收器会把它标 `failed`，然后重发的结果又被 `cas` 丢掉——和上面「提前上报」是同一个坑，只是换了个人来踩。`maxStreamRetries=2` + `retryBaseBackoff=2s`（按 attempt 翻倍）总共十几秒，所以这条路上不用额外补心跳。
- **退避的下限是「上一次的 worker 散场」，不是网络抖动。** 流断时 Agent 那边只是被 `context.add_callback` 置了个 cancel 标志，真正 `unregister` 要等 `run_conversation` 走到下一个 `cancel_check`，最坏是一整次 LLM 调用之后。没散场就重发会撞 `ALREADY_EXISTS`（以及 `_SessionLocks` 的会话锁），所以 `retryBaseBackoff` 是 2s 而不是 `retryPolicy` 里那个 0.1s，而且撞上 `ErrAlreadyRunning` 要退避后再来一次，不是就地放弃。`TestRunTask_RetriesWhileAgentStillHoldsTask`。
- **`error` 事件目前不重发**，只有流中断重发。Agent 报上来的 `error` payload 是纯文本，Go 这边分辨不了「`_SessionBusy`，本任务未执行」（该重试）和「工具参数非法」（重试也一样）。要做准得让 Agent 在 payload 里带一个机器可读的 `retryable` 字段，别在这边猜中文子串。

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
| 流中断且**重试已用尽**（`OnStreamError`） | → `failed` |

**流断了必须给任务收尾**，不然它永远停在 `running`。这不会误伤已完成的任务：事件在同一个协程里按序处理，走到 `OnStreamError` 时 `finished` 要么早处理完了（`cas` 落空），要么根本没来过。

注意 `OnStreamError` 对 service 的语义是**「这个任务没救了，收尾吧」，不是「某一次流断了」**——中途断掉但重发成功的任务不会走到这里，见上面的「流中断重试」。它每个任务最多调一次。

所有状态写入都过 `TaskService.HandleCallback`，**别绕开它直接调仓库层** —— `canceled` 白名单、`cas`、迟到回调的日志三件事都在那儿。事件里的失败原因要用 `truncate` 截到 `maxEventErrMsg`（和 `dto.TaskCallbackReq` 的 `max=2000` 对齐）：gRPC 这条路没有 binding tag 挡着，`tasks.error_msg` 又是 text，不截的话对面一条事件就能塞几 MB 进来，而且会跟着任务详情回显给用户。同理，流中断写进 `error_msg` 的文案不能带 gRPC 原文（里面有内网地址和端口）。

`/internal` 回调**保留但同步 Agent 用不上**，Python 侧不必实现它。两条路都过 `cas`，不会打架。

### 心跳与回收器

上面那张表有个漏洞：**它只覆盖「流还活着」的情况**。进程重启、Pod 被驱逐、Agent 被 OOM kill 时，`OnStreamError` 根本没机会跑，任务就永远停在 `pending` / `running`。`service.Reconciler`（`reconcile.go`）是这条链路的兜底。

一轮回收 = `TaskRepo.ListStale(before, batch)` 捞出「中间态 + `updated_at < now - stale_after`」的任务（这是 `idx_tasks_status_updated` 的第一个消费者，`status` 等值在前、`updated_at` 范围在后，正好是这个索引的形状），逐个走 `HandleCallback` 标 `failed`。

几条不能改的：

- **只写 `failed`，绝不写 `canceled`**。写 `canceled` 用户会以为是自己取消的，一个该被排查的故障就这么藏起来了。
- **走 `HandleCallback`，不直接调仓库层**。`cas` 在那儿——一个在我们捞出来之后才完成的任务不会被盖成 failed。
- **单个失败不中断整轮**（`continue`，不 `return`），一条坏记录不该让后面几十个都收不掉。
- **只收尾，不重跑**。要不要重来是用户的决定，不是回收器的。

  这条的原始理由是「跑了一半的 LLM 任务重跑可能重复扣费、重复调外部工具」，而**那个理由现在只成立一半**：Agent 侧的 `recover()` 会按 `tool_call_id` 从幂等表回填已执行工具的真结果补桩，`_turn_in_flight` 会阻止用户那句话被重复记，重跑的代价被限制在崩掉的那一步（见「流中断重试」）。所以这条留着是**因为还没做**，不是因为做不了。要改成重新派发的话，三件事一件都不能少：加一列 `dispatch_attempts` 封顶（否则一个能把 Agent 搞崩的任务会被无限重发，永远烧钱）；认领动作要原子（写成一条带 `dispatch_attempts < ?` 和 `updated_at < ?` 的 UPDATE，靠 `RowsAffected == 1` 仲裁，否则多副本会同时重发同一个任务）；还得给回收器一个 Agent 客户端，而它现在拿的是 `main.go` 里那个**刻意不带客户端**的 `statusWriter`。
- `ListStale` 的 `limit <= 0` 直接返回空，不是全表 —— GORM 的 `Limit(0)` 是不写 LIMIT 子句，而这跑在后台，真捞了全表也没人会看见。

配套的心跳解决「长任务被误杀」：`TaskEventHandler.OnEvent` 收到进度事件（`thinking` / `tool_call` / `tool_result` / `unknown`）时调 `TaskService.Heartbeat` → `TaskRepo.Touch`，刷一次 `updated_at`。所以 **`stale_after` 要对比的是「Agent 两次事件之间的最长间隔」，不是任务总时长**，一个跑两小时但一直在推进度的任务不会被收掉。改状态的事件不用额外心跳 —— `cas` 那条 UPDATE 本来就刷了 `updated_at`。

**`Touch` 的节流在 SQL 的 `WHERE updated_at < ?` 里，不在内存里。** 内存里记「上次 touch 时间」要按 `task_uid` 存一张表还得清理，而且多副本部署时每个副本各记各的、节流直接失效。`Touch` 只续 `running`：`pending` 意味着还没交接成功，那正是回收器该来看一眼的状态，给它续命等于把交接失败永远藏起来。

#### 选主：多副本只让一个扫

多副本部署时每个副本都会跑这个循环，于是它们捞同一批任务。`cas` 在下游兜着，**不会写错任何数据**，只是白烧几次 `SELECT`。`internal/pkg/redislock` 补的是这一点：一轮开始前先抢一次锁，没抢到的直接跳过这一轮。

**用的是单实例 `SET NX PX`，不是 Redlock，这是刻意的。** 这把锁**不保护正确性**——没有它，多副本重复扫的后果只是重复执行一次幂等操作，不是数据损坏。Redlock 的复杂度（多实例、时钟假设）是为「锁失效会破坏数据」的场景准备的，而那种场景真正需要的是 fencing token；这里 `cas` 本身就是 fencing。

**拿不到锁就跳过，不自旋、不等待。** 回收器是兜底机制，晚一分钟完全无所谓；自旋只会让所有副本挤在一起抢同一把锁。

**Redis 挂了要当作拿到锁，照常扫（fail open）。** 回收器本身就是兜底，让兜底依赖另一个会挂的组件是反的。代价只是退化回「多副本重复扫」。`Acquire` 返回的 `ok` 表示「要不要拦下这一轮」（fail open 时也是 `true`），**`token` 非空才表示「真的持有锁」** —— 判断「要不要释放」必须看 `token`。

**释放锁必须用 Lua**，脚本先比对 token 再删：`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`。`GET` 后再 `DEL` 是不够的——中间锁可能过期并被别人拿走，那就会把别人的锁删掉。`TestRelease_OnlyOwnLock` 盯着这条。

**释放放在 `defer` 里。** `tick` 里有提前 `return` 的分支（`RunOnce` 报错那条），写成函数末尾手动释放的话，锁要挂满整个 TTL（45s）——而 `RunOnce` 报错恰好是数据库有问题的时候，也就是最需要下一轮快点重试的时候。

**锁的 TTL（45s）必须大于 `reconcileTimeout`（30s），且不能做成独立配置项。** 小于的话一轮没跑完锁就过期，另一个副本会进来重复扫——静默失效，没有任何迹象。TTL 由 `reconcileTimeout` 推导出来（`reconcileLockTTL = reconcileTimeout + 15s`），改上面那个常量时它自动跟上。

对应的测试：`TestAcquire_AfterExpiry`（TTL 真的设上了）、`TestAcquire_TwoInstancesAreMutuallyExclusive`（两个副本不能同时持有）、`TestRelease_OnlyOwnLock`（只删自己的锁）、`TestTick_SkipsWhenLockHeld`、`TestTick_ReleasesLockOnError`。

### main.go 的构造环

```
TaskService ──要──> agentclient.Client ──要──> EventHandler ──要──> TaskService
```

拆点是「事件处理器只用 `HandleCallback` / `Heartbeat`，而那两条路径根本不碰 Agent 客户端」：先拿一个不带客户端的 `TaskService` 把处理器建起来，客户端有了之后再给 HTTP 那侧建一个带客户端的。两个都只是仓库之上的无状态包装，不共享东西。别改成 setter —— 那会让「客户端什么时候可用」变成运行时才知道的事。

回收器复用那个**不带客户端**的 `statusWriter`：它只写状态，从不联系 Agent —— 一个连不上的 Agent 正是它要处理的情况。它跑在自己的 `context.WithCancel` 上（不是请求 ctx），退出时先 `stopReconcile()` 再 `srv.Shutdown()`：它是纯后台循环，没有在途请求要照顾，让它在关闭过程中再扫一轮只会拖长关闭时间。

已知未做：Agent 挂掉时**没有任何 HTTP 接口回 503**（`50300` / `50301` / `50400` 的映射已经登记好，但派发失败走「标 failed + 200」、取消通知是 best-effort，两条路都不往上抛）；**限流只在配了 `REDIS_ADDR` 的部署里存在**（没配的话登录注册和建任务三条路全是裸的，只有启动时一条 Warn 兜着，见「限流」一节）；**回收器的选主和 JWT 吊销也都依赖 Redis**（没配 `REDIS_ADDR` 时没有锁，多副本会重复扫同一批任务 —— 靠 `cas` 兜底不会写错，只是白烧几次查询；吊销也不生效，被登出/禁用的令牌还能用到过期。两条都是启动时一条 Warn 兜着，见「JWT 吊销」和「心跳与回收器」那节）；**没有 backend 侧的前置并发信号量**（并发护栏已经有**第一层**：Agent 侧同时能跑的任务数满了会立刻用 `RESOURCE_EXHAUSTED` 拒绝，backend 把它翻成 `ErrAgentBusy`、**不算**下游有病，文案也分开说「并发任务数已达上限」而不是「连接中断」。但**第二层还没有**：backend 在建任务**之前**不查在途计数，所以超额的任务会**先建出记录再失败** —— 用户看到的是一条 failed 的任务，而不是一个明确的拒绝；而且多副本部署下 backend 各数各的，除非引入 Redis 全局计数（那又要处理 slot 泄漏、超时、僵尸项）。配额管总量、限流管速率、信号量管并发，三者不能互相替代 —— 第一层只是把「静默排队 30 分钟」变成「立即失败 + 有明确原因」，没有解决「已创建即失败」）；**「禁用账号」这个洞仍然开着**（`revoke.RevokeUser` 写好也测了，但**没有任何生产代码调它** —— 既没有禁用用户的接口也没有改密码的接口，所以管理员把一个用户改成 disabled 之后，他现有的令牌照样能用到 `AccessTTL` 过期，和做吊销这一层之前完全一样。补齐的前提是先有那两个接口，见「JWT 吊销」一节那张表下面的 ★）；**没有 refresh token**（所以 `AccessTTL` 停在 2h，见「JWT 吊销」那节）；`Login` 里"用户不存在"分支比"口令错"少跑一次 bcrypt，存在约 160ms 的时间侧信道（代码注释里写明了）；`/internal` 目前和业务接口共用一个监听端口，生产上应该单独端口或由网关按前缀挡在公网外。
