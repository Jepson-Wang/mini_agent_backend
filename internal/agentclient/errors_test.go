package agentclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTranslateGRPCError_MapsCodesToSentinels(t *testing.T) {
	cases := []struct {
		code codes.Code
		want error
	}{
		{codes.Unavailable, ErrUnavailable},
		{codes.ResourceExhausted, ErrAgentBusy},
		{codes.DeadlineExceeded, ErrTimeout},
		{codes.InvalidArgument, ErrBadRequest},
		{codes.NotFound, ErrBadRequest},
		{codes.FailedPrecondition, ErrBadRequest},
		{codes.PermissionDenied, ErrBadRequest},
		{codes.Unimplemented, ErrBadRequest},
		{codes.Internal, ErrRemoteInternal},
		{codes.Unknown, ErrRemoteInternal},
		{codes.DataLoss, ErrRemoteInternal},
	}

	for _, c := range cases {
		got := translateGRPCError("测试", status.Error(c.code, "boom"))
		if !errors.Is(got, c.want) {
			t.Errorf("%v 翻成了 %v,期望 %v", c.code, got, c.want)
		}
	}
}

func TestTranslateGRPCError_NilStaysNil(t *testing.T) {
	if got := translateGRPCError("测试", nil); got != nil {
		t.Errorf("nil 不该被翻成 %v", got)
	}
}

// TestTranslateGRPCError_DropsRawMessage 盯的是一条会泄露内网拓扑的路径。
//
// Unavailable 的 status.Message() 里带着拨号目标:
//
//	transport: Error while dialing: dial tcp 10.0.0.7:50051: connect: connection refused
//
// 而这个 error 会一路走到 tasks.error_msg,那一列是**跟着任务详情回显给用户的**。
// 一旦有人图省事在这儿拼上 st.Message(),泄露不会有任何征兆。
func TestTranslateGRPCError_DropsRawMessage(t *testing.T) {
	raw := `transport: Error while dialing: dial tcp 10.0.0.7:50051: connect: connection refused`
	got := translateGRPCError("派发任务", status.Error(codes.Unavailable, raw))

	msg := got.Error()
	for _, leak := range []string{"10.0.0.7", "50051", "dial tcp"} {
		if strings.Contains(msg, leak) {
			t.Errorf("翻译后的错误里还带着 %q,内网地址会跟着 error_msg 回显给用户:%s", leak, msg)
		}
	}
	// 但状态码要留下 —— 那是排查时唯一有用的东西,而且不泄露任何拓扑。
	if !strings.Contains(msg, "Unavailable") {
		t.Errorf("翻译后的错误里没有状态码,排查时无从下手:%s", msg)
	}
}

// 我们自己取消的 ctx 不该被算成 Agent 的问题。
func TestTranslateGRPCError_ContextErrors(t *testing.T) {
	got := translateGRPCError("测试", context.Canceled)
	if !errors.Is(got, context.Canceled) {
		t.Errorf("context.Canceled 该原样保留,得到 %v", got)
	}
	if countsAsUnhealthy(got) {
		t.Error("调用方自己取消,不该算到 Agent 头上,否则一批用户刷新页面就能打开熔断器")
	}

	got = translateGRPCError("测试", context.DeadlineExceeded)
	if !errors.Is(got, ErrTimeout) {
		t.Errorf("ctx 超时该翻成 ErrTimeout,得到 %v", got)
	}
}

// TestCountsAsUnhealthy 是熔断器行为的分界线,改动前先想清楚:
// 归到"有病"那一侧的错误,攒够阈值就会把 Agent 整个隔离掉。
func TestCountsAsUnhealthy(t *testing.T) {
	unhealthy := []codes.Code{
		codes.Unavailable, codes.DeadlineExceeded,
		codes.Internal, codes.Unknown, codes.DataLoss,
	}
	for _, c := range unhealthy {
		if !countsAsUnhealthy(translateGRPCError("测试", status.Error(c, ""))) {
			t.Errorf("%v 该算下游有病", c)
		}
	}

	// 这些是**我们自己**发错了东西。算进去的话,前端一阵坏请求
	// 就能把一个完全健康的 Agent 关在门外,而且没人看得出为什么。
	healthy := []codes.Code{
		codes.InvalidArgument, codes.NotFound, codes.AlreadyExists,
		codes.FailedPrecondition, codes.PermissionDenied,
		codes.Unauthenticated, codes.Unimplemented,
	}
	for _, c := range healthy {
		if countsAsUnhealthy(translateGRPCError("测试", status.Error(c, ""))) {
			t.Errorf("%v 不该算到 Agent 头上", c)
		}
	}

	// ResourceExhausted 从"有病"那一侧挪到了"健康"那一侧。
	//
	// 它以前和 Unavailable 一起归到 ErrUnavailable,于是"执行器满了"
	// 被算成"执行器病了":熔断器攒够 5 次就把一个健康的 Agent 隔离掉,
	// 而 Agent 恰恰是在满载的时候最忙 —— 把它摘掉只会更堵。
	// 「满了 ≠ 病了」,见 TestCountsAsUnhealthy_AgentBusyIsNotUnhealthy。
	if countsAsUnhealthy(translateGRPCError("测试", status.Error(codes.ResourceExhausted, ""))) {
		t.Error("ResourceExhausted(执行器满了)不该算下游有病")
	}

	if countsAsUnhealthy(nil) {
		t.Error("nil 是成功,不该算有病")
	}
}

// TestTranslateGRPCError_ResourceExhaustedIsBusy 盯着并发护栏的那个码。
//
// Agent 侧同时能跑的任务数满了会用 RESOURCE_EXHAUSTED 拒绝。它以前
// 被归到 ErrUnavailable(和「连不上」混为一谈),于是"满了"既会去开熔断
// 器,又会被说成「执行器不可用」。两条路都要改,先钉住翻译这一侧。
func TestTranslateGRPCError_ResourceExhaustedIsBusy(t *testing.T) {
	got := translateGRPCError("接收任务事件流", status.Error(codes.ResourceExhausted, "并发任务数已达上限"))

	if !errors.Is(got, ErrAgentBusy) {
		t.Fatalf("ResourceExhausted 翻成了 %v,期望 ErrAgentBusy", got)
	}
	// 关键在"不是另一个":归回 ErrUnavailable 的话,熔断器和文案
	// 都跟着走错方向,而上面那条用例是看不出区别的。
	if errors.Is(got, ErrUnavailable) {
		t.Fatalf("ResourceExhausted 被归成了 ErrUnavailable,「满了」和「连不上」必须分开")
	}
}

// TestCountsAsUnhealthy_AgentBusyIsNotUnhealthy 是这次改动里最要紧的一条。
//
// 熔断器数的是「持续出错」,不是「接不了活」。把「执行器满了」算进去的
// 后果是:一次流量高峰 → 5 次拒绝 → 熔断器打开 → 一个**完全健康**的
// Agent 被整个隔离掉。而 Agent 恰恰是在满载的时候最忙,把它摘掉只会
// 让剩下的任务更堵,于是更多请求被拒 —— 自己把自己推向更满。
func TestCountsAsUnhealthy_AgentBusyIsNotUnhealthy(t *testing.T) {
	// 走真实翻译路径:Agent 侧 abort(RESOURCE_EXHAUSTED)之后,
	// backend 拿到的就是这个形状的错误。
	err := translateGRPCError("接收任务事件流",
		status.Error(codes.ResourceExhausted, "并发任务数已达上限"))

	if countsAsUnhealthy(err) {
		t.Error("执行器满了被算成了「下游有病」—— 一次流量高峰就能把健康的 Agent 隔离掉")
	}

	// 直接用哨兵也要一样:别指望"翻译路径上碰巧不匹配"。
	if countsAsUnhealthy(ErrAgentBusy) {
		t.Error("ErrAgentBusy 直接传进来也算成了「下游有病」")
	}
	// 包过一层也要一样:真实调用链上错误是被 %w 包过的。
	if countsAsUnhealthy(fmt.Errorf("派发任务: %w", ErrAgentBusy)) {
		t.Error("包过一层的 ErrAgentBusy 也被算成了「下游有病」")
	}
}

// TestRetryable_AgentBusyIsNotRetried 盯着"满了不重试"。
//
// 重试一个「满了」的任务毫无意义:槽不会因为我们重试而多出来,反而
// 多占一次在途名额,把后面的任务一起挤掉。
//
// gotEvent=true 是这里的关键:主路径上 Agent 拒绝时一条事件都没收到,
// `!gotEvent` 那条已经拦住了。写这条显式分支是双保险 —— 万一将来
// 拒绝的时机变了(先推几条事件再拒绝),这条仍然拦得住。
func TestRetryable_AgentBusyIsNotRetried(t *testing.T) {
	c, h := newConsumeClient(t, 0)

	err := translateGRPCError("接收任务事件流",
		status.Error(codes.ResourceExhausted, "并发任务数已达上限"))

	if c.retryable(context.Background(), consumeUID, 0, true, err) {
		t.Error("执行器满了还重试 —— 槽不会因为我们重试而多出来")
	}

	// handler 是放行的(它不拦),所以上面那条变红只可能是 retryable 自己拦的。
	if h.retryAsked != 0 {
		t.Errorf("问了一次要不要重试(%d 次),这条判断应该在 service 之前就做完", h.retryAsked)
	}
}

// AlreadyExists 必须有自己的哨兵,不能混进 ErrBadRequest。
//
// 它在这条链路上只有一个来源:重发时上一次的 worker 还没散场。
// 那是**等一下就好**的情况,而 ErrBadRequest 的语义是"重试一万次也一样"。
// 混在一起的话,重发在最常见的时序下永远失败,而且没有任何迹象。
func TestSentinelFor_AlreadyExistsIsRetryable(t *testing.T) {
	err := translateGRPCError("重发任务", status.Error(codes.AlreadyExists, "任务已在运行中"))

	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("AlreadyExists 翻成了 %v,期望 ErrAlreadyRunning", err)
	}
	if errors.Is(err, ErrBadRequest) {
		t.Error("归到了 ErrBadRequest,重发会被当成'重试也没用'直接放弃")
	}
	// 对面健康得很,只是上一次还没散场 —— 别让它去开熔断器。
	if countsAsUnhealthy(err) {
		t.Error("算成了'下游有病',一次正常的重发时序就能推动熔断")
	}
}
