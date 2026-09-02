package handler

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler/dto"
	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

// SessionService 是 handler 需要的会话能力。
//
// 接口定在使用方这一侧,而且只列真要暴露的方法 —— 这是最小暴露面:
// 以后谁往 *service.SessionService 上加一个方法,HTTP 层够不着它,
// 除非有人显式在这儿再敲一行。
//
// 注意每个方法的第一个业务参数都是 userID:归属校验在 service 里做
// (SessionService.mustOwn),handler 拿到什么就传什么,不自己比对。
type SessionService interface {
	CreateWithFirstTask(ctx context.Context, userID int64, title, input *string) (*model.Session, *model.Task, error)
	ListSessionsByUserID(ctx context.Context, userID int64, limit, offset int) ([]model.Session, error)
	SessionDetail(ctx context.Context, userID int64, sessionID string) (*model.Session, error)
	ModifyTheTitle(ctx context.Context, userID int64, sessionID string, title *string) (*model.Session, error)
	SoftDelete(ctx context.Context, userID int64, sessionID string) error
	CreateTaskWithSessionID(ctx context.Context, userID int64, sessionID string, input *string) (*model.Task, error)
	ListTasksWithSessionID(ctx context.Context, userID int64, sessionID string, limit, offset int) ([]model.Task, error)
}

// SessionHandler 处理会话及其下属任务。全部挂在私有路由组上,必须有登录态。
//
// 所有接口共用同一套错误语义:
//
//	40000  路径参数格式不对 / body 或 query 校验失败
//	40100  没带令牌或令牌无效(middleware.Auth 拦下,到不了这里)
//	40400  会话不存在,**或者不是你的** —— 两种必须完全一致,
//	       分开的话这组接口就成了 session_id 探测器
//	50000  其余一切,原文只进日志
type SessionHandler struct {
	svc SessionService
}

func NewSessionHandler(svc SessionService) *SessionHandler {
	return &SessionHandler{svc: svc}
}

// Create 处理 POST /api/v1/sessions。
//
// 会话和它的首个任务在一个事务里建出来,一起返回 —— 客户端建完
// 立刻就要拿 task_uid 去轮询状态,省一次往返。
func (h *SessionHandler) Create(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	var req dto.CreateSessionReq
	if !bindJSON(c, &req) {
		return
	}

	sess, task, err := h.svc.CreateWithFirstTask(
		c.Request.Context(), middleware.MustUserID(c), req.Title, req.NormalizedInput())
	if err != nil {
		// ErrSessionExists 故意没登记进 errRules:它意味着 ULID 撞了,
		// 属于内部故障,对外就是 50000。
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.SessionWithTaskResp{
		Session: dto.NewSessionResp(sess),
		Task:    dto.NewTaskResp(task),
	})
}

// List 处理 GET /api/v1/sessions?limit=&offset=。
//
// 不需要校验归属:user_id 是 WHERE 条件的一部分,不是拿来事后比对的。
// 所有列表接口都该长这样 —— 先查出来再过滤是另一种越权的写法,
// 一旦漏了那个过滤就全泄了。
func (h *SessionHandler) List(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	var q dto.PageQuery
	if !bindQuery(c, &q) {
		return
	}

	ss, err := h.svc.ListSessionsByUserID(
		c.Request.Context(), middleware.MustUserID(c), q.Limit, q.Offset)
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewSessionListResp(ss, q.Limit, q.Offset))
}

// Detail 处理 GET /api/v1/sessions/:id。
func (h *SessionHandler) Detail(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	id, ok := ulidParam(c, "id", "会话")
	if !ok {
		return
	}

	sess, err := h.svc.SessionDetail(c.Request.Context(), middleware.MustUserID(c), id)
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewSessionResp(sess))
}

// UpdateTitle 处理 PATCH /api/v1/sessions/:id。
//
// 用 PATCH 不用 PUT:body 里只有 title,不是整个会话的替换。
// 清空标题传 {"title": ""},空白串会被归一成 NULL。
func (h *SessionHandler) UpdateTitle(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	id, ok := ulidParam(c, "id", "会话")
	if !ok {
		return
	}

	var req dto.UpdateSessionTitleReq
	if !bindJSON(c, &req) {
		return
	}

	sess, err := h.svc.ModifyTheTitle(
		c.Request.Context(), middleware.MustUserID(c), id, req.NormalizedTitle())
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewSessionResp(sess))
}

// Delete 处理 DELETE /api/v1/sessions/:id。软删除,data 为 null。
//
// 删一个已经删掉的会返回 40400 而不是 200。DELETE 幂等指的是
// "重复调用不会产生额外副作用",不是"重复调用都得成功" ——
// 一律回 200 的话,客户端就再也分不出"删掉了"和"这个 id 根本不存在"。
func (h *SessionHandler) Delete(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	id, ok := ulidParam(c, "id", "会话")
	if !ok {
		return
	}

	if err := h.svc.SoftDelete(c.Request.Context(), middleware.MustUserID(c), id); err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, nil)
}

// CreateTask 处理 POST /api/v1/sessions/:id/tasks。
//
// 任务挂在会话下面,所以路径是嵌套的;而取消和查详情用的是
// /tasks/:uid —— task_uid 全局唯一,拿到它就不必再报会话。
func (h *SessionHandler) CreateTask(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	id, ok := ulidParam(c, "id", "会话")
	if !ok {
		return
	}

	var req dto.CreateTaskReq
	if !bindJSON(c, &req) {
		return
	}

	task, err := h.svc.CreateTaskWithSessionID(
		c.Request.Context(), middleware.MustUserID(c), id, req.NormalizedInput())
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewTaskResp(task))
}

// ListTasks 处理 GET /api/v1/sessions/:id/tasks?limit=&offset=。
func (h *SessionHandler) ListTasks(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	id, ok := ulidParam(c, "id", "会话")
	if !ok {
		return
	}

	var q dto.PageQuery
	if !bindQuery(c, &q) {
		return
	}

	ts, err := h.svc.ListTasksWithSessionID(
		c.Request.Context(), middleware.MustUserID(c), id, q.Limit, q.Offset)
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	response.OK(c, dto.NewTaskListResp(ts, q.Limit, q.Offset))
}

// ready 挡住"路由注册了但依赖没注入"的接线错误。
func (h *SessionHandler) ready(c *gin.Context) bool {
	if h == nil || h.svc == nil {
		notWired(c, "会话接口")
		return false
	}
	return true
}
