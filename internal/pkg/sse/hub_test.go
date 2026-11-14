package sse

import (
	"sync"
	"testing"
	"time"
)

func TestHub_SubscribeReceivesNotify(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("sess-1", 1)

	h.Notify("sess-1")

	select {
	case <-sub.C:
	case <-time.After(time.Second):
		t.Fatal("订阅之后没收到通知")
	}
}

func TestHub_NotifyOnlyMatchingSession(t *testing.T) {
	h := NewHub()
	sub1 := h.Subscribe("sess-1", 1)
	sub2 := h.Subscribe("sess-2", 1)

	h.Notify("sess-1")

	select {
	case <-sub1.C:
	case <-time.After(time.Second):
		t.Fatal("sess-1 的订阅者没收到通知")
	}
	select {
	case <-sub2.C:
		t.Fatal("sess-2 的订阅者不该收到 sess-1 的通知")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_UnsubscribeRemovesSubscriber(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("sess-1", 1)
	if h.Count() != 1 {
		t.Fatalf("Count = %d, 期望 1", h.Count())
	}

	h.Unsubscribe(sub)

	if h.Count() != 0 {
		t.Errorf("注销之后 Count = %d, 期望 0", h.Count())
	}
	if h.CountSession("sess-1") != 0 {
		t.Errorf("注销之后 CountSession = %d, 期望 0", h.CountSession("sess-1"))
	}

	// 摘掉之后不能再收到通知。
	h.Notify("sess-1")
	select {
	case <-sub.C:
		t.Error("注销之后不该再收到通知")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_UnsubscribeIsIdempotent(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("sess-1", 1)

	h.Unsubscribe(sub)
	h.Unsubscribe(sub) // 摘两次不许 panic
	h.Unsubscribe(sub)

	if h.Count() != 0 {
		t.Errorf("Count = %d, 期望 0", h.Count())
	}
}

func TestHub_UnsubscribeNilIsSafe(t *testing.T) {
	h := NewHub()
	h.Unsubscribe(nil) // 不许 panic
	h.Unsubscribe(nil)
}

func TestHub_UnsubscribeOtherHubIsSafe(t *testing.T) {
	// 拿别的 Hub 注册的订阅者来注销:不该 panic,也不该影响自己这张表。
	h1 := NewHub()
	h2 := NewHub()
	sub := h1.Subscribe("sess-1", 1)

	h2.Unsubscribe(sub)

	if h1.Count() != 1 {
		t.Errorf("别的 Hub 不该摘掉本 Hub 的订阅者: Count = %d", h1.Count())
	}
}

func TestHub_UnsubscribeRemovesOwnerEntry(t *testing.T) {
	// 这条锁住**反向索引**的清理。
	//
	// 为什么单独一条:owner 是「订阅者 → 会话」的反向索引,存在的理由是
	// Subscriber.SessionID 是导出字段 —— 调用方改了它,Hub 就再也找不到
	// 这个订阅者,注销不掉。所以 Hub 自己记一份。
	//
	// 变异验证发现:只删 owner 那一行,**其它用例全部照样绿** ——
	// 因为 Count / CountSession 走的是 subs 那半,反向索引没被任何断言碰到。
	// 而不清理的后果是 owner 无限增长:每次 SSE 连接断开都留一条
	// *Subscriber 残留(用户刷新页面就多一条)。而 Hub 自己的注释写着
	// 「摘干净是这个方法存在的全部意义」。
	//
	// owner 是未导出的,但测试在同一个包里,可以直接看。
	h := NewHub()
	sub := h.Subscribe("sess-1", 1)
	if len(h.owner) != 1 {
		t.Fatalf("注册之后反向索引里有 %d 条,期望 1", len(h.owner))
	}

	h.Unsubscribe(sub)

	if len(h.owner) != 0 {
		t.Errorf("注销之后反向索引里还留着 %d 条,期望 0 —— 每断一条连接就漏一条,那是 leaks", len(h.owner))
	}

	// 再注销一次也不能把反向索引弄坏(幂等)。
	h.Unsubscribe(sub)
	if len(h.owner) != 0 {
		t.Errorf("重复注销之后反向索引里有 %d 条,期望 0", len(h.owner))
	}
}

func TestHub_NotifyDoesNotBlockWhenChannelIsFull(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("sess-1", 1)

	// 把通道填满:第一次通知投递成功,第二次就该被丢掉。
	h.Notify("sess-1")
	if len(sub.C) != 1 {
		t.Fatalf("通道长度 = %d, 期望 1", len(sub.C))
	}

	done := make(chan struct{})
	go func() {
		h.Notify("sess-1")
		h.Notify("sess-1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify 在通道满的时候阻塞了 —— 慢订阅者不能拖住事件发布方")
	}
	if len(sub.C) != 1 {
		t.Errorf("通道长度 = %d, 期望仍然是 1(满了就该丢)", len(sub.C))
	}
}

func TestHub_CountSessionCountsPerSession(t *testing.T) {
	h := NewHub()
	h.Subscribe("sess-1", 1)
	h.Subscribe("sess-1", 1)
	h.Subscribe("sess-2", 1)

	if got := h.CountSession("sess-1"); got != 2 {
		t.Errorf("CountSession(sess-1) = %d, 期望 2", got)
	}
	if got := h.CountSession("sess-2"); got != 1 {
		t.Errorf("CountSession(sess-2) = %d, 期望 1", got)
	}
	if got := h.CountSession("sess-none"); got != 0 {
		t.Errorf("CountSession(不存在的会话) = %d, 期望 0", got)
	}
	if got := h.Count(); got != 3 {
		t.Errorf("Count = %d, 期望 3(跨会话总数)", got)
	}
}

// 单用户连接数要**跨会话**算。
//
// 为什么需要这一层:只有「每会话」和「全局」两个闸的话,一个账号拥有 N 个会话
// 就能开 N × MaxConnPerSession 条连接,把全进程的预算吃光,别的用户全部连不上。
// 那等于一个账号替所有人做决定。
func TestHub_CountUserCountsAcrossSessions(t *testing.T) {
	h := NewHub()
	h.Subscribe("sess-1", 7)
	h.Subscribe("sess-2", 7)
	h.Subscribe("sess-3", 8)

	if got := h.CountUser(7); got != 2 {
		t.Errorf("CountUser(7) = %d, 期望 2(跨会话)", got)
	}
	if got := h.CountUser(8); got != 1 {
		t.Errorf("CountUser(8) = %d, 期望 1", got)
	}
	if got := h.CountUser(999); got != 0 {
		t.Errorf("CountUser(没订阅过的用户) = %d, 期望 0", got)
	}
}

// 注销必须把用户计数减掉,而且**只减一次**。
//
// 减漏了 → 那个用户的额度被慢慢吃光,最后他自己连不上;
// 减多了 → 额度虚高,那个闸形同虚设。
//
// 幂等由 owner 那张表保证:第二次摘的时候 owner 里已经没这条了,直接返回。
func TestHub_UnsubscribeDecrementsUserCount(t *testing.T) {
	h := NewHub()
	sub1 := h.Subscribe("sess-1", 7)
	sub2 := h.Subscribe("sess-2", 7)

	if got := h.CountUser(7); got != 2 {
		t.Fatalf("CountUser(7) = %d, 期望 2", got)
	}

	h.Unsubscribe(sub1)
	if got := h.CountUser(7); got != 1 {
		t.Errorf("注销一条之后 CountUser(7) = %d, 期望 1", got)
	}

	// 再注销一次:不许把计数减到 0 以下,也不许变成负数。
	h.Unsubscribe(sub1)
	if got := h.CountUser(7); got != 1 {
		t.Errorf("重复注销之后 CountUser(7) = %d, 期望仍然是 1", got)
	}

	// 全部摘完之后,这个用户的条目要从 users 表里整个消失 ——
	// 那张表是按用户长起来的,不清的话用得越久条目越多。
	h.Unsubscribe(sub2)
	if got := h.CountUser(7); got != 0 {
		t.Errorf("全部注销之后 CountUser(7) = %d, 期望 0", got)
	}
	if len(h.users) != 0 {
		t.Errorf("users 表里还留着 %d 条,期望 0 —— 用户用得越久条目越多,那是泄漏", len(h.users))
	}
}

func TestHub_NilHubIsSafe(t *testing.T) {
	var h *Hub
	sub := h.Subscribe("sess-1", 1)
	if sub == nil || sub.C == nil {
		t.Fatal("nil Hub 的 Subscribe 也该返回一个可用的订阅者")
	}
	h.Notify("sess-1")
	h.Unsubscribe(sub)
	if got := h.Count(); got != 0 {
		t.Errorf("nil Hub 的 Count = %d, 期望 0", got)
	}
	if got := h.CountSession("sess-1"); got != 0 {
		t.Errorf("nil Hub 的 CountSession = %d, 期望 0", got)
	}
}

func TestHub_ConcurrentNotifyAndUnsubscribe(t *testing.T) {
	// 这个用例是给 -race 用的:Hub 会被 HTTP handler 的 goroutine
	// 和事件发布方的 goroutine 同时碰。
	h := NewHub()
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sub := h.Subscribe("sess-1", 1)
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.Notify("sess-1")
		}()
		go func() {
			defer wg.Done()
			h.Unsubscribe(sub)
			// 摘两次:必须是幂等的
			h.Unsubscribe(sub)
		}()
	}
	wg.Wait()

	if got := h.Count(); got != 0 {
		t.Errorf("并发跑完之后 Count = %d, 期望 0(不能有泄漏)", got)
	}
}
