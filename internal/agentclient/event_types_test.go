package agentclient

import (
	"testing"
	"time"

	agentv1 "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"
)

// TestToEvent_KnownTypes 钉住"proto 枚举 → 本地 EventType"的映射。
//
// ★ 这条映射是**两侧契约的唯一翻译点**。 Agent 那侧把压缩事件按
//
//	EVENT_TYPE_UNSPECIFIED 发的时候,这边每一次压缩都会落进 EventUnknown,
//	于是 consume 里那条"收到未知类型的事件"告警每次压缩都响 —— 而那条告警的
//	本意是"对面升级了、我们没跟上",用它报日常事件就把信号稀释了。
//
// 逐个列出来而不是用循环遍历 eventTypes: 少写一个 case 时,遍历会静默通过,
// 而"少登记一个枚举"恰恰是这个映射最可能的错法。
func TestToEvent_KnownTypes(t *testing.T) {
	cases := []struct {
		name string
		in   agentv1.EventType
		want EventType
	}{
		{"started", agentv1.EventType_EVENT_TYPE_STARTED, EventStarted},
		{"thinking", agentv1.EventType_EVENT_TYPE_THINKING, EventThinking},
		{"tool_call", agentv1.EventType_EVENT_TYPE_TOOL_CALL, EventToolCall},
		{"tool_result", agentv1.EventType_EVENT_TYPE_TOOL_RESULT, EventToolResult},
		{"finished", agentv1.EventType_EVENT_TYPE_FINISHED, EventFinished},
		{"error", agentv1.EventType_EVENT_TYPE_ERROR, EventError},
		// 压缩进度:以前按 UNSPECIFIED 发,现在有专属枚举了。
		{"compacting", agentv1.EventType_EVENT_TYPE_COMPACTING, EventCompacting},
		{"compacted", agentv1.EventType_EVENT_TYPE_COMPACTED, EventCompacted},
	}
	for _, c := range cases {
		got := toEvent(&agentv1.TaskEvent{Type: c.in}, consumeSessionID)
		if got.Type != c.want {
			t.Errorf("%s: 翻成了 %q,期望 %q", c.name, got.Type, c.want)
		}
	}
}

// TestToEvent_CompactingIsNotUnknown 是上面那条的核心回归。
//
// 单独拎出来是因为它挡的是一条**告警被稀释**的退化:类型翻对了,
// consume 里就不会再为每次压缩打"未知类型"的告警。
func TestToEvent_CompactingIsNotUnknown(t *testing.T) {
	for _, typ := range []agentv1.EventType{
		agentv1.EventType_EVENT_TYPE_COMPACTING,
		agentv1.EventType_EVENT_TYPE_COMPACTED,
	} {
		got := toEvent(&agentv1.TaskEvent{Type: typ}, consumeSessionID)
		if got.Type == EventUnknown {
			t.Errorf("%s 翻成了 EventUnknown —— Go 侧没跟上新枚举", typ)
		}
	}
}

// TestToEvent_UnknownProtoValueDegradesToUnknown 守住降级方向。
//
// 老客户端遇到不认识的值必须落进 EventUnknown,**不能**被当成别的类型 ——
// 误认成 finished 会让任务被判成功,那是最糟的错法。
func TestToEvent_UnknownProtoValueDegradesToUnknown(t *testing.T) {
	got := toEvent(&agentv1.TaskEvent{Type: agentv1.EventType(9999)}, consumeSessionID)
	if got.Type != EventUnknown {
		t.Errorf("不认识的枚举值翻成了 %q,期望 %q", got.Type, EventUnknown)
	}
}

// TestEventType_IsTerminal 压缩事件不是终态。
//
// 判错的后果不对称:把压缩当成终态,后面真正的 finished 就收不到了。
func TestEventType_IsTerminal(t *testing.T) {
	terminal := map[EventType]bool{
		EventFinished: true,
		EventError:    true,
	}
	for _, typ := range []EventType{
		EventStarted, EventThinking, EventToolCall, EventToolResult,
		EventCompacting, EventCompacted, EventUnknown,
	} {
		if got := typ.IsTerminal(); got != terminal[typ] {
			t.Errorf("%q.IsTerminal() = %v,期望 %v", typ, got, terminal[typ])
		}
	}
}

// TestToEvent_KeepsPayloadAndTimestamp 翻译只换类型,不改别的。
func TestToEvent_KeepsPayloadAndTimestamp(t *testing.T) {
	ts := time.UnixMilli(1700000000123)
	got := toEvent(&agentv1.TaskEvent{
		TaskUid:   "t1",
		Seq:       7,
		Type:      agentv1.EventType_EVENT_TYPE_COMPACTED,
		Payload:   `{"ok":true,"boundary":42}`,
		Timestamp: ts.UnixMilli(),
	}, consumeSessionID)

	if got.Type != EventCompacted {
		t.Errorf("类型 = %q,期望 %q", got.Type, EventCompacted)
	}
	if got.TaskUID != "t1" || got.Seq != 7 {
		t.Errorf("task_uid/seq 被翻译弄丢了: %q / %d", got.TaskUID, got.Seq)
	}
	if got.Payload != `{"ok":true,"boundary":42}` {
		t.Errorf("payload 被改了: %q", got.Payload)
	}
	if !got.Timestamp.Equal(ts) {
		t.Errorf("timestamp = %v,期望 %v", got.Timestamp, ts)
	}
	// session_id 是从请求带上来的,翻译不许把它弄丢 ——
	// 事件表按它索引,丢了这条事件就谁也读不到。
	if got.SessionID != consumeSessionID {
		t.Errorf("session_id = %q,期望 %q", got.SessionID, consumeSessionID)
	}
}
