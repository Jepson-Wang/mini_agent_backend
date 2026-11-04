package ratelimit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTest 起一个内存 Redis 和接在它上面的限流器。
//
// 用 miniredis 而不是真 Redis,是刻意的:它自带 gopher-lua,这个脚本在它
// 上面是真跑的。把一条防滥用规则押在"没有外部服务就会被跳过"的用例上,
// 等于没测 —— 同 internal/service/quota_test.go 的取向。
//
// 返回的 *miniredis.Miniredis 用来控时钟(SetTime)和推进过期(FastForward):
//
//   - **时间一律用 SetTime 推,不要 Sleep。** 脚本的时钟是 Redis 的 TIME,
//     而 miniredis 的 TIME 听 SetTime 的。Sleep 的用例又慢又飘。
//   - **过期只在 FastForward 时发生。** 验 PEXPIRE 的用例必须 FastForward
//     之后再看 PTTL / Exists,否则"去掉 PEXPIRE"这个变异不会让测试变红。
func newTest(t *testing.T) (*miniredis.Miniredis, *Limiter) {
	t.Helper()
	s := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return s, New(c)
}

// 固定一个基准时刻,让用例之间互不干扰也可读。
var base = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// 接收者为 nil = 这个部署没启用限流,必须放行。
//
// 这条让调用方不用到处写 if l != nil —— nil 安全在方法上,不在调用方,
// 同 service.TaskQuota.Check。
func TestAllow_NilLimiterAllows(t *testing.T) {
	var l *Limiter
	res, err := l.Allow(context.Background(), []Bucket{
		{Key: "k", Rule: Rule{Window: time.Minute, Limit: 1}, Scope: "ip"},
	})
	if err != nil {
		t.Fatalf("nil 接收者不该报错: %v", err)
	}
	if !res.Allowed {
		t.Error("nil 接收者必须放行")
	}
}

// 给 Limiter 换上确定的、互不相同的 member 后缀。
//
// 同一个桶里后缀一旦相同,ZSET 会去重,用例就在不知不觉中什么都没断言。
func counterMember(l *Limiter) {
	var n int
	l.member = func() string {
		n++
		return fmt.Sprintf("m%d", n)
	}
}

// 直连同一个 miniredis 的客户端,用来读 PTTL 之类。
func rawClient(t *testing.T, s *miniredis.Miniredis) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// card 返回 key 里的成员数;key 不存在返回 0。
func card(s *miniredis.Miniredis, key string) int {
	m, err := s.ZMembers(key)
	if err != nil {
		return 0
	}
	return len(m)
}

func one(key, scope string, window time.Duration, limit int) []Bucket {
	return []Bucket{{Key: key, Scope: scope, Rule: Rule{Window: window, Limit: limit}}}
}

// Limit <= 0 的桶必须被滤掉,且不在 Redis 里留下任何痕迹。
func TestAllow_ZeroLimitBucketIsSkipped(t *testing.T) {
	s, l := newTest(t)
	s.SetTime(base)
	counterMember(l)
	res, err := l.Allow(context.Background(), []Bucket{
		{Key: "zero", Scope: "off", Rule: Rule{Window: time.Minute, Limit: 0}},
		{Key: "real", Scope: "ip", Rule: Rule{Window: time.Minute, Limit: 5}},
	})
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !res.Allowed {
		t.Fatalf("Limit=0 的桶不该导致拒绝: %+v", res)
	}
	if s.Exists("zero") {
		t.Error("Limit=0 的桶不该在 Redis 里留下 key")
	}
	if card(s, "real") != 1 {
		t.Error("正常的桶应当记了一笔")
	}
}

// 全部桶都不限:放行,且根本不发 EVAL。
//
// rdb 故意给 nil —— 要是真发了 EVAL 会直接 panic,比只查 key 不存在更硬。
func TestAllow_AllBucketsDisabledAllows(t *testing.T) {
	s, _ := newTest(t)
	l := &Limiter{script: redis.NewScript(script), member: defaultMember}
	res, err := l.Allow(context.Background(), []Bucket{
		{Key: "a", Rule: Rule{Window: time.Minute, Limit: 0}},
		{Key: "b", Rule: Rule{Window: time.Minute, Limit: -1}},
	})
	if err != nil || !res.Allowed {
		t.Fatalf("全部不限应当放行: %+v, %v", res, err)
	}
	if s.Exists("a") || s.Exists("b") {
		t.Error("不该有任何 key")
	}
}

func TestAllow_EmptyBucketsAllows(t *testing.T) {
	l := &Limiter{script: redis.NewScript(script), member: defaultMember} // rdb=nil:发了 EVAL 就 panic
	for name, b := range map[string][]Bucket{"nil": nil, "empty": {}} {
		res, err := l.Allow(context.Background(), b)
		if err != nil || !res.Allowed {
			t.Errorf("%s: 应当放行: %+v, %v", name, res, err)
		}
	}
}

func TestAllow_WindowDisabledIsAnError(t *testing.T) {
	s, l := newTest(t)
	_, err := l.Allow(context.Background(), one("k", "ip-scope", 0, 5))
	if err == nil {
		t.Fatal("有额度却没窗口必须报错")
	}
	if !strings.Contains(err.Error(), "ip-scope") {
		t.Errorf("错误里应当带上 Scope: %v", err)
	}
	if s.Exists("k") {
		t.Error("报错路径不该写任何东西")
	}
}

func TestAllow_LimitIsExact(t *testing.T) {
	for _, limit := range []int{1, 5, 10, 30} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			s, l := newTest(t)
			s.SetTime(base)
			counterMember(l)
			b := one("k", "ip", time.Minute, limit)
			for i := 1; i <= limit; i++ {
				res, err := l.Allow(context.Background(), b)
				if err != nil || !res.Allowed {
					t.Fatalf("第 %d 次应当放行: %+v, %v", i, res, err)
				}
			}
			res, err := l.Allow(context.Background(), b)
			if err != nil {
				t.Fatal(err)
			}
			if res.Allowed {
				t.Fatalf("第 %d 次应当被拒", limit+1)
			}
			if got := card(s, "k"); got != limit {
				t.Errorf("被拒的尝试不该入账: card=%d want %d", got, limit)
			}
		})
	}
}

func TestAllow_WindowSlides(t *testing.T) {
	s, l := newTest(t)
	counterMember(l)
	b := one("k", "ip", time.Second, 2)
	s.SetTime(base)
	for i := 0; i < 2; i++ {
		if res, _ := l.Allow(context.Background(), b); !res.Allowed {
			t.Fatal("填充阶段应当放行")
		}
	}
	if res, _ := l.Allow(context.Background(), b); res.Allowed {
		t.Fatal("窗口内满了应当拒绝")
	}
	s.SetTime(base.Add(time.Second + time.Millisecond))
	if res, err := l.Allow(context.Background(), b); err != nil || !res.Allowed {
		t.Fatalf("窗口滑过之后应当放行: %+v, %v", res, err)
	}
}

// 这条是整个设计选滑动窗口的理由:固定窗口会在这里放过 2 倍突发。
func TestAllow_FixedWindowBoundaryBurstIsRejected(t *testing.T) {
	s, l := newTest(t)
	counterMember(l)
	const limit = 3
	b := one("k", "ip", time.Second, limit)
	s.SetTime(base.Add(999 * time.Millisecond))
	for i := 0; i < limit; i++ {
		if res, _ := l.Allow(context.Background(), b); !res.Allowed {
			t.Fatal("填充阶段应当放行")
		}
	}
	s.SetTime(base.Add(1001 * time.Millisecond)) // 跨过了整秒边界,但距上次才 2ms
	res, err := l.Allow(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("边界突发必须被拒:2ms 内不能过 2 倍的量")
	}
}

// 时钟冻住时 limit+1 次调用仍要拒掉最后一次 —— 这靠成员后缀唯一。
func TestAllow_SameMillisecondAttemptsAllCount(t *testing.T) {
	s, l := newTest(t)
	counterMember(l)
	s.SetTime(base)
	const limit = 3
	b := one("k", "ip", time.Minute, limit)
	for i := 0; i < limit; i++ {
		if res, _ := l.Allow(context.Background(), b); !res.Allowed {
			t.Fatalf("第 %d 次应当放行", i+1)
		}
	}
	if got := card(s, "k"); got != limit {
		t.Fatalf("同毫秒的尝试不能被去重: card=%d want %d", got, limit)
	}
	if res, _ := l.Allow(context.Background(), b); res.Allowed {
		t.Fatal("第 limit+1 次必须被拒")
	}
}

func TestAllow_KeyExpiresAfterWindow(t *testing.T) {
	s, l := newTest(t)
	s.SetTime(base)
	c := rawClient(t, s)
	ctx := context.Background()
	if res, err := l.Allow(ctx, one("k", "ip", 10*time.Second, 5)); err != nil || !res.Allowed {
		t.Fatalf("应当放行: %+v, %v", res, err)
	}
	ttl, err := c.PTTL(ctx, "k").Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Fatalf("记账后 key 必须带过期时间,PTTL=%v", ttl)
	}
	s.FastForward(10*time.Second + time.Millisecond)
	if s.Exists("k") {
		t.Error("窗口过去之后 key 应当消失,否则冷门维度会永远占内存")
	}
}

// 本包最重要的用例:被窄桶拒掉的请求,一个宽桶(全局)的名额都不许占。
func TestAllow_NarrowBucketRejectDoesNotConsumeGlobal(t *testing.T) {
	s, l := newTest(t)
	s.SetTime(base)
	counterMember(l)
	b := []Bucket{
		{Key: "wide", Scope: "global", Rule: Rule{Window: time.Minute, Limit: 10}},
		{Key: "narrow", Scope: "ip", Rule: Rule{Window: time.Minute, Limit: 2}},
	}
	for i := 0; i < 2; i++ {
		if res, err := l.Allow(context.Background(), b); err != nil || !res.Allowed {
			t.Fatalf("第 %d 次应当放行: %+v, %v", i+1, res, err)
		}
	}
	res, err := l.Allow(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("第 3 次应当被窄桶拒绝")
	}
	if got := card(s, "wide"); got != 2 {
		t.Fatalf("宽桶 ZCARD=%d,期望 2:被拒的请求占了全局名额", got)
	}
	if got := card(s, "narrow"); got != 2 {
		t.Errorf("窄桶 ZCARD=%d,期望 2", got)
	}
}

// 拒绝时,任何桶都不许多出成员。
func TestAllow_NoPartialWriteOnReject(t *testing.T) {
	s, l := newTest(t)
	s.SetTime(base)
	counterMember(l)
	ctx := context.Background()
	if res, _ := l.Allow(ctx, one("narrow", "ip", time.Minute, 1)); !res.Allowed {
		t.Fatal("预填应当放行")
	}
	res, err := l.Allow(ctx, []Bucket{
		{Key: "first", Scope: "global", Rule: Rule{Window: time.Minute, Limit: 10}},
		{Key: "narrow", Scope: "ip", Rule: Rule{Window: time.Minute, Limit: 1}},
		{Key: "third", Scope: "email", Rule: Rule{Window: time.Minute, Limit: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("应当被拒")
	}
	if s.Exists("first") || s.Exists("third") {
		t.Error("拒绝时不该给其他桶写入任何东西")
	}
	if got := card(s, "narrow"); got != 1 {
		t.Errorf("窄桶成员数=%d,期望仍为 1", got)
	}
}

// RetryAfter 是窗口长度,不是剩余时间 —— 精确剩余会泄露桶里最早一次尝试的时刻。
func TestAllow_RetryAfterIsWindowNotRemaining(t *testing.T) {
	s, l := newTest(t)
	counterMember(l)
	b := one("k", "email", time.Minute, 1)
	s.SetTime(base)
	if res, _ := l.Allow(context.Background(), b); !res.Allowed {
		t.Fatal("首次应当放行")
	}
	s.SetTime(base.Add(20 * time.Second)) // 此时精确剩余是 40s
	res, err := l.Allow(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("应当被拒")
	}
	if res.RetryAfter != time.Minute {
		t.Fatalf("RetryAfter=%v,期望正好是窗口 1m0s", res.RetryAfter)
	}
}

func TestAllow_ScopeIdentifiesRejectingBucket(t *testing.T) {
	s, l := newTest(t)
	s.SetTime(base)
	counterMember(l)
	b := []Bucket{
		{Key: "wide", Scope: "global", Rule: Rule{Window: time.Minute, Limit: 10}},
		{Key: "narrow", Scope: "ip", Rule: Rule{Window: time.Minute, Limit: 1}},
	}
	if res, _ := l.Allow(context.Background(), b); !res.Allowed {
		t.Fatal("首次应当放行")
	}
	res, _ := l.Allow(context.Background(), b)
	if res.Allowed || res.Scope != "ip" {
		t.Fatalf("Scope=%q Allowed=%v,期望被 ip 桶拒绝", res.Scope, res.Allowed)
	}
}

// 全新的 Limiter 没有缓存的 SHA,第一次走 EVALSHA → NOSCRIPT → EVAL。
// 这条回退要是悄悄坏了,每次调用都报错,而别的用例靠 fail-open 的断言形状可能照样绿。
func TestAllow_FirstCallThroughFreshScriptCounts(t *testing.T) {
	s := miniredis.RunT(t)
	s.SetTime(base)
	l := New(rawClient(t, s))
	counterMember(l)
	res, err := l.Allow(context.Background(), one("k", "ip", time.Minute, 5))
	if err != nil || !res.Allowed {
		t.Fatalf("首次调用应当成功: %+v, %v", res, err)
	}
	if got := card(s, "k"); got != 1 {
		t.Fatalf("首次调用必须入账: card=%d", got)
	}
}

// 假 Scripter:返回预设的脚本回复,用来测契约被破坏时报错而不是 panic 或误放行。
type fakeScripter struct{ val any }

func (f fakeScripter) cmd(ctx context.Context) *redis.Cmd {
	c := redis.NewCmd(ctx)
	c.SetVal(f.val)
	return c
}
func (f fakeScripter) Eval(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
	return f.cmd(ctx)
}
func (f fakeScripter) EvalSha(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
	return f.cmd(ctx)
}
func (f fakeScripter) EvalRO(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
	return f.cmd(ctx)
}
func (f fakeScripter) EvalShaRO(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
	return f.cmd(ctx)
}
func (f fakeScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	return redis.NewBoolSliceCmd(ctx)
}
func (f fakeScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	return redis.NewStringCmd(ctx)
}

func TestAllow_MalformedReplyIsAnError(t *testing.T) {
	cases := map[string]any{
		"元素太少":     []any{int64(1), int64(0)},
		"元素太多":     []any{int64(1), int64(0), int64(0), int64(0)},
		"类型不对":     []any{"1", int64(0), int64(0)},
		"序号越界(过大)": []any{int64(0), int64(5), int64(1000)},
		"序号越界(为0)": []any{int64(0), int64(0), int64(1000)},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			l := New(fakeScripter{val: v})
			res, err := l.Allow(context.Background(), one("k", "ip", time.Minute, 1))
			if err == nil {
				t.Fatalf("契约被破坏必须报错,得到 %+v", res)
			}
		})
	}
}
