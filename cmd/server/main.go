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

	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/handler"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
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

	repo := repository.New(db)
	sessionSvc := service.NewSessionService(repo)
	_ = sessionSvc // TODO: 注册到私有路由组

	if debug {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	router := handler.NewRouter(handler.Deps{
		DB: db,
		// TODO: Verifier 等令牌签发器做完再接。
		// 现在是 nil,私有组会 fail closed(返回 50000),不会放行。
		Verifier: nil,
		Debug:    debug,
	})

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second, // 挡住慢速读头攻击
	}

	go func() {
		slog.Info("HTTP 服务启动", "addr", cfg.Server.Addr, "debug", debug)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("HTTP 服务异常退出", err)
		}
	}()

	// 等 Ctrl+C 或 kill,然后把在途请求做完再退。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("收到退出信号,开始优雅关闭")

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
