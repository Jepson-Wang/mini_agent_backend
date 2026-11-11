package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/handler"
	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ratelimit"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/redislock"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/redisx"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/revoke"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/token"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
	"github.com/Jepson-Wang/mini_agent_backend/internal/service"
)

// shutdownTimeout 收到退出信号后等在途请求做完的时间,超时就强杀。
const shutdownTimeout = 10 * time.Second

// fatal 打一条 ERROR 然后退出。
//
// 不用 log.Fatalf:slog.SetDefault 之后标准 log 包的输出会被转到 slog,
// 但级别固定是 INFO —— 启动失败打成 INFO,告警规则根本抓不到。
func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}

func main() {
	// JSON 格式,方便日志系统按 trace_id 检索。
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// 先把 .env 灌进环境变量,再开始读。下面任何 os.Getenv 都得排在这之后 ——
	// 抢跑的那个变量会静默拿到旧值,这种 bug 最难查。
	// 不打印加载了什么:这个文件里装的就是密钥。
	if err := config.LoadEnvFile(""); err != nil {
		fatal("加载 .env 失败", err)
	}

	debug := os.Getenv("APP_ENV") != "production"

	cfg, err := config.Load("")
	if err != nil {
		// 配置不全就直接死,不给"能跑但不安全"的兜底。
		fatal("加载配置失败", err)
	}

	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
		// 仓库层把 ErrDuplicatedKey / ErrForeignKeyViolated 翻译成业务错误,
		// 全靠这个开关,关掉的话那些 errors.Is 分支会静默失效。
		TranslateError: true,
	})
	if err != nil {
		fatal("连接数据库失败", err)
	}

	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Task{}); err != nil {
		fatal("迁移失败", err)
	}

	tokenMgr, err := token.NewManager(cfg.JWT.Secret, cfg.JWT.Issuer, cfg.JWT.AccessTTL)
	if err != nil {
		fatal("初始化令牌签发器失败", err)
	}

	repo := repository.New(db)

	// authSvc 要吊销能力,而吊销器依赖 Redis,Redis 在后面才初始化。
	// 所以这里先建一个不带吊销的,等 Redis 好了再补上 —— 见下面
	// 「Redis 就绪之后」那段。
	authSvc := service.NewAuthService(repo, tokenMgr, nil)

	// 下面这三行在拆一个构造环:
	//
	//	TaskService ──要──> agentclient.Client ──要──> EventHandler ──要──> TaskService
	//
	// 拆点在于事件处理器只用得上 HandleCallback,而那条路径根本不碰
	// Agent 客户端(它只读写仓库)。所以先拿一个不带客户端的 TaskService
	// 把处理器建起来,等客户端有了,再给 HTTP 那侧建一个带客户端的。
	// 两个 TaskService 都只是仓库之上的无状态包装,不共享任何东西。
	statusWriter := service.NewTaskService(repo, nil)
	events := service.NewTaskEventHandler(statusWriter)

	// 懒连接:这里不会真去握手,Agent 没起也能正常启动,
	// 拨不通要到第一次调用才知道。所以报错只代表参数不对。
	agentCli, err := agentclient.NewGRPCClient(cfg.Agent, events)
	if err != nil {
		fatal("初始化 Agent 客户端失败", err)
	}
	defer func() {
		// 等在途的事件流收完(有上限)再关连接。
		if err := agentCli.Close(); err != nil {
			slog.Error("关闭 Agent 客户端失败", "error", err)
		}
	}()

	// 每用户的建任务配额。MaxTasks <= 0 时返回 nil,表示这个部署不限额 ——
	// SessionService 收 nil 也能正常工作(TaskQuota.Check 是 nil 安全的)。
	taskQuota, err := service.NewTaskQuota(repo.Task, cfg.Quota)
	if err != nil {
		fatal("初始化任务配额失败", err)
	}
	if taskQuota == nil {
		slog.Warn("未启用任务配额(quota.max_tasks <= 0),单个账号可以无限建任务")
	}

	// Redis 只用于限流。redis.addr 为空时 New 返回 (nil, nil),表示不启用。
	rdb, err := redisx.New(redisx.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err != nil {
		fatal("初始化 Redis 客户端失败", err)
	}
	if rdb == nil {
		slog.Warn("未启用 Redis(redis.addr 为空),登录注册和建任务都没有限流")
	} else {
		// 启动时探活,重试几次再放弃。
		//
		// 启动期连不上是致命的,运行期出错却是放行(fail open),这是两个不同的时刻:
		// 启动时地址配错是配置错误,本仓库对配置错误一律大声退出
		// (缺 JWT_SECRET 就 os.Exit(1),没有"能跑但不安全"的路径);
		// 运行期的一次抖动则该放行请求,因为口令校验那道防线还在。
		//
		// 这与 handler/health.go 里对 Agent 的判断方向相反(Agent 挂了只降级),
		// 是有意的:那里是运行期的依赖故障,这里是启动期的配置错误。别"统一"它们。
		readyCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		err := rdb.WaitReady(readyCtx, 3, 1500*time.Millisecond)
		cancel()
		if err != nil {
			fatal("连接 Redis 失败", err)
		}
	}

	// ★ 必须用接口类型的变量装,只在 rdb 非 nil 时才赋值。
	// 把一个值为 nil 的 *ratelimit.Limiter 直接赋给接口字段,得到的接口值
	// 有类型、无值,不等于 nil —— 路由层的 `d.Limiter != nil` 会判真,
	// 于是中间件被挂上,运行时才在空指针上炸。
	var limiter middleware.Limiter
	// RedisProber 同理:*redisx.Client 的方法虽然 nil 安全,但会把"没启用"报成 up。
	var redisProber handler.RedisProber
	// 吊销器同理:revoke.New(nil) 返回 nil,表示"这个部署没启用吊销"。
	var revoker *revoke.Revoker
	if rdb != nil {
		limiter = ratelimit.New(rdb.Raw())
		redisProber = rdb
		revoker = revoke.New(rdb.Raw(), cfg.JWT.AccessTTL, token.Leeway)
	}

	// 把吊销能力装进 authSvc。
	//
	// 用接口变量转一手,免得 revoker 为 nil 时把一个"有类型无值"的
	// 接口塞进 service —— 那样 service 里的 `s.revoker == nil` 会判假,
	// 然后在 nil 指针上炸。
	var tokenRevoker service.TokenRevoker
	var revokeChecker middleware.Revoker
	if revoker != nil {
		tokenRevoker = revoker
		revokeChecker = revoker
	}
	authSvc = service.NewAuthService(repo, tokenMgr, tokenRevoker)
	if revoker == nil {
		slog.Warn("未启用 JWT 吊销(redis.addr 为空),登出的令牌在过期前仍然有效")
	}

	rl := cfg.RateLimit
	authLimits := middleware.AuthLimits{
		Global:   ratelimit.Rule{Limit: rl.AuthGlobalLimit, Window: rl.AuthGlobalWindow},
		IP:       ratelimit.Rule{Limit: rl.IPLimit, Window: rl.IPWindow},
		Email:    ratelimit.Rule{Limit: rl.EmailLimit, Window: rl.EmailWindow},
		EmailKey: middleware.DeriveEmailKey(cfg.JWT.Secret),
	}
	chatLimits := middleware.ChatLimits{
		Global: ratelimit.Rule{Limit: rl.ChatGlobalLimit, Window: rl.ChatGlobalWindow},
		User:   ratelimit.Rule{Limit: rl.ChatUserLimit, Window: rl.ChatUserWindow},
	}

	// SessionService 拿客户端是为了建完任务立刻派发(StartTask),
	// TaskService 拿它是为了取消时通知(CancelTask)。
	sessionSvc := service.NewSessionService(repo, agentCli, taskQuota)
	taskSvc := service.NewTaskService(repo, agentCli)

	// 任务回收器:把没人再会来报结果的任务收尾。
	//
	// 它是同步 Agent 那条链路的兜底 —— 进程重启、Pod 被驱逐、流被掐断时,
	// OnStreamError 根本没机会跑,任务会永远停在 pending / running。
	//
	// 用独立的 ctx 而不是请求 ctx:它要活到进程退出为止。
	reconcileCtx, stopReconcile := context.WithCancel(context.Background())
	defer stopReconcile()

	// 用 statusWriter(不带 Agent 客户端的那个)就够了:回收器只写状态,
	// 从不联系 Agent —— 一个连不上的 Agent 正是它要处理的情况。
	//
	// lock 是选主锁:多副本部署时只有拿到锁的那一个会扫,
	// 别的直接跳过这一轮。没配 Redis 时是 nil,回收器照常工作。
	//
	// ★ 用接口类型的变量装,和上面的 limiter / redisProber 同一个理由:
	// 把一个值为 nil 的 *redislock.Lock 直接赋给接口字段,得到的接口值
	// 有类型、无值,不等于 nil。
	var reconcileLock service.LeaseLock
	if rdb != nil {
		reconcileLock = redislock.New(rdb.Raw())
	}

	reconciler, err := service.NewReconciler(repo.Task, statusWriter, reconcileLock, cfg.Reconcile)
	if err != nil {
		fatal("初始化任务回收器失败", err)
	}
	// ★ 这里只许有两条分支:启用了就跑,没启用就说一声。
	//
	// 别再往中间插 `else if`。"没配 Redis 就没有选主"那条警告曾经插在这儿,
	// 于是 REDIS_ADDR 为空(受支持的默认部署)时分支走到警告就结束了,
	// go Run 永远到不了 —— 回收器被构造出来然后静静丢掉,任务永远停在 running。
	// 那条警告现在由 Reconciler.Run 自己打,理由见它的注释。
	if reconciler == nil {
		slog.Warn("未启用任务回收器(reconcile.interval <= 0),卡住的任务不会被自动收尾")
	} else {
		go reconciler.Run(reconcileCtx)
	}

	// 没配 AGENT_CALLBACK_SECRET 的话 /internal 那组接口一律 500 而不是放行。
	// 提前说一声,免得对面调不通时先去怀疑网络。
	if cfg.Agent.CallbackSecret == "" {
		slog.Warn("未配置 AGENT_CALLBACK_SECRET,/internal 下的接口将一律返回 500")
	}

	// 反方向的那把密钥。缺了不影响本服务启动 —— Agent 绑在回环上时本来就不鉴权。
	// 但如果 Agent 是跨主机的,它会自己拒绝启动,那时错误在对面、原因在这边,
	// 先说一声能省一轮排查。
	if cfg.Agent.Token == "" {
		slog.Warn("未配置 AGENT_GRPC_TOKEN,调用 Agent 时不带凭证",
			"agent_addr", cfg.Agent.Addr,
			"提示", "仅当 Agent 绑在回环地址上时才成立")
	}

	if debug {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	router := handler.NewRouter(handler.Deps{
		DB: db,
		// /health 拿它探 Agent。探不通只降级成 degraded,不 500。
		Agent:        agentCli,
		Auth:         authSvc,
		Session:      sessionSvc,
		Task:         taskSvc,
		TaskCallback: taskSvc,
		// 校验器要能返回完整声明(jti 和 iat),吊销检查需要它们。
		// 用适配器包一层,免得 middleware 反过来依赖 token 包。
		Verifier:      middleware.NewClaimsAdapter(tokenMgr),
		Revoker:       revokeChecker,
		InternalToken: cfg.Agent.CallbackSecret,
		Debug:         debug,

		Limiter:        limiter,
		AuthLimits:     authLimits,
		ChatLimits:     chatLimits,
		TrustedProxies: cfg.Server.TrustedProxies,
		Redis:          redisProber,
	})

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second, // 挡住慢速读头攻击
	}

	go func() {
		// 只打有效期,不打密钥,也不打它的长度或指纹。
		slog.Info("HTTP 服务启动",
			"addr", cfg.Server.Addr,
			"debug", debug,
			"jwt_issuer", cfg.JWT.Issuer,
			"jwt_access_ttl", cfg.JWT.AccessTTL.String(),
			"agent_addr", cfg.Agent.Addr,
			// 只说带没带凭证,不打密钥,也不打它的长度或指纹。
			"agent_auth", cfg.Agent.Token != "",
			"agent_task_timeout", cfg.Agent.TaskTimeout.String(),
			"agent_breaker_threshold", cfg.Agent.BreakerThreshold,
			"agent_breaker_cooldown", cfg.Agent.BreakerCooldown.String(),
			"quota_window", cfg.Quota.Window.String(),
			"quota_max_tasks", cfg.Quota.MaxTasks,
			"reconcile_interval", cfg.Reconcile.Interval.String(),
			"reconcile_stale_after", cfg.Reconcile.StaleAfter.String(),
			// 只打地址,不打口令,也不打它的长度或指纹。
			"redis_addr", cfg.Redis.Addr,
			"rl_auth_global", rl.AuthGlobalLimit, "rl_auth_global_window", rl.AuthGlobalWindow.String(),
			"rl_ip", rl.IPLimit, "rl_ip_window", rl.IPWindow.String(),
			"rl_email", rl.EmailLimit, "rl_email_window", rl.EmailWindow.String(),
			"rl_chat_global", rl.ChatGlobalLimit, "rl_chat_global_window", rl.ChatGlobalWindow.String(),
			"rl_chat_user", rl.ChatUserLimit, "rl_chat_user_window", rl.ChatUserWindow.String(),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("HTTP 服务异常退出", err)
		}
	}()

	// 等 Ctrl+C 或 kill,然后把在途请求做完再退。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("收到退出信号,开始优雅关闭")

	// 先停回收器再关 HTTP:它是纯后台循环,没有在途请求要照顾,
	// 而让它在关闭过程中又扫一轮、又去写库,只会拖长关闭时间。
	stopReconcile()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("优雅关闭超时,强制退出", "error", err)
	}

	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	// 显式关,不用 defer:顺序要的是「在途请求先做完(上面的 Shutdown),再撤连接池」。
	// defer 会拖到 main 返回,和前后的关闭动作排不出确定的先后。
	if err := rdb.Close(); err != nil { // nil 客户端的 Close 返回 nil
		slog.Error("关闭 Redis 客户端失败", "error", err)
	}
	slog.Info("已退出")
}
