package model

import "time"

// TaskEvent 是一条任务事件:Agent 跑任务的过程中推上来的每一条消息。
//
// 这张表是 SSE 流式输出的**权威**。事件先落库,唤醒信号只负责"有更新了",
// 订阅方按 id 递增读 —— 信号丢了只是延迟,不会丢事件
// (见 docs/plans/2026-11-13-sse-streaming.md 的决策 1)。
//
// 在此之前 thinking / tool_call / tool_result 这三类进度事件**只进日志**,
// 前端一个字都看不到。
//
// **不存 user_id**:归属校验走 session_id → sessions.user_id 就够了。
// 少一列、少一个可能填错的字段;SSE 是会话级订阅,按 user 查事件这条路用不上。
type TaskEvent struct {
	// ID 是全局单调的自增主键,也是 SSE 帧里的 id 行。
	//
	// 选它当增量游标是因为它跨副本可比(不同副本写同一张表)、
	// 断线重连时 Last-Event-ID 直接用它续传。
	ID int64 `gorm:"primaryKey;autoIncrement"`

	// TaskUID 是这条事件属于哪个任务。
	TaskUID string `gorm:"type:char(26);not null;index:idx_task_events_task_seq,priority:1"`

	// SessionID 是这条事件属于哪个会话。SSE 按会话订阅,增量读也按它过滤。
	SessionID string `gorm:"type:char(26);not null;index:idx_task_events_session_id,priority:1"`

	// Seq 是任务内的序号,和 gRPC 事件的 seq 对齐,调试时能对上 Agent 侧的原序列。
	//
	// **排序用 ID 不用 Seq**:Seq 只在单个任务内有序,而 SSE 的增量游标要跨任务、
	// 跨会话。要留意的是 Seq 不唯一 —— 事件流断了会原样重发,Agent 那边的 seq
	// 每次从 1 重新数,同一个 (task_uid, seq) 会出现两次,所以
	// idx_task_events_task_seq **不能**是唯一索引。
	Seq int64 `gorm:"not null"`

	// Type 是事件类型:started / thinking / tool_call / tool_result / finished / error ...
	//
	// 长度跟着 agentclient.EventType 走(varchar(16) 装得下最长的 tool_result)。
	Type string `gorm:"type:varchar(16);not null"`

	// Payload 是事件内容(截断后)。
	//
	// text 在数据库这边没有上限,而这一列会原样推给浏览器 ——
	// 写入方必须限长,见 service.maxEventPayload。
	Payload string `gorm:"type:text;not null"`

	CreatedAt time.Time `gorm:"not null;default:now();index:idx_task_events_session_id,priority:2"`
}

func (TaskEvent) TableName() string { return "task_events" }
