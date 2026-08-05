package agent

import "context"

type Client interface {
	StartTask(ctx context.Context, req StartTaskReq) error
	CancelTask(ctx context.Context, taskUID string) error
}

type StartTaskReq struct {
	TaskUID   string
	SessionID string
	Input     string
	TraceID   string // ← 透传,别断链路
}
