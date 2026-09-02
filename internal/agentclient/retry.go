package agentclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	agentv1 "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/breaker"
)

// 流中断之后的重发策略。
//
// **为什么需要它:gRPC 自带的 retryPolicy 在这条路上基本是摆设。**
// 透明重试在客户端收到**第一条响应消息**的那一刻就 committed 了,而
// RunTask 的第一条事件(started)几毫秒就到。所以那份配置只覆盖得了
// "流根本没建起来",跑到一半断掉它一次都不会重。
//
// **为什么重发是安全的:Agent 侧本来就是照着"调用方会原样重发"写的。**
// run_conversation 起手三步 —— ensure_session(幂等补 sessions 行)、
// recover(从库里重建历史,顺带按 tool_call_id 从幂等表回填已执行工具的
// 真结果补桩)、_turn_in_flight(上一轮没跑完就不再重复记用户那句话,
// 直接从补桩后的历史续跑)。所以重发的代价被限制在"崩在哪一步就从哪一步
// 接着跑",不是从头再烧一遍 LLM,更不会重复执行有副作用的工具。
//
// 钥匙是 session_id + input,不是 seq —— seq 在 Agent 那侧是
// `itertools.count(1)`,每次 RunTask 都从 1 重新数,它是流内序号不是断点。
const (
	// maxStreamRetries 首发之外最多再发几次。
	//
	// 上限不能大:重发期间任务在库里是 running 且 updated_at 不动,
	// 总窗口超过 reconcile.stale_after(默认 15m)的话,回收器会把它标成
	// failed,然后重发的结果又被 cas 丢掉 —— 和"提前上报"是同一个坑,
	// 只是换了个人来踩。2 次 + 下面的退避总共十几秒,离 15m 很远,
	// 所以这条路上不用额外补心跳。
	maxStreamRetries = 2

	// retryBaseBackoff 第一次重发前等多久,之后按 attempt 翻倍(2s、4s)。
	//
	// 比 gRPC 那份 retryPolicy 里的 0.1s 大得多,因为要等的不是网络抖动,
	// 是**上一次的 worker 散场**:流断的时候 Agent 那边只是被
	// context.add_callback 置了个 cancel 标志,真正 unregister 要等
	// run_conversation 走到下一个 cancel_check,最坏是一整次 LLM 调用之后。
	// 没散场就重发会撞上 ALREADY_EXISTS,以及 _SessionLocks 的会话锁。
	retryBaseBackoff = 2 * time.Second
)

// streamOutcome 是一次 RunTask 尝试的结局。
//
// 合成一个结构而不是返回 (error, bool):两个相邻的返回值里一个是错误、
// 一个是布尔,调用方接反了编译器不会吭声。
type streamOutcome struct {
	// err 为 nil 表示流正常结束(EOF)。
	err error

	// gotEvent 这次尝试有没有收到过至少一条事件。
	// 这是**唯一**的重试判据,理由见 retryable。
	gotEvent bool
}

// runTask 消费一条事件流,断了就按策略原样重发,直到跑完或者重试用尽。
//
// OnStreamError 只在这里调,而且只调一次 —— 它对 service 的语义是
// "这个任务没救了,收尾吧",不是"某一次流断了"。
func (c *GRPCClient) runTask(ctx context.Context, req StartTaskReq,
	stream agentv1.AgentService_RunTaskClient) {

	gotEvent := false
	var lastErr error

	for attempt := 0; ; attempt++ {
		// stream 为 nil 表示上一轮的重发压根没发出去(ALREADY_EXISTS),
		// 这一轮直接去退避重发,没有流可消费。
		if stream != nil {
			out := c.consume(ctx, req.TaskUID, stream)
			// 跨尝试累计:第一次收到过事件就证明 Agent 接住了这个任务,
			// 这个事实不会因为后面某次重发一开头就断而失效。
			gotEvent = gotEvent || out.gotEvent
			stream = nil

			if out.err == nil {
				return // EOF,正常结束
			}
			lastErr = out.err
		}

		if !c.retryable(ctx, req.TaskUID, attempt, gotEvent, lastErr) {
			break
		}

		next, err := c.redial(ctx, req, attempt)
		if err != nil {
			if !errors.Is(err, ErrAlreadyRunning) {
				// 重发本身就没发出去,而且不是"等一下就好"那一类。
				// 注意收尾时上报的仍是 lastErr(流最后一次是怎么断的),
				// 不是这个 —— 用户该看到的不是我们抢救失败的细节。
				slog.Warn("重发任务失败,放弃重试",
					"task_uid", req.TaskUID, "attempt", attempt+1, "error", err)
				break
			}
			// 上一次的 worker 还没散场。下一轮退避更长,再来一次。
			slog.Info("上一次执行还没结束,等下一轮再重发",
				"task_uid", req.TaskUID, "attempt", attempt+1)
			continue
		}
		slog.Info("事件流中断,已原样重发任务",
			"task_uid", req.TaskUID, "attempt", attempt+1, "reason", lastErr)
		stream = next
	}

	c.handler.OnStreamError(ctx, req.TaskUID, lastErr)
}

// retryable 判断这次流中断值不值得重发。
//
// **判据是"之前有没有收到过事件",不是错误类型。** 流跑到一半断掉,在
// gRPC 那层和"压根连不上 Agent"是同一个状态码(Unavailable)、同一个哨兵,
// 按错误分不开这两件事。但收到过事件就证明 Agent 确实接住了这个任务
// 并且在跑,断掉是执行期故障 —— 那正是 Agent 侧续跑机制要处理的情况。
//
// 一条事件都没收到 = 交接压根没成功,重发只是拿 N 倍流量去撞一个已知
// 连不上的下游。那是熔断器的活。
func (c *GRPCClient) retryable(ctx context.Context, taskUID string, attempt int,
	gotEvent bool, err error) bool {

	switch {
	case !gotEvent:
		return false
	case attempt >= maxStreamRetries:
		return false
	case c.closing.Load():
		// 进程在关了,别再起新的流 —— Close() 还等着 wg 归零。
		return false
	case ctx.Err() != nil:
		// streamCtx 到期(AGENT_TASK_TIMEOUT)或被取消。
		return false
	case errors.Is(err, ErrTimeout):
		// ErrTimeout 自己的注释就写着:超时意味着请求可能已经到对面
		// 并且正在执行,重试就是重复执行。而这条路上的超时是
		// AGENT_TASK_TIMEOUT(默认 30m),重两次就是 90 分钟。
		return false
	case errors.Is(err, context.Canceled):
		return false
	default:
		// 机制在这儿,决定在 service:它才知道这个任务还是不是 running。
		return c.handler.ShouldRetry(ctx, taskUID, attempt)
	}
}

// redial 退避之后重新起一条流。
//
// 每次重发都自己拿一次 Allow、还一次 Record:一次 Allow 对应一次 Record
// 是熔断器的记账口径(见 consume)。让重发白嫖首发那个名额的话,
// N 次重试就绕过了熔断器,而且会把半开期的探针计数搞乱。
func (c *GRPCClient) redial(ctx context.Context, req StartTaskReq, attempt int) (
	agentv1.AgentService_RunTaskClient, error) {

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(c.backoffFor(attempt)):
	}
	// 退避期间进程可能开始关了。retryable 在退避**之前**判过一次,
	// 这里再判一次:Close() 只等 wg 五秒,这时候起一条新流只会让它超时,
	// 而那条流的事件谁也不会消费了。
	if c.closing.Load() {
		return nil, ErrClientClosing
	}

	if !c.breaker.Allow() {
		return nil, fmt.Errorf("重发任务: %w(%w)", ErrUnavailable, breaker.ErrOpen)
	}

	stream, err := c.cli.RunTask(ctx, &agentv1.RunTaskRequest{
		TaskUid:   req.TaskUID,
		SessionId: req.SessionID,
		Input:     req.Input,
		TraceId:   req.TraceID,
	})
	if err != nil {
		tErr := translateGRPCError("重发任务", err)
		c.breaker.Record(!countsAsUnhealthy(tErr))
		return nil, tErr
	}
	// 名额留给 consume 去还:RunTask 返回 nil 不代表对面活着(它是
	// server-streaming,头都还没发),第一条 Recv 才是证据。
	return stream, nil
}

// backoffFor 第 attempt 次失败之后等多久。attempt 从 0 起,所以是 base、2*base。
func (c *GRPCClient) backoffFor(attempt int) time.Duration {
	return c.retryBackoff << attempt
}
