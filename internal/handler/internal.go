package handler

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler/dto"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

// TaskCallbackService 是内部回调接口需要的能力。
//
// 签名里没有 userID,这是刻意的:回调方是 Agent,不是人,
// 它证明不了自己代表谁。加一个 userID 参数就等于让调用方自己声明身份。
type TaskCallbackService interface {
	HandleCallback(ctx context.Context, taskUID, status string, errMsg *string) (applied bool, err error)
}

// InternalHandler 处理服务间调用(目前只有 Agent 的任务回调)。
//
// 单独一个 handler 而不是塞进 TaskHandler:这两组接口的信任来源完全不同,
// 一个是用户令牌,一个是服务间的共享密钥。分开放,才不会有人手滑
// 把一个需要登录态的方法挂到 /internal 上 —— 那上面没有 userID 可取,
// middleware.MustUserID 会 panic。
type InternalHandler struct {
	svc TaskCallbackService
}

func NewInternalHandler(svc TaskCallbackService) *InternalHandler {
	return &InternalHandler{svc: svc}
}

// TaskCallback 处理 POST /internal/tasks/:uid/callback。
//
// 为什么不用 404 / 409:回调方是机器,拿到 4xx/5xx 只会按退避策略一直重试,
// 而"任务已经是终态"(最常见的是用户抢先取消了)重试一万次也不会变。
// 真正值得重试的只有数据库不可用,那种情况 service 会抛 error,这里回 50000。
//
// 走到这里说明 middleware.InternalAuth 已经放行 —— 但那只证明了
// "调用方是我们自己的服务",证明不了它代表哪个用户。所以这个 handler
// 只按 :uid 推进状态,不做任何和用户相关的读写。
func (h *InternalHandler) TaskCallback(c *gin.Context) {
	if h == nil || h.svc == nil {
		notWired(c, "内部回调接口")
		return
	}

	uid, ok := ulidParam(c, "uid", "任务")
	if !ok {
		return
	}

	var req dto.TaskCallbackReq
	if !bindJSON(c, &req) {
		return
	}

	applied, err := h.svc.HandleCallback(
		c.Request.Context(), uid, req.Status, req.NormalizedErrorMsg())
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.TaskCallbackResp{Applied: applied})
}
