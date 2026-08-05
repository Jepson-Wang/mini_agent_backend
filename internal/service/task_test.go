package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agent"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ptr"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// fakeAgent 记下被要求取消了哪些任务,可以指定让 CancelTask 报错。
type fakeAgent struct {
	canceled []string
	err      error
}

func (f *fakeAgent) StartTask(context.Context, agent.StartTaskReq) error { return nil }

func (f *fakeAgent) CancelTask(_ context.Context, taskUID string) error {
	f.canceled = append(f.canceled, taskUID)
	return f.err
}

// withTaskSvc 把用例跑在独立事务里,结束一律回滚。
// 和 withSvc 一个套路 —— 绕开它直接用包级 testDB 会在 t.Parallel() 下偶发失败。
func withTaskSvc(t *testing.T, fn func(t *testing.T, s *TaskService, r *repository.Repository, ag *fakeAgent)) {
	t.Helper()
	if testDB == nil {
		t.Skip("未设置 TEST_DSN,跳过需要数据库的用例")
	}
	tx := testDB.Begin()
	if tx.Error != nil {
		t.Fatalf("开启事务失败: %v", tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })

	repo := repository.New(tx)
	ag := &fakeAgent{}
	fn(t, NewTaskService(repo, ag), repo, ag)
}

// seedTask 建一个用户、一个会话和一个 pending 任务,返回用户 id 和 task_uid。
func seedTask(t *testing.T, r *repository.Repository, email string) (int64, string) {
	t.Helper()
	ctx := context.Background()

	u, err := r.User.Create(ctx, email, "$2a$04$fakefakefakefakefakefakefakefakefakefakefa", nil)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	sess := NewSessionService(r)
	_, task, err := sess.CreateWithFirstTask(ctx, u.ID, ptr.To("测试会话"), ptr.To("跑一下"))
	if err != nil {
		t.Fatalf("建会话和任务: %v", err)
	}
	return u.ID, task.TaskUID
}

// ---- Cancel ----

func TestCancel_MarksCanceled(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, ag *fakeAgent) {
		ctx := context.Background()
		userID, uid := seedTask(t, r, "cancel-ok@example.com")

		got, err := s.Cancel(ctx, userID, uid)
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if got.Status != model.TaskCanceled {
			t.Errorf("Status = %q, 期望 %q", got.Status, model.TaskCanceled)
		}
		// 返回的必须是重查之后的那份。直接把 UPDATE 之前读到的对象交出去
		// 的话,这里的 FinishedAt 会是 nil。
		if got.FinishedAt == nil {
			t.Error("FinishedAt 是 nil,说明返回的是 UPDATE 之前的那份数据")
		}
		if len(ag.canceled) != 1 || ag.canceled[0] != uid {
			t.Errorf("应当通知 Agent 取消 %q,实际 %v", uid, ag.canceled)
		}
	})
}

// 这条是整个取消流程的关键:取消之后,Agent 迟到的"成功"回调
// 不能把状态翻回去。靠的是 repository 那条带 status IN (...) 的 UPDATE。
func TestCancel_LateCallbackCannotRevive(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		userID, uid := seedTask(t, r, "cancel-race@example.com")

		if _, err := s.Cancel(ctx, userID, uid); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		applied, err := s.HandleCallback(ctx, uid, model.TaskSucceeded, nil)
		if err != nil {
			t.Fatalf("HandleCallback 不该报错: %v", err)
		}
		if applied {
			t.Fatal("迟到的成功回调把已取消的任务改回去了")
		}

		after, err := r.Task.GetByUID(ctx, uid)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if after.Status != model.TaskCanceled {
			t.Errorf("Status = %q, 期望仍是 %q", after.Status, model.TaskCanceled)
		}
	})
}

// 别人的任务必须和"不存在"一模一样地报 ErrTaskNotFound,不能是"无权限"。
// 分得开的话,这个接口就成了 task_uid 探测器。
func TestCancel_OtherUsersTaskLooksMissing(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, ag *fakeAgent) {
		ctx := context.Background()
		_, uid := seedTask(t, r, "owner@example.com")
		intruderID, _ := seedTask(t, r, "intruder@example.com")

		_, errMine := s.Cancel(ctx, intruderID, uid)
		_, errGone := s.Cancel(ctx, intruderID, "01J8ZQXK7PZZZZZZZZZZZZZZZZ")

		if !errors.Is(errMine, model.ErrTaskNotFound) {
			t.Errorf("取消别人的任务 = %v, 期望 ErrTaskNotFound", errMine)
		}
		if !errors.Is(errGone, model.ErrTaskNotFound) {
			t.Errorf("取消不存在的任务 = %v, 期望 ErrTaskNotFound", errGone)
		}

		// 状态没被动过,而且不能顺手通知 Agent —— 那等于让人隔空掐别人的任务。
		after, err := r.Task.GetByUID(ctx, uid)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if after.Status != model.TaskPending {
			t.Errorf("Status = %q, 别人的取消不该改动它", after.Status)
		}
		if len(ag.canceled) != 0 {
			t.Errorf("不该通知 Agent,实际 %v", ag.canceled)
		}
	})
}

func TestCancel_AlreadyTerminal(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		userID, uid := seedTask(t, r, "cancel-done@example.com")

		if _, err := r.Task.MarkSucceeded(ctx, uid); err != nil {
			t.Fatalf("MarkSucceeded: %v", err)
		}

		_, err := s.Cancel(ctx, userID, uid)
		if !errors.Is(err, model.ErrTaskNotCancelable) {
			t.Fatalf("Cancel = %v, 期望 ErrTaskNotCancelable", err)
		}
	})
}

// 通知 Agent 失败不能让取消失败 —— 库里已经是 canceled,那才是权威状态。
// 让用户重试也没用,重试同样会撞上"已经是终态"。
func TestCancel_AgentFailureDoesNotFailTheRequest(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, ag *fakeAgent) {
		ctx := context.Background()
		ag.err = errors.New("connection refused")
		userID, uid := seedTask(t, r, "cancel-agent-down@example.com")

		got, err := s.Cancel(ctx, userID, uid)
		if err != nil {
			t.Fatalf("Agent 挂了不该让取消失败: %v", err)
		}
		if got.Status != model.TaskCanceled {
			t.Errorf("Status = %q", got.Status)
		}
	})
}

// Agent 客户端还没接上(nil)时同样不该失败。
func TestCancel_NilAgent(t *testing.T) {
	if testDB == nil {
		t.Skip("未设置 TEST_DSN,跳过需要数据库的用例")
	}
	tx := testDB.Begin()
	t.Cleanup(func() { tx.Rollback() })

	r := repository.New(tx)
	s := NewTaskService(r, nil)
	userID, uid := seedTask(t, r, "cancel-no-agent@example.com")

	if _, err := s.Cancel(context.Background(), userID, uid); err != nil {
		t.Fatalf("未注入 Agent 不该让取消失败: %v", err)
	}
}

// ---- HandleCallback ----

func TestHandleCallback_Transitions(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		_, uid := seedTask(t, r, "callback-flow@example.com")

		applied, err := s.HandleCallback(ctx, uid, model.TaskRunning, nil)
		if err != nil || !applied {
			t.Fatalf("running: applied=%v err=%v", applied, err)
		}

		msg := "模型超时"
		applied, err = s.HandleCallback(ctx, uid, model.TaskFailed, &msg)
		if err != nil || !applied {
			t.Fatalf("failed: applied=%v err=%v", applied, err)
		}

		got, err := r.Task.GetByUID(ctx, uid)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if got.Status != model.TaskFailed {
			t.Errorf("Status = %q", got.Status)
		}
		if got.ErrorMsg == nil || *got.ErrorMsg != msg {
			t.Errorf("ErrorMsg = %v, 期望 %q", got.ErrorMsg, msg)
		}
		if got.StartedAt == nil || got.FinishedAt == nil {
			t.Error("StartedAt / FinishedAt 都该有值")
		}
	})
}

// Agent 不许把任务改成"已取消":那是用户的动作。
// 放开的话,一个被攻破的 Agent 就能悄悄吞掉任务,用户还以为是自己取消的。
func TestHandleCallback_RejectsCanceled(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		_, uid := seedTask(t, r, "callback-cancel@example.com")

		applied, err := s.HandleCallback(ctx, uid, model.TaskCanceled, nil)
		if !errors.Is(err, model.ErrTaskStatusInvalid) {
			t.Fatalf("err = %v, 期望 ErrTaskStatusInvalid", err)
		}
		if applied {
			t.Error("applied 应当是 false")
		}

		got, err := r.Task.GetByUID(ctx, uid)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if got.Status != model.TaskPending {
			t.Errorf("Status = %q, 不该被改动", got.Status)
		}
	})
}

// 未知的 task_uid 不当错误抛:回调方是机器,给它 5xx 只会换来无限重试,
// 而这种情况重试多少次结果都一样。日志里有记录就够了。
func TestHandleCallback_UnknownTaskIsNotAnError(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, _ *repository.Repository, _ *fakeAgent) {
		applied, err := s.HandleCallback(
			context.Background(), "01J8ZQXK7PZZZZZZZZZZZZZZZZ", model.TaskSucceeded, nil)
		if err != nil {
			t.Fatalf("未知任务不该报错: %v", err)
		}
		if applied {
			t.Error("applied 应当是 false")
		}
	})
}

// pending → succeeded 是允许的(任务太快,running 那条回调可能还没到),
// 但 succeeded 之后再来任何回调都必须落空。
func TestHandleCallback_TerminalIsFinal(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		_, uid := seedTask(t, r, "callback-final@example.com")

		if applied, err := s.HandleCallback(ctx, uid, model.TaskSucceeded, nil); err != nil || !applied {
			t.Fatalf("pending → succeeded 应当成功: applied=%v err=%v", applied, err)
		}

		msg := "假的失败"
		applied, err := s.HandleCallback(ctx, uid, model.TaskFailed, &msg)
		if err != nil {
			t.Fatalf("HandleCallback: %v", err)
		}
		if applied {
			t.Fatal("终态之后的回调不该生效")
		}

		got, err := r.Task.GetByUID(ctx, uid)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if got.Status != model.TaskSucceeded || got.ErrorMsg != nil {
			t.Errorf("Status = %q, ErrorMsg = %v", got.Status, got.ErrorMsg)
		}
	})
}
