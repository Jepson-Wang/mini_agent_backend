package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// maxEventErrMsg 从事件里取失败原因时的截断长度。
//
// 必须截:这个值会原样落进 tasks.error_msg,而那是一列 text,
// 数据库自己不设上限,而且它还会跟着任务详情接口发回给用户。
// HTTP 回调那条路由 dto.TaskCallbackReq 的 max=2000 挡着,
// gRPC 这条路没有 binding tag,只能在这儿挡。两边数字保持一致。
const maxEventErrMsg = 2000

// maxEventOutput 最终答复的截断长度。
//
// 比 error_msg 宽得多:这是给用户看的正文,截短了就没法用了。
// 但仍然必须有上限 —— tasks.output 是一列 text,数据库自己不设限,
// 而它会跟着任务详情原样发回给用户。
const maxEventOutput = 64 * 1024

// finishedPayload 是 finished 事件 payload 的形状,由 Agent 那边拼出来
// (grpcserver/server.py 里的 json.dumps(out))。
//
// 只解出要用的那个字段。多余的键(completed / used_turns)留在 JSON 里
// 不管 —— 对面加字段不该让这边解析失败。
type finishedPayload struct {
	FinalResponse string `json:"final_response"`
}

// outputFromPayload 从 finished 事件里取出最终答复。
//
// 解不出来时返回 nil 并记一条 Warn,**不让整条事件失败**:状态是
// "成功"这件事比正文更要紧,把它一起丢掉的话任务会永远停在 running,
// 而那是个比"答案没显示出来"糟糕得多的结局。
//
// 也不把原始 payload 当答案兜底 —— 那样用户会看到一坨 JSON。
func outputFromPayload(payload, taskUID, traceID string) *string {
	if strings.TrimSpace(payload) == "" {
		return nil
	}
	var p finishedPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		slog.Warn("finished 事件的 payload 不是预期的 JSON,答复将不落库",
			"task_uid", taskUID, "trace_id", traceID, "error", err)
		return nil
	}
	return truncate(p.FinalResponse, maxEventOutput)
}

// taskStatusWriter 是事件处理器需要的能力,生产实现是 *TaskService。
//
// 定成接口是为了打破一个构造环:TaskService 要 agentclient.Client,
// 客户端要 EventHandler,而 EventHandler 又要回头调 TaskService。
// 见 cmd/server/main.go 里的接线注释。
type taskStatusWriter interface {
	HandleCallback(ctx context.Context, taskUID, status string, res CallbackResult) (bool, error)

	// Heartbeat 给正在跑的任务续一次命,防止回收器把它当成卡住的。
	// 有节流,可以对每条进度事件都调。
	Heartbeat(ctx context.Context, taskUID string) error

	// IsRunning 给重发那条路判断任务还在不在跑,见 TaskEventHandler.ShouldRetry。
	IsRunning(ctx context.Context, taskUID string) (bool, error)
}

// TaskEventHandler 把 Agent 推上来的事件流翻成任务状态。
//
// 它实现 agentclient.EventHandler。所有状态写入都过 HandleCallback,
// 不直接碰仓库层 —— 那里面有三件不能绕开的事:canceled 不许由 Agent 写、
// cas 保证终态不被翻回去、没生效的回调要留日志。
//
// 这是**同步 Agent** 下的接线:一次 RunTask 调用从头跑到尾,
// 事件流就是任务结果的权威来源。流断了就没有第二个人会来报结果了,
// 所以 OnStreamError 必须给任务收尾,否则它会永远停在 running。
type TaskEventHandler struct {
	tasks taskStatusWriter

	// events 把每一条事件记进 task_events 表。
	//
	// **加这个依赖不会破坏构造环。** 环是
	// TaskService → agentclient.Client → EventHandler → TaskService,
	// 破在"事件处理器只用得上 HandleCallback,而那条路径根本不碰 Agent 客户端"。
	// 事件记录器只写库,既不碰 Agent 客户端,也不反向依赖 TaskService ——
	// 它要的只是"往表里写一行"和"通知一下订阅方"。
	//
	// 在此之前 thinking / tool_call / tool_result 这些进度事件**只进日志**,
	// 前端一个字都看不到。事件表是 SSE 的权威,见 service.TaskEventService。
	events TaskEventRecorder
}

// NewTaskEventHandler 组装事件处理器。
//
// events 传 nil 表示这个部署不记事件(比如事件表还没迁移):
// 事件流照常工作,状态照常写,只是前端看不到进度。
// **不因为它报错** —— 事件记录是给前端看的附加能力,不是任务执行的前提。
func NewTaskEventHandler(tasks taskStatusWriter, events TaskEventRecorder) *TaskEventHandler {
	return &TaskEventHandler{tasks: tasks, events: events}
}

// eventStatus 是「会改变任务状态的事件」到目标状态的映射。
//
// 不在这张表里的事件(thinking / tool_call / tool_result / unknown)
// 是进度,不是状态:它们只记日志,不写库。
//
// 注意这里没有任何一项映射到 model.TaskCanceled ——
// 取消只能由用户发起,HandleCallback 那边还会再挡一次。
var eventStatus = map[agentclient.EventType]string{
	agentclient.EventStarted:  model.TaskRunning,
	agentclient.EventFinished: model.TaskSucceeded,
	agentclient.EventError:    model.TaskFailed,
}

// OnEvent 消费一条事件。
//
// 返回错误只会被 agentclient 记一条日志,流不会中断 ——
// 一条事件写库失败不该让后面的事件(尤其是 finished)全都收不到。
func (h *TaskEventHandler) OnEvent(ctx context.Context, ev agentclient.Event) error {
	traceID := trace.FromContext(ctx)

	// 事件先落库,再处理状态。
	//
	// 顺序反过来(先处理状态、状态处理失败就不记了)的话,一条 finished
	// 写库失败就等于这条事件彻底没了 —— 而事件表是 SSE 的**权威**,
	// 少了它前端就永远看不到那次运行到底发生了什么。
	//
	// 记录失败**不往上抛**:这条路径上真正要紧的是别让后面的事件收不到。
	// 记一条 Warn 就继续,符合"一个错误只上报一次"。
	h.record(ctx, ev, traceID)

	status, ok := eventStatus[ev.Type]
	if !ok {
		// 进度事件。它不改状态,但内容已经落进事件表了 ——
		// 前端能靠它看到"Agent 此刻在想什么、调了什么工具"。
		slog.Info("任务进度",
			"task_uid", ev.TaskUID, "seq", ev.Seq,
			"type", string(ev.Type), "trace_id", traceID)

		// 进度事件不改状态,但它是"这任务还活着"的唯一证据 ——
		// 一个跑二十分钟的任务,中间只有 thinking / tool_call,
		// 不续命的话回收器会把它当成卡住的收掉。
		//
		// 心跳失败不往上抛:上层拿到 error 只会记一条日志,而这条路径上
		// 真正要紧的是**别中断事件流**。这里自己消化并记一条 Warn,
		// 符合"一个错误只上报一次"。
		if err := h.tasks.Heartbeat(ctx, ev.TaskUID); err != nil {
			slog.Warn("刷新任务心跳失败",
				"task_uid", ev.TaskUID, "seq", ev.Seq, "trace_id", traceID, "error", err)
		}
		return nil
	}

	// 只有 failed 才带原因、只有 succeeded 才带答复 —— 成功的任务不该留着
	// 上一次的错误信息,失败的任务也不该有正文。
	var res CallbackResult
	switch status {
	case model.TaskFailed:
		res.ErrorMsg = truncate(ev.Payload, maxEventErrMsg)
	case model.TaskSucceeded:
		res.Output = outputFromPayload(ev.Payload, ev.TaskUID, traceID)
	}

	applied, err := h.tasks.HandleCallback(ctx, ev.TaskUID, status, res)
	if err != nil {
		return fmt.Errorf("事件 %s(seq=%d)写库失败: %w", ev.Type, ev.Seq, err)
	}
	if !applied {
		// 不是错误:最常见的是用户抢先取消了,任务已经是终态。
		// HandleCallback 里已经打过一条更详细的日志,这里不重复。
		return nil
	}

	slog.Info("任务状态已更新",
		"task_uid", ev.TaskUID, "status", status,
		"seq", ev.Seq, "trace_id", traceID)
	return nil
}

// record 把一条事件写进事件表。
//
// 失败只记 Warn,不返回错误:事件表是"给前端看进度"的附加能力,
// 它坏了不该把任务的状态一起带坏 —— 状态机走的是 HandleCallback,
// 和这里完全独立。
//
// 顺带说清一件事:**事件表记的是"Agent 实际发了什么",不是"任务最后是什么状态"。**
// 所以用户取消之后迟到的 finished 事件照样会被记下来 —— 那是事实,
// 状态机那边不认它是另一回事(见 HandleCallback 的 cas)。
// 两张表各说各的,才不会出现"日志说成功了、库里说取消了"这种没法解释的组合。
func (h *TaskEventHandler) record(ctx context.Context, ev agentclient.Event, traceID string) {
	if h.events == nil {
		return
	}
	err := h.events.RecordEvent(ctx, TaskEventRecord{
		TaskUID:   ev.TaskUID,
		SessionID: ev.SessionID,
		Seq:       ev.Seq,
		Type:      string(ev.Type),
		// 原始 payload,不做任何加工。事件表是 Agent 发的那一份的忠实副本,
		// 消费方自己按 type 解 —— 两套解析逻辑迟早会漂。
		//
		// 截断在 service.TaskEventService.RecordEvent 里做(那是唯一入口),
		// finished 的 payload 是 JSON {"final_response":...},被截断后可能不再是
		// 合法 JSON:消费方必须容忍解不出来(同 outputFromPayload 那条约定)。
		Payload: ev.Payload,
	})
	if err != nil {
		slog.Warn("记录任务事件失败",
			"task_uid", ev.TaskUID, "seq", ev.Seq,
			"type", string(ev.Type), "trace_id", traceID, "error", err)
	}
}

// OnStreamError 流异常结束。正常结束(EOF)不会走到这里。
//
// 把任务标成 failed 是刻意的:同步 Agent 只有事件流这一条汇报通道,
// 流断了就再没有人会来报结果,不收尾的话任务会永远停在 running,
// 用户界面上转圈转到天荒地老。
//
// 这不会覆盖掉已经跑完的任务:事件是在同一个协程里按序处理的,
// 走到这里说明 finished 要么早就处理完了(那时状态已是终态,
// 这次 cas 落空),要么根本没来过。
func (h *TaskEventHandler) OnStreamError(ctx context.Context, taskUID string, err error) {
	traceID := trace.FromContext(ctx)

	slog.Error("与 Agent 的事件流中断,将任务标记为失败",
		"task_uid", taskUID, "trace_id", traceID, "error", err)

	// 给用户看的原因不带 err 原文 —— 那里面有地址、端口、gRPC 内部细节,
	// 而 error_msg 是会回显给用户的。
	msg := truncate(streamErrorMessage(err), maxEventErrMsg)

	applied, err := h.tasks.HandleCallback(ctx, taskUID, model.TaskFailed,
		CallbackResult{ErrorMsg: msg})
	if err != nil {
		// 这里没有上层可以返回了,只能记下来。
		slog.Error("流中断后给任务收尾也失败了",
			"task_uid", taskUID, "trace_id", traceID, "error", err)
		return
	}
	if !applied {
		slog.Info("流中断时任务已是终态,不用收尾",
			"task_uid", taskUID, "trace_id", traceID)
	}
}

// streamErrorMessage 给流中断挑一句能落进 tasks.error_msg 的原因。
//
// **「并发满了」和「连接断了」必须分开说。** 前者是执行器好得很、只是
// 同时能跑的任务数到顶了,用户过一会儿重发就行;后者是链路或者执行器
// 出了问题。混成一句的话,用户会在最不该放弃的时候放弃 —— 执行器正忙
// 的时候 —— 或者在最该放弃的时候不放弃。
//
// 两句都是固定文案,不带 err 原文:error_msg 会跟着任务详情回显给用户,
// 原文里有内网地址、端口和 gRPC 内部细节。
func streamErrorMessage(err error) string {
	if errors.Is(err, agentclient.ErrAgentBusy) {
		return "执行器并发任务数已达上限,请稍后重试"
	}
	return "与执行器的连接中断"
}

// ShouldRetry 回答 agentclient:这个任务的流断了,还值不值得原样重发。
//
// 判据只有一条 —— 它还在 running 吗。
//
//   - 用户取消了(canceled):重发等于把一个明确叫停的任务重跑一遍。
//   - 回收器先收了尾(failed):重发跑出来的结果撞上 cas 写不进去,纯浪费。
//   - 已经成功了(succeeded):流断在 finished 之后,任务本来就完成了。
//
// **查不到或者库报错时不重发。** 这条路上判错的两个方向代价不对称:
// 不重发只是退回原来的行为(标 failed,用户自己重发一次),看得见;
// 错误地重发是在用户不知情的情况下再烧一轮 LLM,还可能重跑外部工具。
func (h *TaskEventHandler) ShouldRetry(ctx context.Context, taskUID string, attempt int) bool {
	running, err := h.tasks.IsRunning(ctx, taskUID)
	if err != nil {
		slog.Warn("查不到任务状态,不重发",
			"task_uid", taskUID, "trace_id", trace.FromContext(ctx),
			"attempt", attempt+1, "error", err)
		return false
	}
	return running
}

// truncate 去掉首尾空白并按**字符数**截断,空的返回 nil(对应库里的 NULL)。
//
// 按 []rune 而不是 []byte 切:按字节切会把一个汉字劈成两半,
// 落库就是一段乱码。
func truncate(s string, max int) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return &s
}
