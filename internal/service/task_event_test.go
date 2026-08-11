// task_event_test.go 只放 TaskEventHandler 的用例。
// fakeStatusWriter / withTaskSvc / seedTask 在 helpers_test.go。
package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

const evUID = "01J8ZQXK7P0000000000000000"

// 哪种事件写哪个状态、哪些根本不写。
func TestTaskEventHandler_MapsEventsToStatus(t *testing.T) {
	for _, tc := range []struct {
		evType     agentclient.EventType
		wantStatus string // 空表示不该写库
	}{
		{agentclient.EventStarted, model.TaskRunning},
		{agentclient.EventFinished, model.TaskSucceeded},
		{agentclient.EventError, model.TaskFailed},

		// 进度事件:有内容,但不改状态。
		{agentclient.EventThinking, ""},
		{agentclient.EventToolCall, ""},
		{agentclient.EventToolResult, ""},
		{agentclient.EventUnknown, ""},
	} {
		t.Run(string(tc.evType), func(t *testing.T) {
			w := newFakeStatusWriter()
			h := NewTaskEventHandler(w)

			err := h.OnEvent(context.Background(), agentclient.Event{
				TaskUID: evUID, Type: tc.evType, Seq: 1, Payload: "内容",
			})
			if err != nil {
				t.Fatalf("OnEvent: %v", err)
			}

			if tc.wantStatus == "" {
				if len(w.calls) != 0 {
					t.Fatalf("%s 不该写库,实际写了 %+v", tc.evType, w.calls)
				}
				return
			}
			if len(w.calls) != 1 {
				t.Fatalf("写库 %d 次,期望 1 次", len(w.calls))
			}
			if got := w.calls[0]; got.status != tc.wantStatus || got.taskUID != evUID {
				t.Errorf("写入 = %+v, 期望 status=%q uid=%q", got, tc.wantStatus, evUID)
			}
		})
	}
}

// 没有任何一种事件能把任务写成 canceled。
//
// 取消只能由用户发起 —— 让 Agent 也能写 canceled,等于给一个被攻破的
// Agent 一个悄悄吞任务的开关:用户看到的是自己取消的,根本不会去查。
// HandleCallback 里还有一道 switch 挡着,这条盯的是映射表这一端,
// 免得有人图省事往 eventStatus 里加一行。
func TestTaskEventHandler_NeverWritesCanceled(t *testing.T) {
	for ev, status := range eventStatus {
		if status == model.TaskCanceled {
			t.Errorf("eventStatus[%s] = canceled,取消只能由用户发起", ev)
		}
	}

	// 把所有已知事件类型都过一遍,确认没有一条走出 canceled。
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w)
	for _, ev := range []agentclient.EventType{
		agentclient.EventUnknown, agentclient.EventStarted, agentclient.EventThinking,
		agentclient.EventToolCall, agentclient.EventToolResult,
		agentclient.EventFinished, agentclient.EventError,
	} {
		if err := h.OnEvent(context.Background(), agentclient.Event{
			TaskUID: evUID, Type: ev, Payload: "canceled", // payload 里写 canceled 也没用
		}); err != nil {
			t.Fatalf("OnEvent(%s): %v", ev, err)
		}
	}
	for _, c := range w.calls {
		if c.status == model.TaskCanceled {
			t.Errorf("有事件写出了 canceled: %+v", c)
		}
	}
}

// 失败原因要截断。tasks.error_msg 是 text,数据库自己不设上限,
// 而 gRPC 这条路没有 binding tag 挡着 —— 不截的话对面一条事件
// 就能往库里塞几 MB,而且它还会跟着任务详情发回给用户。
func TestTaskEventHandler_TruncatesErrorMsg(t *testing.T) {
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w)

	// 用汉字:按字节截会把一个汉字劈成两半,落库是乱码。
	long := strings.Repeat("错", maxEventErrMsg+500)
	if err := h.OnEvent(context.Background(), agentclient.Event{
		TaskUID: evUID, Type: agentclient.EventError, Payload: long,
	}); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}

	got := w.calls[0].errMsg
	if got == nil {
		t.Fatal("errMsg 是 nil")
	}
	if n := len([]rune(*got)); n != maxEventErrMsg {
		t.Errorf("截断后 %d 个字符,期望 %d", n, maxEventErrMsg)
	}
	if !strings.HasPrefix(*got, "错") || strings.Contains(*got, "�") {
		t.Error("截断切坏了字符")
	}
}

// 成功的任务不该带失败原因,空白的原因要存 NULL 而不是空串。
func TestTaskEventHandler_ErrMsgOnlyOnFailure(t *testing.T) {
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w)
	ctx := context.Background()

	// finished 带了 payload 也不该写进 error_msg。
	if err := h.OnEvent(ctx, agentclient.Event{
		TaskUID: evUID, Type: agentclient.EventFinished, Payload: "跑完了",
	}); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	if w.calls[0].errMsg != nil {
		t.Errorf("succeeded 不该带 error_msg,实际 %q", *w.calls[0].errMsg)
	}

	// error 但 payload 全是空白 → NULL,不是 ""。
	if err := h.OnEvent(ctx, agentclient.Event{
		TaskUID: evUID, Type: agentclient.EventError, Payload: "   ",
	}); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	if w.calls[1].errMsg != nil {
		t.Errorf("空白原因该存 NULL,实际 %q", *w.calls[1].errMsg)
	}
}

// 写库失败要往上抛,让 agentclient 记一条日志。
// 但流不会因此中断 —— 那是 agentclient 那边的行为,这里只管把错误交出去。
func TestTaskEventHandler_PropagatesWriteFailure(t *testing.T) {
	w := newFakeStatusWriter()
	w.err = errors.New("数据库炸了")

	err := NewTaskEventHandler(w).OnEvent(context.Background(), agentclient.Event{
		TaskUID: evUID, Type: agentclient.EventFinished,
	})
	if err == nil {
		t.Fatal("写库失败了却返回 nil")
	}
}

// applied=false 不是错误:最常见的是用户抢先取消了,任务已经是终态。
func TestTaskEventHandler_LateEventIsNotAnError(t *testing.T) {
	w := newFakeStatusWriter()
	w.applied = false

	if err := NewTaskEventHandler(w).OnEvent(context.Background(), agentclient.Event{
		TaskUID: evUID, Type: agentclient.EventFinished,
	}); err != nil {
		t.Fatalf("迟到的事件不该报错: %v", err)
	}
}

// ---- OnStreamError ----

// 流断了必须给任务收尾。同步 Agent 只有事件流这一条汇报通道,
// 不收尾的话任务永远停在 running,用户界面上转圈转到天荒地老。
func TestTaskEventHandler_StreamErrorFailsTheTask(t *testing.T) {
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w)

	h.OnStreamError(context.Background(), evUID,
		errors.New(`rpc error: code = Unavailable desc = dial tcp 10.0.0.7:50051: refused`))

	if len(w.calls) != 1 {
		t.Fatalf("写库 %d 次,期望 1 次", len(w.calls))
	}
	got := w.calls[0]
	if got.status != model.TaskFailed || got.taskUID != evUID {
		t.Errorf("写入 = %+v, 期望 failed", got)
	}

	// error_msg 会回显给用户,不能带 gRPC 原文 —— 那里面有内网地址和端口。
	if got.errMsg == nil {
		t.Fatal("errMsg 是 nil,用户看不到任何解释")
	}
	for _, leak := range []string{"10.0.0.7", "50051", "rpc error", "dial tcp", "Unavailable"} {
		if strings.Contains(*got.errMsg, leak) {
			t.Errorf("error_msg 泄露了 %q: %q", leak, *got.errMsg)
		}
	}
}

// 流断的时候任务可能已经跑完了(finished 先到、流才断)。
// 这时 cas 会让这次收尾落空,不能把一个成功的任务盖成失败。
func TestTaskEventHandler_StreamErrorDoesNotOverwriteTerminal(t *testing.T) {
	w := newFakeStatusWriter()
	w.applied = false // 模拟 cas 没匹配到行

	// 不 panic、不重试就算过。
	NewTaskEventHandler(w).OnStreamError(context.Background(), evUID, errors.New("断了"))

	if len(w.calls) != 1 {
		t.Errorf("应当只尝试一次,实际 %d 次", len(w.calls))
	}
}

// ---- 连着真库跑一遍 ----

// 上面那些用的是假 writer,验的是映射。这条验的是接到真 TaskService
// 之后整条路真的能把状态写进库。
func TestTaskEventHandler_EndToEnd(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		_, uid := seedTask(t, r)
		h := NewTaskEventHandler(s)

		if err := h.OnEvent(ctx, agentclient.Event{
			TaskUID: uid, Type: agentclient.EventStarted, Seq: 1,
		}); err != nil {
			t.Fatalf("started: %v", err)
		}
		mid, err := r.Task.GetByUID(ctx, uid)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if mid.Status != model.TaskRunning {
			t.Fatalf("Status = %q, 期望 %q", mid.Status, model.TaskRunning)
		}
		if mid.StartedAt == nil {
			t.Error("StartedAt 没被填上")
		}

		if err := h.OnEvent(ctx, agentclient.Event{
			TaskUID: uid, Type: agentclient.EventFinished, Seq: 2,
		}); err != nil {
			t.Fatalf("finished: %v", err)
		}
		got, err := r.Task.GetByUID(ctx, uid)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if got.Status != model.TaskSucceeded {
			t.Errorf("Status = %q, 期望 %q", got.Status, model.TaskSucceeded)
		}
		if got.FinishedAt == nil {
			t.Error("FinishedAt 没被填上")
		}
	})
}

// 用户取消之后,Agent 迟到的 finished 事件不能把状态翻回去。
// 这是取消那条不变式在 gRPC 这条新路径上的复验 ——
// HTTP 回调那侧已经有 TestCancel_LateCallbackCannotRevive 盯着了。
func TestTaskEventHandler_CannotReviveCanceledTask(t *testing.T) {
	withTaskSvc(t, func(t *testing.T, s *TaskService, r *repository.Repository, _ *fakeAgent) {
		ctx := context.Background()
		userID, uid := seedTask(t, r)

		if _, err := s.Cancel(ctx, userID, uid, "不想跑了"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		h := NewTaskEventHandler(s)
		if err := h.OnEvent(ctx, agentclient.Event{
			TaskUID: uid, Type: agentclient.EventFinished, Seq: 9,
		}); err != nil {
			t.Fatalf("迟到的事件不该报错: %v", err)
		}
		// 流跟着断了也一样。
		h.OnStreamError(ctx, uid, errors.New("断了"))

		got, err := r.Task.GetByUID(ctx, uid)
		if err != nil {
			t.Fatalf("复查: %v", err)
		}
		if got.Status != model.TaskCanceled {
			t.Errorf("Status = %q, 期望仍是 %q", got.Status, model.TaskCanceled)
		}
		if got.CancelReason == nil || *got.CancelReason != "不想跑了" {
			t.Errorf("CancelReason = %v, 不该被事件冲掉", got.CancelReason)
		}
	})
}
