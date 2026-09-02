package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// listLimitDefault 是列表接口的默认页大小。
// 上限由仓库层的 maxListLimit 兜着,这里只管"没传"的情况。
const listLimitDefault = 20

type SessionService struct {
	repo  *repository.Repository
	agent agentclient.Client

	// quota 可以是 nil(表示这个部署不限额)。Check 是 nil 安全的,
	// 所以下面两个建任务的方法里不用判空。
	quota *TaskQuota
}

// NewSessionService 组装会话服务。
//
// ag 是任务派发用的 Agent 客户端。传 nil 表示这个部署不派发任务:
// 任务照常建出来,但会一直停在 pending,只记一条 Warn。
// 和 TaskService 那边同一个取向 —— 库里的记录才是权威,派发是之后的事。
// quota 传 nil 表示不限额,见 TaskQuota.Check。
func NewSessionService(repo *repository.Repository, ag agentclient.Client,
	quota *TaskQuota) *SessionService {

	return &SessionService{repo: repo, agent: ag, quota: quota}
}

// CreateWithFirstTask 在一个事务里建会话和它的首个任务,
// 两个 ID 都在内部生成,调用方不用关心。
func (s *SessionService) CreateWithFirstTask(ctx context.Context,
	userID int64, title, input *string) (*model.Session, *model.Task, error) {

	// 配额检查放在最前面:后面每一步都要花钱或者占资源,
	// 而这一步只是一次走索引的 COUNT。
	if err := s.quota.Check(ctx, userID); err != nil {
		return nil, nil, err
	}

	// input 为 nil 表示"先开一个空会话",这一趟不建任务,也就没什么可派发的。
	// 空白串在 handler 那层已经被 CreateSessionReq.NormalizedInput 归成 nil ——
	// 一个空 input 的任务照样会被原样交给 Agent,驱动一整轮什么都没有的 LLM 循环。
	//
	// 这里不把它当成坏请求返回错误:接口的契约就是两个字段都可空
	// (见 dto.CreateSessionReq),建空会话是正常用法。真正必填的是
	// CreateTaskReq.Input,那条在 handler 用 required,notblank 挡,回 40000。
	var task *model.Task
	if input != nil {
		// 事务里只留数据库操作,ID 先在外面生成好
		taskUID, err := ulid.New()
		if err != nil {
			return nil, nil, err
		}
		task = &model.Task{TaskUID: taskUID, UserID: userID, Input: input}
	}

	var sess *model.Session
	err := s.repo.Transaction(ctx, func(r *repository.Repository) error {
		var err error
		if sess, err = r.Session.Create(ctx, userID, title); err != nil {
			return err // 已翻译成 ErrUserNotFound / ErrSessionExists
		}
		if task == nil {
			return nil
		}
		task.SessionID = sess.SessionID // 会话建出来才知道
		return r.Task.Create(ctx, task)
	})
	if err != nil {
		return nil, nil, err
	}

	if task == nil {
		return sess, nil, nil
	}

	// 派发放在事务外面,理由见 dispatch 的注释。
	return sess, s.dispatch(ctx, task), nil
}

// dispatch 把任务交给 Agent 去跑,返回派发之后的任务。
//
// **必须在事务提交之后调用。** Agent 是同步的,它可能在几毫秒内就把
// started 事件推回来,而那条事件是按 task_uid 去 UPDATE 的 ——
// 事务还没提交的话那一行对别的连接根本不可见,UPDATE 匹配不到任何行,
// cas 返回 false(这在它眼里是"迟到的回调",不是错误),
// 于是任务永远停在 pending。这种 bug 只在 Agent 快、库慢的时候出现,
// 本地几乎复现不出来。
//
// 派发失败就地标记 failed,不是留在 pending:同步 Agent 只有这一次
// 交接机会,交不出去就再也没人会推动它了。留在 pending 的话,
// 用户界面上就是一个永远转圈的任务,而且没有任何地方能解释为什么。
func (s *SessionService) dispatch(ctx context.Context, task *model.Task) *model.Task {
	traceID := trace.FromContext(ctx)

	if s.agent == nil {
		slog.Warn("未注入 Agent 客户端,任务只建了记录没有派发",
			"task_uid", task.TaskUID, "trace_id", traceID)
		return task
	}

	var input string
	if task.Input != nil {
		input = *task.Input
	}

	err := s.agent.StartTask(ctx, agentclient.StartTaskReq{
		TaskUID:   task.TaskUID,
		SessionID: task.SessionID,
		Input:     input,
		TraceID:   traceID,
	})
	if err == nil {
		return task
	}

	slog.Error("派发任务给 Agent 失败,标记为失败",
		"task_uid", task.TaskUID, "trace_id", traceID, "error", err)

	// 给用户看的原因是一句固定文案,不带 err 原文。
	// agentclient 那边已经把 gRPC 的地址和 message 断掉了,这里再兜一层:
	// error_msg 是会跟着任务详情回显出去的,不能让下游的错误文本
	// 变成对外契约的一部分。
	msg := agentFailureReason(translateAgentErr(err))
	applied, markErr := s.repo.Task.MarkFailed(ctx, task.TaskUID, &msg)
	if markErr != nil {
		// 记录已经在库里了,标记失败又失败 —— 只能记下来,
		// 任务会停在 pending。不往上抛:任务确实建成功了。
		slog.Error("派发失败后标记任务失败也失败了",
			"task_uid", task.TaskUID, "trace_id", traceID, "error", markErr)
		return task
	}
	if !applied {
		// 没匹配到行说明状态已经被推动过了(极少见:StartTask 报错但
		// 流其实建起来了,事件先落了地)。以库里的为准。
		slog.Warn("派发失败但任务状态已被改动,不覆盖",
			"task_uid", task.TaskUID, "trace_id", traceID)
	}

	// 重查一次,别把 UPDATE 之前那份过期数据交给客户端。
	fresh, err := s.repo.Task.GetByUID(ctx, task.TaskUID)
	if err != nil {
		slog.Error("派发失败后重查任务失败",
			"task_uid", task.TaskUID, "trace_id", traceID, "error", err)
		return task
	}
	return fresh
}

// ListSessionsByUserID 根据用户 ID 返回 session 列表,带分页。
//
// 这个方法天然没有越权问题:user_id 是 WHERE 条件的一部分,
// 不是拿来事后比对的。所有列表接口都该长这样。
func (s *SessionService) ListSessionsByUserID(ctx context.Context, userID int64,
	limit, offset int) ([]model.Session, error) {

	return s.repo.Session.ListByUser(ctx, userID, limit, offset)
}

// SessionDetail 返回属于该用户的会话详情。
func (s *SessionService) SessionDetail(ctx context.Context, userID int64,
	sessionID string) (*model.Session, error) {

	return s.mustOwn(ctx, userID, sessionID)
}

// ModifyTheTitle 改标题。title 传 nil 表示清空。
func (s *SessionService) ModifyTheTitle(ctx context.Context, userID int64,
	sessionID string, title *string) (*model.Session, error) {

	if _, err := s.mustOwn(ctx, userID, sessionID); err != nil {
		return nil, err
	}

	ss, err := s.repo.Session.UpdateTheTitle(ctx, sessionID, title)
	if err != nil {
		// %s 不是 %d —— session_id 是 26 位字符串。
		return nil, fmt.Errorf("更新会话 %s 的标题: %w", sessionID, err)
	}
	return ss, nil
}

// SoftDelete 软删除属于该用户的会话。
func (s *SessionService) SoftDelete(ctx context.Context, userID int64, sessionID string) error {
	if _, err := s.mustOwn(ctx, userID, sessionID); err != nil {
		return err
	}

	// 包装必须放在 if err != nil 里面。写成
	//   err := ...; return fmt.Errorf("...: %w", err)
	// 的话,err 为 nil 时也会造出一个非 nil 的 error,
	// 于是这个方法**永远返回失败** —— 而且编译器不会说什么。
	if err := s.repo.Session.SoftDelete(ctx, sessionID); err != nil {
		return fmt.Errorf("软删除会话 %s: %w", sessionID, err)
	}
	return nil
}

// CreateTaskWithSessionID 在指定会话下创建任务。
func (s *SessionService) CreateTaskWithSessionID(ctx context.Context, userID int64,
	sessionID string, input *string) (*model.Task, error) {

	if _, err := s.mustOwn(ctx, userID, sessionID); err != nil {
		return nil, err
	}
	// 归属校验之后再查配额:配额超限这条信息只该给会话真正的主人。
	// 反过来的话,拿别人的 session_id 撞一下就能从"配额超限"和"会话不存在"
	// 的差别里判断出这个 ID 是不是真的存在。
	if err := s.quota.Check(ctx, userID); err != nil {
		return nil, err
	}

	taskUID, err := ulid.New()
	if err != nil {
		return nil, err
	}
	task := &model.Task{TaskUID: taskUID, SessionID: sessionID, UserID: userID, Input: input}
	if err := s.repo.Task.Create(ctx, task); err != nil {
		return nil, err
	}

	// Create 是单条写入,走到这里已经落库了,可以派发。
	return s.dispatch(ctx, task), nil
}

// ListTasksWithSessionID 列出会话下的任务。
func (s *SessionService) ListTasksWithSessionID(ctx context.Context, userID int64,
	sessionID string, limit, offset int) ([]model.Task, error) {

	if _, err := s.mustOwn(ctx, userID, sessionID); err != nil {
		return nil, err
	}

	// TaskRepo.ListBySession 不像 SessionRepo.ListByUser 那样自己夹上限,
	// 所以这里必须夹住:limit<=0 会变成"不写 LIMIT 子句",也就是全表。
	switch {
	case limit <= 0:
		limit = listLimitDefault
	case limit > 100:
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return s.repo.Task.ListBySession(ctx, sessionID, limit, offset)
}

// mustOwn 取出会话并确认它属于 userID。
//
// 不是本人的会话一律返回 model.ErrSessionNotFound,不返回"无权限"。
// 403 和 404 分开的话,这个接口就成了一台 session_id 探测器 ——
// 拿到 403 说明这个 ID 真实存在,拿到 404 说明不存在。对外必须是同一个结果。
//
// 归属校验放在这一层,不放 handler:handler 里出现 if x.UserID != userID
// 就是层错了(见 CLAUDE.md 的分层约定)。
func (s *SessionService) mustOwn(ctx context.Context, userID int64, sessionID string) (*model.Session, error) {
	// 参数非法也归到 NotFound。单独给一个"参数错误"会让调用方能区分
	// "ID 格式不对"和"ID 不属于我",格式那关 handler 已经把过了。
	if userID <= 0 || sessionID == "" {
		return nil, model.ErrSessionNotFound
	}

	sess, err := s.repo.Session.GetByID(ctx, sessionID)
	if err != nil {
		return nil, err // 已翻译成 model.ErrSessionNotFound
	}
	if sess.UserID != userID {
		return nil, model.ErrSessionNotFound
	}
	return sess, nil
}
