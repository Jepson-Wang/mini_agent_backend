package service

import (
	"context"
	"fmt"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// listLimitDefault 是列表接口的默认页大小。
// 上限由仓库层的 maxListLimit 兜着,这里只管"没传"的情况。
const listLimitDefault = 20

type SessionService struct {
	repo *repository.Repository
}

func NewSessionService(repo *repository.Repository) *SessionService {
	return &SessionService{repo: repo}
}

// CreateWithFirstTask 在一个事务里建会话和它的首个任务,
// 两个 ID 都在内部生成,调用方不用关心。
func (s *SessionService) CreateWithFirstTask(ctx context.Context,
	userID int64, title, input *string) (*model.Session, *model.Task, error) {

	// 事务里只留数据库操作,ID 先在外面生成好
	taskUID, err := ulid.New()
	if err != nil {
		return nil, nil, err
	}

	var sess *model.Session
	task := &model.Task{TaskUID: taskUID, UserID: userID, Input: input}
	err = s.repo.Transaction(ctx, func(r *repository.Repository) error {
		var err error
		if sess, err = r.Session.Create(ctx, userID, title); err != nil {
			return err // 已翻译成 ErrUserNotFound / ErrSessionExists
		}
		task.SessionID = sess.SessionID // 会话建出来才知道
		return r.Task.Create(ctx, task)
	})
	if err != nil {
		return nil, nil, err
	}
	return sess, task, nil
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

	taskUID, err := ulid.New()
	if err != nil {
		return nil, err
	}
	task := &model.Task{TaskUID: taskUID, SessionID: sessionID, UserID: userID, Input: input}
	if err := s.repo.Task.Create(ctx, task); err != nil {
		return nil, err
	}
	return task, nil
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
