// Package handler 是 HTTP 入口层:解析请求、调 service、按统一外壳返回。
// 这一层不碰数据库,也不写业务规则。
package handler

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

// Deps 是路由需要的全部外部依赖,由 main 组装好传进来。
// 这样 NewRouter 不用自己 new 任何东西,测试里换成假的就行。
type Deps struct {
	// DB 给健康检查探活用。传 nil 则 /health 不探数据库。
	DB *gorm.DB

	// Verifier 校验访问令牌。私有路由组靠它。
	// 传 nil 的话私有接口一律 500 而不是放行,见 middleware.Auth。
	Verifier middleware.TokenVerifier

	// Debug 打开后会注册 /debug/panic 这类只用于验证的路由。
	// 生产环境必须是 false。
	Debug bool
}

// NewRouter 装配路由。
//
// 中间件顺序:TraceID → Recovery → Logger,私有组再加 Auth。
//   - TraceID 在最外层:后面两个都要往日志里写 trace_id。
//   - Recovery 在 Logger 外层:Logger 自己 panic 也能被兜住。
//   - Auth 只挂私有组:公开接口不该因为没带 token 就 401。
func NewRouter(d Deps) *gin.Engine {
	// 用 gin.New() 而不是 gin.Default():后者自带 Logger 和 Recovery,
	// 它们的输出不走我们的统一外壳,会和下面这三个重复。
	r := gin.New()

	// 默认是 false,不开的话 405 会退化成 404。
	r.HandleMethodNotAllowed = true

	r.Use(
		middleware.TraceID(),
		middleware.Recovery(),
		middleware.Logger(),
	)

	// 兜底也要走统一外壳 —— 否则客户端遇到 404 拿到的是 gin 的纯文本
	// "404 page not found",跟正常响应完全对不上。
	r.NoRoute(func(c *gin.Context) {
		response.Fail(c, response.CodeNotFound, "接口不存在")
	})
	r.NoMethod(func(c *gin.Context) {
		// 用 40500 而不是 40000:HTTP 状态是从业务码除出来的,
		// 拿 40000 兜的话这里会回 400,语义不对(路径是存在的,只是方法不对)。
		response.Fail(c, response.CodeMethodNotAllowed, "")
	})

	// ---- 公开路由 ----

	health := NewHealthHandler(d.DB)
	r.GET("/health", health.Check)

	api := r.Group("/api/v1")

	// 登录注册这类不需要令牌的接口挂这儿
	_ = api.Group("")

	// ---- 私有路由:统一挂 Auth ----

	private := api.Group("")
	private.Use(middleware.Auth(d.Verifier))
	// TODO: session / task 的接口注册到 private 上,
	// handler 里用 middleware.MustUserID(c) 取当前用户。
	_ = private

	// ---- 仅调试 ----

	if d.Debug {
		registerDebugRoutes(r)
	}

	return r
}

// registerDebugRoutes 注册只在调试模式下存在的路由。
// /debug/panic 是用来验证 Recovery 的:进程不该崩,响应该是 50000 外壳。
func registerDebugRoutes(r *gin.Engine) {
	r.GET("/debug/panic", func(c *gin.Context) {
		panic("这是一个故意触发的 panic,用于验证 Recovery 中间件")
	})
}
