package agentclient

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 这几个是本包对外暴露的失败原因。上层只认它们,不认 gRPC 的 codes ——
// 和 Event / StartTaskReq 是同一条规矩:wire 层的东西不往业务层顶。
//
// 分成四个而不是一个,是因为**上层要按它们做不同的决定**:
// 不可用可以重试、超时不知道对面做没做、参数错重试一万次也一样。
// 分不开的话,调用方只能一律当"调用失败"处理。
var (
	// ErrUnavailable 连不上 Agent,或者它明确说自己过载了。
	// 这是唯一一个"等一会儿再来大概率就好了"的类别。
	ErrUnavailable = errors.New("执行器暂时不可用")

	// ErrTimeout 在我们给的时限内没等到 Agent 的回应。
	//
	// 注意它和 ErrUnavailable 的区别不只是"慢":超时意味着**请求可能已经
	// 到对面并且正在执行**,重试就是重复执行。取消这类操作可以重试,
	// 派发任务不行。
	ErrTimeout = errors.New("执行器响应超时")

	// ErrRemoteInternal Agent 自己炸了(Internal / Unknown / DataLoss)。
	ErrRemoteInternal = errors.New("执行器内部错误")

	// ErrBadRequest Agent 说我们发的东西不对。
	//
	// 这一类**不算下游有病**,别让它去开熔断器 —— 否则前端一阵坏请求
	// 就能把一个完全健康的 Agent 关在门外。见 countsAsUnhealthy。
	ErrBadRequest = errors.New("执行器拒绝了这次请求")
)

// translateGRPCError 把一个 gRPC 错误翻成本包的哨兵错误。
//
// **返回值里刻意不带 status.Message()。** Unavailable 的 message 长这样:
//
//	connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:50051: ..."
//
// 里面是内网地址和端口。而这个 error 会一路走到日志、走到 tasks.error_msg,
// 而 error_msg 是**会跟着任务详情回显给用户的**。所以原文在这里就断掉,
// 只留下 op(我们自己写的字面量)和 gRPC 的状态码名。
//
// 丢掉原文不影响排查:状态码已经说明了是哪一类故障,而 Agent 的地址
// 启动日志里就有一份(main.go 打的 agent_addr),不需要每条错误再抄一遍。
//
// op 是给人看的操作名(如"派发任务"),不要往里塞 task_uid 之类的变量 ——
// 那些该以结构化字段的形式进日志,不该拼进 error 文本里。
func translateGRPCError(op string, err error) error {
	if err == nil {
		return nil
	}

	// ctx 自己的错误要先挡:调用方主动取消 / 自己设的 deadline 到了,
	// 这时候根本没走到 Agent,不该算成对面的问题。
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s 被取消: %w", op, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", op, ErrTimeout)
	}

	code := status.Code(err) // 非 status 错误给 codes.Unknown
	return fmt.Errorf("%s: %w(gRPC %s)", op, sentinelFor(code), code)
}

// sentinelFor 把 gRPC 状态码归到四个哨兵之一。
func sentinelFor(code codes.Code) error {
	switch code {
	case codes.Unavailable, codes.ResourceExhausted:
		// ResourceExhausted 归到"不可用":对面还活着,但现在接不了活,
		// 处置方式和连不上是一样的 —— 退避、稍后再来。
		return ErrUnavailable

	case codes.DeadlineExceeded:
		return ErrTimeout

	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists,
		codes.FailedPrecondition, codes.OutOfRange,
		codes.PermissionDenied, codes.Unauthenticated:
		return ErrBadRequest

	case codes.Unimplemented:
		// 对面**回话了**,只是没实现这个方法 —— 版本不匹配,不是它有病。
		// 归到 BadRequest 一类(不开熔断):把 Agent 隔离掉并不能让
		// 一个没部署的方法长出来,只会顺带把能用的方法也断掉。
		return ErrBadRequest

	default:
		// Internal / Unknown / DataLoss / Aborted,以及将来新增的码。
		// 默认按"对面有病"处理:宁可多熔断一次,也不要把一个正在
		// 持续报错的下游当成正常的一直打。
		return ErrRemoteInternal
	}
}

// countsAsUnhealthy 判断这次调用的失败该不该算到 Agent 头上。
//
// 熔断器数的是"下游有病"的次数,不是"调用返回了错误"的次数。
// 参数错、方法没实现、我们自己取消 —— 这些重试多少次结果都一样,
// 但它们证明不了 Agent 不健康,算进去的话一阵坏请求就能把熔断器打开。
//
// err 为 nil 时返回 false(健康),所以调用方可以直接
//
//	c.breaker.Record(!countsAsUnhealthy(err))
func countsAsUnhealthy(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrUnavailable) ||
		errors.Is(err, ErrTimeout) ||
		errors.Is(err, ErrRemoteInternal)
}
