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
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/breaker"
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

// healthTimeout 探活等回应的时间。
//
// 必须很短:调用方是 /health,而 /health 自己是被 k8s 探针、负载均衡器
// 按秒轮询的。这里等 10 秒的话,Agent 一挂,每个探针请求都占住一个
// HTTP 处理协程 10 秒 —— 探活反倒成了压垮自己的那根稻草。
const healthTimeout = 2 * time.Second

// keepalive 的三个参数是**跟着服务端默认策略定的**,不是随手挑的间隔。
// 调小任何一个之前先往下读,不然会直接把连接搞断。
const (
	// keepaliveTime 两次 keepalive ping 的间隔。
	//
	// 5 分钟是服务端默认允许的**下限**:grpc-go 的
	// keepalive.EnforcementPolicy.MinTime 和 grpcio(Python)的
	// grpc.http2.min_ping_interval_without_data_ms 默认都是 5 分钟。
	// 比它小,服务端攒够两次就回一个
	//
	//	GOAWAY / ENHANCE_YOUR_CALM / "too_many_pings"
	//
	// 把整条连接踢掉 —— 在途的事件流跟着断,OnStreamError 会把那些
	// 任务全标成 failed。也就是说这个参数配错了不是"少了点保活",
	// 是会周期性地误杀正在跑的任务。
	keepaliveTime = 5 * time.Minute

	// keepaliveTimeout 发出 ping 之后等回应的时间,超时就认为连接死了。
	keepaliveTimeout = 20 * time.Second

	// keepalivePermitWithoutStream 必须是 false。
	//
	// 服务端那边对应的开关(grpc.keepalive_permit_without_calls)默认也是关的:
	// 没有在途调用还 ping,对面直接当违规计数。而这个客户端大部分时间是空闲的
	// (任务和任务之间没有流),开着它等于持续给服务端送违规。
	//
	// 关掉的代价是空闲连接不再保活,可能被中间的 NAT / LB 静默回收 ——
	// 不要紧:grpc.NewClient 是懒连接,下次调用会自己重连。
	keepalivePermitWithoutStream = false
)

// 想让掉线检测更快(比如 30 秒),**两边都得改**,只改这边必然被踢:
//
//	# Python 侧
//	grpc.server(..., options=[
//	    ("grpc.keepalive_permit_without_calls", 1),
//	    ("grpc.http2.min_ping_interval_without_data_ms", 10000),
//	])

// agentTokenMetadataKey 是共享密钥挂的 metadata 键。
//
// 必须小写:gRPC 会把 metadata 的 key 统一小写,写成 X-Agent-Token 的话
// 本地看着对、到线上就和对面的比较对不上。Agent 那侧写死的也是这个小写形式
// (grpcserver/server.py 的 _AUTH_METADATA_KEY)。
//
// 刻意不复用 authorization:那是用户令牌的地盘,理由同
// middleware.InternalTokenHeader —— 两套凭证混在一个键里,谁顶掉谁很难看出来。
const agentTokenMetadataKey = "x-agent-token"

// staticToken 把共享密钥挂到每一次 RPC 上。
//
// 用 PerRPCCredentials 而不是自己写 interceptor:后者要给 unary 和 stream
// 各写一个,漏掉 stream 的话最危险的那个方法(RunTask)恰好没带凭证 ——
// 而它要到部署时才会暴露,本地起个不鉴权的 Agent 一切正常。
type staticToken struct{ token string }

func (t staticToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{agentTokenMetadataKey: t.token}, nil
}

// RequireTransportSecurity 返回 false:这条连接目前是明文的(见 NewGRPCClient
// 里的 insecure.NewCredentials)。返回 true 的话 grpc-go 会直接拒绝拨号。
//
// 代价要说清楚:明文传输意味着密钥在链路上是裸的。所以这套凭证防的是
// "谁都能连上这个端口就能跑任意任务",不是"链路被人监听"。
// 跨主机部署时该走 mTLS 或者把这条链路关进内网/service mesh。
func (t staticToken) RequireTransportSecurity() bool { return false }

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

	// Health 探一次 Agent 活着没有。给 /health 用。
	//
	// 返回 error 表示"问不到"(连不上、超时);返回 (HealthStatus{OK:false}, nil)
	// 表示"问到了,它说自己不好"。这两种要分开 —— 前者是网络/进程层面的事,
	// 后者是 Agent 自己的自检结论(比如模型后端连不上),排查方向完全不同。
	Health(ctx context.Context) (HealthStatus, error)

	// BreakerState 返回熔断器当前状态(closed / open / half_open),
	// 只读,不推进状态机。给 /health 展示用。
	BreakerState() string
}

// HealthStatus 是一次探活的结果。
type HealthStatus struct {
	// OK 是 Agent 自己的自检结论。
	OK bool

	// Version 是 Agent 报的版本号,可能是空串。
	// 有它才能在两边行为对不上时,一眼看出是不是版本没对齐。
	Version string
}

// 编译期钉住:GRPCClient 必须满足 Client。
//
// 生产代码确实会做这个赋值(cmd/server/main.go 把 *GRPCClient 传给
// NewSessionService / NewTaskService),所以漂移了本来也编不过。留着它是为了让
// 错误出现在**正确的位置**:少了这一行,给 Client 加一个方法会让报错落在
// cmd/server 和一堆测试文件里,而问题其实在这个包 —— 是它没实现完。
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

	// EventCompacting 是"开始压缩上下文",EventCompacted 是"压缩结束"。
	//
	// ★ 这两个值以前不存在 —— Agent 那侧把压缩事件按 EVENT_TYPE_UNSPECIFIED 发,
	//   于是每一次压缩,这边都会收到一条"未知类型的事件"告警。而那条告警的意义
	//   是"对面升级了、我们没跟上",拿它报"对面没跟上"的日常事件,信号就被稀释了:
	//   真漏了某个新事件时,这条告警已经淹没在压缩告警里。
	//
	// ★ 两个都是**进度**事件,不是终态。 IsTerminal 对它们返回 false。
	//
	// ★ compacted 的 payload 里有 ok 字段: compacted{ok:false, reason:"noop"}
	//   表示这轮**没有**压成(没什么可压),不是失败。具体语义见 Agent 那侧
	//   的 _maybe_compress。
	EventCompacting EventType = "compacting"
	EventCompacted  EventType = "compacted"
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

	// OnStreamError 流异常结束、且重试已经用尽。正常结束(EOF)不会走到这里。
	//
	// 注意它的语义是"这个任务没救了,收尾吧",不是"某一次流断了" ——
	// 中途断掉但重发成功的任务不会走到这里。见 runTask。
	OnStreamError(ctx context.Context, taskUID string, err error)

	// ShouldRetry 在流中断、agentclient 准备重发之前问一次。返回 false 就地收尾。
	//
	// **重发的机制在 agentclient,要不要重发是业务决定。** 这个包不读库,
	// 判断不了任务是不是已经被用户取消了(取消是"先落库、再通知 Agent",
	// 库里已经是 canceled 了),也判断不了回收器是不是已经先把它收了尾。
	// 这两种情况下重发都是白烧一轮 LLM:跑出来的结果撞上 cas 也写不进去。
	//
	// attempt 从 0 起,是刚刚失败的那一次。
	ShouldRetry(ctx context.Context, taskUID string, attempt int) bool
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
	// 压缩进度。proto 里从 EVENT_TYPE_UNSPECIFIED 挪到了专属枚举(7/8)。
	agentv1.EventType_EVENT_TYPE_COMPACTING: EventCompacting,
	agentv1.EventType_EVENT_TYPE_COMPACTED:  EventCompacted,
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
	breaker     *breaker.Breaker

	// retryBackoff 是第一次重发前等的时间,之后按 attempt 翻倍。
	// 做成字段而不是直接用常量,是为了测试不用真睡几秒。
	retryBackoff time.Duration

	wg      sync.WaitGroup
	closing atomic.Bool
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

	// HalfOpenMax 固定 1,不做成配置项:半开期放出去的每一个请求
	// 都可能打在一个还没缓过来的 Agent 上,而这里一个"请求"就是一整个任务。
	br, err := breaker.New(breaker.Config{
		FailureThreshold: cfg.BreakerThreshold,
		Cooldown:         cfg.BreakerCooldown,
		HalfOpenMax:      1,
	})
	if err != nil {
		return nil, err
	}

	// 没配 token 就不挂凭证。这不是"降级放行":Agent 那侧只有绑在回环地址上
	// 才允许不鉴权,配错的后果是它拒绝启动,不会变成一个静默没防护的服务。
	dialOpts := []grpc.DialOption{
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
			Time:                keepaliveTime,
			Timeout:             keepaliveTimeout,
			PermitWithoutStream: keepalivePermitWithoutStream,
		}),
	}
	if cfg.Token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(staticToken{cfg.Token}))
	}

	conn, err := grpc.NewClient(cfg.Addr, dialOpts...)
	if err != nil {
		return nil, err
	}
	return &GRPCClient{
		conn:        conn,
		cli:         agentv1.NewAgentServiceClient(conn),
		handler:     handler,
		taskTimeout: taskTimeout,
		breaker:     br,

		retryBackoff: retryBaseBackoff,
	}, nil
}

// StartTask 让 Agent 开跑,然后在后台消费它推回来的事件流。
//
// 这个方法是非阻塞的:建流成功就返回,事件在另一个协程里收。
// 熔断器怎么记这条路径的结果,值得单独说一句:
//
// RunTask 是 server-streaming,`c.cli.RunTask(...)` **不等对面回话就返回**
// (gRPC 只是把请求排上,头都还没发)。所以它返回 nil 完全不能说明 Agent 活着 ——
// Agent 进程没了的时候它照样返回 nil,错误要到第一次 Recv() 才浮出来。
//
// 因此这里的 Record 交给 consume 去做:第一条事件收到了才算成功,
// 流一开头就报错才算失败。在这儿按 RunTask 的返回值记的话,
// 熔断器会永远认为下游是好的,等于没装。
func (c *GRPCClient) StartTask(ctx context.Context, req StartTaskReq) error {
	if c.closing.Load() {
		return ErrClientClosing
	}
	if !c.breaker.Allow() {
		// 快速失败,不去撞一个已知连不上的 Agent。
		// 包成 ErrUnavailable 是为了让上层只认一个错误 —— 对调用方来说
		// "熔断了"和"连不上"要做的事完全一样:别等了,标记失败。
		return fmt.Errorf("派发任务: %w(%w)", ErrUnavailable, breaker.ErrOpen)
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
		// 这个分支基本只在参数不合法或连接已经关掉时触发(见上面的说明),
		// 但既然拿了名额就得还回去,否则半开期的探针会漏掉一个,
		// 熔断器再也回不到 closed。
		tErr := translateGRPCError("派发任务", err)
		c.breaker.Record(!countsAsUnhealthy(tErr))
		return tErr
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
		c.runTask(streamCtx, req, stream)
	}()
	return nil
}

// consume 把流里的事件一条条交给 handler,直到流结束或出错。
//
// 它同时负责给熔断器一次(且仅一次)汇报,理由见 StartTask 上面那段注释。
func (c *GRPCClient) consume(ctx context.Context, taskUID string,
	stream agentv1.AgentService_RunTaskClient) streamOutcome {

	// gotEvent 是重试判据的全部依据,见 runTask。
	gotEvent := false

	// 一次 Allow 对应一次 Record,多记会把半开期的计数搞乱,
	// 少记会让探针名额漏掉。用一个标志钉死"只记第一次结果"。
	recorded := false
	record := func(healthy bool) {
		if recorded {
			return
		}
		recorded = true
		c.breaker.Record(healthy)
	}
	// 兜底:走到这儿还没记过,说明流是正常结束的(EOF),对面是活的。
	defer func() { record(true) }()

	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return streamOutcome{gotEvent: gotEvent}
		}
		if err != nil {
			// 翻译一次再往外交:上报方(runTask)会把它交给 OnStreamError,
			// 而实现方(service)拿到的不该是一串带内网地址的 gRPC 原文。
			//
			// 这里**不直接调 OnStreamError**:要不要重发是 runTask 的决定,
			// 提前上报的话任务就成了终态,重发跑出来的事件会撞上 cas 的
			// from=[pending,running] 一条都匹配不上,被当成迟到回调静静丢掉。
			tErr := translateGRPCError("接收任务事件流", err)
			record(!countsAsUnhealthy(tErr))
			return streamOutcome{err: tErr, gotEvent: gotEvent}
		}
		// 收到第一条事件 = 对面确实在干活,这是唯一可信的"健康"证据。
		record(true)
		gotEvent = true
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
	if !c.breaker.Allow() {
		return fmt.Errorf("取消任务: %w(%w)", ErrUnavailable, breaker.ErrOpen)
	}

	reqCtx, cancel := context.WithTimeout(ctx, cancelTimeout)
	defer cancel()

	res, err := c.cli.CancelTask(reqCtx, &agentv1.CancelTaskRequest{
		TaskUid: req.TaskUID,
		Reason:  req.Reason,
		TraceId: req.TraceID,
	})
	if err != nil {
		tErr := translateGRPCError("取消任务", err)
		c.breaker.Record(!countsAsUnhealthy(tErr))
		return tErr
	}
	// 一元调用不像 RunTask,拿到响应就是真的通了。
	c.breaker.Record(true)

	if !res.Cancelled {
		// 带上 Message —— 那是 Agent 唯一能说明"为什么取消不了"的地方。
		// 这条只进日志(见 service.TaskService.notifyCancel),不回显给用户。
		return fmt.Errorf("%w: %s", ErrCancelTask, res.Message)
	}
	return nil
}

// Health 问一次 Agent 活着没有。
//
// **它刻意不走熔断器**,既不占名额也不汇报结果:
//
//   - 不 Allow:熔断打开时探活更该照常发出去,否则一旦断开就再也
//     看不到"对面回来了"。半开期那一个宝贵的探针名额也不该被监控轮询吃掉。
//   - 不 Record:/health 通常被按秒轮询,让它去驱动熔断器,等于把
//     熔断器的状态机交给了监控系统的采集频率 —— 真实业务流量反而说了不算。
//
// 熔断器的状态由 BreakerState 单独报,两个字段各说各的:
// agent=up 而 breaker=open 是一个完全合理(而且很有用)的组合,
// 意思是"对面已经恢复了,业务流量再过几秒也会恢复"。
func (c *GRPCClient) Health(ctx context.Context) (HealthStatus, error) {
	reqCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	res, err := c.cli.Health(reqCtx, &agentv1.HealthRequest{})
	if err != nil {
		return HealthStatus{}, translateGRPCError("探测执行器", err)
	}
	return HealthStatus{OK: res.Ok, Version: res.Version}, nil
}

// BreakerState 返回熔断器当前状态,只读。
func (c *GRPCClient) BreakerState() string { return c.breaker.State().String() }

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
