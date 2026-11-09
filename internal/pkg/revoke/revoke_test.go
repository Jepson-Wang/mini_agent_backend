package revoke

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTest 起一个内存 Redis 和接在它上面的吊销器。
//
// 用 miniredis:它支持 TTL 和 MGET,而且不依赖外部服务 ——
// 把一条安全规则押在"没有 Redis 就会被跳过"的用例上等于没测,
// 同 pkg/redislock、pkg/ratelimit 的取向。
//
// 返回的 *miniredis.Miniredis 用来推进过期(FastForward)。
func newTest(t *testing.T) (*miniredis.Miniredis, *Revoker) {
	t.Helper()
	s := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return s, New(c, 2*time.Hour)
}

const testJTI = "01J8Z3K9RQ7M2N4P6T8V0X2Y4A"

// 基本正确性:吊销之后 IsRevoked 返回 true。
func TestRevokeToken_ThenRevoked(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	exp := time.Now().Add(30 * time.Minute)
	if err := r.RevokeToken(ctx, testJTI, exp); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	revoked, err := r.IsRevoked(ctx, testJTI, 1, time.Now())
	if err != nil {
		t.Fatalf("查询不该报错: %v", err)
	}
	if !revoked {
		t.Error("吊销后应该判定为已吊销")
	}
}

// ★ 黑名单的 TTL 必须等于 token 的剩余有效期。
//
// 这是整个包最要紧的一条。写死一个固定 TTL 的实现能过上一条,
// 然后在生产上要么漏吊销(TTL 短了)要么泄内存(TTL 长了)。
func TestRevokeToken_TTLMatchesExpiry(t *testing.T) {
	s, r := newTest(t)
	ctx := context.Background()

	// 一个 30 分钟后过期的 token。
	exp := time.Now().Add(30 * time.Minute)
	if err := r.RevokeToken(ctx, testJTI, exp); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	ttl := s.TTL(tokenKeyPrefix + testJTI)
	// miniredis 的 TTL 精度是秒,给一点余量。
	if ttl <= 29*time.Minute || ttl > 30*time.Minute {
		t.Errorf("黑名单的 TTL 是 %v,期望落在 (29m, 30m] 区间 —— "+
			"短了吊销会静默失效,长了纯泄内存", ttl)
	}
}

// 已经过期的 token 不用写黑名单。
//
// 不挡的话,SET k v EX -1 在 Redis 上会直接报错。
func TestRevokeToken_ExpiredIsNoop(t *testing.T) {
	s, r := newTest(t)
	ctx := context.Background()

	// 一个已经过期的 token。
	exp := time.Now().Add(-time.Minute)
	if err := r.RevokeToken(ctx, testJTI, exp); err != nil {
		t.Fatalf("吊销已过期的 token 不该报错: %v", err)
	}

	if keys := s.Keys(); len(keys) != 0 {
		t.Errorf("已过期的 token 不该写任何 key,实际写了 %v", keys)
	}
}

// 吊销项会自己过期 —— token 过期后黑名单就没有存在的必要了。
func TestRevokeToken_AutoExpires(t *testing.T) {
	s, r := newTest(t)
	ctx := context.Background()

	exp := time.Now().Add(30 * time.Minute)
	if err := r.RevokeToken(ctx, testJTI, exp); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	// 快进到 token 过期之后。
	s.FastForward(31 * time.Minute)

	revoked, err := r.IsRevoked(ctx, testJTI, 1, time.Now())
	if err != nil {
		t.Fatalf("查询不该报错: %v", err)
	}
	if revoked {
		t.Error("token 自己都过期了,黑名单也该跟着消失")
	}
}

// 用户级吊销:早于 not_before 的 token 判为已吊销。
func TestRevokeUser_AffectsOlderTokens(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	now := time.Now()
	if err := r.RevokeUser(ctx, 42, time.Hour); err != nil {
		t.Fatalf("吊销用户不该报错: %v", err)
	}

	// 一个在 not_before 之前签发的 token。
	revoked, err := r.IsRevoked(ctx, testJTI, 42, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("查询不该报错: %v", err)
	}
	if !revoked {
		t.Error("早于 not_before 签发的 token 应该判为已吊销")
	}
}

// ★ 用户级吊销不能影响之后签发的 token。
//
// **比较方向写反的话,用户改完密码之后新登录的 token 也立刻失效** ——
// 表现是「登录成功但马上又被踢」的死循环,单用户手工测试时很难
// 第一时间联想到吊销逻辑。
func TestRevokeUser_DoesNotAffectNewerTokens(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	now := time.Now()
	if err := r.RevokeUser(ctx, 42, time.Hour); err != nil {
		t.Fatalf("吊销用户不该报错: %v", err)
	}

	// 一个在 not_before 之后签发的 token。
	revoked, err := r.IsRevoked(ctx, testJTI, 42, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("查询不该报错: %v", err)
	}
	if revoked {
		t.Error("晚于 not_before 签发的 token 不该被吊销 —— " +
			"写反的话用户改完密码后新登录的 token 也会立刻失效")
	}
}

// 单 token 黑名单和用户级 not_before 是两套独立的机制,不能互相影响。
func TestRevokeToken_DoesNotAffectOtherUser(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	exp := time.Now().Add(30 * time.Minute)
	if err := r.RevokeToken(ctx, testJTI, exp); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	// 另一个用户、同一个 jti(现实中不会发生,但判据要独立)。
	revoked, err := r.IsRevoked(ctx, testJTI, 99, time.Now())
	if err != nil {
		t.Fatalf("查询不该报错: %v", err)
	}
	if !revoked {
		t.Error("jti 在黑名单里就该判定为已吊销 —— 黑名单是按 jti 的,不看用户")
	}
}

// 没吊销过的 token 不该被判为已吊销。
func TestIsRevoked_NotRevoked(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	revoked, err := r.IsRevoked(ctx, "SOME-OTHER-JTI", 1, time.Now())
	if err != nil {
		t.Fatalf("查询不该报错: %v", err)
	}
	if revoked {
		t.Error("没吊销过的 token 不该被判为已吊销")
	}
}

// nil 接收者:没配 Redis 的部署一律未吊销,而且不 panic。
//
// 准则一:nil 安全放在方法上,不在调用方。
func TestIsRevoked_NilIsNotRevoked(t *testing.T) {
	var r *Revoker
	revoked, err := r.IsRevoked(context.Background(), testJTI, 1, time.Now())
	if err != nil {
		t.Fatalf("nil 接收者不该报错: %v", err)
	}
	if revoked {
		t.Error("nil 接收者必须判为未吊销")
	}
}

// nil 接收者的写操作也不该报错(defer 之类的路径要能安全跑)。
func TestRevoke_NilIsNoop(t *testing.T) {
	var r *Revoker
	ctx := context.Background()
	if err := r.RevokeToken(ctx, testJTI, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("nil 接收者不该报错: %v", err)
	}
	if err := r.RevokeUser(ctx, 1, time.Hour); err != nil {
		t.Fatalf("nil 接收者不该报错: %v", err)
	}
}

// Redis 挂了 → 未吊销 + 非 nil err。
//
// **fail open**:JWT 签名校验已经过了,身份是可信的,
// 吊销只是在一个已验证的身份上再加一层。它失灵不代表身份可疑。
func TestIsRevoked_RedisDownIsNotRevoked(t *testing.T) {
	s, r := newTest(t)
	ctx := context.Background()

	s.Close() // Redis 挂了

	revoked, err := r.IsRevoked(ctx, testJTI, 1, time.Now())
	if err == nil {
		t.Error("Redis 挂了必须返回错误 —— 调用方要靠它记日志")
	}
	if revoked {
		t.Error("Redis 挂了必须判为未吊销(fail open)")
	}
}

// New(nil) 返回 nil,表示"这个部署没有 Redis"。
func TestNew_NilClientReturnsNil(t *testing.T) {
	if r := New(nil, time.Hour); r != nil {
		t.Error("New(nil) 必须返回 nil")
	}
}

// 用户级 key 也要有 TTL —— 不设的话每个改过密码的用户都永久占一个 key。
func TestRevokeUser_HasTTL(t *testing.T) {
	s, r := newTest(t)
	ctx := context.Background()

	if err := r.RevokeUser(ctx, 42, time.Hour); err != nil {
		t.Fatalf("吊销用户不该报错: %v", err)
	}

	ttl := s.TTL(userKeyPrefix + "42")
	if ttl <= 0 {
		t.Fatalf("用户级 key 必须有 TTL,实际是 %v", ttl)
	}
	if ttl > time.Hour {
		t.Errorf("用户级 key 的 TTL 是 %v,期望不超过 1h", ttl)
	}
}

// 非法的 userID 要报错,不能拿它当 key 写进 Redis。
func TestRevokeUser_RejectsBadUserID(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	if err := r.RevokeUser(ctx, 0, time.Hour); err == nil {
		t.Error("userID <= 0 必须报错")
	}
}

// 空 jti 不能吊销 —— 没有它就没有黑名单的 key 可写。
func TestRevokeToken_RejectsEmptyJTI(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	if err := r.RevokeToken(ctx, "", time.Now().Add(time.Hour)); err == nil {
		t.Error("空 jti 必须报错")
	}
}
