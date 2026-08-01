package main

import (
	"log"
	"os"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
	"github.com/Jepson-Wang/mini_agent_backend/internal/service"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "host=localhost user=postgres password=123456 dbname=mini_agent_backend port=5432 sslmode=disable"
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
		// 仓库层把 ErrDuplicatedKey / ErrForeignKeyViolated 翻译成业务错误,
		// 全靠这个开关,关掉的话那些 errors.Is 分支会静默失效。
		TranslateError: true,
	})
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}

	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Task{}); err != nil {
		log.Fatalf("迁移失败: %v", err)
	}

	repo := repository.New(db)
	sessionSvc := service.NewSessionService(repo)

	// TODO: handler.NewRouter(sessionSvc) + http.Server + 优雅关闭
	_ = sessionSvc
	log.Println("依赖初始化完成")
}
