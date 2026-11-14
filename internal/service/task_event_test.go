// task_event_test.go 只放 TaskEventHandler 的用例。
// fakeStatusWriter / withTaskSvc / seedTask 在 helpers_test.go。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

const evUID = "01J8ZQXK7P0000000000000000"

// evSessionID 是这些事件所属的会话。
//
// 它必须一路带到事件里:事件表按 session_id 索引,归属校验也走
// session_id → sessions.user_id —— 少了它,事件一条都写不进去。
const evSessionID = "01J8ZQXK7P0000000000000001"

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
			h := NewTaskEventHandler(w, newFakeEventRecorder())

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
	h := NewTaskEventHandler(w, newFakeEventRecorder())
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
	h := NewTaskEventHandler(w, newFakeEventRecorder())

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
	h := NewTaskEventHandler(w, newFakeEventRecorder())
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

	err := NewTaskEventHandler(w, newFakeEventRecorder()).OnEvent(context.Background(), agentclient.Event{
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

	if err := NewTaskEventHandler(w, newFakeEventRecorder()).OnEvent(context.Background(), agentclient.Event{
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
	h := NewTaskEventHandler(w, newFakeEventRecorder())

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

// TestOnStreamError_AgentBusyMessage 盯着"满了"和"断了"的文案分流。
//
// 执行器满了不是连接断了:前者是执行器好得很、只是同时能跑的任务数
// 到顶了,用户过一会儿重发就行;后者是链路或者执行器出了问题。
// 写成同一句的话,用户会在最不该放弃的时候放弃 —— 执行器正忙的时候。
//
// 这一条还顺带盯着:error_msg 是**跟着任务详情回显给用户**的,
// 里面绝不能出现 gRPC 原文(地址、端口、内部细节)。
func TestOnStreamError_AgentBusyMessage(t *testing.T) {
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w, newFakeEventRecorder())

	h.OnStreamError(context.Background(), evUID,
		fmt.Errorf("接收任务事件流: %w", agentclient.ErrAgentBusy))

	if len(w.calls) != 1 {
		t.Fatalf("写库 %d 次,期望 1 次", len(w.calls))
	}
	got := w.calls[0]
	if got.status != model.TaskFailed {
		t.Fatalf("status = %q, 期望 failed", got.status)
	}
	if got.errMsg == nil {
		t.Fatal("errMsg 是 nil,用户看不到任何解释")
	}

	msg := *got.errMsg
	if strings.Contains(msg, "连接中断") {
		t.Errorf("执行器满了却说成「连接中断」: %q", msg)
	}
	if !strings.Contains(msg, "并发") {
		t.Errorf("文案里没提并发: %q", msg)
	}
	for _, leak := range []string{"RESOURCE_EXHAUSTED", "gRPC", "10.0.0.7", "50051"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error_msg 泄露了 %q: %q", leak, msg)
		}
	}
}

// 流断的时候任务可能已经跑完了(finished 先到、流才断)。
// 这时 cas 会让这次收尾落空,不能把一个成功的任务盖成失败。
func TestTaskEventHandler_StreamErrorDoesNotOverwriteTerminal(t *testing.T) {
	w := newFakeStatusWriter()
	w.applied = false // 模拟 cas 没匹配到行

	// 不 panic、不重试就算过。
	NewTaskEventHandler(w, newFakeEventRecorder()).OnStreamError(context.Background(), evUID, errors.New("断了"))

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
		h := NewTaskEventHandler(s, newFakeEventRecorder())

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

		h := NewTaskEventHandler(s, newFakeEventRecorder())
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

// ---- 心跳 ----

// TestTaskEventHandler_ProgressEventsSendHeartbeat 盯着回收器和长任务的接缝。
//
// 进度事件不改状态,所以很容易被当成"什么都不用做"。但它是一个跑很久的
// 任务**唯一**的活着证据:不续 updated_at 的话,一个中间只有 thinking /
// tool_call 的任务撑过 stale_after 就会被回收器当成卡住的收掉,
// 而它其实跑得好好的。这种误杀只在长任务上出现,本地几乎复现不出来。
func TestTaskEventHandler_ProgressEventsSendHeartbeat(t *testing.T) {
	for _, evType := range []agentclient.EventType{
		agentclient.EventThinking,
		agentclient.EventToolCall,
		agentclient.EventToolResult,
		agentclient.EventUnknown,
	} {
		t.Run(string(evType), func(t *testing.T) {
			w := newFakeStatusWriter()
			h := NewTaskEventHandler(w, newFakeEventRecorder())

			err := h.OnEvent(context.Background(), agentclient.Event{
				TaskUID: evUID, Type: evType, Seq: 1, Payload: "内容",
			})
			if err != nil {
				t.Fatalf("OnEvent: %v", err)
			}
			if len(w.touched) != 1 || w.touched[0] != evUID {
				t.Errorf("进度事件没有续命(touched=%v),长任务会被回收器误杀", w.touched)
			}
			if len(w.calls) != 0 {
				t.Errorf("进度事件不该写状态,却写了 %d 次", len(w.calls))
			}
		})
	}
}

// 改状态的事件本身就会刷新 updated_at(cas 那条 UPDATE 带着它),
// 不需要再额外发一条心跳 —— 多一条就是多一次写同一行。
func TestTaskEventHandler_StatusEventsDoNotHeartbeat(t *testing.T) {
	for _, evType := range []agentclient.EventType{
		agentclient.EventStarted, agentclient.EventFinished, agentclient.EventError,
	} {
		t.Run(string(evType), func(t *testing.T) {
			w := newFakeStatusWriter()
			h := NewTaskEventHandler(w, newFakeEventRecorder())

			if err := h.OnEvent(context.Background(), agentclient.Event{
				TaskUID: evUID, Type: evType, Seq: 1,
			}); err != nil {
				t.Fatalf("OnEvent: %v", err)
			}
			if len(w.touched) != 0 {
				t.Errorf("状态事件不该再单独发心跳,cas 那条 UPDATE 已经刷过 updated_at 了")
			}
		})
	}
}

// 心跳失败不能中断事件处理:续命是尽力而为的一步,
// 而这条路径上真正要紧的是别让后面的事件(尤其是 finished)收不到。
func TestTaskEventHandler_HeartbeatFailureIsSwallowed(t *testing.T) {
	w := newFakeStatusWriter()
	w.touchErr = errors.New("库挂了")
	h := NewTaskEventHandler(w, newFakeEventRecorder())

	err := h.OnEvent(context.Background(), agentclient.Event{
		TaskUID: evUID, Type: agentclient.EventThinking, Seq: 1,
	})
	if err != nil {
		t.Errorf("心跳失败不该往上抛: %v", err)
	}
}

// ---------------------------------------------------------------------------
// finished 事件里的最终答复
// ---------------------------------------------------------------------------

// finished 的 payload 是 Agent 拼的 JSON(grpcserver/server.py 里的
// json.dumps(out)),答复在 final_response 字段里。解出来落库,
// 前端的对话页才有东西可渲染 —— 在此之前这个值是被整个丢掉的。
func TestOnEvent_FinishedCarriesFinalResponse(t *testing.T) {
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w, newFakeEventRecorder())

	payload := `{"final_response":"北京今天多云,22 到 30 度。","completed":true,"used_turns":3}`
	err := h.OnEvent(context.Background(), agentclient.Event{
		TaskUID: "T1", Type: agentclient.EventFinished, Seq: 9, Payload: payload,
	})
	if err != nil {
		t.Fatalf("OnEvent: %v", err)
	}

	if len(w.calls) != 1 {
		t.Fatalf("写了 %d 次状态,期望 1", len(w.calls))
	}
	c := w.calls[0]
	if c.status != model.TaskSucceeded {
		t.Fatalf("status = %q, 想要 %q", c.status, model.TaskSucceeded)
	}
	if c.output == nil {
		t.Fatal("答复没落库 —— 对话页会是空的")
	}
	if *c.output != "北京今天多云,22 到 30 度。" {
		t.Fatalf("output = %q", *c.output)
	}
	// 成功的任务不该带错误信息。
	if c.errMsg != nil {
		t.Errorf("成功的任务不该有 error_msg: %q", *c.errMsg)
	}
}

// payload 解不出来时**不能**让整条事件失败:状态是"成功"这件事比正文更要紧,
// 一起丢掉的话任务会永远停在 running,那比"答案没显示出来"糟糕得多。
// 也不能把原始 payload 当答案兜底 —— 用户会看到一坨 JSON。
func TestOnEvent_FinishedWithBadPayloadStillSucceeds(t *testing.T) {
	for _, payload := range []string{"", "   ", "not json at all", "{"} {
		w := newFakeStatusWriter()
		h := NewTaskEventHandler(w, newFakeEventRecorder())

		err := h.OnEvent(context.Background(), agentclient.Event{
			TaskUID: "T1", Type: agentclient.EventFinished, Seq: 1, Payload: payload,
		})
		if err != nil {
			t.Fatalf("payload=%q 不该让事件失败: %v", payload, err)
		}
		if len(w.calls) != 1 || w.calls[0].status != model.TaskSucceeded {
			t.Fatalf("payload=%q 仍然应当写成 succeeded, calls=%+v", payload, w.calls)
		}
		if out := w.calls[0].output; out != nil && *out == payload {
			t.Fatalf("payload=%q 被当成答案兜底了,用户会看到一坨原文", payload)
		}
	}
}

// 失败的任务只带原因,不带正文 —— 反过来也一样。两个字段混了的话,
// 用户会把报错当成答案读。
func TestOnEvent_ErrorCarriesOnlyErrorMsg(t *testing.T) {
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w, newFakeEventRecorder())

	err := h.OnEvent(context.Background(), agentclient.Event{
		TaskUID: "T1", Type: agentclient.EventError, Seq: 2, Payload: "模型后端连不上",
	})
	if err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	c := w.calls[0]
	if c.status != model.TaskFailed {
		t.Fatalf("status = %q", c.status)
	}
	if c.errMsg == nil || *c.errMsg != "模型后端连不上" {
		t.Fatalf("errMsg = %v", c.errMsg)
	}
	if c.output != nil {
		t.Errorf("失败的任务不该有正文: %q", *c.output)
	}
}

// 重发之前那道闸门:任务不在 running 就不重发。
//
// 取消是"先落库、再通知 Agent",所以用户点了取消之后库里已经是 canceled,
// 而 agentclient 不读库、判断不了这件事。这道闸门漏了的话,一个用户
// 明确叫停的任务会被原样重跑一遍,再烧一轮 LLM。
func TestShouldRetry_OnlyWhenStillRunning(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		err     error
		want    bool
	}{
		{name: "还在跑,该重发", running: true, want: true},
		{name: "已经不是 running(取消/收尾/成功),不重发", running: false, want: false},
		{name: "库报错时不重发", running: true, err: errors.New("库炸了"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newFakeStatusWriter()
			w.running = tc.running
			w.runningErr = tc.err
			h := NewTaskEventHandler(w, newFakeEventRecorder())

			if got := h.ShouldRetry(context.Background(), evUID, 0); got != tc.want {
				t.Errorf("ShouldRetry = %v,期望 %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 事件落库
// ---------------------------------------------------------------------------

// fakeEventRecorder 是 TaskEventRecorder 的假实现,用来验
// "事件有没有被交出来记录",跟数据库没关系。
//
// err 让记录失败,验它不会把事件流带崩。
type fakeEventRecorder struct {
	events []TaskEventRecord
	err    error
}

func newFakeEventRecorder() *fakeEventRecorder { return &fakeEventRecorder{} }

func (f *fakeEventRecorder) RecordEvent(_ context.Context, ev TaskEventRecord) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, ev)
	return nil
}

// 进度事件以前**只进日志**,前端一个字都看不到。
// 这条盯的是"它们现在也被记下来了" —— SSE 的全部价值都压在这一条上。
func TestTaskEventHandler_RecordsEveryEvent(t *testing.T) {
	for _, evType := range []agentclient.EventType{
		agentclient.EventStarted,
		agentclient.EventThinking,
		agentclient.EventToolCall,
		agentclient.EventToolResult,
		agentclient.EventFinished,
		agentclient.EventError,
	} {
		t.Run(string(evType), func(t *testing.T) {
			w := newFakeStatusWriter()
			rec := newFakeEventRecorder()
			h := NewTaskEventHandler(w, rec)

			if err := h.OnEvent(context.Background(), agentclient.Event{
				TaskUID: evUID, SessionID: evSessionID, Type: evType, Seq: 3, Payload: "内容",
			}); err != nil {
				t.Fatalf("OnEvent: %v", err)
			}

			if len(rec.events) != 1 {
				t.Fatalf("%s 记了 %d 条,期望 1 条", evType, len(rec.events))
			}
			got := rec.events[0]
			if got.TaskUID != evUID || got.SessionID != evSessionID ||
				got.Seq != 3 || got.Type != string(evType) || got.Payload != "内容" {
				t.Errorf("记下来的事件 = %+v,和收到的不一致", got)
			}
		})
	}
}

// 记录失败**不许**把状态处理一起带坏。
//
// 这是"一断全断"的反面:session_id 没带下来(或者事件表挂了)时,
// 事件是记不下来,但任务的状态必须照常推进 ——
// 否则一个纯附加的观察功能就能让用户的任务永远停在 running。
func TestTaskEventHandler_RecordFailureDoesNotBreakStatus(t *testing.T) {
	w := newFakeStatusWriter()
	rec := newFakeEventRecorder()
	rec.err = errors.New("事件表炸了")
	h := NewTaskEventHandler(w, rec)

	err := h.OnEvent(context.Background(), agentclient.Event{
		TaskUID: evUID, SessionID: evSessionID, Type: agentclient.EventFinished, Seq: 1,
	})
	if err != nil {
		t.Fatalf("记录失败不该往上抛: %v", err)
	}
	if len(w.calls) != 1 || w.calls[0].status != model.TaskSucceeded {
		t.Errorf("状态没照常写: %+v", w.calls)
	}
}

// 事件记录器没接上时(事件表还没迁移)不许 panic。
func TestTaskEventHandler_NilRecorderIsFine(t *testing.T) {
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w, nil)

	if err := h.OnEvent(context.Background(), agentclient.Event{
		TaskUID: evUID, SessionID: evSessionID, Type: agentclient.EventThinking, Seq: 1,
	}); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	if len(w.calls) != 0 {
		t.Errorf("进度事件不该写状态,却写了 %d 次", len(w.calls))
	}
}

// ---------------------------------------------------------------------------
// TaskEventService
// ---------------------------------------------------------------------------

// fakeEventStore 是 eventStore 的内存实现。
//
// 它按真仓库的语义来:id 自增、按 id 升序、limit 生效。
type fakeEventStore struct {
	rows []model.TaskEvent
	err  error
}

func (f *fakeEventStore) Append(_ context.Context, ev *model.TaskEvent) error {
	if f.err != nil {
		return f.err
	}
	ev.ID = int64(len(f.rows) + 1)
	f.rows = append(f.rows, *ev)
	return nil
}

func (f *fakeEventStore) ListSince(_ context.Context, sessionID string,
	afterID int64, limit int) ([]model.TaskEvent, error) {

	if f.err != nil {
		return nil, f.err
	}
	var out []model.TaskEvent
	for _, r := range f.rows {
		if r.SessionID != sessionID || r.ID <= afterID {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// fakeSessionLookup 是 sessionLookup 的内存实现。
type fakeSessionLookup struct {
	sessions map[string]*model.Session
	err      error
}

func (f *fakeSessionLookup) GetByID(_ context.Context, sessionID string) (*model.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	s, ok := f.sessions[sessionID]
	if !ok {
		return nil, model.ErrSessionNotFound
	}
	return s, nil
}

// fakeNotifier 记下被通知的会话。panicWith 让它炸,验通知炸了也不影响记录。
type fakeNotifier struct {
	sessions  []string
	panicWith any
}

func (f *fakeNotifier) Publish(_ context.Context, sessionID string) {
	f.sessions = append(f.sessions, sessionID)
	if f.panicWith != nil {
		panic(f.panicWith)
	}
}

// payload 必须截断:tool_result 的 content 可能是整个文件的内容,几百 KB。
// 事件会落库(text 列没有上限),也会推给浏览器。
//
// 按**字符**截:按字节切会把汉字劈成两半,落库就是乱码。
func TestTaskEventService_RecordEvent_TruncatesPayload(t *testing.T) {
	store := &fakeEventStore{}
	svc := NewTaskEventService(store, &fakeSessionLookup{}, nil)

	// 用汉字:一个汉字 3 字节,按字节切和按字符切的结果差三倍。
	long := strings.Repeat("错", maxEventPayload+500)
	err := svc.RecordEvent(context.Background(), TaskEventRecord{
		TaskUID: evUID, SessionID: evSessionID, Seq: 1,
		Type: "tool_result", Payload: long,
	})
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	if len(store.rows) != 1 {
		t.Fatalf("写了 %d 行,期望 1 行", len(store.rows))
	}
	got := store.rows[0].Payload
	if n := len([]rune(got)); n != maxEventPayload {
		t.Errorf("截断后 %d 个字符,期望 %d", n, maxEventPayload)
	}
	if !strings.HasPrefix(got, "错") || strings.Contains(got, "�") {
		t.Error("截断切坏了字符")
	}
}

// 没超长的 payload 要**原样**存。
//
// 事件表是 Agent 发的那一份的忠实副本:消费方自己按 type 解,
// 两套解析逻辑迟早会漂(而且去掉首尾空白等于悄悄改了内容)。
func TestTaskEventService_RecordEvent_KeepsPayloadVerbatim(t *testing.T) {
	store := &fakeEventStore{}
	svc := NewTaskEventService(store, &fakeSessionLookup{}, nil)

	payload := `{"final_response":"北京今天多云。","completed":true}`
	if err := svc.RecordEvent(context.Background(), TaskEventRecord{
		TaskUID: evUID, SessionID: evSessionID, Seq: 1, Type: "finished", Payload: payload,
	}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if store.rows[0].Payload != payload {
		t.Errorf("payload 被改了: %q", store.rows[0].Payload)
	}
}

// 记完要通知订阅方 —— 否则 SSE 那边只能靠轮询等。
func TestTaskEventService_RecordEvent_NotifiesSubscriber(t *testing.T) {
	store := &fakeEventStore{}
	n := &fakeNotifier{}
	svc := NewTaskEventService(store, &fakeSessionLookup{}, n)

	if err := svc.RecordEvent(context.Background(), TaskEventRecord{
		TaskUID: evUID, SessionID: evSessionID, Seq: 1, Type: "thinking", Payload: "在想",
	}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if len(n.sessions) != 1 || n.sessions[0] != evSessionID {
		t.Errorf("通知的会话 = %v,期望 [%s]", n.sessions, evSessionID)
	}
}

// 通知方炸了不许影响记录:事件已经在库里了,订阅方下一次唤醒
// (或者兜底轮询)就能拿到。让它冒泡只会把整条事件流带断。
func TestTaskEventService_RecordEvent_NotifyPanicIsSwallowed(t *testing.T) {
	store := &fakeEventStore{}
	n := &fakeNotifier{panicWith: "通知实现炸了"}
	svc := NewTaskEventService(store, &fakeSessionLookup{}, n)

	err := svc.RecordEvent(context.Background(), TaskEventRecord{
		TaskUID: evUID, SessionID: evSessionID, Seq: 1, Type: "thinking",
	})
	if err != nil {
		t.Fatalf("通知失败不该让记录失败: %v", err)
	}
	if len(store.rows) != 1 {
		t.Errorf("事件没记上: %d 行", len(store.rows))
	}
}

// 没有 session_id 的事件归不到任何会话上,谁也读不到 ——
// 这是接线错误(session_id 没有一路带下来),不是正常输入。
//
// **必须报错而不是静默丢掉**:静默丢掉的症状是"前端什么都不来",
// 而报错会留下一条 Warn,一眼就能看出是哪条链没接上。
func TestTaskEventService_RecordEvent_WithoutSessionIDIsRejected(t *testing.T) {
	store := &fakeEventStore{}
	svc := NewTaskEventService(store, &fakeSessionLookup{}, nil)

	err := svc.RecordEvent(context.Background(), TaskEventRecord{
		TaskUID: evUID, Seq: 1, Type: "thinking",
	})
	if err == nil {
		t.Fatal("缺 session_id 却返回成功")
	}
	if len(store.rows) != 0 {
		t.Errorf("缺 session_id 的事件不该入库: %d 行", len(store.rows))
	}
}

// 写库失败要交出去:调用方据此判断"这条事件没记上"。
func TestTaskEventService_RecordEvent_WriteFailureIsReported(t *testing.T) {
	store := &fakeEventStore{err: errors.New("库炸了")}
	svc := NewTaskEventService(store, &fakeSessionLookup{}, nil)

	err := svc.RecordEvent(context.Background(), TaskEventRecord{
		TaskUID: evUID, SessionID: evSessionID, Seq: 1, Type: "thinking",
	})
	if err == nil {
		t.Fatal("写库失败却返回成功")
	}
}

// 别人的会话看起来就是"不存在"。
//
// **不许是 403**:403 和 404 分开的话,这个接口就成了一台 session_id 探测器
// —— 拿到 403 说明这个 ID 真实存在,拿到 404 说明不存在。
func TestTaskEventService_MustOwnSession_OtherUsersSessionLooksMissing(t *testing.T) {
	sessions := &fakeSessionLookup{sessions: map[string]*model.Session{
		evSessionID: {SessionID: evSessionID, UserID: 42},
	}}
	svc := NewTaskEventService(&fakeEventStore{}, sessions, nil)

	err := svc.MustOwnSession(context.Background(), 7, evSessionID)
	if !errors.Is(err, model.ErrSessionNotFound) {
		t.Fatalf("err = %v,期望 %v", err, model.ErrSessionNotFound)
	}
}

// 会话确实存在、也确实是这个用户的 → 放行。
func TestTaskEventService_MustOwnSession_OwnSessionPasses(t *testing.T) {
	sessions := &fakeSessionLookup{sessions: map[string]*model.Session{
		evSessionID: {SessionID: evSessionID, UserID: 42},
	}}
	svc := NewTaskEventService(&fakeEventStore{}, sessions, nil)

	if err := svc.MustOwnSession(context.Background(), 42, evSessionID); err != nil {
		t.Errorf("自己的会话不该被拒: %v", err)
	}
}

// 不存在的会话和参数非法,都归到同一个 NotFound。
//
// 单独给一个"参数错误"会让调用方能区分"ID 格式不对"和"ID 不属于我",
// 而格式那关 handler 已经把过了。
func TestTaskEventService_MustOwnSession_RejectsBadInput(t *testing.T) {
	sessions := &fakeSessionLookup{sessions: map[string]*model.Session{
		evSessionID: {SessionID: evSessionID, UserID: 42},
	}}
	svc := NewTaskEventService(&fakeEventStore{}, sessions, nil)

	for _, tc := range []struct {
		name      string
		userID    int64
		sessionID string
	}{
		{"用户 id 非法", 0, evSessionID},
		{"用户 id 是负数", -1, evSessionID},
		{"会话 id 是空串", 42, ""},
		{"会话不存在", 42, "01J8ZQXK7P0000000000009999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.MustOwnSession(context.Background(), tc.userID, tc.sessionID)
			if !errors.Is(err, model.ErrSessionNotFound) {
				t.Errorf("err = %v,期望 %v", err, model.ErrSessionNotFound)
			}
		})
	}
}

// 增量读:只给 afterID 之后的,按 id 升序。
func TestTaskEventService_ListEvents_ReturnsIncremental(t *testing.T) {
	store := &fakeEventStore{}
	svc := NewTaskEventService(store, &fakeSessionLookup{}, nil)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if err := svc.RecordEvent(ctx, TaskEventRecord{
			TaskUID: evUID, SessionID: evSessionID, Seq: int64(i), Type: "thinking",
		}); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}

	evs, err := svc.ListEvents(ctx, evSessionID, 1, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("拿到 %d 条,期望 2 条(afterID=1 之后的两条)", len(evs))
	}
	if evs[0].ID >= evs[1].ID {
		t.Errorf("没按 id 升序: %d 然后 %d", evs[0].ID, evs[1].ID)
	}
}

// 依赖没接上时返回错误,不许 panic。
//
// "接口存在但没接好"必须和"接口不存在"分开:500 说接线坏了,
// 404 说路由没了,混在一起排查会走错方向。
func TestTaskEventService_NilDependenciesFailClosed(t *testing.T) {
	svc := NewTaskEventService(nil, nil, nil)
	ctx := context.Background()

	if err := svc.RecordEvent(ctx, TaskEventRecord{
		TaskUID: evUID, SessionID: evSessionID, Seq: 1, Type: "thinking",
	}); err == nil {
		t.Error("事件表没接上却返回成功")
	}
	if _, err := svc.ListEvents(ctx, evSessionID, 0, 10); err == nil {
		t.Error("事件表没接上却读出了东西")
	}
	if err := svc.MustOwnSession(ctx, 1, evSessionID); err == nil {
		t.Error("会话仓库没接上却校验通过")
	}
}

// ---------------------------------------------------------------------------
// 接真库跑一遍
// ---------------------------------------------------------------------------

// TestTaskEventHandler_EndToEnd_RecordsEvents 接真库跑一遍:
// 事件处理器 → 真的 TaskEventService → 真的 task_events 表。
//
// 上面那些用的是内存实现,验的是逻辑;这条验的是真表真列真索引。
func TestTaskEventHandler_EndToEnd_RecordsEvents(t *testing.T) {
	r := withTx(t)
	// task_events 表由包里的 migrateTestDB 建(和 User / Session / Task 一起),
	// 表结构只有 model 这一处定义 —— 生产库的 AutoMigrate 用的也是同一个模型。
	// 这里不再现建:两处各建一次的话,"测试库里的表结构"和"生产库里的表结构"
	// 可能不一样,而测试照样绿着 —— 那是最坏的一种不一致。
	_, sess, task := seed(t, r)

	notifier := &fakeNotifier{}
	svc := NewTaskEventService(r.TaskEvent, r.Session, notifier)
	w := newFakeStatusWriter()
	h := NewTaskEventHandler(w, svc)

	ctx := context.Background()
	for _, ev := range []agentclient.Event{
		{TaskUID: task.TaskUID, SessionID: sess.SessionID, Type: agentclient.EventStarted, Seq: 1},
		{TaskUID: task.TaskUID, SessionID: sess.SessionID, Type: agentclient.EventThinking,
			Seq: 2, Payload: "我在想"},
		{TaskUID: task.TaskUID, SessionID: sess.SessionID, Type: agentclient.EventFinished, Seq: 3},
	} {
		if err := h.OnEvent(ctx, ev); err != nil {
			t.Fatalf("OnEvent: %v", err)
		}
	}

	rows, err := r.TaskEvent.ListSince(ctx, sess.SessionID, 0, 10)
	if err != nil {
		t.Fatalf("读事件: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("事件表里有 %d 行,期望 3 行", len(rows))
	}
	for i, row := range rows {
		if row.SessionID != sess.SessionID {
			t.Errorf("第 %d 行的 session_id = %q,期望 %q", i, row.SessionID, sess.SessionID)
		}
		if row.TaskUID != task.TaskUID {
			t.Errorf("第 %d 行的 task_uid = %q,期望 %q", i, row.TaskUID, task.TaskUID)
		}
	}
	if rows[1].Payload != "我在想" {
		t.Errorf("进度事件的 payload = %q,期望 %q", rows[1].Payload, "我在想")
	}
	if len(notifier.sessions) != 3 {
		t.Errorf("通知了 %d 次,期望 3 次", len(notifier.sessions))
	}
}
