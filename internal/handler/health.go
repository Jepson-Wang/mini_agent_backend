package handler

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

// pingTimeout 健康检查探数据库的超时。探活接口自己不能被拖死,
// 数据库卡住的时候要迅速返回不健康,而不是跟着一起挂起。
const pingTimeout = 2 * time.Second

// HealthHandler 健康检查。db 传 nil 表示只检查进程活着,不探数据库。
type HealthHandler struct {
	db *gorm.DB
}

func NewHealthHandler(db *gorm.DB) *HealthHandler {
	return &HealthHandler{db: db}
}

// healthData 是 /health 的 data 字段。
type healthData struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

// Check 处理 GET /health。
// 数据库连不上时返回 50000,让编排系统能把这个实例摘掉。
func (h *HealthHandler) Check(c *gin.Context) {
	data := healthData{Status: "ok", Database: "skipped"}

	if h.db != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), pingTimeout)
		defer cancel()

		sqlDB, err := h.db.DB()
		if err == nil {
			err = sqlDB.PingContext(ctx)
		}
		if err != nil {
			_ = c.Error(err) // 真实原因进日志,不回给客户端
			response.Fail(c, response.CodeInternal, "数据库不可用")
			return
		}
		data.Database = "up"
	}

	response.OK(c, data)
}
