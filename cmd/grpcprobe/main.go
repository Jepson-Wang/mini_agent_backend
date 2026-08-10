package main

import (
	"context"
	"errors"
	"io"
	"log"
	"time"

	agentv1 "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"
	agentclient "github.com/Jepson-Wang/mini_agent_backend/internal/agent"
	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"google.golang.org/grpc/status"
)

// probeHandler 就是最简单的探针 handler,只把收到的事件打出来。
type probeHandler struct{}

func (probeHandler) OnEvent(_ context.Context, taskUID string, ev agentv1.EventType) error {
	log.Printf("[EVENT] task=%s type=%v", taskUID, ev)
	return nil
}

func (probeHandler) OnStreamError(_ context.Context, taskUID string, err error) {
	log.Printf("[STREAM_ERROR] task=%s: %v", taskUID, err)
}

func main() {
	cfg := config.Config{Agent: config.Agent{Addr: "localhost:50051", CallbackSecret: ""}}
	client, err := agentclient.NewGRPCClient(cfg.Agent, probeHandler{})
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer client.Close()

	runTask(client)
}

// runTask 发起一个 RunTask 调用并消费事件流。
// 这里直接拿 GRPCClient 的 conn 建一个 protobuf client 来调,
// 实际业务里是 TaskService 通过 agentclient.Client 接口调 StartTask。
func runTask(c *agentclient.GRPCClient) {
	// 为探针单独建一个 protobuf client,跟 GRPCClient 共享同一条连接。
	cli := agentv1.NewAgentServiceClient(c.Conn())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := cli.RunTask(ctx, &agentv1.RunTaskRequest{
		TaskUid:   "01TESTTASK0000",
		SessionId: "-1TESTSESS000000",
		Input:     "你好",
		TraceId:   "probe-trace-001",
	})
	if err != nil {
		st, _ := status.FromError(err)
		log.Fatalf("RunTask err: code=%s msg=%s", st.Code(), st.Message())
	}

	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			log.Println("Stream ended normally")
			return
		}

		if err != nil {
			st, _ := status.FromError(err)
			log.Fatalf("RunTask recv failed: code=%s msg=%s", st.Code(), st.Message())
		}
		log.Printf("event seq=%d type=%s payload=%s", ev.Seq, ev.Type, ev.Payload)
	}
}
