// task_test.go 只放 TaskService 的用例。
// withTaskSvc / fakeAgent / seedTask 在 helpers_test.go。
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// ---- Cancel ----

func TestCancel_MarksCanceled(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, ag *fakeAgent) {
		ctx := context.Background()
		userID, uid := seedTask(t, r)

		got, err := s.Cancel(ctx, userID, uid, "")
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
		if got := ag.canceledUIDs(); len(got) != 1 || got[0] != uid {
			t.Errorf("应当通知 Agent 取消 %q,实际 %v", uid, got)
		}
	})
}

// 取消理由要落库,而且要原样带给 Agent。
//
// 只发给 Agent 不落库的话,Agent 没接上(生产现在就是 nil)时
// 用户填的理由直接蒸发,事后谁也说不清这单为什么被取消了。
func TestCancel_ReasonIsStoredAndForwarded(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, ag *fakeAgent) {
		ctx := context.Background()
		userID, uid := seedTask(t, r)
		const reason = "点错了"

		got, err := s.Cancel(ctx, userID, uid, reason)
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if got.CancelReason == nil || *got.CancelReason != reason {
			t.Errorf("CancelReason = %v, 期望 %q", got.CancelReason, reason)
		}
		if len(ag.canceled) != 1 {
			t.Fatalf("通知了 Agent %d 次", len(ag.canceled))
		}
		if ag.canceled[0].Reason != reason {
			t.Errorf("发给 Agent 的 Reason = %q, 期望 %q", ag.canceled[0].Reason, reason)
		}
	})
}

// 没填理由要存 NULL,不是空串 —— 否则库里会有两种"没填",
// 和 normalizeNickname 是同一条规矩。
func TestCancel_BlankReasonIsNull(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		got, err := func() (*model.Task, error) {
			userID, uid := seedTask(t, r)
			return s.Cancel(context.Background(), userID, uid, "   ")
		}()
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if got.CancelReason != nil {
			t.Errorf("CancelReason = %q, 期望 NULL", *got.CancelReason)
		}
	})
}

// 这条是整个取消流程的关键:取消之后,Agent 迟到的"成功"回调
// 不能把状态翻回去。靠的是 repository 那条带 status IN (...) 的 UPDATE。
func TestCancel_LateCallbackCannotRevive(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		userID, uid := seedTask(t, r)

		if _, err := s.Cancel(ctx, userID, uid, ""); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		applied, err := s.HandleCallback(ctx, uid, model.TaskSucceeded, CallbackResult{})
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
		_, uid := seedTask(t, r)
		intruderID, _ := seedTask(t, r)

		_, errMine := s.Cancel(ctx, intruderID, uid, "")
		_, errGone := s.Cancel(ctx, intruderID, "01J8ZQXK7PZZZZZZZZZZZZZZZZ", "")

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
			t.Errorf("不该通知 Agent,实际 %v", ag.canceledUIDs())
		}
	})
}

func TestCancel_AlreadyTerminal(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		userID, uid := seedTask(t, r)

		if _, err := r.Task.MarkSucceeded(ctx, uid, nil); err != nil {
			t.Fatalf("MarkSucceeded: %v", err)
		}

		_, err := s.Cancel(ctx, userID, uid, "")
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
		userID, uid := seedTask(t, r)

		got, err := s.Cancel(ctx, userID, uid, "")
		if err != nil {
			t.Fatalf("Agent 挂了不该让取消失败: %v", err)
		}
		if got.Status != model.TaskCanceled {
			t.Errorf("Status = %q", got.Status)
		}
	})
}

// Agent 客户端还没接上(nil)时同样不该失败。
// 这条不能用 withTaskSvc —— 它塞的是假 Agent,正好把要验的东西盖掉了。
func TestCancel_NilAgent(t *testing.T) {
	r := withTx(t)
	s := NewTaskService(r, nil)
	userID, uid := seedTask(t, r)

	if _, err := s.Cancel(context.Background(), userID, uid, ""); err != nil {
		t.Fatalf("未注入 Agent 不该让取消失败: %v", err)
	}
}

// ---- HandleCallback ----

func TestHandleCallback_Transitions(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		_, uid := seedTask(t, r)

		applied, err := s.HandleCallback(ctx, uid, model.TaskRunning, CallbackResult{})
		if err != nil || !applied {
			t.Fatalf("running: applied=%v err=%v", applied, err)
		}

		msg := "模型超时"
		applied, err = s.HandleCallback(ctx, uid, model.TaskFailed, CallbackResult{ErrorMsg: &msg})
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
		_, uid := seedTask(t, r)

		applied, err := s.HandleCallback(ctx, uid, model.TaskCanceled, CallbackResult{})
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
			context.Background(), "01J8ZQXK7PZZZZZZZZZZZZZZZZ", model.TaskSucceeded, CallbackResult{})
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
		_, uid := seedTask(t, r)

		if applied, err := s.HandleCallback(ctx, uid, model.TaskSucceeded, CallbackResult{}); err != nil || !applied {
			t.Fatalf("pending → succeeded 应当成功: applied=%v err=%v", applied, err)
		}

		msg := "假的失败"
		applied, err := s.HandleCallback(ctx, uid, model.TaskFailed, CallbackResult{ErrorMsg: &msg})
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
