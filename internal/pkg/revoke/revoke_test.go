package revoke

import (
	"context"
	"strconv"
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
	// 第三个参数是校验侧的时钟偏差余量。生产里传 token.Leeway,
	// 测试里传 0 —— 让「黑名单 TTL 对齐 exp」这条断言是精确的。
	return s, New(c, 2*time.Hour, 0)
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

// 新建一个带校验余量的吊销器。
func newTestWithLeeway(t *testing.T, leeway time.Duration) (*miniredis.Miniredis, *Revoker) {
	t.Helper()
	s := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return s, New(c, 2*time.Hour, leeway)
}

// ★ 黑名单的 TTL 必须覆盖校验侧的时钟偏差余量。
//
// 校验带着 30s 余量(token.Leeway):一个 token 到 exp + 30s 之前都还验得过。
// 而黑名单的 TTL 如果只算到 exp,中间那 30 秒里 token 有效、黑名单却已经
// 没了 —— **登出静默失效**,而且没有任何迹象。
//
// 这条测的是「余量被计入 TTL」,不是「TTL 等于 exp」——
// 生产上传的就是 token.Leeway,两者是同一个数。
func TestRevokeToken_TTLIncludesVerifyLeeway(t *testing.T) {
	const leeway = 30 * time.Second
	s, r := newTestWithLeeway(t, leeway)
	ctx := context.Background()

	exp := time.Now().Add(30 * time.Minute)
	if err := r.RevokeToken(ctx, testJTI, exp); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	ttl := s.TTL(tokenKeyPrefix + testJTI)
	// 期望落在 (30m, 30m+leeway] 区间:TTL 必须**覆盖** exp + leeway。
	if ttl <= 30*time.Minute {
		t.Errorf("黑名单的 TTL 是 %v,必须大于 30m —— "+
			"少了校验余量,那段时间里 token 还有效、黑名单却没了,登出会静默失效", ttl)
	}
	if ttl > 30*time.Minute+leeway {
		t.Errorf("黑名单的 TTL 是 %v,不应超过 30m + 余量(%v)", ttl, 30*time.Minute+leeway)
	}
}

// 余量期间 token 还有效,黑名单必须还在。
//
// 这是上面那条的实际后果:不补余量的话,登出之后的 30 秒里
// 那个 token 照样能用。
func TestRevokeToken_SurvivesDuringLeeway(t *testing.T) {
	const leeway = 30 * time.Second
	s, r := newTestWithLeeway(t, leeway)
	ctx := context.Background()

	exp := time.Now().Add(30 * time.Minute)
	if err := r.RevokeToken(ctx, testJTI, exp); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	// 快进到 token 已经过期、但还在校验余量里。
	s.FastForward(30*time.Minute + 10*time.Second)

	revoked, err := r.IsRevoked(ctx, testJTI, 1, time.Now())
	if err != nil {
		t.Fatalf("查询不该报错: %v", err)
	}
	if !revoked {
		t.Error("校验余量期间 token 还有效,黑名单必须还在 —— 不然登出会静默失效")
	}
}

// 空余量是合法的:按 0 处理,行为和「只算到 exp」一致。
func TestNew_ZeroLeewayIsAllowed(t *testing.T) {
	s := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	r := New(c, time.Hour, 0)
	if r == nil {
		t.Fatal("余量为 0 不该让 New 返回 nil")
	}
	_ = s
}

// New(nil) 返回 nil,表示"这个部署没有 Redis"。
func TestNew_NilClientReturnsNil(t *testing.T) {
	if r := New(nil, time.Hour, 0); r != nil {
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

// ★ 这一组盯的是 iat 和 not_before 的**精度不匹配**。
//
// JWT 的 iat 只有秒精度(golang-jwt 的 NewNumericDate 做
// t.Truncate(TimePrecision),TimePrecision 默认 time.Second),
// 而 not_before 存的是毫秒。原来直接存 r.now().UnixMilli(),于是
// 吊销发生在 12:00:05.200 时 nbf = ...5200,而同一秒内重新登录拿到的
// 新 token 的 iat 被截断成 ...5000 —— 5000 < 5200,刚签出来的票被判吊销。
//
// 现在 nbf 向上取整到整秒,行为从「毫秒位决定、看起来随机」变成
// 「同一秒内必然被吊销、下一秒必然恢复」。下面三条分别钉住这三件事:
// 边界在哪、取整方向是哪边、以及存储格式仍然是毫秒。

// 吊销那一秒**之后**签发的 token 必须正常 —— 这是最要紧的一条:
// 它保证「改完密码重新登录」在下一秒一定能用,不会变成死循环。
func TestRevokeUser_TokenFromNextSecondSurvives(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	// 吊销发生在 12:00:05.200 —— 刻意带一个非零的毫秒位,
	// 那正是原来触发 bug 的形状。
	at := time.Date(2026, 11, 10, 12, 0, 5, int(200*time.Millisecond), time.UTC)
	r.now = func() time.Time { return at }
	if err := r.RevokeUser(ctx, 7, time.Hour); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	// 用户在下一秒重新登录。iat 被 jwt 截断到整秒 → 12:00:06.000。
	iat := time.Date(2026, 11, 10, 12, 0, 6, 0, time.UTC)
	revoked, err := r.IsRevoked(ctx, "", 7, iat)
	if err != nil {
		t.Fatalf("查吊销不该报错: %v", err)
	}
	if revoked {
		t.Error("吊销那一秒之后签发的 token 被判成已吊销 —— " +
			"用户改完密码会陷入「登录成功又马上被踢」的死循环")
	}
}

// 吊销那一秒**之内**签发的 token 一律被吊销。
//
// 这是刻意选的 fail-closed 方向,不是遗漏:向下取整的话这张票的
// iat 恰好等于 nbf,而判据是 iat < nbf,于是它永远不算被吊销 ——
// 代价不是多活一秒,是多活它自己完整的 AccessTTL(2 小时)。
// 宁可让一次重新登录白跑一趟,也不能让改了密码/禁了账号却不生效。
func TestRevokeUser_TokenFromSameSecondIsRevoked(t *testing.T) {
	_, r := newTest(t)
	ctx := context.Background()

	at := time.Date(2026, 11, 10, 12, 0, 5, int(200*time.Millisecond), time.UTC)
	r.now = func() time.Time { return at }
	if err := r.RevokeUser(ctx, 7, time.Hour); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	// 同一秒内签发(无论在 .200 之前还是之后),iat 都是 12:00:05.000。
	iat := time.Date(2026, 11, 10, 12, 0, 5, 0, time.UTC)
	revoked, err := r.IsRevoked(ctx, "", 7, iat)
	if err != nil {
		t.Fatalf("查吊销不该报错: %v", err)
	}
	if !revoked {
		t.Error("吊销那一秒内签发的 token 没被吊销 —— " +
			"向下取整会让它活满整个 AccessTTL,改密码/禁用等于没生效")
	}
}

// not_before 存进 Redis 的值必须落在整秒上,而且**仍然是毫秒格式**。
//
// 格式不能改成秒:滚动发布期间 Redis 里还躺着旧的毫秒值,
// 按秒解析会得到一个巨大的数字,瞬间把所有人判成已吊销 ——
// 而那些 key 的 TTL 是 accessTTL(2 小时),窗口足够长。
func TestRevokeUser_StoredNotBeforeIsWholeSecondMillis(t *testing.T) {
	s, r := newTest(t)
	ctx := context.Background()

	at := time.Date(2026, 11, 10, 12, 0, 5, int(200*time.Millisecond), time.UTC)
	r.now = func() time.Time { return at }
	if err := r.RevokeUser(ctx, 7, time.Hour); err != nil {
		t.Fatalf("吊销不该报错: %v", err)
	}

	raw, err := s.Get(userKeyPrefix + "7")
	if err != nil {
		t.Fatalf("读 not_before: %v", err)
	}
	nbf, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("not_before 不是整数(存储格式变了?): %q", raw)
	}

	// 向上取整到 12:00:06.000。
	want := time.Date(2026, 11, 10, 12, 0, 6, 0, time.UTC).UnixMilli()
	if nbf != want {
		t.Errorf("not_before = %d, 期望 %d(向上取整到整秒的毫秒值)", nbf, want)
	}
	if nbf%1000 != 0 {
		t.Errorf("not_before = %d 不在整秒上 —— iat 只有秒精度,带毫秒位会误判", nbf)
	}
}
