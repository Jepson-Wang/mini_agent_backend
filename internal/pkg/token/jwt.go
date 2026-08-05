// Package token 负责访问令牌的签发与校验。
//
// 包名叫 token 而不是 jwt,是为了不和 github.com/golang-jwt/jwt/v5 撞名字;
// 顺带也说明了对上层的承诺:上层只知道"令牌",不知道底下是 JWT 还是别的。
// *Manager 直接实现了 middleware.TokenVerifier,换成查 session 表的实现
// 不用动中间件。
package token

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
)

// signingAlg 固定用 HS256,对称密钥,单体服务够用。
//
// 要换成 RS256(公钥下发给别的服务自行校验)的话,得同时改签发和校验两侧,
// 而且 Verify 里的白名单也要跟着改 —— 那份白名单是安全边界,不是可选项。
const signingAlg = "HS256"

// leeway 允许的时钟偏差。签发方和校验方是同一个进程时用不上,
// 但多实例部署时机器间差几秒很常见,不留余量会出现"刚签出来就说没生效"。
const leeway = 30 * time.Second

var (
	// ErrInvalidToken 令牌不合法:格式错、签名不对、签发方不匹配、uid 缺失等。
	// 对客户端一律说"登录状态无效",不区分具体哪种 —— 区分了就是在帮人调试伪造。
	ErrInvalidToken = errors.New("invalid token")

	// ErrExpiredToken 令牌过期。单独拎出来是给客户端做静默续期用的:
	// 前端可以据此判断该走刷新流程,而不是把用户直接踢到登录页。
	// 注意对外的响应文案仍然和 ErrInvalidToken 一致。
	ErrExpiredToken = errors.New("token expired")
)

// Claims 是令牌载荷。
//
// uid 是自定义声明,没有复用标准的 sub:sub 是字符串,存 int64 得来回转,
// 两个字段表示同一件事迟早会不一致。这里只留一个来源。
type Claims struct {
	UID int64 `json:"uid"`
	jwt.RegisteredClaims
}

// Manager 签发和校验访问令牌。构造后只读,可以并发使用。
type Manager struct {
	secret []byte
	issuer string
	ttl    time.Duration

	// now 可替换,测试里用来伪造时间验证过期行为,生产上就是 time.Now。
	now func() time.Time
}

// NewManager 构造签发器。
//
// 密钥长度由 config 层把关(minSecretLen),这里只兜空值 ——
// 空密钥能签出结构完全合法、任何人都能伪造的令牌,必须在启动阶段就炸掉。
func NewManager(secret, issuer string, ttl time.Duration) (*Manager, error) {
	if secret == "" {
		return nil, errors.New("token: 签名密钥不能为空")
	}
	if issuer == "" {
		return nil, errors.New("token: issuer 不能为空")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("token: 有效期必须大于 0,当前为 %v", ttl)
	}
	return &Manager{
		secret: []byte(secret),
		issuer: issuer,
		ttl:    ttl,
		now:    time.Now,
	}, nil
}

// TTL 返回配置的有效期,给登录响应里的 expires_in 用。
func (m *Manager) TTL() time.Duration { return m.ttl }

// Issue 为 userID 签发一个访问令牌,同时返回它的过期时刻。
//
// 返回过期时刻是为了让登录接口能直接把它放进响应体,
// 省得客户端自己去解 JWT —— 客户端解出来的东西不该被信任,但可以拿来做 UI。
func (m *Manager) Issue(userID int64) (string, time.Time, error) {
	if userID <= 0 {
		// 拿 0 当用户 id 签出去的令牌,校验时会通过,然后拿去查一个不存在的用户。
		return "", time.Time{}, fmt.Errorf("token: 非法的 userID %d", userID)
	}

	now := m.now()
	expiresAt := now.Add(m.ttl)

	// jti 失败不影响签发 —— 它只用于日志追踪和将来做吊销名单,不是必需声明。
	jti, err := ulid.New()
	if err != nil {
		jti = ""
	}

	claims := Claims{
		UID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token: 签名失败: %w", err)
	}
	return signed, expiresAt, nil
}

// Verify 校验令牌并返回它代表的用户 id。实现 middleware.TokenVerifier。
//
// ctx 现在用不上,留着是因为接口里有 —— 换成查 session 表或调认证服务的实现时
// 必须能传超时下去。
func (m *Manager) Verify(_ context.Context, tokenStr string) (int64, error) {
	var claims Claims

	_, err := jwt.ParseWithClaims(tokenStr, &claims,
		func(*jwt.Token) (any, error) { return m.secret, nil },

		// 这一行是整个校验里最要紧的。不限定算法的话:
		//   1. 攻击者把头部改成 {"alg":"none"} 去掉签名;
		//   2. 或者服务端用 RS256 时,攻击者改成 HS256,
		//      让服务拿"公钥"当 HMAC 密钥去验 —— 而公钥是公开的。
		// 白名单在 keyfunc 之前执行,不匹配直接拒。
		jwt.WithValidMethods([]string{signingAlg}),

		// 不加这个的话,一个没有 exp 的令牌永远有效。
		jwt.WithExpirationRequired(),

		// 核对签发方,防止别处签的令牌拿到这儿来用。
		jwt.WithIssuer(m.issuer),

		jwt.WithIssuedAt(),
		jwt.WithLeeway(leeway),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		// 过期是最常见的一种,单独区分;其余全部归为非法。
		// 原始 err 用 %v 拼进去只为了写日志,不会回给客户端(见 middleware.Auth)。
		if errors.Is(err, jwt.ErrTokenExpired) {
			return 0, fmt.Errorf("%w: %v", ErrExpiredToken, err)
		}
		return 0, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	// 签名合法不代表载荷有意义:uid 可能压根没带,反序列化后就是零值。
	// 不拦的话下游会拿着 userID=0 去查数据。
	if claims.UID <= 0 {
		return 0, fmt.Errorf("%w: uid 缺失或非法", ErrInvalidToken)
	}
	return claims.UID, nil
}
