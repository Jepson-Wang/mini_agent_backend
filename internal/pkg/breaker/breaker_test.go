package breaker

import (
	"sync"
	"testing"
	"time"
)

// newTestBreaker 建一个用假时钟的熔断器,返回它和一个"往前拨表"的函数。
//
// 用假时钟而不是 Sleep:冷却期是按秒算的,真睡的话这几条用例得跑一分钟,
// 而且在 CI 上会因为调度抖动偶发失败 —— 那种失败最后都会被当成 flaky 忽略掉。
func newTestBreaker(t *testing.T, cfg Config) (*Breaker, func(time.Duration)) {
	t.Helper()

	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	now := time.Unix(0, 0)
	b.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	return b, func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
}

// fail 汇报 n 次失败。
func fail(b *Breaker, n int) {
	for range n {
		b.Allow()
		b.Record(false)
	}
}

func TestClosed_AllowsAndCountsConsecutiveFailures(t *testing.T) {
	b, _ := newTestBreaker(t, Config{FailureThreshold: 3, Cooldown: time.Minute})

	fail(b, 2)
	if b.State() != StateClosed {
		t.Fatalf("失败 2 次(阈值 3)不该打开,当前 %v", b.State())
	}

	// 成功一次就清零 —— 数的是连续失败。
	b.Allow()
	b.Record(true)

	fail(b, 2)
	if b.State() != StateClosed {
		t.Fatalf("中间成功过一次,计数该清零,当前 %v", b.State())
	}
}

func TestClosed_OpensAtThreshold(t *testing.T) {
	b, _ := newTestBreaker(t, Config{FailureThreshold: 3, Cooldown: time.Minute})

	fail(b, 3)
	if b.State() != StateOpen {
		t.Fatalf("连续失败到阈值该打开,当前 %v", b.State())
	}
	if b.Allow() {
		t.Error("打开状态还放行,熔断等于没做")
	}
}

func TestOpen_StaysOpenUntilCooldownElapses(t *testing.T) {
	b, advance := newTestBreaker(t, Config{FailureThreshold: 1, Cooldown: 30 * time.Second})

	fail(b, 1)

	advance(29 * time.Second)
	if b.Allow() {
		t.Error("冷却还没到就放行了")
	}

	advance(2 * time.Second)
	if !b.Allow() {
		t.Error("冷却期满该放一个探针出去")
	}
	if b.State() != StateHalfOpen {
		t.Fatalf("放行探针后该是半开,当前 %v", b.State())
	}
}

func TestHalfOpen_OnlyOneProbeAtATime(t *testing.T) {
	b, advance := newTestBreaker(t, Config{FailureThreshold: 1, Cooldown: time.Second})

	fail(b, 1)
	advance(2 * time.Second)

	if !b.Allow() {
		t.Fatal("第一个探针该放行")
	}
	if b.Allow() {
		t.Error("半开期只该放一个探针 —— 多放的那些会打在一个可能还没缓过来的下游上")
	}
}

func TestHalfOpen_SuccessCloses(t *testing.T) {
	b, advance := newTestBreaker(t, Config{FailureThreshold: 1, Cooldown: time.Second})

	fail(b, 1)
	advance(2 * time.Second)

	b.Allow()
	b.Record(true)

	if b.State() != StateClosed {
		t.Fatalf("探针成功该恢复,当前 %v", b.State())
	}
	if !b.Allow() {
		t.Error("恢复之后该正常放行")
	}
}

func TestHalfOpen_FailureReopensAndRestartsCooldown(t *testing.T) {
	b, advance := newTestBreaker(t, Config{FailureThreshold: 1, Cooldown: 30 * time.Second})

	fail(b, 1)
	advance(31 * time.Second)

	b.Allow()
	b.Record(false) // 探针失败

	if b.State() != StateOpen {
		t.Fatalf("探针失败该回到打开,当前 %v", b.State())
	}
	// 关键:冷却是**重新**计时的。如果沿用上一次的 openedAt,
	// 下一次 Allow 会立刻又放一个探针出去,半开就成了摆设。
	advance(29 * time.Second)
	if b.Allow() {
		t.Error("探针失败后冷却该重新开始计时")
	}
	advance(2 * time.Second)
	if !b.Allow() {
		t.Error("新的冷却期满后该再放一个探针")
	}
}

// TestOpen_LateRecordDoesNotExtendCooldown 盯的是一个能让熔断器永远出不来的 bug。
//
// 转 Open 的那一刻,通常还有几个在途请求没回来。它们回来时会各自 Record(false)。
// 如果 Open 状态下的 Record 也去刷新 openedAt,那么只要在途请求陆续回来,
// 冷却就一直被往后推 —— 下游早就恢复了,熔断器还关着门,而且没有任何日志能解释。
func TestOpen_LateRecordDoesNotExtendCooldown(t *testing.T) {
	b, advance := newTestBreaker(t, Config{FailureThreshold: 1, Cooldown: 30 * time.Second})

	fail(b, 1)

	advance(20 * time.Second)
	b.Record(false) // 一个在途请求迟到回来了

	advance(11 * time.Second) // 距离最初打开已经 31s
	if !b.Allow() {
		t.Error("迟到的失败汇报把冷却期往后推了,熔断器会一直出不来")
	}
}

func TestDisabled_AlwaysAllows(t *testing.T) {
	b, _ := newTestBreaker(t, Config{FailureThreshold: 0})

	fail(b, 100)
	if !b.Allow() {
		t.Error("阈值 <= 0 表示不启用熔断,该永远放行")
	}
	if b.State() != StateClosed {
		t.Errorf("没启用时状态该报 closed,当前 %v", b.State())
	}
}

func TestNew_RejectsZeroCooldownWhenEnabled(t *testing.T) {
	if _, err := New(Config{FailureThreshold: 3, Cooldown: 0}); err == nil {
		t.Error("启用熔断但冷却为 0 该报错 —— 那样打开之后下一次调用立刻就放行了")
	}
	// 没启用的时候冷却是多少都无所谓,不该拦。
	if _, err := New(Config{FailureThreshold: 0, Cooldown: 0}); err != nil {
		t.Errorf("没启用熔断不该校验冷却: %v", err)
	}
}

// TestConcurrentUse 用 -race 跑才有意义:验的是所有方法都在同一把锁下面。
func TestConcurrentUse(t *testing.T) {
	b, _ := newTestBreaker(t, Config{FailureThreshold: 5, Cooldown: time.Millisecond})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow() {
				b.Record(i%2 == 0)
			}
			_ = b.State()
		}()
	}
	wg.Wait()
}
