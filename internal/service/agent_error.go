package service

import (
	"errors"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// translateAgentErr 把 agentclient 的哨兵错误翻成 model 里的业务错误。
//
// 这是**跨包边界的翻译**,不是包装:handler 和 response 不该认识 agentclient,
// 否则以后换一种执行方式(HTTP、消息队列),response 那张错误表就得跟着改。
// 同一条规矩下,仓库层也是把 gorm.ErrDuplicatedKey 翻成 model.ErrEmailOccupied,
// 而不是 %w 包一层。
//
// 认不出来的错误原样返回 —— 会被 response 归到 CodeInternal,对外只说
// "内部错误"。硬翻成"执行器不可用"更糟:那是在猜,而且会让客户端去重试
// 一件重试也没用的事。
func translateAgentErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, agentclient.ErrUnavailable):
		return model.ErrAgentUnavailable
	case errors.Is(err, agentclient.ErrTimeout):
		return model.ErrAgentTimeout
	default:
		return err
	}
}

// agentFailureReason 给出一句能落进 tasks.error_msg 的派发失败原因。
//
// 入参是 translateAgentErr 翻过的错误。这个串会**跟着任务详情接口回显给用户**,
// 所以只能是这里的几句固定文案,绝不能拼 err.Error():哪怕 agentclient 已经
// 把 gRPC 原文断掉了,也不该让下游的错误文本成为对外契约的一部分。
//
// 分开写而不是一律"执行器不可用",是因为用户能据此做不同的事:
// 不可用 → 过会儿重发一次;超时 → 请求可能已经送到了,先去列表看看。
func agentFailureReason(err error) string {
	if errors.Is(err, model.ErrAgentTimeout) {
		return "任务派发超时:执行器没有在规定时间内响应"
	}
	// 兜底也说"暂时不可用":派发这一步失败,对用户来说能做的动作只有重试,
	// 再细分下去("参数不合法")既看不懂,也可能透露内部约定。
	return "任务派发失败:执行器暂时不可用"
}
