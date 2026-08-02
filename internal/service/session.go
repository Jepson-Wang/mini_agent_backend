package service

import (
	"context"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

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
