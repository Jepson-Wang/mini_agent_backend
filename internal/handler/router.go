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

	// Auth 处理注册和登录。传 nil 时这两个接口一律 500 —— 路由照常注册,
	// 因为"接口不存在(404)"和"接口没接好(500)"是两回事,别混。
	Auth AuthService

	// Task 处理用户对任务的操作。传 nil 同上,500 不是 404。
	Task TaskService

	// TaskCallback 消费 Agent 报上来的任务状态,挂在 /internal 下面。
	TaskCallback TaskCallbackService

	// InternalToken 是 /internal 路由组的共享密钥,来自 AGENT_CALLBACK_SECRET。
	//
	// 传空则那组接口一律 500 而不是放行 —— 它能直接改任务状态,
	// 漏配时放行等于把它开给全世界。见 middleware.InternalAuth。
	InternalToken string

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

	// 登录注册这类不需要令牌的接口挂这儿。
	// 单独开一个组是为了将来往上加限流中间件 —— 撞库和批量注册
	// 只会打这两个接口,限流不该挂到全站。
	public := api.Group("")
	{
		auth := NewAuthHandler(d.Auth)
		public.POST("/auth/register", auth.Register)
		public.POST("/auth/login", auth.Login)
	}

	// ---- 私有路由:统一挂 Auth ----

	private := api.Group("")
	private.Use(middleware.Auth(d.Verifier))
	{
		// 单个任务。task_uid 全局唯一,所以不挂在 /sessions 下面 ——
		// 客户端拿到 uid 之后不必再记它属于哪个会话。
		//
		// 注意 gin 的限制:同一层级上的路径参数必须同名。
		// /tasks/:uid 和 /tasks/:id 混用会在启动时直接 panic。
		task := NewTaskHandler(d.Task)
		private.POST("/tasks/:uid/cancel", task.Cancel)

		// TODO: session 的接口也注册到这儿。
	}

	// ---- 服务间路由:共享密钥,没有用户身份 ----
	//
	// 挂在引擎根上而不是 /api/v1 下面,是为了让"这条路径不是给用户的"
	// 从 URL 上就能看出来 —— 网关按前缀就能把 /internal 挡在公网之外,
	// 不用去认某一条具体的路由。
	//
	// 这个组下面绝不能出现拿入参当身份的接口(比如"列出用户 X 的任务"):
	// 共享密钥只能证明调用方是我们自己的服务,证明不了它代表谁。
	internalGroup := r.Group("/internal")
	internalGroup.Use(middleware.InternalAuth(d.InternalToken))
	{
		ih := NewInternalHandler(d.TaskCallback)
		internalGroup.POST("/tasks/:uid/callback", ih.TaskCallback)
	}

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
