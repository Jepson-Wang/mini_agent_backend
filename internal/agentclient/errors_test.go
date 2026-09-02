package agentclient

import (
	"context"
	"errors"
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
		{codes.ResourceExhausted, ErrUnavailable},
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
		codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded,
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

	if countsAsUnhealthy(nil) {
		t.Error("nil 是成功,不该算有病")
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
