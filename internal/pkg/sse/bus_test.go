package sse

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestBus 起一个连着 miniredis 的总线。
//
// pub 和 sub 传同一个客户端:miniredis 上两者没有区别。
// 「发布必须有界」那条不变量由 NewBus 自己兜(见 TestBus_PublishIsBoundedEvenOnUnboundedClient),
// 不用靠这里传对客户端。
func newTestBus(t *testing.T) (*Bus, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	s := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr(), MaxRetries: -1})
	t.Cleanup(func() { rdb.Close() })
	bus := NewBus(rdb, rdb)
	if bus == nil {
		t.Fatal("NewBus 返回 nil")
	}
	return bus, s, rdb
}

// waitReady 等订阅建立起来。
//
// 端到端用例必须确定"订阅已经建好了"再发信号,
// 否则测的是调度器的运气。
func waitReady(t *testing.T, bus *Bus) {
	t.Helper()
	select {
	case <-bus.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("订阅迟迟没建立")
	}
}

func TestBus_NilIsNoop(t *testing.T) {
	if bus := NewBus(nil, nil); bus != nil {
		t.Errorf("NewBus(nil, nil) = %v, 期望 nil", bus)
	}
	// 只给一个也不行:两个客户端的语义不同,缺一个就没法保证那条不变量。
	if bus := NewBus(redis.NewClient(&redis.Options{}), nil); bus != nil {
		t.Errorf("NewBus(有 pub, 无 sub) = %v, 期望 nil", bus)
	}
	if bus := NewBus(nil, redis.NewClient(&redis.Options{})); bus != nil {
		t.Errorf("NewBus(无 pub, 有 sub) = %v, 期望 nil", bus)
	}

	// nil 总线的每个方法都得是安全的 —— "这个部署没启用 Redis"是合法状态。
	var bus *Bus
	bus.Publish(context.Background(), "sess-1")
	bus.Subscribe(context.Background(), NewHub())
	if err := bus.Close(); err != nil {
		t.Errorf("nil 总线 Close = %v, 期望 nil", err)
	}
}

func TestBus_PublishReachesSubscriber(t *testing.T) {
	bus, _, _ := newTestBus(t)
	hub := NewHub()
	sub := hub.Subscribe("sess-1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus.Subscribe(ctx, hub)
	waitReady(t, bus)

	bus.Publish(ctx, "sess-1")

	select {
	case <-sub.C:
	case <-time.After(5 * time.Second):
		t.Fatal("没收到唤醒信号")
	}
}

func TestBus_PublishOnlyWakesMatchingSession(t *testing.T) {
	bus, _, _ := newTestBus(t)
	hub := NewHub()
	sub1 := hub.Subscribe("sess-1", 1)
	sub2 := hub.Subscribe("sess-2", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus.Subscribe(ctx, hub)
	waitReady(t, bus)

	bus.Publish(ctx, "sess-2")

	select {
	case <-sub2.C:
	case <-time.After(5 * time.Second):
		t.Fatal("sess-2 的订阅者没收到唤醒")
	}
	select {
	case <-sub1.C:
		t.Error("sess-1 的订阅者不该被 sess-2 的信号叫醒")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBus_PublishWithEmptySessionIDIsIgnored(t *testing.T) {
	bus, _, _ := newTestBus(t)
	hub := NewHub()
	sub := hub.Subscribe("sess-1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus.Subscribe(ctx, hub)
	waitReady(t, bus)

	// 空 session_id 定位不到任何订阅者:不该炸,也不该叫醒任何人。
	bus.Publish(ctx, "")

	select {
	case <-sub.C:
		t.Error("空 session_id 不该叫醒任何订阅者")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBus_PublishDoesNotPanicWhenRedisIsGone(t *testing.T) {
	bus, s, _ := newTestBus(t)
	s.Close() // Redis 没了

	done := make(chan struct{})
	go func() {
		bus.Publish(context.Background(), "sess-1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Redis 挂了之后 Publish 还在阻塞")
	}
}

// 总线关掉之后不该再发信号。
//
// 关掉之后连接池可能已经不可用了,再发只会报错 ——
// 而且"关停中"和"正在发"混在一起会让关停顺序变得不可预测。
//
// 这条锁住 Publish 里的 isClosed 检查。
func TestBus_PublishAfterCloseDoesNothing(t *testing.T) {
	bus, _, rdb := newTestBus(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 自己订阅那个频道,直接看有没有消息 —— 走总线内部那条路是看不出来的:
	// 总线关闭时它自己的订阅也退了,Notify 根本不会发生。
	ps := rdb.Subscribe(ctx, wakeChannel)
	defer ps.Close()
	if _, err := ps.Receive(ctx); err != nil {
		t.Fatalf("建立测试订阅失败: %v", err)
	}

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	bus.Publish(ctx, "sess-1")

	recvCtx, cancelRecv := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelRecv()
	if _, err := ps.Receive(recvCtx); err == nil {
		t.Error("总线关闭之后不该再发唤醒信号")
	}
}

// ★ 「发布必须有界」这条不变量,哪怕调用方传的是**无超时**的客户端。
//
// ## 为什么这条值得单独立一条用例
//
// Publish 跑在事件发布路径上(Agent 的 gRPC 事件流那个 goroutine),
// 那条链路卡不得。而 go-redis 的命令路径是:
//
//	cn.WithReader(c.context(ctx), c.opt.ReadTimeout, ...)
//
// `baseClient.context()` 在 ContextTimeoutEnabled=false(默认)时
// 把调用方的 ctx 换成 context.Background(),于是 **ctx 上的超时到不了 socket**;
// 剩下的只有 opt.ReadTimeout。而订阅用的那个客户端 ReadTimeout 是 -1,
// 被 options.init() 归一成 0 = 「没有超时」。
//
// 结果:Redis 半开(TCP 建立了但对端不回话)时 PUBLISH 永久阻塞。
//
// 所以 NewBus 里必须给发布客户端套一层 WithTimeout(publishTimeout)。
// 少了它,这个用例会超时失败。
func TestBus_PublishIsBoundedEvenOnUnboundedClient(t *testing.T) {
	addr := blackholeAddr(t)

	// 复刻 redisx.NewPubSub 造出来的那个客户端:读/写超时都是 -1,
	// 而 options.init() 会把 -1 归一成 0(没有超时)。
	unbounded := redis.NewClient(&redis.Options{
		Addr:         addr,
		ReadTimeout:  -1,
		WriteTimeout: -1,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { unbounded.Close() })

	bus := NewBus(unbounded, unbounded)
	if bus == nil {
		t.Fatal("NewBus 返回 nil")
	}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		bus.Publish(context.Background(), "sess-1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish 在 Redis 半开时阻塞了 —— 它跑在 Agent 事件流那条 goroutine 上," +
			"一卡就是整个任务流卡住")
	}
	// 兜底:即使没卡死,也不该远超 publishTimeout。
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Publish 花了 %v,期望它在 publishTimeout(200ms)附近就返回", elapsed)
	}
}

// blackholeAddr 起一个「连上了但永远不回话」的 TCP 服务端,返回它的地址。
//
// 这模拟的是 Redis **半开**:TCP 握手成功,但对端不再应答。
// 和「Redis 直接挂了」不一样 —— 那种是连接被拒,会快速失败,
// 根本触发不了「无限阻塞」这个bug。
func blackholeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
			// 收下就放着,不回一个字。
			go func(c net.Conn) {
				buf := make([]byte, 512)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestBus_SubscribeExitsOnContextCancel(t *testing.T) {
	bus, _, _ := newTestBus(t)
	hub := NewHub()

	ctx, cancel := context.WithCancel(context.Background())
	bus.Subscribe(ctx, hub)
	waitReady(t, bus)

	cancel()

	select {
	case <-bus.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消之后 Subscribe 没有退出")
	}
}

func TestBus_CloseStopsSubscribeLoop(t *testing.T) {
	bus, _, _ := newTestBus(t)
	hub := NewHub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus.Subscribe(ctx, hub)
	waitReady(t, bus)

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close 幂等。
	if err := bus.Close(); err != nil {
		t.Fatalf("第二次 Close: %v", err)
	}

	select {
	case <-bus.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 之后后台循环没有退出")
	}
}

func TestBus_SubscribeWithNilHubIsSafe(t *testing.T) {
	bus, _, _ := newTestBus(t)
	bus.Subscribe(context.Background(), nil) // 不许 panic
}

func TestBus_SubscribeSurvivesRedisBeingDown(t *testing.T) {
	// Redis 在订阅之前就挂了:总线不能因此崩掉,
	// 它应该退避重连,等 Redis 回来。
	bus, s, _ := newTestBus(t)
	s.Close()

	hub := NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus.Subscribe(ctx, hub)

	// 挂着的时候不许 panic,也不许把总线卡死。
	time.Sleep(200 * time.Millisecond)

	cancel()
	select {
	case <-bus.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消之后 Subscribe 没有退出")
	}
}

func TestBus_RecoversAfterRedisComesBack(t *testing.T) {
	// Redis 重启之后,总线要能自己恢复 —— 唤醒总线是加速器,
	// 它自己先倒了不该让 SSE 整个不可用。
	bus, s, _ := newTestBus(t)
	hub := NewHub()
	sub := hub.Subscribe("sess-1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus.Subscribe(ctx, hub)
	waitReady(t, bus)

	s.Close()
	time.Sleep(100 * time.Millisecond)
	s.Restart()

	// 反复发:信号是 fire-and-forget,重连完成前发的那些会丢,
	// 所以这里要一直发到收到为止(最多 5 秒)。
	deadline := time.Now().Add(5 * time.Second)
	for {
		bus.Publish(ctx, "sess-1")
		select {
		case <-sub.C:
			return
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("Redis 恢复之后总线没能自己接回来")
		}
	}
}

func TestDecodeWake(t *testing.T) {
	sessionID, err := decodeWake(`{"session_id":"sess-1"}`)
	if err != nil {
		t.Fatalf("decodeWake: %v", err)
	}
	if sessionID != "sess-1" {
		t.Errorf("sessionID = %q, 期望 %q", sessionID, "sess-1")
	}
}

func TestDecodeWake_RejectsBadPayload(t *testing.T) {
	// 解不出来就不能拿去通知 —— 宁可少一次唤醒,
	// 也不能把信号送到错的会话上。
	for _, payload := range []string{"", "不是 JSON", `{"session_id":""}`, `{"其它":"字段"}`} {
		if _, err := decodeWake(payload); err == nil {
			t.Errorf("payload = %q 应当解不出来", payload)
		}
	}
}
