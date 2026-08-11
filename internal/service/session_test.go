package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ptr"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// TestSessionService
func TestSessionService_AllMethodsRejectOtherUsers(t *testing.T) {
	withSvc(t, NewSessionService, func(t *testing.T, s *SessionService, r *repository.Repository) {
		ctx := context.Background()
		ownerID, sess, _ := seed(t, r)
		intruderID, _, _ := seed(t, r) // 另一个真实用户,不是随便编个 id

		calls := map[string]func() error{
			"SessionDetail": func() error {
				_, err := s.SessionDetail(ctx, intruderID, sess.SessionID)
				return err
			},
			"ModifyTheTitle": func() error {
				_, err := s.ModifyTheTitle(ctx, intruderID, sess.SessionID, ptr.To("被改了"))
				return err
			},
			"SoftDelete": func() error {
				return s.SoftDelete(ctx, intruderID, sess.SessionID)
			},
			"CreateTaskWithSessionID": func() error {
				_, err := s.CreateTaskWithSessionID(ctx, intruderID, sess.SessionID, ptr.To("插一条"))
				return err
			},
			"ListTasksWithSessionID": func() error {
				_, err := s.ListTasksWithSessionID(ctx, intruderID, sess.SessionID, 10, 0)
				return err
			},
		}
		for name, call := range calls {
			// 必须是 ErrSessionNotFound,不能是"无权限"。
			// 分得开的话,接口就成了 session_id 探测器。
			if err := call(); !errors.Is(err, model.ErrSessionNotFound) {
				t.Errorf("%s 拿别人的会话 = %v, 期望 ErrSessionNotFound", name, err)
			}
		}

		after, err := s.SessionDetail(ctx, ownerID, sess.SessionID)
		if err != nil {
			t.Fatalf("会话被删掉了: %v", err)
		}
		if after.Title == nil || *after.Title != "测试会话" {
			t.Errorf("标题被改成了 %v", after.Title)
		}
		tasks, err := s.ListTasksWithSessionID(ctx, ownerID, sess.SessionID, 10, 0)
		if err != nil {
			t.Fatalf("列任务: %v", err)
		}
		if len(tasks) != 1 { // seed 建的那一条
			t.Errorf("任务数 = %d, 期望 1 —— 有人往别人的会话里插进去了", len(tasks))
		}
	})
}
