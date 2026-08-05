package middleware

import (
	"crypto/subtle"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// InternalTokenHeader 是服务间调用携带共享密钥的头。
//
// 刻意不复用 Authorization:那个头是用户令牌的地盘。两套凭证混在一个头里,
// 哪天有人给某条路由同时挂上 Auth 和 InternalAuth,它们会互相顶掉,
// 而且很难从代码上看出来。
const InternalTokenHeader = "X-Internal-Token"

// InternalAuth 校验服务间调用的共享密钥。
//
// 这是一条**没有用户身份**的路径。通过之后 gin.Context 里没有 userID,
// 挂在它后面的 handler 不能调 middleware.MustUserID —— 那会 panic。
//
// 共享密钥只能证明"调用方是我们自己的服务",证明不了"它代表哪个用户"。
// 所以 /internal 下面只应该有"按资源 ID 推进状态"这类接口,
// 绝不能出现"列出某用户的全部数据"这种拿入参当身份的接口 ——
// 那样一来,拿到密钥就等于拿到了所有人的数据。
//
// 密钥是最后一道防线,不是唯一一道:部署上还应该让 /internal 不对公网暴露
// (单独端口,或者网关只放行内网来源)。
func InternalAuth(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 没配密钥就全拒,和 Auth 遇到 nil Verifier 时的处理一致。
		// 这条路径能直接改任务状态,漏配时放行等于把它开给全世界。
		if secret == "" {
			slog.Error("内部路由未配置共享密钥,拒绝所有请求",
				"trace_id", trace.FromGin(c),
				"path", c.Request.URL.Path,
			)
			response.Fail(c, response.CodeInternal, "")
			return
		}

		// 定长比较。用 == 的话,比较会在第一个不同的字节上提前返回,
		// 逐字节计时就能把密钥一位一位问出来。
		// ConstantTimeCompare 在长度不等时直接返回 0(泄露长度,可以接受)。
		got := c.GetHeader(InternalTokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			// 不区分"没带"和"带错了",也绝不把收到的值记进日志 ——
			// 那等于把别人手抖贴错的真密钥写进日志系统。
			slog.Warn("内部路由鉴权失败",
				"trace_id", trace.FromGin(c),
				"path", c.Request.URL.Path,
				"client_ip", c.ClientIP(),
			)
			response.Fail(c, response.CodeUnauthorized, "")
			return
		}

		c.Next()
	}
}
