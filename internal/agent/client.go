package agentclient

import (
	"context"
	"errors"
	"io"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"
	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

var (
	ErrClientClosing = errors.New("client connection is closing")
	ErrCancelTask    = errors.New("cancel task failed")
)

type GRPCClient struct {
	conn    *grpc.ClientConn
	cli     agentv1.AgentServiceClient
	handler EventHandler
	wg      sync.WaitGroup
	closing atomic.Bool
}

type EventHandler interface {
	OnEvent(ctx context.Context, taskUID string, ev agentv1.EventType) error
	OnStreamError(ctx context.Context, taskUID string, err error)
}

type Client interface {
	StartTask(ctx context.Context, req StartTaskReq) error
	CancelTask(ctx context.Context, request *agentv1.CancelTaskRequest) error
}

func (c *GRPCClient) StartTask(ctx context.Context, req StartTaskReq) error {
	if c.closing.Load() {
		return ErrClientClosing
	}

	streamCtx, cancel := context.WithTimeout(ctx, time.Minute)

	stream, err := c.cli.RunTask(streamCtx, &agentv1.RunTaskRequest{
		TaskUid:   req.TaskUID,
		SessionId: req.SessionID,
		Input:     req.Input,
		TraceId:   req.TraceID,
	})

	if err != nil {
		cancel()
		return err
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[panic] stream task=%s: %v\n%s",
					req.TaskUID, r, debug.Stack())
			}
		}()
		c.consume(streamCtx, req.TaskUID, stream)
	}()
	return nil
}

func (c *GRPCClient) consume(ctx context.Context, taskUID string,
	stream agentv1.AgentService_RunTaskClient) {

	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			c.handler.OnStreamError(ctx, taskUID, err)
			return
		}
		if err := c.handler.OnEvent(ctx, taskUID, ev.Type); err != nil {
			log.Printf("[EVENT] handle failed task=%s seq=%d: %v", taskUID, ev.Seq, err)
		}
	}
}

func (c *GRPCClient) CancelTask(ctx context.Context, request *agentv1.CancelTaskRequest) error {
	reqCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	res, err := c.cli.CancelTask(reqCtx, request)

	if err != nil {
		return err
	}

	if !res.Cancelled {
		return ErrCancelTask
	}

	// 如果远程任务成功取消的话，那么就返回nil
	return nil
}

func (c *GRPCClient) Close() error {
	c.closing.Store(true)
	c.wg.Wait()
	return c.conn.Close()
}

// Conn 返回底层的 gRPC 连接。暴露它是为了让调用方在特殊场景下
// （比如探针）直接拿连接建别的 client,正常业务走 Client 接口就够了。
func (c *GRPCClient) Conn() *grpc.ClientConn {
	return c.conn
}

type StartTaskReq struct {
	TaskUID   string
	SessionID string
	Input     string
	TraceID   string // ← 透传,别断链路
}

func NewGRPCClient(cfg config.Agent, handler EventHandler) (*GRPCClient, error) {
	conn, err := grpc.NewClient(cfg.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{
			"loadBalancingPolicy":"round_robin",
			"methodConfig":[{
				"name":[{"service":"agent.v1.AgentService"}],
				"retryPolicy": {
					"MaxAttempts": 3,
					"InitialBackoff": "0.1s",
					"MaxBackoff": "1s",
					"BackoffMultiplier": 2,
					"retryableStatusCodes":["UNAVAILABLE"]
				}
			}]
		}`),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, err
	}
	cli := agentv1.NewAgentServiceClient(conn)
	return &GRPCClient{
		conn:    conn,
		cli:     cli,
		handler: handler,
		wg:      sync.WaitGroup{},
		closing: atomic.Bool{},
	}, nil
}
