package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/token"
)

// fakeRevoker 是 middleware.Revoker 的假实现。
//
// 不连 Redis:中间件这一层要验的是「吊销了怎么办、查不动怎么办」,
// 不是 Redis 本身的行为(那些在 pkg/revoke 里对着 miniredis 测)。
type fakeRevoker struct {
	revoked bool
	err     error

	// 记下查的时候传了什么参数。
	gotJTI     string
	gotUserID  int64
	gotIssued  time.Time
	queryCount int
}

func (f *fakeRevoker) IsRevoked(_ context.Context, jti string, userID int64, issuedAt time.Time) (bool, error) {
	f.queryCount++
	f.gotJTI = jti
	f.gotUserID = userID
	f.gotIssued = issuedAt
	return f.revoked, f.err
}

// claimsVerifier 是能返回完整声明的假校验器。
type claimsVerifier struct {
	uid   int64
	jti   string
	iat   time.Time
	exp   time.Time
	fail  bool
	calls int
}

func (v *claimsVerifier) Verify(context.Context, string) (int64, error) {
	v.calls++
	if v.fail {
		return 0, errors.New("令牌无效")
	}
	return v.uid, nil
}

func (v *claimsVerifier) VerifyClaims(context.Context, string) (*middleware.TokenClaims, error) {
	v.calls++
	if v.fail {
		return nil, errors.New("令牌无效")
	}
	return &middleware.TokenClaims{
		UserID:    v.uid,
		JTI:       v.jti,
		IssuedAt:  v.iat,
		ExpiresAt: v.exp,
	}, nil
}

// newClaimsEngine 装一个用 claimsVerifier 的私有路由。
func newClaimsEngine(v middleware.TokenVerifier, revoker middleware.Revoker) *gin.Engine {
	r := gin.New()
	r.Use(middleware.TraceID())
	private := r.Group("")
	private.Use(middleware.Auth(v, revoker))
	private.GET("/me", func(c *gin.Context) {
		response.OK(c, gin.H{"uid": middleware.MustUserID(c)})
	})
	return r
}

// 被吊销的令牌必须被拒。
func TestAuth_RevokedTokenRejected(t *testing.T) {
	v := &claimsVerifier{uid: 42, jti: "JTI-1", iat: time.Now()}
	rev := &fakeRevoker{revoked: true}
	r := newClaimsEngine(v, rev)

	w := do(r, "Bearer sometoken")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, 期望 401 —— 被吊销的令牌不能放行", w.Code)
	}
	if got := decode(t, w); got.Code != response.CodeUnauthorized {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeUnauthorized)
	}
}

// ★ 被吊销时的文案必须和其它失败**完全一样**。
//
// 仓库既有的规矩:令牌过期、签名不对、用户被禁用,对外统一都是
// "登录状态无效",免得成了探测工具。"已吊销"是这个列表上的第四项,
// 分开说的话,「这个 token 是被登出的还是被禁用的」就成了一条
// 可观测的信息 —— 和账号枚举是同一类问题。
func TestAuth_RevokedMessageIsGeneric(t *testing.T) {
	v := &claimsVerifier{uid: 42, jti: "JTI-1", iat: time.Now()}
	rev := &fakeRevoker{revoked: true}
	r := newClaimsEngine(v, rev)

	w := do(r, "Bearer sometoken")
	got := decode(t, w)

	lower := strings.ToLower(got.Message)
	for _, leak := range []string{"吊销", "revoked", "登出", "logout", "禁用", "disabled"} {
		if strings.Contains(lower, leak) {
			t.Errorf("响应文案泄露了吊销细节 %q: %s", leak, got.Message)
		}
	}
	// 文案应该和"令牌无效"时的那句一样。
	if got.Message != "登录状态无效或已过期" {
		t.Errorf("文案 = %q,期望和别的失败一样是 %q", got.Message, "登录状态无效或已过期")
	}
}

// 不注入 revoker 时照常放行 —— 那是"没启用吊销",不是故障。
func TestAuth_RevokerNilAllows(t *testing.T) {
	v := &claimsVerifier{uid: 42, jti: "JTI-1", iat: time.Now()}
	r := newClaimsEngine(v, nil)

	w := do(r, "Bearer sometoken")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 期望 200", w.Code)
	}
}

// ★ revoker 报错时**放行**(fail open)。
//
// 判据是「前一道防线还在不在」:JWT 签名校验已经过了,身份是可信的;
// 吊销只是在一个已验证的身份上再加一层,它失灵不代表身份可疑。
//
// fail closed 会让一次 Redis 抖动变成全站登不进去。
//
// 注意这和 Verifier == nil 的 fail closed **不矛盾**:那是接线错误,
// 放行等于私有接口彻底裸奔。
func TestAuth_RevokerErrorAllows(t *testing.T) {
	v := &claimsVerifier{uid: 42, jti: "JTI-1", iat: time.Now()}
	rev := &fakeRevoker{err: errors.New("redis 连不上")}
	r := newClaimsEngine(v, rev)

	w := do(r, "Bearer sometoken")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 期望 200 —— 吊销查询失败不该把用户踢出去", w.Code)
	}
	if rev.queryCount != 1 {
		t.Errorf("查了 %d 次吊销状态,期望 1 次", rev.queryCount)
	}
}

// 吊销检查要把 jti 和 userID 传下去 —— 少了任何一个都判不准。
func TestAuth_PassesJTIAndUserID(t *testing.T) {
	v := &claimsVerifier{uid: 42, jti: "JTI-1", iat: time.Now()}
	rev := &fakeRevoker{}
	r := newClaimsEngine(v, rev)

	do(r, "Bearer sometoken")

	if rev.gotJTI != "JTI-1" {
		t.Errorf("传给 revoker 的 jti = %q,期望 %q", rev.gotJTI, "JTI-1")
	}
	if rev.gotUserID != 42 {
		t.Errorf("传给 revoker 的 userID = %d,期望 42", rev.gotUserID)
	}
}

// 校验器只实现了 TokenVerifier(拿不到 jti)时,不做吊销检查但照常放行。
//
// 这条路径对应"没实现 ClaimsVerifier 的实现方" —— 中间件不能因此拒绝请求。
func TestAuth_WithoutClaimsVerifierSkipsRevocation(t *testing.T) {
	rev := &fakeRevoker{revoked: true}
	// plainVerifier 只实现 Verify,没有 VerifyClaims。
	r := newEngineWithRevoker(&plainVerifier{uid: 42}, rev)

	w := do(r, "Bearer sometoken")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 期望 200 —— 拿不到 jti 时不该拦", w.Code)
	}
	if rev.queryCount != 0 {
		t.Errorf("查了 %d 次吊销状态,期望 0 次(没有 jti 就没法查)", rev.queryCount)
	}
}

// 校验失败时**不查**吊销名单。
//
// 前一道防线没过,查了也没意义 —— 而且吊销查询在关键路径上,
// 不该给无效请求花这个 RTT。
func TestAuth_SkipsRevocationWhenVerifyFails(t *testing.T) {
	v := &claimsVerifier{fail: true}
	rev := &fakeRevoker{}
	r := newClaimsEngine(v, rev)

	w := do(r, "Bearer badtoken")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, 期望 401", w.Code)
	}
	if rev.queryCount != 0 {
		t.Errorf("令牌无效时还查了 %d 次吊销状态", rev.queryCount)
	}
}

// jti 和 exp 要放进 context,登出接口靠它们吊销。
func TestAuth_PutsClaimsInContext(t *testing.T) {
	iat := time.Now()
	exp := iat.Add(time.Hour)
	v := &claimsVerifier{uid: 42, jti: "JTI-1", iat: iat, exp: exp}

	var gotJTI string
	var gotExp time.Time
	r := gin.New()
	r.Use(middleware.TraceID())
	private := r.Group("")
	private.Use(middleware.Auth(v, nil))
	private.GET("/me", func(c *gin.Context) {
		gotJTI, _ = middleware.TokenJTI(c)
		gotExp, _ = middleware.TokenExpiresAt(c)
		response.OK(c, gin.H{})
	})

	do(r, "Bearer sometoken")

	if gotJTI != "JTI-1" {
		t.Errorf("context 里的 jti = %q,期望 %q", gotJTI, "JTI-1")
	}
	if !gotExp.Equal(exp) {
		t.Errorf("context 里的 exp = %v,期望 %v", gotExp, exp)
	}
}

// 适配器:pkg/token.Manager 包一层之后,既能拿 userID 也能拿完整声明。
func TestNewClaimsAdapter_WrapsTokenManager(t *testing.T) {
	mgr := newManager(t)
	adapter := middleware.NewClaimsAdapter(mgr)
	if adapter == nil {
		t.Fatal("适配器不该是 nil")
	}

	tok, _, err := mgr.Issue(42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	claims, err := adapter.(middleware.ClaimsVerifier).VerifyClaims(context.Background(), tok)
	if err != nil {
		t.Fatalf("VerifyClaims: %v", err)
	}
	if claims.UserID != 42 {
		t.Errorf("uid = %d,期望 42", claims.UserID)
	}
	if claims.JTI == "" {
		t.Error("claims 里必须有 jti")
	}
	if claims.ExpiresAt.IsZero() {
		t.Error("claims 里必须有过期时刻")
	}
}

// NewClaimsAdapter(nil) 返回 nil —— 接线错误要在中间件那里 fail closed。
func TestNewClaimsAdapter_NilReturnsNil(t *testing.T) {
	if middleware.NewClaimsAdapter(nil) != nil {
		t.Error("NewClaimsAdapter(nil) 必须返回 nil")
	}
}

// 没实现 ClaimsVerifier 的校验器:走 Verify 那条老路。
var _ middleware.TokenVerifier = (*plainVerifier)(nil)

type plainVerifier struct{ uid int64 }

func (v *plainVerifier) Verify(context.Context, string) (int64, error) { return v.uid, nil }

// 编译期钉住:token.Manager 满足 middleware.TokenVerifier。
var _ middleware.TokenVerifier = (*token.Manager)(nil)
