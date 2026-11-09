package middleware

import (
	"context"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/token"
)

// claimsAdapter 把 pkg/token.Manager 适配成 middleware.ClaimsVerifier。
//
// **为什么要一个适配器而不是让 token.Manager 直接实现那个接口:**
//
// middleware.ClaimsVerifier 返回的是 middleware.TokenClaims(本地类型),
// 而 token.Manager 的 VerifyClaims 返回的是 token.Claims。
// 要让 token.Manager 直接实现,token 包就得 import middleware ——
// 而 middleware 是被 handler 导入的下层包,那样就形成了
// middleware ← token ← middleware 的循环依赖。
//
// 把适配器放在 middleware 里、让它 import token,方向是单向的
// (middleware → token),没有环。
//
// 这个类型只在 main.go 的接线处用一次,测试里不需要它。
type claimsAdapter struct {
	mgr *token.Manager
}

// NewClaimsAdapter 把一个签发器包装成能返回完整声明的校验器。
//
// 传 nil 返回 nil —— 没注入签发器时中间件会 fail closed(500),
// 而不是拿着一个 nil 指针往下跑。
func NewClaimsAdapter(mgr *token.Manager) TokenVerifier {
	if mgr == nil {
		return nil
	}
	return &claimsAdapter{mgr: mgr}
}

// VerifyClaims 校验令牌并返回完整声明。
func (a *claimsAdapter) VerifyClaims(ctx context.Context, tok string) (*TokenClaims, error) {
	claims, err := a.mgr.VerifyClaims(ctx, tok)
	if err != nil {
		return nil, err
	}
	if claims == nil {
		return nil, nil
	}
	out := &TokenClaims{
		UserID: claims.UID,
		JTI:    claims.ID,
	}
	if claims.IssuedAt != nil {
		out.IssuedAt = claims.IssuedAt.Time
	}
	if claims.ExpiresAt != nil {
		out.ExpiresAt = claims.ExpiresAt.Time
	}
	return out, nil
}

// Verify 校验令牌并返回用户 id。
//
// 适配器同时实现两个接口,是因为 middleware.Auth 要的是 TokenVerifier
// (拿 userID),而 verifyClaims 内部会再断言一次 ClaimsVerifier
// (拿 jti 和 iat)。一个对象同时满足两者,接线处才不会写出
// "传 A 却被当成 B" 的怪代码。
func (a *claimsAdapter) Verify(ctx context.Context, tok string) (int64, error) {
	return a.mgr.Verify(ctx, tok)
}

// 编译期钉住:适配器必须同时满足两个接口。
var (
	_ TokenVerifier  = (*claimsAdapter)(nil)
	_ ClaimsVerifier = (*claimsAdapter)(nil)
)
