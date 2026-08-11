// Package agentclient 是 Agent 那侧 gRPC 服务的客户端。
//
// 本服务是 HTTP server + gRPC **client**,Agent 才是 gRPC server。
// **这个包是整个仓库里唯一允许 import gen/agent/v1 的地方**,
// imports_test.go 里有一条测试盯着这件事。
//
// 进出这个包的都是本地类型:StartTaskReq / CancelTaskReq / Event。
// 让 proto 类型跑到 service 或 handler 里去的话,Agent 那边改一次
// 字段名或者加一个枚举值,业务代码就得跟着动 —— 而那是 wire 格式的
// 实现细节,不该是业务的 API 契约。
package agentclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	ErrClientClosing = errors.New("客户端正在关闭,不再接受新任务")
	ErrCancelTask    = errors.New("Agent 拒绝了取消请求")
)

// cancelTimeout 通知 Agent 取消时等它的时间。
// 取消是 best-effort,不值得等太久 —— 库里已经是 canceled 了。
const cancelTimeout = 10 * time.Second

// closeTimeout 关闭时最多等在途的流收多久。
//
// 不无限等:一条卡住的流不该让整个进程停不下来。等不及就直接关连接,
// 那些流会拿到 Unavailable,和 Agent 挂掉时的表现一样,消费方本来就得处理。
const closeTimeout = 5 * time.Second

// Client 是上层需要的 Agent 能力。
//
// 接口定在这里而不是 service 包,是因为实现和它同包 ——
// service 那侧只认这个接口,测试塞个假的就能跑,不用起 gRPC server。
type Client interface {
	StartTask(ctx context.Context, req StartTaskReq) error
	CancelTask(ctx context.Context, req CancelTaskReq) error
}

// 编译期钉住:GRPCClient 必须满足 Client。
//
// 少了这一行,接口和实现漂移了编译器不会吭声 —— 生产代码目前给
// service 传的是 nil(见 cmd/server/main.go),没有任何一处会做这个赋值。
var _ Client = (*GRPCClient)(nil)

// StartTaskReq 是发起一个任务要的东西。
type StartTaskReq struct {
	TaskUID   string
	SessionID string
	Input     string
	TraceID   string // ← 透传,别断链路
}

// CancelTaskReq 是取消一个任务要的东西。
//
// 用本地结构而不是直接收 *agentv1.CancelTaskRequest:让 service 去拼一个
// protobuf message,等于把 gRPC 的 wire 类型顶到业务层,proto 一改
// handler 和 service 都得跟着动。
type CancelTaskReq struct {
	TaskUID string
	Reason  string
	TraceID string
}

// EventType 是任务事件的类型。
//
// 用自己的字符串常量而不是把 agentv1.EventType 透出去:消费方 switch 的
// 应该是稳定的业务概念,而 proto 那个枚举是 wire 格式的一部分 ——
// 对面加一个值、改一次名字,不该让 service 和 handler 跟着重编译。
type EventType string

const (
	// EventUnknown 是我们还不认识的事件类型。不丢弃是刻意的:
	// payload 还在,记下来才能发现"对面升级了、我们没跟上"。
	EventUnknown EventType = "unknown"

	EventStarted    EventType = "started"
	EventThinking   EventType = "thinking"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventFinished   EventType = "finished"
	EventError      EventType = "error"
)

// IsTerminal 报告这条事件之后还会不会有别的事件。
//
// 未知类型按「不是终态」处理,和 model.IsTerminal 同一个取向:
// 宁可多等一会儿(等流自己 EOF),也不要漏掉后面真正的结果。
func (t EventType) IsTerminal() bool {
	return t == EventFinished || t == EventError
}

// Event 是一条任务事件,由 agentclient 从 gRPC 的 TaskEvent 翻过来。
type Event struct {
	TaskUID string
	Type    EventType

	// Seq 单调递增,从 1 开始,用来排序和去重。
	Seq int64

	// Payload 是这条事件的内容:思考文本、工具调用参数、错误原因……
	// 按 Type 解释。
	Payload string

	// Timestamp 是事件在 Agent 那边发生的时刻。对面没给就是零值。
	Timestamp time.Time
}

// EventHandler 消费任务事件流。实现方负责把事件落到业务上
// (比如把 started / finished 写进 tasks 表)。
//
// 两个方法都在后台协程里被调用,别在里面做阻塞很久的事。
type EventHandler interface {
	// OnEvent 收到一条事件。返回错误只会被记日志,流不会因此中断 ——
	// 一条事件处理失败不该让后面的事件全都收不到。
	OnEvent(ctx context.Context, ev Event) error

	// OnStreamError 流异常结束。正常结束(EOF)不会走到这里。
	OnStreamError(ctx context.Context, taskUID string, err error)
}

// eventTypes 把 proto 的枚举翻成本地类型。
// 对面加了新值而这里没跟上时,toEvent 会给 EventUnknown,不会静默当成别的。
var eventTypes = map[agentv1.EventType]EventType{
	agentv1.EventType_EVENT_TYPE_STARTED:     EventStarted,
	agentv1.EventType_EVENT_TYPE_THINKING:    EventThinking,
	agentv1.EventType_EVENT_TYPE_TOOL_CALL:   EventToolCall,
	agentv1.EventType_EVENT_TYPE_TOOL_RESULT: EventToolResult,
	agentv1.EventType_EVENT_TYPE_FINISHED:    EventFinished,
	agentv1.EventType_EVENT_TYPE_ERROR:       EventError,
}

// toEvent 把一条 protobuf 事件翻成本地事件。
// 这是 proto 类型能走到的最远的地方,再往外一步就是业务代码了。
func toEvent(ev *agentv1.TaskEvent) Event {
	t, ok := eventTypes[ev.Type]
	if !ok {
		t = EventUnknown
	}

	// timestamp 是 Unix 毫秒。没给(0)时留零值,不要翻成 1970 —— 那会让
	// "对面没填"看起来像"一条 55 年前的事件",排查时很误导。
	var ts time.Time
	if ev.Timestamp != 0 {
		ts = time.UnixMilli(ev.Timestamp)
	}

	return Event{
		TaskUID:   ev.TaskUid,
		Type:      t,
		Seq:       ev.Seq,
		Payload:   ev.Payload,
		Timestamp: ts,
	}
}

type GRPCClient struct {
	conn        *grpc.ClientConn
	cli         agentv1.AgentServiceClient
	handler     EventHandler
	taskTimeout time.Duration
	wg          sync.WaitGroup
	closing     atomic.Bool
}

// NewGRPCClient 建一个连 Agent 的客户端。
//
// grpc.NewClient 是懒连接:这里不会真去握手,拨不通要到第一次调用才知道。
// 所以返回 error 只代表参数不对(地址格式、服务配置),不代表 Agent 活着。
//
// handler 为 nil 直接报错,不接受"先跑起来再说" —— 事件全丢的客户端
// 比连不上更难查:任务发出去了,状态永远停在 pending。
func NewGRPCClient(cfg config.Agent, handler EventHandler) (*GRPCClient, error) {
	if cfg.Addr == "" {
		return nil, errors.New("agent.addr 不能为空")
	}
	if handler == nil {
		return nil, errors.New("EventHandler 不能为 nil")
	}
	taskTimeout := cfg.TaskTimeout
	if taskTimeout <= 0 {
		taskTimeout = config.DefaultAgentTaskTimeout
	}

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
	return &GRPCClient{
		conn:        conn,
		cli:         agentv1.NewAgentServiceClient(conn),
		handler:     handler,
		taskTimeout: taskTimeout,
	}, nil
}

// StartTask 让 Agent 开跑,然后在后台消费它推回来的事件流。
//
// 这个方法是非阻塞的:建流成功就返回,事件在另一个协程里收。
func (c *GRPCClient) StartTask(ctx context.Context, req StartTaskReq) error {
	if c.closing.Load() {
		return ErrClientClosing
	}

	// WithoutCancel 是必须的:下面那个协程要活过发起它的那次 HTTP 请求。
	// 直接从 ctx 派生的话,handler 一 return(或者用户把浏览器关了)
	// ctx 就被取消,stream.Recv() 立刻拿到 context canceled,
	// 整条事件流断在开头,任务在库里永远停在 pending。
	//
	// 断开取消信号之后必须自己配一个上限,否则 Agent 那边不吭声时
	// 这个协程会一直挂着,Close() 也就永远等不到它。
	streamCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.taskTimeout)

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
			// handler 是别人写的代码,它 panic 不该把整个进程带走。
			if r := recover(); r != nil {
				slog.Error("消费任务事件流时 panic",
					"task_uid", req.TaskUID, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		c.consume(streamCtx, req.TaskUID, stream)
	}()
	return nil
}

// consume 把流里的事件一条条交给 handler,直到流结束或出错。
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
		// Agent 报的 task_uid 应该和我们发过去的一致,不一致说明对面串了 ——
		// 这时候按我们自己的为准,免得一条事件写到别人的任务上。
		if ev.TaskUid != taskUID {
			slog.Error("事件里的 task_uid 和请求的对不上,丢弃",
				"want", taskUID, "got", ev.TaskUid, "seq", ev.Seq)
			continue
		}

		e := toEvent(ev)
		if e.Type == EventUnknown {
			// 照样往下发:payload 还在,消费方自己决定怎么处理。
			// 但要留个记录,否则"对面升级了、我们没跟上"这件事没人会发现。
			slog.Warn("收到未知类型的事件",
				"task_uid", taskUID, "seq", ev.Seq, "raw_type", ev.Type.String())
		}
		if err := c.handler.OnEvent(ctx, e); err != nil {
			// 处理失败不中断流:后面的事件(尤其是 finished)还得收。
			slog.Warn("处理任务事件失败",
				"task_uid", taskUID, "seq", e.Seq, "type", string(e.Type), "error", err)
		}
	}
}

// CancelTask 告诉 Agent 别跑了。
func (c *GRPCClient) CancelTask(ctx context.Context, req CancelTaskReq) error {
	reqCtx, cancel := context.WithTimeout(ctx, cancelTimeout)
	defer cancel()

	res, err := c.cli.CancelTask(reqCtx, &agentv1.CancelTaskRequest{
		TaskUid: req.TaskUID,
		Reason:  req.Reason,
		TraceId: req.TraceID,
	})
	if err != nil {
		return err
	}
	if !res.Cancelled {
		// 带上 Message —— 那是 Agent 唯一能说明"为什么取消不了"的地方。
		return fmt.Errorf("%w: %s", ErrCancelTask, res.Message)
	}
	return nil
}

// Close 停止接新任务,等在途的事件流收完,然后关连接。
//
// 等待有上限(closeTimeout):一条卡住的流不该让进程停不下来。
func (c *GRPCClient) Close() error {
	c.closing.Store(true)

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(closeTimeout):
		slog.Warn("等待事件流结束超时,直接关闭连接", "timeout", closeTimeout)
	}
	return c.conn.Close()
}
