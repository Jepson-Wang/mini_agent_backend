package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 32 字节,和 config 的下限一致

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(testSecret, "mini-agent", time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// payloadOf 把令牌中段解出来,用于断言载荷里到底有哪些声明。
func payloadOf(t *testing.T, tokenStr string) map[string]any {
	t.Helper()
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		t.Fatalf("令牌应有 3 段,实际 %d 段", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解 base64: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解 JSON: %v", err)
	}
	return m
}

func TestIssueVerify_RoundTrip(t *testing.T) {
	m := newTestManager(t)

	tok, expiresAt, err := m.Issue(42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	uid, err := m.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if uid != 42 {
		t.Fatalf("uid = %d, 期望 42", uid)
	}

	// 过期时刻应当是签发时刻加上 TTL,允许一点执行耗时。
	if d := time.Until(expiresAt); d > time.Hour || d < time.Hour-5*time.Second {
		t.Fatalf("距过期还有 %v, 期望接近 1h", d)
	}
}

// 要求:claims 至少含 uid / exp。
func TestIssue_ClaimsContainUIDAndExp(t *testing.T) {
	m := newTestManager(t)

	tok, _, err := m.Issue(7)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	p := payloadOf(t, tok)

	uid, ok := p["uid"]
	if !ok {
		t.Fatalf("载荷缺少 uid,实际字段: %v", p)
	}
	// JSON 数字解成 float64。
	if n, isNum := uid.(float64); !isNum || int64(n) != 7 {
		t.Fatalf("uid = %#v, 期望 7", uid)
	}

	if _, ok := p["exp"]; !ok {
		t.Fatalf("载荷缺少 exp,实际字段: %v", p)
	}
	for _, k := range []string{"iss", "iat", "nbf", "jti"} {
		if _, ok := p[k]; !ok {
			t.Errorf("载荷缺少 %s", k)
		}
	}
	// 载荷是 base64 不是加密,任何人都能看。这里确认没塞任何敏感信息。
	for k := range p {
		switch k {
		case "uid", "exp", "iss", "iat", "nbf", "jti":
		default:
			t.Errorf("载荷里出现了预期之外的声明 %q,确认它不含敏感信息", k)
		}
	}
}

func TestVerify_Expired(t *testing.T) {
	m := newTestManager(t)

	// 把签发时间挪到两小时前,TTL 一小时,签出来就是过期的。
	m.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	tok, _, err := m.Issue(42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	m.now = time.Now
	_, err = m.Verify(context.Background(), tok)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("err = %v, 期望 ErrExpiredToken", err)
	}
	// 过期和"非法"必须能分开,前端才知道该走续期还是踢回登录页。
	if errors.Is(err, ErrInvalidToken) {
		t.Fatal("过期不该同时匹配 ErrInvalidToken")
	}
}

func TestVerify_NotExpiredWithinLeeway(t *testing.T) {
	m := newTestManager(t)

	// 刚过期几秒,应当被时钟偏差容忍。多实例部署时机器差几秒很常见。
	m.now = func() time.Time { return time.Now().Add(-time.Hour - 5*time.Second) }
	tok, _, err := m.Issue(42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	m.now = time.Now
	if _, err := m.Verify(context.Background(), tok); err != nil {
		t.Fatalf("偏差 5s(容忍 %v)不该被拒: %v", Leeway, err)
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	m := newTestManager(t)
	tok, _, err := m.Issue(42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	other, err := NewManager("ffffffffffffffffffffffffffffffff", "mini-agent", time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := other.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, 期望 ErrInvalidToken", err)
	}
}

// alg 混淆攻击:把头部改成 {"alg":"none"} 并去掉签名。
// 不限定算法白名单的话,这种令牌会被当成合法的。
func TestVerify_RejectsAlgNone(t *testing.T) {
	m := newTestManager(t)

	claims := Claims{
		UID: 999,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "mini-agent",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	forged, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("构造伪造令牌: %v", err)
	}

	uid, err := m.Verify(context.Background(), forged)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("alg=none 的令牌被接受了(uid=%d, err=%v)", uid, err)
	}
}

// 同样是 alg 混淆:换一个 HMAC 变种。白名单只放 HS256,HS512 也要拒。
func TestVerify_RejectsOtherAlg(t *testing.T) {
	m := newTestManager(t)

	claims := Claims{
		UID: 999,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "mini-agent",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造令牌: %v", err)
	}

	if _, err := m.Verify(context.Background(), forged); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("HS512 的令牌被接受了: err=%v", err)
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	other, err := NewManager(testSecret, "someone-else", time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	tok, _, err := other.Issue(42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// 密钥相同但签发方不同,不能通过 —— 否则多个服务共用密钥时会互相串。
	m := newTestManager(t)
	if _, err := m.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, 期望 ErrInvalidToken", err)
	}
}

// 没有 exp 的令牌永远有效,必须拒。
func TestVerify_RejectsMissingExp(t *testing.T) {
	m := newTestManager(t)

	claims := Claims{
		UID:              42,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "mini-agent"},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造令牌: %v", err)
	}

	if _, err := m.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("无 exp 的令牌被接受了: err=%v", err)
	}
}

// 签名合法不代表载荷有意义:uid 缺失时反序列化出来是 0,
// 放过去的话下游会拿 userID=0 查数据。
func TestVerify_RejectsMissingUID(t *testing.T) {
	m := newTestManager(t)

	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer:    "mini-agent",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造令牌: %v", err)
	}

	if _, err := m.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("无 uid 的令牌被接受了: err=%v", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	m := newTestManager(t)

	for name, bad := range map[string]string{
		"空串":   "",
		"不是令牌": "hello",
		"段数不对": "a.b",
		"载荷被篡改": func() string {
			tok, _, err := m.Issue(1)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			parts := strings.Split(tok, ".")
			// 换成 {"uid":99999,...} 的载荷,签名保持原样。
			p := payloadOf(t, tok)
			p["uid"] = 99999
			raw, _ := json.Marshal(p)
			parts[1] = base64.RawURLEncoding.EncodeToString(raw)
			return strings.Join(parts, ".")
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Verify(context.Background(), bad); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, 期望 ErrInvalidToken", err)
			}
		})
	}
}

func TestNewManager_Validation(t *testing.T) {
	if _, err := NewManager("", "mini-agent", time.Hour); err == nil {
		t.Error("空密钥应当报错 —— 它能签出人人可伪造的令牌")
	}
	if _, err := NewManager(testSecret, "", time.Hour); err == nil {
		t.Error("空 issuer 应当报错")
	}
	if _, err := NewManager(testSecret, "mini-agent", 0); err == nil {
		t.Error("TTL 为 0 应当报错")
	}
	if _, err := NewManager(testSecret, "mini-agent", -time.Hour); err == nil {
		t.Error("负数 TTL 应当报错")
	}
}

func TestIssue_RejectsInvalidUserID(t *testing.T) {
	m := newTestManager(t)
	for _, uid := range []int64{0, -1} {
		if _, _, err := m.Issue(uid); err == nil {
			t.Errorf("Issue(%d) 应当报错", uid)
		}
	}
}

// 每次签发的 jti 必须不同,否则将来做吊销名单时会误伤。
func TestIssue_UniqueJTI(t *testing.T) {
	m := newTestManager(t)

	a, _, err := m.Issue(1)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	b, _, err := m.Issue(1)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if payloadOf(t, a)["jti"] == payloadOf(t, b)["jti"] {
		t.Fatal("两次签发的 jti 相同")
	}
}
