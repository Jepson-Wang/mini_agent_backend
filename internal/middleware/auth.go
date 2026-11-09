package middleware

import (
	"context"
	"log/slog"
	"strings"
	"time"

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

// ClaimsVerifier 是能返回完整声明的校验器。
//
// 吊销检查需要 jti(单 token 吊销的键)和 iat(和用户级 not_before 比较),
// 而 TokenVerifier 只返回 userID,信息不够。
//
// **这是可选接口,不是替代品。** 实现方实现了它,中间件就多查一次吊销;
// 没实现的话中间件就只做签名校验(吊销功能整体不启用)。
//
// 为什么不把吊销检查塞进 Verify 的实现里:那会让 pkg/token(纯签名工具)
// 依赖 pkg/revoke(依赖 Redis),把一个能脱离一切依赖单测的包污染成
// 要 Redis 才能测的包。组合发生在 middleware 层 —— 那才是"把几件事串起来"
// 该待的地方。
type ClaimsVerifier interface {
	VerifyClaims(ctx context.Context, token string) (*TokenClaims, error)
}

// TokenClaims 是校验通过的令牌声明。
//
// 是个本地的结构而不是直接暴露 pkg/token 的 Claims:
// middleware 这一层不该知道底下是 JWT 的哪套类型。
type TokenClaims struct {
	// UserID 是令牌代表的用户。
	UserID int64

	// JTI 是令牌的唯一标识,吊销单 token 时用它做 key。
	JTI string

	// IssuedAt 是签发时刻,和用户级 not_before 比较时用它。
	IssuedAt time.Time

	// ExpiresAt 是过期时刻,吊销单 token 时用它算黑名单的 TTL。
	ExpiresAt time.Time
}

// Revoker 查令牌的吊销状态。
//
// 定成接口是为了测试能塞个假的,不用连 Redis。
type Revoker interface {
	// IsRevoked 判断令牌是否已被吊销。
	// 返回的 err 表示"Redis 出问题了",**不表示是否被吊销**。
	IsRevoked(ctx context.Context, jti string, userID int64, issuedAt time.Time) (bool, error)
}

// context 里放用户 id 和令牌声明的键。
const (
	userIDKey = "auth_user_id"

	// jtiKey 是令牌的唯一标识在 gin.Context 里的键。登出接口要拿它去吊销。
	jtiKey = "auth_jti"

	// tokenExpKey 是令牌的过期时刻在 gin.Context 里的键。
	// 吊销黑名单的 TTL 取 exp - now,所以这个值是必需的。
	tokenExpKey = "auth_token_exp"
)

// Auth 校验 Authorization 头,通过后把用户 id 放进 context。
// 只挂在私有路由组上,公开接口不要挂。
//
// ## 吊销检查
//
// 签名校验过了之后,再查一次这个令牌有没有被吊销。
//
// **吊销检查 fail open,而 Verifier == nil 仍然 fail closed。** 判据是
// 「前一道防线还在不在」:
//
//   - Verifier == nil → fail closed。那是接线错误,放行等于私有接口彻底裸奔。
//   - Revoker 报错 → fail open。JWT 签名校验**已经过了**,身份是可信的;
//     吊销只是在一个已验证的身份上再加一层,它失灵不代表身份可疑。
//
// 两者的方向相反,而且必须相反 —— 把"这个部署没启用防护"和"代码接错了"
// 混成同一个分支,就是把一个 bug 伪装成一个配置。
func Auth(v TokenVerifier, revoker Revoker) gin.HandlerFunc {
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

		// 先做签名校验。吊销检查在它之后 —— 前一道防线必须先过。
		claims, err := verifyClaims(c.Request.Context(), v, token)
		if err != nil {
			// 不把 err 回给客户端:令牌过期、签名不对、用户被禁用,
			// 对外统一都是"登录状态无效",免得成了探测工具。原因进日志。
			_ = c.Error(err)
			response.Fail(c, response.CodeUnauthorized, "登录状态无效或已过期")
			return
		}

		// 吊销检查。
		if revoker != nil && claims != nil && claims.JTI != "" {
			revoked, err := revoker.IsRevoked(c.Request.Context(), claims.JTI,
				claims.UserID, claims.IssuedAt)
			if err != nil {
				// Redis 出问题了 → **fail open,放行**。
				//
				// 签名已经验过了,身份是可信的。吊销失灵不代表身份可疑,
				// 而 fail closed 会让一次 Redis 抖动变成全站登不进去。
				//
				// 错误要记下来:吊销检查失效这事运维该知道。
				slog.Warn("查吊销名单失败,放行本次请求",
					"trace_id", trace.FromGin(c),
					"user_id", claims.UserID,
					"error", err)
			} else if revoked {
				// 文案和别的失败**完全一样**:"登录状态无效或已过期"。
				//
				// 单独说"已吊销"会泄露信息:「这个 token 是被登出的还是
				// 被禁用的」就成了一条可观测的信号,和账号枚举是同一类问题。
				slog.Info("令牌已被吊销",
					"trace_id", trace.FromGin(c),
					"user_id", claims.UserID)
				response.Fail(c, response.CodeUnauthorized, "登录状态无效或已过期")
				return
			}
		}

		c.Set(userIDKey, claims.UserID)
		if claims.JTI != "" {
			c.Set(jtiKey, claims.JTI)
		}
		if !claims.ExpiresAt.IsZero() {
			c.Set(tokenExpKey, claims.ExpiresAt)
		}
		c.Next()
	}
}

// verifyClaims 尽量拿到完整声明,拿不到就退回只拿 userID。
//
// 两条路:
//
//   - 实现方实现了 ClaimsVerifier → 一次拿到 jti 和 iat,能做吊销检查。
//   - 没实现 → 只有 userID,没有 jti,跳过吊销检查(但签名校验照做)。
//
// 为什么不把 ClaimsVerifier 做成必需接口:那会让所有实现方(包括测试里的假的)
// 都得实现它,而中间件已经能在缺它的情况下正常工作。
func verifyClaims(ctx context.Context, v TokenVerifier, token string) (*TokenClaims, error) {
	if cv, ok := v.(ClaimsVerifier); ok {
		claims, err := cv.VerifyClaims(ctx, token)
		if err != nil {
			return nil, err
		}
		if claims == nil {
			return nil, nil
		}
		return &TokenClaims{
			UserID:    claims.UserID,
			JTI:       claims.JTI,
			IssuedAt:  claims.IssuedAt,
			ExpiresAt: claims.ExpiresAt,
		}, nil
	}

	// 退回老路径:只有 userID。
	userID, err := v.Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	return &TokenClaims{UserID: userID}, nil
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

// TokenJTI 取当前令牌的 jti。取不到时 ok 为 false。
//
// 只有实现了 ClaimsVerifier 的校验器才会有 —— 登出接口靠它吊销。
func TokenJTI(c *gin.Context) (string, bool) {
	v, exists := c.Get(jtiKey)
	if !exists {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// TokenExpiresAt 取当前令牌的过期时刻。取不到时 ok 为 false。
//
// 吊销黑名单的 TTL 要用它算(exp - now),所以必须拿到。
func TokenExpiresAt(c *gin.Context) (time.Time, bool) {
	v, exists := c.Get(tokenExpKey)
	if !exists {
		return time.Time{}, false
	}
	t, ok := v.(time.Time)
	return t, ok
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
