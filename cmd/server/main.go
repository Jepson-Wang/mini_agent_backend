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
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
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
	authSvc := service.NewAuthService(repo, tokenMgr)

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
	reconciler, err := service.NewReconciler(repo.Task, statusWriter, cfg.Reconcile)
	if err != nil {
		fatal("初始化任务回收器失败", err)
	}
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
		Agent:         agentCli,
		Auth:          authSvc,
		Session:       sessionSvc,
		Task:          taskSvc,
		TaskCallback:  taskSvc,
		Verifier:      tokenMgr,
		InternalToken: cfg.Agent.CallbackSecret,
		Debug:         debug,
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
	slog.Info("已退出")
}
