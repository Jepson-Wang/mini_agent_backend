package agentclient

import (
	"context"
	"errors"
	"testing"
	"time"

	agentv1 "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/breaker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeAgentClient 按顺序吐出预先排好的流(或错误),并记下每次 RunTask 的入参。
// 记入参是为了验一条契约:重发必须原样带上同一个 session_id / input,
// Agent 那边的续跑全靠这两个字段。
type fakeAgentClient struct {
	agentv1.AgentServiceClient
	results []redialResult
	calls   []*agentv1.RunTaskRequest
}

type redialResult struct {
	stream *fakeStream
	err    error
}

func (f *fakeAgentClient) RunTask(_ context.Context, in *agentv1.RunTaskRequest,
	_ ...grpc.CallOption) (grpc.ServerStreamingClient[agentv1.TaskEvent], error) {

	f.calls = append(f.calls, in)
	i := len(f.calls) - 1
	if i >= len(f.results) {
		return nil, status.Error(codes.Unavailable, "测试没准备这么多次重发")
	}
	if f.results[i].err != nil {
		return nil, f.results[i].err
	}
	return f.results[i].stream, nil
}

// newRetryClient 造一个能跑 runTask 的客户端:退避压到几乎为零,
// 否则每条用例都要真睡几秒。
func newRetryClient(t *testing.T, results ...redialResult) (*GRPCClient, *recordingHandler, *fakeAgentClient) {
	t.Helper()

	c, h := newConsumeClient(t, 0) // 不启用熔断,这几条只看重发本身
	cli := &fakeAgentClient{results: results}
	c.cli = cli
	c.retryBackoff = time.Millisecond
	return c, h, cli
}

var retryReq = StartTaskReq{
	TaskUID:   consumeUID,
	SessionID: "sess-1",
	Input:     "你好",
	TraceID:   "trace-1",
}

// TestRunTask_RetriesAfterStreamBreak 是这条链路的主线。
//
// 收到过事件 = Agent 确实接住了这个任务并且在跑,后面流断了是执行期故障。
// 这时候原样重发一次,Agent 侧的 recover() 会把历史补桩后从断点续跑。
// 重发成功的话,这个任务从头到尾都不该走 OnStreamError —— 走了就是终态,
// 之后重发跑出来的 started / finished 会撞上 cas 的 from=[pending,running]
// 一条都匹配不上,被当成迟到回调静静丢掉。
func TestRunTask_RetriesAfterStreamBreak(t *testing.T) {
	// 第二次:干干净净跑完。
	second := &fakeStream{events: []*agentv1.TaskEvent{
		ev(1, agentv1.EventType_EVENT_TYPE_STARTED),
		ev(2, agentv1.EventType_EVENT_TYPE_FINISHED),
	}}
	c, h, cli := newRetryClient(t, redialResult{stream: second})

	// 第一次:推了一条事件之后断掉。
	first := &fakeStream{
		events: []*agentv1.TaskEvent{ev(1, agentv1.EventType_EVENT_TYPE_STARTED)},
		err:    status.Error(codes.Unavailable, "connection reset"),
	}
	c.runTask(context.Background(), retryReq, first)

	if len(cli.calls) != 1 {
		t.Fatalf("重发了 %d 次,期望 1 次", len(cli.calls))
	}
	if h.streamErr != nil {
		t.Errorf("重发之后跑成功了,却还是把流错误交了上去(%v)——任务会被标成 failed", h.streamErr)
	}
	if len(h.events) != 3 {
		t.Errorf("交上来 %d 条事件,期望 3 条(第一次 1 条 + 第二次 2 条)", len(h.events))
	}
}

// 重发必须原样带上同一个 session_id + input:Agent 的续跑契约就是这两个字段
// (ensure_session → recover → _turn_in_flight)。少带一个,对面会当成一次
// 全新的对话从头再跑一遍。
func TestRunTask_RetryCarriesSameSessionAndInput(t *testing.T) {
	c, _, cli := newRetryClient(t, redialResult{stream: &fakeStream{}})

	c.runTask(context.Background(), retryReq, &fakeStream{
		events: []*agentv1.TaskEvent{ev(1, agentv1.EventType_EVENT_TYPE_STARTED)},
		err:    status.Error(codes.Unavailable, "connection reset"),
	})

	if len(cli.calls) != 1 {
		t.Fatalf("重发了 %d 次,期望 1 次", len(cli.calls))
	}
	got := cli.calls[0]
	if got.TaskUid != retryReq.TaskUID || got.SessionId != retryReq.SessionID ||
		got.Input != retryReq.Input || got.TraceId != retryReq.TraceID {
		t.Errorf("重发的请求是 %+v,期望和首发完全一致 %+v", got, retryReq)
	}
}

// 一条事件都没收到就断了 = 交接压根没成功。这是熔断器的活,不是重试的。
// 这条线不划清楚的话,Agent 挂掉时每个新任务都会自带 N 次重发,
// 把一个已知连不上的下游按 N 倍流量打。
func TestRunTask_NoEventMeansNoRetry(t *testing.T) {
	c, h, cli := newRetryClient(t, redialResult{stream: &fakeStream{}})

	c.runTask(context.Background(), retryReq,
		&fakeStream{err: status.Error(codes.Unavailable, "connection refused")})

	if len(cli.calls) != 0 {
		t.Errorf("一条事件都没收到还重发了 %d 次", len(cli.calls))
	}
	if h.streamErr == nil {
		t.Fatal("没重发也没上报,任务会永远停在 running")
	}
	if !errors.Is(h.streamErr, ErrUnavailable) {
		t.Errorf("上报的是 %v,期望 ErrUnavailable", h.streamErr)
	}
}

// 用户在退避期间点了取消,库里已经是 canceled 了。这时候还重发的话,
// Agent 会把一个用户明确叫停的任务重跑一遍 —— 白烧一轮 LLM,而且
// 跑出来的 finished 撞上 cas 也写不进去,纯粹是浪费。
//
// agentclient 不读库,所以"还该不该重发"只能问 service。
func TestRunTask_HandlerCanVetoRetry(t *testing.T) {
	c, h, cli := newRetryClient(t, redialResult{stream: &fakeStream{}})
	h.vetoRetry = true

	c.runTask(context.Background(), retryReq, &fakeStream{
		events: []*agentv1.TaskEvent{ev(1, agentv1.EventType_EVENT_TYPE_STARTED)},
		err:    status.Error(codes.Unavailable, "connection reset"),
	})

	if len(cli.calls) != 0 {
		t.Errorf("handler 说了别重发,还是重发了 %d 次", len(cli.calls))
	}
	if h.streamErr == nil {
		t.Error("不重发就得就地收尾,否则任务停在 running")
	}
}

// 流断了的时候,Agent 那边的 worker 通常还没散场:gRPC 只是通过
// context.add_callback 给它置了个 cancel 标志,真正 unregister 要等
// run_conversation 走到下一个 cancel_check —— 最坏是一整次 LLM 调用之后。
// 这段窗口里重发会被 self._tasks.register 挡回来(ALREADY_EXISTS)。
//
// 这**不是**"请求有问题",退避之后再来一次就好了。归到 ErrBadRequest
// 那一类(不重试)的话,重发在最常见的时序下每次都会失败。
func TestRunTask_RetriesWhileAgentStillHoldsTask(t *testing.T) {
	c, h, cli := newRetryClient(t,
		redialResult{err: status.Error(codes.AlreadyExists, "任务已在运行中")},
		redialResult{stream: &fakeStream{events: []*agentv1.TaskEvent{
			ev(1, agentv1.EventType_EVENT_TYPE_FINISHED),
		}}},
	)

	c.runTask(context.Background(), retryReq, &fakeStream{
		events: []*agentv1.TaskEvent{ev(1, agentv1.EventType_EVENT_TYPE_STARTED)},
		err:    status.Error(codes.Unavailable, "connection reset"),
	})

	if len(cli.calls) != 2 {
		t.Fatalf("重发了 %d 次,期望 2 次(第一次撞上 ALREADY_EXISTS,退避后再来)", len(cli.calls))
	}
	if h.streamErr != nil {
		t.Errorf("第二次重发跑通了,不该收尾: %v", h.streamErr)
	}
}

// 每次重发都要自己过一遍熔断器。
//
// 让重发白嫖首发那个 Allow 名额的话,一个已知连不上的 Agent 会被按
// (1 + maxStreamRetries)倍的流量打 —— 熔断器装了等于没装。而且
// 一次 Allow 对多次 Record 会把半开期的计数搞乱。
func TestRunTask_EachRetryGoesThroughBreaker(t *testing.T) {
	// 两次重发都准备好了流,但第二次轮不到:熔断器会先打开。
	c, h, cli := newRetryClient(t,
		redialResult{stream: &fakeStream{err: status.Error(codes.Unavailable, "down")}},
		redialResult{stream: &fakeStream{}},
	)
	br, err := breaker.New(breaker.Config{FailureThreshold: 1, Cooldown: time.Hour, HalfOpenMax: 1})
	if err != nil {
		t.Fatalf("breaker.New: %v", err)
	}
	c.breaker = br
	c.breaker.Allow() // 首发那次名额

	c.runTask(context.Background(), retryReq, &fakeStream{
		events: []*agentv1.TaskEvent{ev(1, agentv1.EventType_EVENT_TYPE_STARTED)},
		err:    status.Error(codes.Unavailable, "connection reset"),
	})

	if c.breaker.State() != breaker.StateOpen {
		t.Errorf("重发一路撞在死掉的 Agent 上,熔断器却是 %v", c.breaker.State())
	}
	if len(cli.calls) != 1 {
		t.Errorf("重发了 %d 次,期望 1 次 —— 第二次该被熔断器挡下", len(cli.calls))
	}
	if h.streamErrCalls != 1 {
		t.Errorf("OnStreamError 调了 %d 次,期望恰好 1 次", h.streamErrCalls)
	}
}

// 重试用尽之后必须收尾,而且**只收一次**。
// 漏了收尾任务永远停在 running;收多次会在日志里刷出一串假故障。
func TestRunTask_ExhaustedRetriesReportOnce(t *testing.T) {
	broken := func() *fakeStream {
		return &fakeStream{
			events: []*agentv1.TaskEvent{ev(1, agentv1.EventType_EVENT_TYPE_THINKING)},
			err:    status.Error(codes.Unavailable, "connection reset"),
		}
	}
	c, h, cli := newRetryClient(t,
		redialResult{stream: broken()},
		redialResult{stream: broken()},
		redialResult{stream: broken()}, // 这次不该发生
	)

	c.runTask(context.Background(), retryReq, broken())

	if len(cli.calls) != maxStreamRetries {
		t.Errorf("重发了 %d 次,期望 %d 次", len(cli.calls), maxStreamRetries)
	}
	if h.streamErrCalls != 1 {
		t.Fatalf("OnStreamError 调了 %d 次,期望恰好 1 次", h.streamErrCalls)
	}
	if !errors.Is(h.streamErr, ErrUnavailable) {
		t.Errorf("上报的是 %v,期望 ErrUnavailable", h.streamErr)
	}
}

// 超时不重发。ErrTimeout 自己的注释就写着:超时意味着请求可能已经到了
// 对面并且正在执行,重试就是重复执行。而这条路上的超时是
// AGENT_TASK_TIMEOUT(默认 30m),重两次就是 90 分钟。
func TestRunTask_TimeoutIsNotRetried(t *testing.T) {
	c, h, cli := newRetryClient(t, redialResult{stream: &fakeStream{}})

	c.runTask(context.Background(), retryReq, &fakeStream{
		events: []*agentv1.TaskEvent{ev(1, agentv1.EventType_EVENT_TYPE_STARTED)},
		err:    status.Error(codes.DeadlineExceeded, "task timeout"),
	})

	if len(cli.calls) != 0 {
		t.Errorf("超时也重发了 %d 次", len(cli.calls))
	}
	if !errors.Is(h.streamErr, ErrTimeout) {
		t.Errorf("上报的是 %v,期望 ErrTimeout", h.streamErr)
	}
}
