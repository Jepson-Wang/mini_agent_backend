package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// fakeLock 是 leaseLock 的假实现。
//
// **不连 Redis**:回收器的选主逻辑("拿不到就跳过""结束时要释放")和 Redis 无关,
// 钉在 SQL 上也一样没测。这一层要验的是控制流,不是 Redis 的行为
// (Redis 那些在 internal/pkg/redislock 里对着 miniredis 测)。
type fakeLock struct {
	// ok 是 Acquire 返回的 ok。false = 锁被别人持有着。
	ok bool
	// err 是 Acquire 返回的错误。模拟 Redis 挂了。
	err error

	// token 是 Acquire 返回的 token。
	token string

	// acquireCalls / releaseCalls 记调用次数。
	acquireCalls int
	releaseCalls int

	// releasedKey / releasedToken 记最后释放时传的参数。
	releasedKey   string
	releasedToken string

	// releaseErr 是 Release 返回的错误。
	releaseErr error
}

func (f *fakeLock) Acquire(_ context.Context, _ string, _ time.Duration) (string, bool, error) {
	f.acquireCalls++
	if f.err != nil {
		return "", true, f.err
	}
	if !f.ok {
		return "", false, nil
	}
	return f.token, true, nil
}

func (f *fakeLock) Release(_ context.Context, key, token string) error {
	f.releaseCalls++
	f.releasedKey = key
	f.releasedToken = token
	return f.releaseErr
}

// newLockedReconciler 造一个带锁的回收器。
func newLockedReconciler(t *testing.T, l *fakeLister, w *fakeStatusWriter, lock LeaseLock) *Reconciler {
	t.Helper()
	r, err := NewReconciler(l, w, lock, testReconcileCfg())
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	if r == nil {
		t.Fatal("NewReconciler 返回了 nil,但配置里是启用的")
	}
	return r
}

// 拿不到锁就跳过这一轮:一次库都不该查。
//
// 这是选主的全部意义 —— 别的副本在扫,这一轮就不扫。
// 写成"照样扫"的话,锁就是白加,多副本还是各扫各的。
func TestTick_SkipsWhenLockHeld(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{{TaskUID: "A", Status: model.TaskRunning}}}
	w := newFakeStatusWriter()
	lock := &fakeLock{ok: false} // 锁被别人持有着

	newLockedReconciler(t, l, w, lock).tick(context.Background())

	if l.calls != 0 {
		t.Errorf("拿不到锁还查了 %d 次库 —— 这一轮该直接跳过", l.calls)
	}
	if len(w.calls) != 0 {
		t.Errorf("拿不到锁还写了 %d 次状态", len(w.calls))
	}
	if lock.acquireCalls != 1 {
		t.Errorf("试了 %d 次加锁,期望 1 次", lock.acquireCalls)
	}
	// 没拿到锁就不该释放 —— 那把锁不是自己的。
	if lock.releaseCalls != 0 {
		t.Errorf("没拿到锁却释放了 %d 次 —— 会把别人的锁删掉", lock.releaseCalls)
	}
}

// 跑完一轮之后锁必须被释放,而且 token 要和 Acquire 返回的一致。
//
// token 不一致的话,释放脚本比对不上,锁不会被删 ——
// 于是下一轮别的副本拿不到锁,回收器就这么停摆到 TTL 过期。
func TestTick_ReleasesLock(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{{TaskUID: "A", Status: model.TaskRunning}}}
	w := newFakeStatusWriter()
	lock := &fakeLock{ok: true, token: "TOKEN-1"}

	newLockedReconciler(t, l, w, lock).tick(context.Background())

	if lock.releaseCalls != 1 {
		t.Errorf("释放了 %d 次,期望 1 次", lock.releaseCalls)
	}
	if lock.releasedToken != "TOKEN-1" {
		t.Errorf("释放时传的 token 是 %q,期望 %q", lock.releasedToken, "TOKEN-1")
	}
	if lock.releasedKey != reconcileLockKey {
		t.Errorf("释放时传的 key 是 %q,期望 %q", lock.releasedKey, reconcileLockKey)
	}
}

// ★ 查询报错时锁仍然必须被释放。
//
// 这条最容易漏。写成"跑完了才释放"的话,每次 ListStale 报错,锁都要挂满
// 整个 TTL(45s)—— 而 ListStale 报错恰好是数据库有问题的时候,
// 也就是最需要下一轮快点重试的时候。
//
// 少了 defer 的实现在这里会红。
func TestTick_ReleasesLockOnError(t *testing.T) {
	l := &fakeLister{err: errors.New("连接超时")}
	w := newFakeStatusWriter()
	lock := &fakeLock{ok: true, token: "TOKEN-1"}

	newLockedReconciler(t, l, w, lock).tick(context.Background())

	if lock.releaseCalls != 1 {
		t.Errorf("查询失败时释放了 %d 次锁,期望 1 次 —— 锁会挂满整个 TTL", lock.releaseCalls)
	}
}

// 没有锁(没配 Redis 的部署)时照常扫。
//
// 这是 fail open:回收器是兜底机制,不能因为没配 Redis 就不干活。
func TestTick_NilLockStillScans(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{{TaskUID: "A", Status: model.TaskRunning}}}
	w := newFakeStatusWriter()

	newLockedReconciler(t, l, w, nil).tick(context.Background())

	if l.calls != 1 {
		t.Errorf("没有锁时查了 %d 次库,期望 1 次", l.calls)
	}
	if len(w.calls) != 1 {
		t.Errorf("没有锁时写了 %d 次状态,期望 1 次", len(w.calls))
	}
}

// Redis 挂了(Acquire 报错)时照常扫。
//
// 回收器本身就是兜底,让兜底依赖另一个会挂的组件是反的。
// 代价只是退化回多副本重复扫,不会写错数据。
func TestTick_LockErrorStillScans(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{{TaskUID: "A", Status: model.TaskRunning}}}
	w := newFakeStatusWriter()
	lock := &fakeLock{err: errors.New("redis 连不上")}

	newLockedReconciler(t, l, w, lock).tick(context.Background())

	if l.calls != 1 {
		t.Errorf("加锁失败时查了 %d 次库,期望 1 次(fail open)", l.calls)
	}
	if len(w.calls) != 1 {
		t.Errorf("加锁失败时写了 %d 次状态,期望 1 次(fail open)", len(w.calls))
	}
}

// Redis 挂了的时候并没有真的拿到锁,不该去释放一把不存在的锁。
//
// 释放动作本身是安全的(脚本比对 token,token 是空串就匹配不上),
// 但发一次没意义的 Redis 请求没有必要。
func TestTick_LockErrorDoesNotRelease(t *testing.T) {
	l := &fakeLister{}
	w := newFakeStatusWriter()
	lock := &fakeLock{err: errors.New("redis 连不上")}

	newLockedReconciler(t, l, w, lock).tick(context.Background())

	if lock.releaseCalls != 0 {
		t.Errorf("加锁失败却释放了 %d 次 —— 那里根本没有锁可放", lock.releaseCalls)
	}
}

// 释放失败只记日志,不该让整轮崩掉。
//
// 释放失败的后果由 TTL 兜底(45 秒后锁自己就没了),
// 没别的可做的,也不该因此影响回收本身。
func TestTick_ReleaseErrorDoesNotBreakTheRound(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{{TaskUID: "A", Status: model.TaskRunning}}}
	w := newFakeStatusWriter()
	lock := &fakeLock{ok: true, token: "TOKEN-1", releaseErr: errors.New("redis 连不上")}

	// tick 内部不返回错误,这里只看回收有没有真的发生。
	newLockedReconciler(t, l, w, lock).tick(context.Background())

	if len(w.calls) != 1 {
		t.Errorf("释放失败时写了 %d 次状态,期望 1 次 —— 释放失败不该影响回收本身", len(w.calls))
	}
}

// 锁的 TTL 必须大于一轮回收的硬超时。
//
// 小于的话,一轮还没跑完锁就过期了,另一个副本进来重复扫 ——
// 静默失效,没有任何迹象。
func TestReconcile_LockTTLExceedsRoundTimeout(t *testing.T) {
	if reconcileLockTTL <= reconcileTimeout {
		t.Errorf("锁的 TTL(%v)必须大于一轮回收的超时(%v)—— "+
			"一轮没跑完锁就过期,另一个副本会重复扫",
			reconcileLockTTL, reconcileTimeout)
	}
}

// signalLister 在每次 ListStale 时往 channel 里塞一下。
//
// 不复用 fakeLister / fakeStatusWriter 来做这件事:它们的计数是裸字段、
// 没有锁,而这条用例要从**另一个协程**观察 Run 的进展 —— 读那些字段
// 就是数据竞争,-race 下会直接炸。channel 自带同步,不用给共享夹具加锁。
type signalLister struct{ hit chan struct{} }

func (l *signalLister) ListStale(_ context.Context, _ time.Time, _ int) ([]model.Task, error) {
	select {
	case l.hit <- struct{}{}:
	default: // 已经通知过了,不堵住回收循环
	}
	// 返回空列表:这条用例只要证明"扫了",不关心收尾逻辑。
	return nil, nil
}

// TestRun_NilLockStillScans 盯的是一个真发生过的接线 bug。
//
// "没配 Redis 就没有选主"那条警告原来写在 main.go 里,形如
//
//	if reconciler == nil         { warn }
//	else if reconcileLock == nil { warn }               // ← 分支到这儿就结束了
//	else                         { go reconciler.Run(ctx) }
//
// 于是 REDIS_ADDR 为空(受支持的默认部署)时 go Run **永远到不了**:
// 回收器被构造出来然后静静丢掉,卡住的任务永远停在 running,
// 而日志里只有一条看起来无害的 Warn。
//
// 为什么已有的 TestTick_NilLockStillScans 抓不到它:那条直接调 tick,
// 而 bug 恰恰是"tick 根本没人调"。所以这条必须从 Run 进去。
//
// 它测不到 main.go 里的分支本身(那个文件没有测试),但它把"nil 锁的
// 回收器跑起来之后真的会扫"钉死在 service 这一层。配套的结构性修法是:
// 那条警告改由 Reconciler.Run 自己打,接线方只剩 if/else —— 想打警告
// 就必须已经进了 Run,原来那个形态不可能再出现。
func TestRun_NilLockStillScans(t *testing.T) {
	lister := &signalLister{hit: make(chan struct{}, 1)}

	cfg := testReconcileCfg()
	cfg.Interval = 10 * time.Millisecond

	// lock 显式传 nil —— 这就是没配 REDIS_ADDR 的部署。
	r, err := NewReconciler(lister, newFakeStatusWriter(), nil, cfg)
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	if r == nil {
		t.Fatal("interval > 0 时不该返回 nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	select {
	case <-lister.hit:
		// 扫了,这就是要验的。
	case <-time.After(3 * time.Second):
		t.Fatal("nil 锁的回收器跑起来之后一轮都没扫 —— 没配 Redis 的部署等于没有回收器")
	}
}
