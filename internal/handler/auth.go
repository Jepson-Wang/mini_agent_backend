package handler

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler/dto"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
	"github.com/Jepson-Wang/mini_agent_backend/internal/service"
)

// AuthService 是 handler 需要的认证能力。
//
// 接口定义在使用方这一侧,不在 service 包里:这样 handler 的测试塞个假的
// 就能跑,不用连数据库,也不用真签 JWT。生产实现是 *service.AuthService。
type AuthService interface {
	Register(ctx context.Context, email, password, nickname string) (*service.Credential, error)
	Login(ctx context.Context, email, password string) (*service.Credential, error)
}

// AuthHandler 处理注册和登录。
type AuthHandler struct {
	svc AuthService
}

func NewAuthHandler(svc AuthService) *AuthHandler {
	return &AuthHandler{svc: svc}
}

// Register 处理 POST /api/v1/auth/register。
func (h *AuthHandler) Register(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	var req dto.RegisterReq
	if !bindJSON(c, &req) {
		return
	}

	cred, err := h.svc.Register(c.Request.Context(), req.Email, req.Password, req.Nickname)
	if err != nil {
		// 查 response.errRules:ErrEmailOccupied → 40000,没登记的一律 50000。
		// 原始 err 进日志,不进响应体。
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewAuthResp(cred.User, cred.Token, cred.ExpiresAt))
}

// Login 处理 POST /api/v1/auth/login。
//
// TODO: 这个接口需要限流。没有的话一个脚本可以按每秒几百次的速度撞库,
// bcrypt 的耗时只是让它慢一点,拦不住。按 IP + 邮箱两个维度都要限。
func (h *AuthHandler) Login(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	var req dto.LoginReq
	if !bindJSON(c, &req) {
		return
	}

	cred, err := h.svc.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewAuthResp(cred.User, cred.Token, cred.ExpiresAt))
}

// ready 挡住"路由注册了但依赖没注入"的接线错误。
// 和 middleware.Auth 里对 nil Verifier 的处理是一个思路:宁可 500,
// 也不能拿一个 nil service 往下跑。
func (h *AuthHandler) ready(c *gin.Context) bool {
	if h == nil || h.svc == nil {
		slog.Error("认证接口未注入 service",
			"trace_id", trace.FromGin(c),
			"path", c.Request.URL.Path,
		)
		response.Fail(c, response.CodeInternal, "")
		return false
	}
	return true
}
