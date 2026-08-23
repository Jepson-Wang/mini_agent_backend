// Package breaker 是一个三态熔断器,用来在下游持续失败时快速失败,
// 而不是让每一次调用都去撞一遍那个已经死掉的下游。
//
// 它解决的不是"调用会失败"——那本来就会失败——而是**失败要多久才知道**。
// Agent 进程没了的时候,一次 gRPC 拨号要等到 TCP 连接超时才返回,
// 而在那之前调用方的协程、连接、请求处理器全都占着。并发一上来,
// 一个下游故障就变成本服务自己的线程池耗尽。
//
// 这个包不依赖 gRPC,也不知道"什么样的错误算失败"——那是调用方的判断。
// 见 agentclient 里的 countsAsUnhealthy:参数传错(InvalidArgument)是
// 我们自己的问题,不该算到 Agent 头上,否则一阵坏请求就能把熔断器打开,
// 把一个健康的下游隔离掉。
package breaker

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrOpen 是熔断器处于打开状态时,调用方该返回给上层的错误。
// 这个包自己不返回它(Allow 只给 bool),放在这儿是为了各调用方用同一个值。
var ErrOpen = errors.New("熔断器已打开,暂时不调用下游")

// State 是熔断器的状态。
type State int

const (
	// StateClosed 正常放行。连续失败攒够阈值就转 Open。
	StateClosed State = iota

	// StateOpen 一律拒绝。冷却时间到了之后,下一次 Allow 会转 HalfOpen。
	StateOpen

	// StateHalfOpen 试探期:只放很少的几个请求过去探路。
	// 探针成功就转 Closed,失败就立刻转回 Open 并重新开始冷却。
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Config 是熔断器的参数。
type Config struct {
	// FailureThreshold 连续失败多少次就打开。
	//
	// 数的是**连续**失败:中间成功一次就清零。用连续计数而不是错误率,
	// 是因为这个客户端的调用量很低(一次任务一两个 RPC),
	// 错误率在小样本上抖得厉害,一次偶发失败就能算出 100%。
	//
	// <= 0 表示不启用熔断,Allow 永远放行。
	FailureThreshold int

	// Cooldown 打开之后要等多久才允许试探。
	Cooldown time.Duration

	// HalfOpenMax 半开状态下最多同时放几个探针过去,
	// 也是"要连续成功几次才算恢复"。<= 0 时按 1 处理。
	//
	// 别调大:半开期放出去的每一个请求都可能打在一个还没缓过来的下游上。
	HalfOpenMax int
}

// Breaker 是一个熔断器。零值不可用,必须走 New。
// 所有方法都是并发安全的。
type Breaker struct {
	threshold   int
	cooldown    time.Duration
	halfOpenMax int

	// now 可替换,给测试用。生产里就是 time.Now ——
	// 熔断器的行为全靠时间推进,用真时钟测就得 Sleep,那样的用例又慢又飘。
	now func() time.Time

	mu       sync.Mutex
	state    State
	failures int       // Closed 状态下的连续失败数
	openedAt time.Time // 转入 Open 的时刻
	probes   int       // HalfOpen 状态下已放出去的探针数
	oks      int       // HalfOpen 状态下成功的探针数
}

// New 建一个熔断器。
//
// FailureThreshold <= 0 时返回一个"永远放行"的实例而不是报错:
// 把熔断关掉是一个合法的部署选择(比如本地开发),不该逼调用方
// 在自己那边再写一遍 if 判断。
func New(cfg Config) (*Breaker, error) {
	if cfg.FailureThreshold > 0 && cfg.Cooldown <= 0 {
		return nil, fmt.Errorf("熔断冷却时间必须大于 0,当前为 %v", cfg.Cooldown)
	}
	if cfg.HalfOpenMax <= 0 {
		cfg.HalfOpenMax = 1
	}
	return &Breaker{
		threshold:   cfg.FailureThreshold,
		cooldown:    cfg.Cooldown,
		halfOpenMax: cfg.HalfOpenMax,
		now:         time.Now,
		state:       StateClosed,
	}, nil
}

// Allow 问一次能不能放行。返回 true 的话,调用方**必须**在拿到结果后
// 调一次 Record,否则半开状态下的探针名额会漏掉,熔断器再也回不到 Closed。
//
// 标准写法:
//
//	if !b.Allow() {
//	    return breaker.ErrOpen
//	}
//	err := call()
//	b.Record(healthy(err))
//	return err
func (b *Breaker) Allow() bool {
	if b.threshold <= 0 {
		return true // 没启用
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateOpen {
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		// 冷却期满,转试探。注意状态是在 Allow 里推进的,不靠定时器 ——
		// 没有调用的时候熔断器不需要动,也就不需要一个后台协程陪着它。
		b.state = StateHalfOpen
		b.probes = 0
		b.oks = 0
	}

	if b.state == StateHalfOpen {
		if b.probes >= b.halfOpenMax {
			return false
		}
		b.probes++
		return true
	}
	return true // Closed
}

// Record 汇报一次调用的结果。healthy=false 表示"下游有病",
// 不是"这次调用返回了错误" —— 参数错误、找不到资源这类是调用方自己的问题,
// 传 true(或者干脆别走熔断器),别让它们把一个健康的下游关在门外。
func (b *Breaker) Record(healthy bool) {
	if b.threshold <= 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		if healthy {
			b.failures = 0
			return
		}
		b.failures++
		if b.failures >= b.threshold {
			b.open()
		}

	case StateHalfOpen:
		if !healthy {
			// 探针失败:立刻回到 Open,冷却重新计时。
			// 不给"再试一次"的机会 —— 半开期本来就是那次机会。
			b.open()
			return
		}
		b.oks++
		if b.oks >= b.halfOpenMax {
			b.state = StateClosed
			b.failures = 0
			b.probes = 0
			b.oks = 0
		}

	case StateOpen:
		// 冷却期内不该有调用发生。真收到了(调用方没先问 Allow,
		// 或者是一个在转 Open 之前就发出去的在途请求),忽略即可:
		// 拿它去重置冷却计时会让熔断器永远打不开。
	}
}

// open 转入 Open 并重置计数。调用方必须已经持有锁。
func (b *Breaker) open() {
	b.state = StateOpen
	b.openedAt = b.now()
	b.failures = 0
	b.probes = 0
	b.oks = 0
}

// State 返回当前状态,给 /health 这类地方看。
//
// 只读,不推进状态机:Open 冷却期已满但还没有人来 Allow 时,
// 这里仍然报 open。让一次探活把熔断器推进半开是不对的 ——
// 探活的目的是观察,不是消费掉那个宝贵的探针名额。
func (b *Breaker) State() State {
	if b.threshold <= 0 {
		return StateClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
