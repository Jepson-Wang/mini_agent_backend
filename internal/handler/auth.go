package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler/dto"
	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
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

	// Logout 吊销当前这一个令牌。
	//
	// 登出接口必须走 middleware.Auth:它要拿到 jti 和 exp 才知道吊销谁。
	Logout(ctx context.Context, jti string, exp time.Time) error
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
// 限流不在这儿,在 middleware.AuthRateLimit 上(挂在 router 的 public 组)。
// **但只在配了 REDIS_ADDR 的部署里才有** —— 没配的话这个接口就是裸的:
// 一个脚本可以按每秒几百次的速度撞库,bcrypt 的耗时只是让它慢一点,拦不住。
// 所以 main.go 在没启用时会打一条 Warn,别把那条 Warn 当噪音划掉。
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

// Logout 处理 POST /api/v1/auth/logout。
//
// 挂在**私有路由组**上(走 middleware.Auth):它要知道当前是哪个令牌,
// 才能吊销它。挂在公开组上的话就没有 jti 可用。
//
// **只吊销当前这一个令牌**,不动该用户的其它设备 ——
// 用户在手机上还登着,不该被这一次的登出踢下线。
//
// 幂等:重复登出、或者令牌本来就没被吊销过,都回 200。
// 前端登出时不该因为"服务端说没这回事"而报错留在登录页。
func (h *AuthHandler) Logout(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	// jti 和 exp 由 middleware.Auth 放进来。
	//
	// 取不到 jti 说明这个部署的校验器没实现 ClaimsVerifier
	// (也就是没启用吊销)。那就不吊销,直接回成功 ——
	// 前端照样清本地 token,服务端这边没有可吊销的东西。
	jti, _ := middleware.TokenJTI(c)
	exp, expOK := middleware.TokenExpiresAt(c)

	// ★ 有 jti 却取不到 exp 是接线错误,不能静静当成功。
	//
	// Logout → RevokeToken 的 TTL 是按 exp 算的。exp 是零值的话 TTL 会是一个
	// 巨大的负数,RevokeToken 直接 return nil —— 接口回 200 说"登出成功",
	// 而那个 token 在剩下的有效期里照样能用。这正是 pkg/revoke 包注释里
	// 点名的"吊销静默失效"。
	//
	// 不 fail closed(登出失败对用户没有可操作性,前端照样该清本地 token),
	// 但必须留下痕迹 —— service 那边对缺 jti 已经有一条 Warn,这里补齐另一半,
	// 否则两个分支一个有声一个没声,排查时只会看到"登出明明成功了"。
	if jti != "" && !expOK {
		slog.Error("登出时取不到令牌过期时刻,无法计算黑名单 TTL,本次吊销未生效",
			"trace_id", trace.FromGin(c),
			"提示", "middleware.Auth 只在 ExpiresAt 非零时才写入,校验器可能没填 exp",
		)
	}

	if err := h.svc.Logout(c.Request.Context(), jti, exp); err != nil {
		// 原始 err 进日志,不进响应体。
		response.FailFromError(c, err)
		return
	}

	response.OK(c, gin.H{})
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
