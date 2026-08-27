package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ptr"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// ---- 任务派发 ----

// 两个建任务的入口都必须把任务发给 Agent。
func TestCreate_DispatchesToAgent(t *testing.T) {
	t.Run("CreateWithFirstTask", func(t *testing.T) {
		withSessionSvc(t, func(t *testing.T, s *SessionService, r *repository.Repository, ag *fakeAgent) {
			u := seedUser(t, r)

			_, task, err := s.CreateWithFirstTask(context.Background(), u,
				ptr.To("标题"), ptr.To("帮我订张票"))
			if err != nil {
				t.Fatalf("CreateWithFirstTask: %v", err)
			}
			assertDispatched(t, ag, task, "帮我订张票")
		})
	})

	t.Run("CreateTaskWithSessionID", func(t *testing.T) {
		withSessionSvc(t, func(t *testing.T, s *SessionService, r *repository.Repository, ag *fakeAgent) {
			ctx := context.Background()
			u := seedUser(t, r)
			sess, _, err := s.CreateWithFirstTask(ctx, u, ptr.To("标题"), nil)
			if err != nil {
				t.Fatalf("建会话: %v", err)
			}
			ag.started = nil // 只看下面这一次

			task, err := s.CreateTaskWithSessionID(ctx, u, sess.SessionID, ptr.To("再来一单"))
			if err != nil {
				t.Fatalf("CreateTaskWithSessionID: %v", err)
			}
			assertDispatched(t, ag, task, "再来一单")
		})
	})
}

// 这条是整件事的关键:派发必须发生在事务提交之后。
//
// Agent 是同步的,它可能在几毫秒内就把 started 事件推回来,而那条事件
// 按 task_uid 去 UPDATE —— 事务没提交的话那一行对别的连接不可见,
// UPDATE 匹配不到,cas 返回 false(在它眼里只是"迟到的回调"),
// 任务就永远停在 pending 了。
//
// 验法:在假 Agent 的 StartTask 里,用**另一条连接**去查这个任务。
// 查得到 = 事务已经提交了;查不到 = StartTask 还在事务里面被调的。
//
// 这条**不能**用 withTx。withTx 把整个用例包在一个永不提交的事务里,
// 从别的连接看什么都看不见,断言会恒为假 —— 而要验的恰恰就是可见性。
// 代价是它真往库里写,所以自己收尾(t.Cleanup)。
func TestCreate_DispatchesAfterCommit(t *testing.T) {
	if testDB == nil {
		t.Skip("需要 TEST_DSN")
	}

	r := repository.New(testDB) // 不套外层事务
	ag := &fakeAgent{}
	s := NewSessionService(r, ag, nil)

	userID := seedUser(t, r)
	t.Cleanup(func() {
		// 顺序:任务 → 会话 → 用户,反着来会撞外键(将来加上的话)。
		testDB.Exec("DELETE FROM tasks WHERE user_id = ?", userID)
		testDB.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
		testDB.Exec("DELETE FROM users WHERE id = ?", userID)
	})

	var visibleElsewhere bool
	ag.onStart = func(req agentclient.StartTaskReq) {
		// 从连接池另取一条连接查。CreateWithFirstTask 的事务把它自己那条
		// 连接占着,所以这里拿到的一定是另一条 —— 只有提交过的数据才看得见。
		other := repository.New(testDB)
		if _, err := other.Task.GetByUID(context.Background(), req.TaskUID); err == nil {
			visibleElsewhere = true
		}
	}

	if _, _, err := s.CreateWithFirstTask(context.Background(), userID,
		nil, ptr.To("跑一下")); err != nil {
		t.Fatalf("CreateWithFirstTask: %v", err)
	}

	if len(ag.started) != 1 {
		t.Fatalf("派发了 %d 次,期望 1 次", len(ag.started))
	}
	if !visibleElsewhere {
		t.Error("派发时别的连接还查不到这个任务 —— " +
			"说明 StartTask 是在事务提交之前调的。Agent 推回来的 started 事件会打空,任务会永远停在 pending")
	}
}

// 派发失败要就地把任务标成 failed,不能留在 pending。
//
// 同步 Agent 只有这一次交接机会,交不出去就再没人会推动它。
// 留在 pending 的话,用户界面上是一个永远转圈的任务,还没人能解释为什么。
func TestCreate_DispatchFailureMarksTaskFailed(t *testing.T) {
	withSessionSvc(t, func(t *testing.T, s *SessionService, r *repository.Repository, ag *fakeAgent) {
		ag.startErr = errors.New("connection refused: 10.0.0.7:50051")
		u := seedUser(t, r)

		_, task, err := s.CreateWithFirstTask(context.Background(), u, nil, ptr.To("跑一下"))
		// 任务确实建出来了,不该报错。
		if err != nil {
			t.Fatalf("派发失败不该让创建失败: %v", err)
		}
		if task.Status != model.TaskFailed {
			t.Errorf("Status = %q, 期望 %q", task.Status, model.TaskFailed)
		}
		if task.ErrorMsg == nil {
			t.Fatal("ErrorMsg 是 nil,用户看不到任何解释")
		}
		// 原因会回显给用户,不能带 gRPC 原文(里面是内网地址和端口)。
		for _, leak := range []string{"10.0.0.7", "50051", "connection refused"} {
			if strings.Contains(*task.ErrorMsg, leak) {
				t.Errorf("ErrorMsg 泄露了 %q: %q", leak, *task.ErrorMsg)
			}
		}

		// 返回的必须是重查之后那份,库里也得真的是 failed。
		got, err := r.Task.GetByUID(context.Background(), task.TaskUID)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if got.Status != model.TaskFailed {
			t.Errorf("库里 Status = %q", got.Status)
		}
	})
}

// 没注入 Agent 时任务照常建出来,停在 pending,只记 Warn。
// 和 TaskService 那边"没接上就只记日志"是同一个取向。
func TestCreate_NilAgentStillCreates(t *testing.T) {
	r := withTx(t)
	s := NewSessionService(r, nil, nil)
	u := seedUser(t, r)

	_, task, err := s.CreateWithFirstTask(context.Background(), u, nil, ptr.To("跑一下"))
	if err != nil {
		t.Fatalf("未注入 Agent 不该让创建失败: %v", err)
	}
	if task.Status != model.TaskPending {
		t.Errorf("Status = %q, 期望 %q", task.Status, model.TaskPending)
	}
}

// assertDispatched 验派发出去的那条请求。
func assertDispatched(t *testing.T, ag *fakeAgent, task *model.Task, wantInput string) {
	t.Helper()

	if len(ag.started) != 1 {
		t.Fatalf("派发了 %d 次,期望 1 次", len(ag.started))
	}
	got := ag.started[0]
	if got.TaskUID != task.TaskUID {
		t.Errorf("TaskUID = %q, 期望 %q", got.TaskUID, task.TaskUID)
	}
	if got.SessionID != task.SessionID {
		t.Errorf("SessionID = %q, 期望 %q", got.SessionID, task.SessionID)
	}
	if got.Input != wantInput {
		t.Errorf("Input = %q, 期望 %q", got.Input, wantInput)
	}
}

// TestSessionService
func TestSessionService_AllMethodsRejectOtherUsers(t *testing.T) {
	withSessionSvc(t, func(t *testing.T, s *SessionService, r *repository.Repository, _ *fakeAgent) {
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
