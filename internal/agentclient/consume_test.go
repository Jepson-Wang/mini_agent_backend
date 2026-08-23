package agentclient

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	agentv1 "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/breaker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeStream 假装是 RunTask 的返回流。consume 只用 Recv,
// 剩下的 ClientStream 方法嵌一个 nil 接口占位就够了 —— 真被调到会 panic,
// 那正是我们想知道的(说明 consume 开始依赖流的其他能力了)。
type fakeStream struct {
	grpc.ClientStream
	events []*agentv1.TaskEvent
	err    error // 事件放完之后返回它;nil 表示正常 EOF
	i      int
}

func (f *fakeStream) Recv() (*agentv1.TaskEvent, error) {
	if f.i < len(f.events) {
		ev := f.events[f.i]
		f.i++
		return ev, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return nil, io.EOF
}

// recordingHandler 记下 consume 交上来的东西。
type recordingHandler struct {
	events    []Event
	streamErr error
}

func (h *recordingHandler) OnEvent(_ context.Context, ev Event) error {
	h.events = append(h.events, ev)
	return nil
}

func (h *recordingHandler) OnStreamError(_ context.Context, _ string, err error) {
	h.streamErr = err
}

// newConsumeClient 造一个只够跑 consume 的客户端:没有连接,没有 gRPC。
func newConsumeClient(t *testing.T, threshold int) (*GRPCClient, *recordingHandler) {
	t.Helper()

	br, err := breaker.New(breaker.Config{
		FailureThreshold: threshold,
		// 冷却给得很短:下面有一条用例要等到半开。State() 是只读的,
		// 不会因为时间流逝就自己推进,所以其余用例不受影响。
		Cooldown:    cooldown,
		HalfOpenMax: 1,
	})
	if err != nil {
		t.Fatalf("breaker.New: %v", err)
	}
	h := &recordingHandler{}
	return &GRPCClient{handler: h, breaker: br}, h
}

const (
	consumeUID = "01J8ZQXK7P0000000000000000"

	// cooldown 短到可以用一次 Sleep 跨过去。真实时钟是单调的,
	// 睡 20 倍的时长不会抖 —— 这里不值得为了一条用例给 breaker
	// 开一个只有测试用的时钟注入口。
	cooldown = 10 * time.Millisecond
)

func ev(seq int64, typ agentv1.EventType) *agentv1.TaskEvent {
	return &agentv1.TaskEvent{TaskUid: consumeUID, Seq: seq, Type: typ}
}

// TestConsume_StreamFailureOpensBreaker 是熔断这条线上最容易写错的一处。
//
// RunTask 是 server-streaming:c.cli.RunTask(...) **不等对面回话就返回**。
// Agent 进程没了的时候它照样返回 nil,错误要到第一次 Recv() 才浮出来。
// 所以熔断器只能在这里记账 —— 在 StartTask 里按 RunTask 的返回值记的话,
// 熔断器会永远认为下游是好的,装了等于没装,而且没有任何迹象。
func TestConsume_StreamFailureOpensBreaker(t *testing.T) {
	c, h := newConsumeClient(t, 1) // 一次失败就该打开

	if !c.breaker.Allow() {
		t.Fatal("初始状态该放行")
	}
	c.consume(context.Background(), consumeUID,
		&fakeStream{err: status.Error(codes.Unavailable, "connection refused")})

	if c.breaker.State() != breaker.StateOpen {
		t.Errorf("流一开头就断,熔断器却是 %v —— 之后每个新任务还会去撞一遍这个死掉的 Agent",
			c.breaker.State())
	}
	if h.streamErr == nil {
		t.Fatal("没有把流错误交给 handler,任务会永远停在 running")
	}
	if !errors.Is(h.streamErr, ErrUnavailable) {
		t.Errorf("交给 handler 的错误是 %v,期望翻译过的 ErrUnavailable", h.streamErr)
	}
}

// 收到过事件就说明对面在干活。之后流断了(网络抖动、Agent 中途挂了)
// 不该算它有病 —— 不然一个跑到一半被 kill 的任务就能推动熔断,
// 而那更可能是任务自己的问题。
func TestConsume_EventBeforeFailureCountsAsHealthy(t *testing.T) {
	c, _ := newConsumeClient(t, 1)

	c.breaker.Allow()
	c.consume(context.Background(), consumeUID, &fakeStream{
		events: []*agentv1.TaskEvent{ev(1, agentv1.EventType_EVENT_TYPE_STARTED)},
		err:    status.Error(codes.Unavailable, "connection reset"),
	})

	if c.breaker.State() != breaker.StateClosed {
		t.Errorf("对面明明推过事件,熔断器却成了 %v", c.breaker.State())
	}
}

// 正常 EOF 结束 = 对面是活的。
func TestConsume_CleanEOFCountsAsHealthy(t *testing.T) {
	c, h := newConsumeClient(t, 1)

	c.breaker.Allow()
	c.consume(context.Background(), consumeUID, &fakeStream{
		events: []*agentv1.TaskEvent{
			ev(1, agentv1.EventType_EVENT_TYPE_STARTED),
			ev(2, agentv1.EventType_EVENT_TYPE_FINISHED),
		},
	})

	if c.breaker.State() != breaker.StateClosed {
		t.Errorf("流正常结束,熔断器却是 %v", c.breaker.State())
	}
	if h.streamErr != nil {
		t.Errorf("EOF 不是错误,不该走 OnStreamError: %v", h.streamErr)
	}
	if len(h.events) != 2 {
		t.Errorf("交上来 %d 条事件,期望 2 条", len(h.events))
	}
}

// TestConsume_RecordsExactlyOnce:一次 Allow 只能对一次 Record。
//
// 多记会把半开期的计数搞乱(一个探针成功却被记了三次,直接闭合),
// 少记会让探针名额漏掉、熔断器再也回不到 closed。这里用半开状态验:
// 半开期只有一个名额,收了三条事件之后状态必须是 closed 而不是别的。
func TestConsume_RecordsExactlyOnce(t *testing.T) {
	c, _ := newConsumeClient(t, 1)

	// 先打开,再等到冷却期满进半开。
	c.breaker.Allow()
	c.consume(context.Background(), consumeUID,
		&fakeStream{err: status.Error(codes.Unavailable, "down")})
	if c.breaker.State() != breaker.StateOpen {
		t.Fatalf("前置条件不成立:熔断器该是 open,实际 %v", c.breaker.State())
	}

	time.Sleep(20 * cooldown) // 跨过冷却期
	if !c.breaker.Allow() {
		t.Fatal("冷却期满该放一个探针")
	}

	c.consume(context.Background(), consumeUID, &fakeStream{
		events: []*agentv1.TaskEvent{
			ev(1, agentv1.EventType_EVENT_TYPE_STARTED),
			ev(2, agentv1.EventType_EVENT_TYPE_THINKING),
			ev(3, agentv1.EventType_EVENT_TYPE_FINISHED),
		},
	})

	if c.breaker.State() != breaker.StateClosed {
		t.Errorf("探针成功后该恢复,实际 %v", c.breaker.State())
	}
	// 再验一次:恢复之后照常放行,说明计数是干净的。
	if !c.breaker.Allow() {
		t.Error("恢复之后不放行,说明 Record 的次数对不上")
	}
}

// task_uid 对不上的事件要丢掉,别写到别人的任务上去。
func TestConsume_DropsMismatchedTaskUID(t *testing.T) {
	c, h := newConsumeClient(t, 0) // 不启用熔断,这条只看事件

	c.consume(context.Background(), consumeUID, &fakeStream{
		events: []*agentv1.TaskEvent{
			{TaskUid: "01J8ZQXK7P000000000000OTHER", Seq: 1,
				Type: agentv1.EventType_EVENT_TYPE_FINISHED},
			ev(2, agentv1.EventType_EVENT_TYPE_FINISHED),
		},
	})

	if len(h.events) != 1 {
		t.Fatalf("交上来 %d 条,期望只有 1 条(另一条 task_uid 对不上)", len(h.events))
	}
	if h.events[0].Seq != 2 {
		t.Errorf("留下的是 seq=%d,期望 2", h.events[0].Seq)
	}
}
