package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ratelimit"
)

// 编译期断言:Raw() 的结果能当 redis.Scripter 交给限流器。
var _ redis.Scripter = (*redis.Client)(nil)

func TestNew_EmptyAddrIsDisabled(t *testing.T) {
	c, err := New(Options{})
	if c != nil || err != nil {
		t.Fatalf("New(空地址) = (%v, %v), 期望 (nil, nil)", c, err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("nil 客户端 Ping = %v, 期望 nil", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("nil 客户端 Close = %v, 期望 nil", err)
	}
	if c.Raw() != nil {
		t.Error("nil 客户端 Raw 应返回 nil")
	}
}

func TestNew_SetsExplicitTimeouts(t *testing.T) {
	s := miniredis.RunT(t)
	c, err := New(Options{Addr: s.Addr()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	o := c.Raw().Options()
	if o.MaxRetries != 0 || o.DialerRetries != 1 {
		t.Errorf("MaxRetries = %d, DialerRetries = %d, 期望 0(go-redis 把 -1 归一成 0,默认是 3)和 1", o.MaxRetries, o.DialerRetries)
	}
	if o.DialTimeout == 0 || o.ReadTimeout == 0 || o.WriteTimeout == 0 || o.PoolSize == 0 {
		t.Errorf("存在未显式设置的项: %+v", o)
	}
}

func TestNew_RespectsProvidedOptions(t *testing.T) {
	s := miniredis.RunT(t)
	c, err := New(Options{
		Addr:         s.Addr(),
		DialTimeout:  time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     7,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	o := c.Raw().Options()
	if o.DialTimeout != time.Second || o.ReadTimeout != 2*time.Second ||
		o.WriteTimeout != 3*time.Second || o.PoolSize != 7 {
		t.Errorf("显式传入的参数被覆盖了: %+v", o)
	}
}

func TestPing_Succeeds(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestPing_FailsWhenServerGone(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	s.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("服务端已关闭,Ping 应当失败")
	}
}

func TestWaitReady_SucceedsFirstTry(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	if err := c.WaitReady(context.Background(), 3, time.Hour); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestWaitReady_ReturnsLastErrorAfterAttempts(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	s.Close()

	const gap = 50 * time.Millisecond
	start := time.Now()
	err := c.WaitReady(context.Background(), 3, gap)
	if err == nil {
		t.Fatal("地址不通,WaitReady 应当返回错误")
	}
	// 3 次尝试之间有 2 段间隔;只试一次的话会远小于这个下限。
	if elapsed := time.Since(start); elapsed < 2*gap {
		t.Errorf("耗时 %v 少于 %v,说明没有真的重试多次", elapsed, 2*gap)
	}
}

func TestWaitReady_StopsOnContextCancel(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := c.WaitReady(ctx, 100, time.Second)
	if err == nil {
		t.Fatal("ctx 已取消,应当返回错误")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("取消后仍耗时 %v,没有及时返回", elapsed)
	}
}

func TestRaw_IsUsableByRatelimit(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()

	var sc redis.Scripter = c.Raw()
	if lim := ratelimit.New(sc); lim == nil {
		t.Fatal("ratelimit.New 返回 nil")
	}
}

func TestNewPubSub_EmptyAddrIsDisabled(t *testing.T) {
	c := NewPubSub(Options{})
	if c != nil {
		t.Fatalf("NewPubSub(空地址) = %v, 期望 nil", c)
	}
	// nil 客户端的每个方法都得安全。
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("nil 客户端 Ping = %v, 期望 nil", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("nil 客户端 Close = %v, 期望 nil", err)
	}
	if c.Raw() != nil {
		t.Error("nil 客户端 Raw 应返回 nil")
	}
}

func TestNewPubSub_HasNoReadTimeout(t *testing.T) {
	s := miniredis.RunT(t)
	c := NewPubSub(Options{Addr: s.Addr()})
	if c == nil {
		t.Fatal("NewPubSub 返回 nil")
	}
	defer c.Close()

	o := c.Raw().Options()
	// go-redis 把 -1 归一成 0 = 没有读超时。
	// 如果这里变成 5s,说明写的是 0(被当成"未设置")而不是 -1。
	if o.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, 期望 0(无读超时)", o.ReadTimeout)
	}
	if o.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, 期望 0(无写超时)", o.WriteTimeout)
	}
	if o.MaxRetries != 0 || o.DialerRetries != 1 {
		t.Errorf("MaxRetries = %d, DialerRetries = %d, 期望 0 和 1", o.MaxRetries, o.DialerRetries)
	}
	if o.DialTimeout == 0 || o.PoolSize == 0 {
		t.Errorf("存在未显式设置的项: %+v", o)
	}
}

func TestNew_CannotDisableReadTimeout(t *testing.T) {
	// ★ 这条用例锁住一个真实的限制:**普通客户端配不出"无读超时"**。
	//
	// New 里的 `opt.ReadTimeout <= 0` 兜底会把 -1 和 -2 都改写成 200ms,
	// 而 go-redis 的语义里 -1 才是"无超时",0 是"未设置"→ 5s 默认。
	// 所以想给普通客户端关掉读超时是做不到的,而且没有任何迹象。
	// 需要无读超时的客户端只能用 NewPubSub。
	s := miniredis.RunT(t)
	c, err := New(Options{Addr: s.Addr(), ReadTimeout: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if got := c.Raw().Options().ReadTimeout; got != defaultTimeout {
		t.Errorf("ReadTimeout = %v, 期望 %v(-1 被兜底改写成了默认值)", got, defaultTimeout)
	}
}

// TestSubscribe_SurvivesShortReadTimeout 实测并锁住一个结论:
//
// **go-redis v9.23.0 的 PubSub 接收循环不受 Options.ReadTimeout 约束。**
//
// 源码路径:Receive(ctx) → ReceiveTimeout(ctx, 0) → cn.WithReader(ctx, 0, ...)
// → deadline(ctx, 0):timeout==0 且 ctx 没有 deadline 时返回零值 time.Time{},
// 也就是 SetReadDeadline(time.Time{}) = 清除 deadline,无限期阻塞。
//
// 所以哪怕把 ReadTimeout 设成 200ms,订阅连接在空闲期间也不会被读超时打断。
//
// 这条结论是**客户端侧**的(deadline 由 go-redis 算),和对端是不是真 Redis 无关;
// 这里的 miniredis 只负责"订阅期间不发消息"这件事。
// 哪天 go-redis 改了这条路径,这个用例会红 —— 那正是它存在的意义。
func TestSubscribe_SurvivesShortReadTimeout(t *testing.T) {
	s := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:         s.Addr(),
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1,
	})
	defer rdb.Close()

	ctx := context.Background()
	ps := rdb.Subscribe(ctx, "chan")
	defer ps.Close()
	ch := ps.Channel()

	// 空闲 3 倍读超时,期间不发任何消息。
	// 如果订阅会被读超时打断,这里就会看到 i/o timeout 或者消息收不到。
	time.Sleep(600 * time.Millisecond)

	if err := rdb.Publish(ctx, "chan", "hello").Err(); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case msg := <-ch:
		if msg.Payload != "hello" {
			t.Errorf("消息内容 = %q, 期望 %q", msg.Payload, "hello")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("空闲超过读超时之后,订阅收不到消息了")
	}
}

func TestNewPubSub_SubscribeSurvivesLongIdle(t *testing.T) {
	// Pub/Sub 专用客户端(无读超时)在长时间空闲之后照样能收到消息。
	s := miniredis.RunT(t)
	c := NewPubSub(Options{Addr: s.Addr()})
	if c == nil {
		t.Fatal("NewPubSub 返回 nil")
	}
	defer c.Close()

	ctx := context.Background()
	ps := c.Raw().Subscribe(ctx, "chan")
	defer ps.Close()
	ch := ps.Channel()

	time.Sleep(600 * time.Millisecond)

	if err := c.Raw().Publish(ctx, "chan", "hello").Err(); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case msg := <-ch:
		if msg.Payload != "hello" {
			t.Errorf("消息内容 = %q, 期望 %q", msg.Payload, "hello")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("空闲之后订阅收不到消息了")
	}
}
