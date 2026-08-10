package handler

import (
	"context"

	agentv1 "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler/dto"
	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

// TaskService 是 handler 需要的任务能力。
//
// 接口定在使用方这一侧,不在 service 包里 —— 这样 handler 的测试
// 塞一个假的就能跑完整条 router 链路,不连数据库。
// 生产实现是 *service.TaskService。
type TaskService interface {
	GetTaskDetailByTaskID(ctx context.Context, userID int64, taskUID string) (*model.Task, error)
	Cancel(ctx context.Context, userID int64, request *agentv1.CancelTaskRequest) (*model.Task, error)
}

// TaskHandler 处理用户对任务的操作。挂在私有路由组上,必须有登录态。
type TaskHandler struct {
	svc TaskService
}

func NewTaskHandler(svc TaskService) *TaskHandler {
	return &TaskHandler{svc: svc}
}

// Detail 处理 GET /api/v1/tasks/:uid。
//
// 客户端建完任务就靠轮询这个接口拿状态。路径不带 session_id ——
// task_uid 全局唯一,归属校验看的是 tasks.user_id(从会话冗余下来的那一列,
// 就是为了这一步不用 join)。
func (h *TaskHandler) Detail(c *gin.Context) {
	if h == nil || h.svc == nil {
		notWired(c, "任务接口")
		return
	}

	uid, ok := ulidParam(c, "uid", "任务")
	if !ok {
		return
	}

	t, err := h.svc.GetTaskDetailByTaskID(c.Request.Context(), middleware.MustUserID(c), uid)
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewTaskResp(t))
}

// Cancel 处理 POST /api/v1/tasks/:uid/cancel。
//
// 可能的结果:
//
//	40000  uid 不是 26 位
//	40100  没带令牌 / 令牌无效(middleware.Auth 拦下,到不了这里)
//	40400  任务不存在,**或者不是你的** —— 两种情况必须长得一模一样,
//	       分开的话这个接口就成了 task_uid 探测器
//	40900  任务已经跑完了,取消不了
//	50000  数据库炸了
//
// 注意 40400 那条:归属校验在 service.TaskService.mustOwn 里做,
// 这里一行 if t.UserID != userID 都不该有。
func (h *TaskHandler) Cancel(c *gin.Context) {
	if h == nil || h.svc == nil {
		notWired(c, "任务接口")
		return
	}

	uid, ok := ulidParam(c, "uid", "任务")
	if !ok {
		return
	}

	// 取消理由是可选的,客户端完全可以不带 body ——
	// gin 的 ShouldBindJSON 碰上空 body 报 io.EOF,不先挡一下会把
	// "我就是想取消"变成 40000。带了 body 就照常走 bindJSON,
	// 失败时它已经把响应写好了,这里直接 return。
	var req dto.CancelTaskReasonReq
	if c.Request.ContentLength != 0 && !bindJSON(c, &req) {
		return
	}

	// trace_id 由 TraceID 中间件种在最外层,取不到也只是链路断一截,
	// 不该因此拒掉一次取消 —— 库里的状态才是要紧的那个。
	traceID := trace.FromGin(c)

	// MustUserID 取不到会 panic,那说明这条路由忘了挂 Auth ——
	// 是接线 bug,早点炸出来好过默默拿 0 当用户 id 去查别人的数据。
	// Recovery 会兜住,对外是 50000。
	t, err := h.svc.Cancel(c.Request.Context(), middleware.MustUserID(c),
		&agentv1.CancelTaskRequest{
			TaskUid: uid,
			Reason:  req.Reason,
			TraceId: traceID,
		})
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewTaskResp(t))
}
