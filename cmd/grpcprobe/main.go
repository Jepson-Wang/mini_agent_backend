// Command grpcprobe 对着 Agent 打一发 RunTask,把收到的事件打到标准输出。
//
// 用途是确认「backend 能不能连上 Agent、事件流通不通」,和业务无关。
// 地址取 AGENT_ADDR,没设就用 config 的默认值:
//
//	AGENT_ADDR=10.0.0.7:50051 go run ./cmd/grpcprobe
//
// 它走的是和生产代码同一条路径(agentclient.StartTask + EventHandler),
// 不绕过 Client 接口 —— 探针跑通了才说明业务那条路也通。
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
	"google.golang.org/grpc/status"
)

// probeTimeout 等事件流的上限。探针是人在盯着跑的,别让它悬太久。
const probeTimeout = 2 * time.Minute

// fatal 打一条 ERROR 然后退出。和 cmd/server 那个同名函数一个理由:
// 标准 log 包的 Fatalf 转到 slog 之后级别固定是 INFO,启动失败会被告警规则漏掉。
func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}

// probeHandler 把事件原样打出来。done 在流结束时关闭,让 main 知道可以收工了。
type probeHandler struct{ done chan struct{} }

func (h probeHandler) OnEvent(_ context.Context, ev agentclient.Event) error {
	slog.Info("事件",
		"seq", ev.Seq, "type", string(ev.Type),
		"payload", ev.Payload, "at", ev.Timestamp)

	// 终态事件之后不会再有东西了,不用干等到超时。
	if ev.Type.IsTerminal() {
		close(h.done)
	}
	return nil
}

func (h probeHandler) OnStreamError(_ context.Context, taskUID string, err error) {
	st, _ := status.FromError(err)
	slog.Error("流异常结束",
		"task_uid", taskUID, "code", st.Code().String(), "message", st.Message())
	close(h.done)
}

func main() {
	// 探针是给人看的,用 Text 而不是 cmd/server 那份 JSON。
	// 输出走 stderr,这样 stdout 可以留给将来要 pipe 的东西。
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	addr := os.Getenv("AGENT_ADDR")
	if addr == "" {
		addr = "localhost:50051"
	}

	// task_uid / session_id 用真的 ULID:Agent 那边只要校验格式,
	// 随手编的字符串会换来一个和连通性毫无关系的 InvalidArgument,
	// 排查时容易顺着错误方向找半天。
	taskUID, err := ulid.New()
	if err != nil {
		fatal("生成 task_uid 失败", err)
	}
	sessionID, err := ulid.New()
	if err != nil {
		fatal("生成 session_id 失败", err)
	}
	traceID, err := ulid.New()
	if err != nil {
		fatal("生成 trace_id 失败", err)
	}

	h := probeHandler{done: make(chan struct{})}
	client, err := agentclient.NewGRPCClient(config.Agent{
		Addr:        addr,
		TaskTimeout: config.DefaultAgentTaskTimeout,
	}, h)
	if err != nil {
		fatal("建客户端失败", err)
	}

	slog.Info("开始探测", "addr", addr, "task_uid", taskUID, "trace_id", traceID)

	// grpc.NewClient 是懒连接,拨不通要到这里才知道。
	if err := client.StartTask(context.Background(), agentclient.StartTaskReq{
		TaskUID:   taskUID,
		SessionID: sessionID,
		Input:     "你好",
		TraceID:   traceID,
	}); err != nil {
		st, _ := status.FromError(err)
		slog.Error("RunTask 失败", "code", st.Code().String(), "message", st.Message())
		os.Exit(1)
	}

	select {
	case <-h.done:
		slog.Info("流结束")
	case <-time.After(probeTimeout):
		slog.Warn("等超时了,不再等", "timeout", probeTimeout)
	}

	// Close 会等在途的流收完(有上限),所以放在这儿才有意义。
	if err := client.Close(); err != nil {
		slog.Warn("关闭连接失败", "error", err)
	}
}
