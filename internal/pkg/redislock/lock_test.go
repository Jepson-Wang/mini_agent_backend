package redislock

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTest 起一个内存 Redis 和接在它上面的锁。
//
// 用 miniredis 而不是真 Redis:它自带 gopher-lua,这个释放脚本在它上面
// 是真跑的。把一条并发规则押在「没有外部服务就会被跳过」的用例上等于没测
// —— 同 internal/pkg/ratelimit 和 internal/service/quota_test.go 的取向。
//
// 返回的 *miniredis.Miniredis 用来推进过期(FastForward)。
//
// **时间一律用 FastForward 推,不要 Sleep。** 用例又慢又飘。
// 验 PEXPIRE 的用例必须 FastForward 之后再看 PTTL / Exists,
// 否则「去掉 PX」这个变异不会让测试变红。
func newTest(t *testing.T) (*miniredis.Miniredis, *Lock) {
	t.Helper()
	s := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return s, New(c)
}

const testKey = "lock:reconcile"

// 基本正确性:空 Redis 上加锁成功,返回一个非空 token。
func TestAcquire_Succeeds(t *testing.T) {
	s, l := newTest(t)
	ctx := context.Background()

	token, ok, err := l.Acquire(ctx, testKey, 45*time.Second)
	if err != nil {
		t.Fatalf("加锁不该报错: %v", err)
	}
	if !ok {
		t.Fatal("空 Redis 上加锁必须成功")
	}
	if token == "" {
		t.Error("token 不能为空 —— 它是释放锁时的凭证")
	}
	// 锁确实写进去了。
	got, err := s.Get(testKey)
	if err != nil {
		t.Fatalf("读锁失败: %v", err)
	}
	if got != token {
		t.Errorf("Redis 里的值是 %q,期望是 token %q", got, token)
	}
}

// 互斥:同一个 key 第二次加锁必须失败。
func TestAcquire_SecondFails(t *testing.T) {
	_, l := newTest(t)
	ctx := context.Background()

	if _, ok, err := l.Acquire(ctx, testKey, 45*time.Second); err != nil || !ok {
		t.Fatalf("第一次加锁应该成功,ok=%v err=%v", ok, err)
	}
	token, ok, err := l.Acquire(ctx, testKey, 45*time.Second)
	if err != nil {
		t.Fatalf("第二次加锁不该报错(是正常的并发,不是故障): %v", err)
	}
	if ok {
		t.Error("同一把锁不能被两个持有者同时拿到")
	}
	if token != "" {
		t.Errorf("没拿到锁就不该返回 token,实际拿到 %q", token)
	}
}

// ★ 锁必须按正确时间过期。
//
// 这是本组用例里最重要的一条:它锁住「PX 真的设上了」。
// 漏了 PX 的实现能过前两条,然后在生产上第一次进程崩溃时永久死锁 ——
// 而那个现象和「没加锁」一模一样(回收器再也不跑了),没有任何迹象。
func TestAcquire_AfterExpiry(t *testing.T) {
	s, l := newTest(t)
	ctx := context.Background()

	ttl := 45 * time.Second
	if _, ok, err := l.Acquire(ctx, testKey, ttl); err != nil || !ok {
		t.Fatalf("第一次加锁应该成功,ok=%v err=%v", ok, err)
	}

	// TTL 还没到:锁还在,拿不到。
	s.FastForward(ttl - time.Second)
	if _, ok, _ := l.Acquire(ctx, testKey, ttl); ok {
		t.Fatal("TTL 还没到,锁必须还在")
	}

	// TTL 到了:锁自动消失,能拿到。
	s.FastForward(2 * time.Second)
	token, ok, err := l.Acquire(ctx, testKey, ttl)
	if err != nil {
		t.Fatalf("过期后加锁不该报错: %v", err)
	}
	if !ok {
		t.Fatal("锁过期之后必须能重新拿到 —— PX 没生效的话这把锁会永不过期")
	}
	if token == "" {
		t.Error("重新加锁必须返回新 token")
	}
}

// TTL 确实写进了 Redis:PTTL 应该约等于给定的 TTL。
func TestAcquire_SetsTTL(t *testing.T) {
	s, l := newTest(t)
	ctx := context.Background()

	ttl := 45 * time.Second
	if _, ok, err := l.Acquire(ctx, testKey, ttl); err != nil || !ok {
		t.Fatalf("加锁应该成功,ok=%v err=%v", ok, err)
	}
	pttl := s.TTL(testKey)
	if pttl <= 0 {
		t.Fatalf("锁必须有 TTL,实际是 %v", pttl)
	}
	// miniredis 的 TTL 精度是秒,给一点余量。
	if pttl > ttl || pttl < ttl-time.Second {
		t.Errorf("TTL 是 %v,期望在 [%v, %v] 之间", pttl, ttl-time.Second, ttl)
	}
}

// ★ 只能释放自己的锁。
//
// A 加锁 → 过期 → B 加锁 → A 调 Release → **B 的锁必须还在**。
//
// 这是整组的核心。它锁住「Lua 比对 token」这件事。
// 改成裸 DEL 的实现会在**单机测试下永远是绿的** —— 只有这个场景能区分,
// 所以这条用例必须写。
func TestRelease_OnlyOwnLock(t *testing.T) {
	s, l := newTest(t)
	ctx := context.Background()

	ttl := 45 * time.Second

	// A 拿到锁。
	tokenA, ok, err := l.Acquire(ctx, testKey, ttl)
	if err != nil || !ok {
		t.Fatalf("A 加锁应该成功,ok=%v err=%v", ok, err)
	}

	// A 卡住了,锁过期了。
	s.FastForward(ttl + time.Second)

	// B 拿到锁。
	tokenB, ok, err := l.Acquire(ctx, testKey, ttl)
	if err != nil || !ok {
		t.Fatalf("A 的锁过期后 B 应该能拿到,ok=%v err=%v", ok, err)
	}
	if tokenB == tokenA {
		t.Fatal("两个持有者的 token 必须不同 —— 否则分不清谁是谁")
	}

	// A 恢复过来,执行释放。
	if err := l.Release(ctx, testKey, tokenA); err != nil {
		t.Fatalf("释放锁不该报错: %v", err)
	}

	// B 的锁必须还在!
	got, err := s.Get(testKey)
	if err != nil {
		t.Fatalf("B 的锁被 A 释放掉了: %v", err)
	}
	if got != tokenB {
		t.Errorf("B 的锁没了,现在是 %q,期望 %q", got, tokenB)
	}
}

// 释放自己的锁之后,锁确实没了。
func TestRelease_RemovesOwnLock(t *testing.T) {
	s, l := newTest(t)
	ctx := context.Background()

	token, ok, err := l.Acquire(ctx, testKey, 45*time.Second)
	if err != nil || !ok {
		t.Fatalf("加锁应该成功,ok=%v err=%v", ok, err)
	}
	if err := l.Release(ctx, testKey, token); err != nil {
		t.Fatalf("释放锁不该报错: %v", err)
	}
	if s.Exists(testKey) {
		t.Error("释放之后锁应该不存在")
	}
}

// 幂等:同一个 token 连 Release 两次,第二次不报错。
//
// `defer release()` 和显式 release 并存时不该炸。
func TestRelease_Idempotent(t *testing.T) {
	_, l := newTest(t)
	ctx := context.Background()

	token, ok, err := l.Acquire(ctx, testKey, 45*time.Second)
	if err != nil || !ok {
		t.Fatalf("加锁应该成功,ok=%v err=%v", ok, err)
	}
	if err := l.Release(ctx, testKey, token); err != nil {
		t.Fatalf("第一次释放不该报错: %v", err)
	}
	// 第二次:锁已经不在了,不该报错。
	if err := l.Release(ctx, testKey, token); err != nil {
		t.Fatalf("重复释放不该报错: %v", err)
	}
}

// 没拿到锁就没锁可放:token 为空时 Release 不该去删别人的锁。
func TestRelease_EmptyTokenIsNoop(t *testing.T) {
	s, l := newTest(t)
	ctx := context.Background()

	// 先让 B 持锁。
	tokenB, ok, err := l.Acquire(ctx, testKey, 45*time.Second)
	if err != nil || !ok {
		t.Fatalf("加锁应该成功,ok=%v err=%v", ok, err)
	}

	// A 没拿到锁,token 是空的。释放不能动 B 的锁。
	if err := l.Release(ctx, testKey, ""); err != nil {
		t.Fatalf("空 token 释放不该报错: %v", err)
	}
	got, err := s.Get(testKey)
	if err != nil {
		t.Fatalf("B 的锁被删掉了: %v", err)
	}
	if got != tokenB {
		t.Errorf("B 的锁没了,现在是 %q,期望 %q", got, tokenB)
	}
}

// nil 接收者 = 这个部署没启用 Redis,加锁必须「成功」。
//
// 这是 fail open:回收器是兜底机制,让它依赖另一个会挂的组件是反的。
// 代价只是退化回多副本重复扫,不会写错数据。
func TestAcquire_NilLockAlwaysSucceeds(t *testing.T) {
	var l *Lock
	token, ok, err := l.Acquire(context.Background(), testKey, 45*time.Second)
	if err != nil {
		t.Fatalf("nil 接收者不该报错: %v", err)
	}
	if !ok {
		t.Error("nil 接收者必须当作拿到了锁")
	}
	if token != "" {
		t.Errorf("nil 接收者不该返回 token,实际 %q", token)
	}
}

// nil 接收者释放锁也不该报错(defer release 要能安全地跑)。
func TestRelease_NilLockIsNoop(t *testing.T) {
	var l *Lock
	if err := l.Release(context.Background(), testKey, "token"); err != nil {
		t.Fatalf("nil 接收者释放不该报错: %v", err)
	}
}

// Redis 挂了 → fail open,当作拿到了锁,但 err 要传出去让调用方能告警。
func TestAcquire_RedisDownSucceeds(t *testing.T) {
	s, l := newTest(t)
	ctx := context.Background()

	s.Close() // Redis 挂了

	token, ok, err := l.Acquire(ctx, testKey, 45*time.Second)
	if err == nil {
		t.Error("Redis 挂了必须返回错误 —— 调用方要靠它告警")
	}
	if !ok {
		t.Error("Redis 挂了必须当作拿到了锁(fail open),否则回收器会停摆")
	}
	if token != "" {
		t.Errorf("没真正拿到锁就不该返回 token,实际 %q", token)
	}
}

// TTL <= 0 是接线错误:SET NX PX 0 意味着永不过期,持锁方崩了锁就永远不放。
// 大声报错,不默默放行。
func TestAcquire_NonPositiveTTLFails(t *testing.T) {
	_, l := newTest(t)
	ctx := context.Background()

	_, ok, err := l.Acquire(ctx, testKey, 0)
	if err == nil {
		t.Error("TTL <= 0 必须报错 —— 永不过期的锁会让回收器永久死锁")
	}
	if ok {
		t.Error("TTL <= 0 时不该报告拿到了锁")
	}
}

// New(nil) 返回 nil,表示「这个部署没有 Redis」。
func TestNew_NilScripterReturnsNil(t *testing.T) {
	if l := New(nil); l != nil {
		t.Error("New(nil) 必须返回 nil")
	}
}

// ★ 同一把锁不能被不同副本同时持有。
//
// 这是用户提的第二个要求:两个「副本」各持一把 Lock 实例(共用同一个 Redis),
// 必须互斥 —— 只有一个能拿到。
//
// 和 TestAcquire_SecondFails 的区别:那条是同一个 Lock 实例连着加两次,
// 这条是两个独立实例(模拟两个进程/副本)。
func TestAcquire_TwoInstancesAreMutuallyExclusive(t *testing.T) {
	s := miniredis.RunT(t)
	ctx := context.Background()

	// 两个副本,各自的 redis 客户端和 Lock 实例(模拟两个进程)。
	c1 := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	c2 := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c2.Close() })

	l1 := New(c1)
	l2 := New(c2)

	// 副本 1 拿到锁。
	token1, ok1, err := l1.Acquire(ctx, testKey, 45*time.Second)
	if err != nil || !ok1 {
		t.Fatalf("副本 1 加锁应该成功,ok=%v err=%v", ok1, err)
	}

	// 副本 2 必须拿不到 —— 同一把锁不能被两个副本同时持有。
	token2, ok2, err := l2.Acquire(ctx, testKey, 45*time.Second)
	if err != nil {
		t.Fatalf("副本 2 加锁不该报错: %v", err)
	}
	if ok2 {
		t.Fatal("同一把锁不能被两个副本同时持有")
	}
	if token2 != "" {
		t.Errorf("没拿到锁就不该返回 token,实际 %q", token2)
	}

	// 副本 1 释放之后,副本 2 才能拿到。
	if err := l1.Release(ctx, testKey, token1); err != nil {
		t.Fatalf("释放锁不该报错: %v", err)
	}
	if _, ok, err := l2.Acquire(ctx, testKey, 45*time.Second); err != nil || !ok {
		t.Fatalf("副本 1 释放后副本 2 应该能拿到,ok=%v err=%v", ok, err)
	}
}

// 副本 2 拿不到锁时,**不能动副本 1 的锁**,也不能把它删掉。
func TestRelease_FromLoserDoesNotAffectWinner(t *testing.T) {
	s := miniredis.RunT(t)
	ctx := context.Background()

	c1 := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	c2 := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c2.Close() })

	l1 := New(c1)
	l2 := New(c2)

	token1, _, _ := l1.Acquire(ctx, testKey, 45*time.Second)
	// 副本 2 拿不到锁,token 是空的。
	_, ok2, _ := l2.Acquire(ctx, testKey, 45*time.Second)
	if ok2 {
		t.Fatal("副本 2 不该拿到锁")
	}

	// 副本 2 走一遍「拿到锁 → 释放」的流程(拿到的是空 token)。
	if err := l2.Release(ctx, testKey, ""); err != nil {
		t.Fatalf("释放不该报错: %v", err)
	}

	got, err := s.Get(testKey)
	if err != nil {
		t.Fatalf("副本 1 的锁被副本 2 删掉了: %v", err)
	}
	if got != token1 {
		t.Errorf("副本 1 的锁没了,现在是 %q,期望 %q", got, token1)
	}
}
