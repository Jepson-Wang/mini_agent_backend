package middleware

import (
	"context"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// TokenVerifier 校验访问令牌并返回它代表的用户。
//
// 中间件只依赖这个接口,不关心令牌是 JWT 还是查 session 表 ——
// 换实现不用动这里,写测试也不用真造一个 JWT。
// 实现方在 service 层(auth.go)。
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (userID int64, err error)
}

// userIDKey 是登录用户 id 在 gin.Context 里的键。
const userIDKey = "auth_user_id"

// Auth 校验 Authorization 头,通过后把用户 id 放进 context。
// 只挂在私有路由组上,公开接口不要挂。
func Auth(v TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 路由挂了 Auth 但没注入实现,是接线错误。
		// 这种情况必须 fail closed —— 放行的话私有接口就彻底裸奔了。
		if v == nil {
			slog.Error("私有路由未注入 TokenVerifier,拒绝所有请求",
				"trace_id", trace.FromGin(c),
				"path", c.Request.URL.Path,
			)
			response.Fail(c, response.CodeInternal, "")
			return
		}

		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			response.Fail(c, response.CodeUnauthorized, "缺少或格式错误的 Authorization 头")
			return
		}

		userID, err := v.Verify(c.Request.Context(), token)
		if err != nil {
			// 不把 err 回给客户端:令牌过期、签名不对、用户被禁用,
			// 对外统一都是"登录状态无效",免得成了探测工具。原因进日志。
			_ = c.Error(err)
			response.Fail(c, response.CodeUnauthorized, "登录状态无效或已过期")
			return
		}

		c.Set(userIDKey, userID)
		c.Next()
	}
}

// UserID 取当前登录用户 id。路由没挂 Auth 时 ok 为 false。
func UserID(c *gin.Context) (int64, bool) {
	v, exists := c.Get(userIDKey)
	if !exists {
		return 0, false
	}
	id, ok := v.(int64)
	return id, ok
}

// MustUserID 同 UserID,但取不到直接 panic。
//
// 取不到只有一个可能:这条路由忘了挂 Auth。那是接线 bug,
// 早点炸出来(Recovery 会兜住,返回 50000)好过默默拿 0 当用户 id 去查数据。
func MustUserID(c *gin.Context) int64 {
	id, ok := UserID(c)
	if !ok {
		panic("当前路由没有挂 Auth 中间件,取不到登录用户")
	}
	return id
}

// bearerToken 从 "Bearer xxx" 里抠出 xxx。
// 前缀按 RFC 6750 大小写不敏感处理。
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
