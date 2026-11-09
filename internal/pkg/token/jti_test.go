package token

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ★ jti 现在是签发的必需项。
//
// 以前 jti 生成失败就用空串兜底,理由是它只用于日志。
// 有了吊销机制之后那个理由不成立了:**没有 jti 的 token 吊销不掉** ——
// 你没有任何东西可以拿去写进黑名单,等于发了一张注销不掉的卡。
//
// 这组用例守的就是这个转变。

// 正常签发出来的 token,jti 必须非空。
func TestIssue_JTIAlwaysPresent(t *testing.T) {
	m := newTestManager(t)

	tok, _, err := m.Issue(1)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	jti, _ := payloadOf(t, tok)["jti"].(string)
	if jti == "" {
		t.Fatal("签发出来的 token 必须带 jti")
	}
}

// jti 生成失败必须让整个签发失败,而且**不能返回 token**。
//
// 返回一个"能用但吊销不掉"的令牌比直接失败糟糕得多。
func TestIssue_JTIFailureIsFatal(t *testing.T) {
	m := newTestManager(t)
	m.newJTI = func() (string, error) {
		return "", errors.New("熵源挂了")
	}

	tok, exp, err := m.Issue(1)
	if err == nil {
		t.Fatal("jti 生成失败必须让 Issue 失败")
	}
	if tok != "" {
		t.Error("签发失败时不能返回令牌")
	}
	if !exp.IsZero() {
		t.Error("签发失败时不能返回过期时刻")
	}
}

// jti 生成器返回空串也算失败 —— 空串和"没有 jti"是一回事。
func TestIssue_EmptyJTIIsFatal(t *testing.T) {
	m := newTestManager(t)
	m.newJTI = func() (string, error) {
		return "", nil
	}

	tok, _, err := m.Issue(1)
	if err == nil {
		t.Fatal("空 jti 必须让 Issue 失败")
	}
	if tok != "" {
		t.Error("签发失败时不能返回令牌")
	}
}

// ★ 校验侧必须拒绝没有 jti 的 token。
//
// 这是**部署期的关键防线**:升级上线的那一刻,外面流通着一批老 token,
// 它们没有 jti,吊销不了。校验侧拒绝它们,等于强制所有人重新登录一次。
//
// 这是个有意的部署决策,不是 bug —— 发版说明里必须写「所有用户需重新登录」。
func TestVerify_RejectsTokenWithoutJTI(t *testing.T) {
	m := newTestManager(t)

	// 手工构造一个**没有 jti** 但签名完全合法的 token。
	// 用真实密钥签,所以签名校验会过 —— 要被拒的只能是"缺 jti"这一条。
	now := time.Now()
	claims := Claims{
		UID: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "mini-agent",
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			// 故意不设 ID(jti)
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造测试令牌: %v", err)
	}

	if _, err := m.Verify(context.Background(), tok); err == nil {
		t.Fatal("没有 jti 的令牌必须被拒绝 —— 它吊销不掉")
	}
}

// VerifyClaims 要返回完整声明,吊销检查需要 jti 和 iat。
func TestVerifyClaims_ReturnsFullClaims(t *testing.T) {
	m := newTestManager(t)

	tok, _, err := m.Issue(42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	claims, err := m.VerifyClaims(context.Background(), tok)
	if err != nil {
		t.Fatalf("VerifyClaims: %v", err)
	}
	if claims.UID != 42 {
		t.Errorf("UID = %d,期望 42", claims.UID)
	}
	if claims.ID == "" {
		t.Error("claims 里必须带 jti")
	}
	if claims.IssuedAt == nil {
		t.Error("claims 里必须带 iat")
	}
}

// Verify 和 VerifyClaims 必须给出一致的结论 —— 不能有两份校验实现。
func TestVerify_AndVerifyClaims_Agree(t *testing.T) {
	m := newTestManager(t)

	tok, _, err := m.Issue(7)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	uid, err := m.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	claims, err := m.VerifyClaims(context.Background(), tok)
	if err != nil {
		t.Fatalf("VerifyClaims: %v", err)
	}
	if uid != claims.UID {
		t.Errorf("两个方法给出的 uid 不一致:%d vs %d", uid, claims.UID)
	}
}
