package sse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// wakeChannel 是唤醒频道名。
	//
	// 带上用途前缀:同一个 Redis 实例可能和限流、吊销名单共用,
	// 不加前缀会和别人的频道撞上。
	wakeChannel = "sse:wake"

	// publishTimeout 是单次 PUBLISH 的上限。
	//
	// Publish 在事件发布路径上(Agent 的事件流那个 goroutine),
	// 那条链路还连着 Agent 的 gRPC 长流,卡不得。
	// 所以即使调用方给了一个没有超时的 ctx,这里也要兜一个上限。
	//
	// ## ★ 这个上限必须落在 socket 上,不能只靠 ctx
	//
	// 直觉写法是"给 ctx 加个超时就行",但那**是假的**。go-redis v9 的命令路径是:
	//
	//   cn.WithReader(c.context(ctx), c.opt.ReadTimeout, ...)
	//
	// 而 `baseClient.context()`(redis.go:1892)在 `ContextTimeoutEnabled=false`
	// (默认值)时把调用方的 ctx **换成 context.Background()**。
	// 于是 ctx 上的 deadline 根本到不了 socket。
	//
	// 剩下的只有 `c.opt.ReadTimeout`。而 `redisx.NewPubSub` 那个客户端的
	// ReadTimeout 是 -1,`options.init()` 会把它归一成 0(见 options.go:735),
	// 0 的语义是"没有超时"—— `pool.Conn.deadline(ctx, 0)` 在 ctx 没有 deadline 时
	// 返回 `noDeadline`,也就是**socket 上一个 deadline 都没有**。
	//
	// 后果:Redis 半开(TCP 建立了但对端不回话)时,PUBLISH 永久阻塞,
	// 而它跑在 Agent 事件流那条 goroutine 上 → 事件流停摆 → 任务状态机停摆。
	//
	// 所以真正的修法是**让发布用的客户端本身有界**:
	// NewBus 里 `pub.WithTimeout(publishTimeout)` 把读/写超时都钉成 200ms,
	// `pool.Conn.deadline(ctx, 200ms)` 于是返回 now+200ms,socket 上真的有 deadline。
	// WithTimeout 是克隆一份配置、**共享连接池**,不会多开连接。
	//
	// ctx 上的超时不是没用:它兜的是"从连接池里拿连接"那一段。
	// 两层都要,各管一段。
	publishTimeout = 200 * time.Millisecond

	// subscribeTimeout 是"建立订阅"的上限:等 Redis 回应 SUBSCRIBE 确认。
	//
	// 只覆盖建立订阅这一步,不影响订阅之后的长期等待 ——
	// 订阅建立之后是阻塞读,靠 ctx 取消和 Close 收尾。
	subscribeTimeout = 5 * time.Second

	// reconnectGap 是订阅断开后重连的间隔。
	//
	// Redis 挂掉的时候不能变成疯狂重连 —— 那会把 Redis 刚恢复的瞬间
	// 再压垮一次。
	reconnectGap = time.Second
)

// Bus 是跨副本唤醒总线(Redis Pub/Sub)。
//
// ## 它传的是信号,不是事件
//
// 消息体里只有一个 sessionID,意思是"这个会话有更新了,去库里读"。
// 事件本身在数据库里,所以**信号丢了不丢数据**:丢的只是实时性,
// 订阅方还有定时轮询兜底。
//
// 这条取向是整个 SSE 设计的地基(见 docs/plans/2026-11-13-sse-streaming.md):
// Redis 是加速器,不是权威。
//
// ## nil 安全
//
// **所有方法都对 nil 接收者安全**:没配 Redis 是合法部署,
// NewBus(nil, nil) 返回 nil,拿到的就是一个"什么都不做"的总线。
// 和 service.NewTaskQuota、pkg/revoke 一个路子:
// nil 安全放在方法里,而不是散落在每个调用点。
type Bus struct {
	// pub 是**发布**用的 Redis 客户端:有界超时。
	//
	// 用具体的 *redis.Client 而不是接口:本包的边界上只需要
	// Publish/Subscribe 两件事,接口反而会把 go-redis 的类型签名
	// (比如 *redis.PubSub)漏到边界上。
	//
	// ★ 它在 NewBus 里被换成了"有界视图"(WithTimeout(publishTimeout)),
	// 原因见下面 publishTimeout 的注释 —— 那不是锦上添花,是必须。
	pub *redis.Client

	// sub 是**订阅**用的 Redis 客户端:无读超时。
	//
	// 订阅建立之后是长期阻塞读,读超时一到期订阅就断了 ——
	// 所以这个客户端必须是无界的(见 redisx.NewPubSub)。
	//
	// 它和 pub 分开是必须的:两者的超时要求正好相反。
	sub *redis.Client

	// ready 在订阅建立后关闭。
	//
	// 给测试用:端到端用例需要确定"订阅已经建起来了"再发信号,
	// 否则测的是调度器的运气。
	ready chan struct{}
	once  sync.Once

	// stopped 在后台循环退出后关闭。
	//
	// 给测试用:要确定性地验证"ctx 取消后 Subscribe 真的退出了",
	// 而不是靠 sleep 猜。
	stopped  chan struct{}
	stopOnce sync.Once

	mu     sync.Mutex
	closed bool
	// done 在 Close 时关闭,让后台循环退出。
	done chan struct{}
}

// NewBus 建一个跨副本唤醒总线。
//
// pub 用来发信号,sub 用来收信号 —— **两个客户端的超时要求正好相反,
// 不能混用**:
//
//   - sub 必须无读超时:订阅建立之后是长期阻塞读,超时一到期订阅就断。
//   - pub 必须有界:Publish 跑在事件发布路径上(Agent 的 gRPC 事件流那个
//     goroutine),Redis 半开一次就会把整条事件流永久卡住。
//
// 任一为 nil 时返回 nil,表示"这个部署不启用 Redis"。
// 返回 nil 而不是返回一个空实现,是为了让调用方一眼看出"没有总线"这件事,
// 也免得每个调用点都要判空。
func NewBus(pub, sub *redis.Client) *Bus {
	if pub == nil || sub == nil {
		return nil
	}
	return &Bus{
		// ★ 这里把发布客户端换成"有界视图",不是随便加的保险:
		//
		//   pub:  pub.WithTimeout(publishTimeout)
		//
		// 原因见 publishTimeout 的注释 —— 靠调用方传对客户端是不行的,
		// 这个不变量必须由总线自己兜住。
		pub:     pub.WithTimeout(publishTimeout),
		sub:     sub,
		ready:   make(chan struct{}),
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Publish 发一条"这个会话有更新了"的唤醒信号。
//
// **它发的是信号,不是事件**:订阅方收到之后要去库里读增量。
// 所以信号丢了不丢数据,只是延迟到下一轮轮询。
//
// 失败不上抛:Redis 挂了不该把写事件的那条路径断掉。
// 事件已经在库里了,下一次信号或下一次轮询都会把它带出去。
func (b *Bus) Publish(ctx context.Context, sessionID string) {
	if b == nil || b.pub == nil {
		return
	}
	if sessionID == "" {
		// 空 session_id 定位不到任何订阅者,发了也是白发。
		// 这只可能是调用方的 bug,记一条让它露出来。
		slog.Warn("发布 SSE 唤醒信号失败:session_id 为空")
		return
	}
	if b.isClosed() {
		return
	}

	payload, err := json.Marshal(wakeMessage{SessionID: sessionID})
	if err != nil {
		// wakeMessage 只有一个 string 字段,理论上不会失败。
		// 真失败了就是程序 bug,记下来。
		slog.Error("编码 SSE 唤醒信号失败", "error", err)
		return
	}

	if ctx == nil {
		ctx = context.Background()
	}
	// 带超时:Redis 卡住时不能把事件发布方一起拖住。
	//
	// 这一层兜的是「从连接池里拿连接」;真正兜住 socket 读写的是
	// NewBus 里给 pub 换上的那个有界视图(见 publishTimeout 的注释)。
	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	if err := b.pub.Publish(pubCtx, wakeChannel, payload).Err(); err != nil {
		// 失败就是失败,调用方负责消化 —— 这里是消化的地方。
		// 记一条 Warn 就够了:信号丢不丢数据,库里的事件还在。
		slog.Warn("发布 SSE 唤醒信号失败", "error", err)
	}
}

// Subscribe 订阅唤醒频道,把收到的信号转成本地 hub 通知。
//
// **它不阻塞调用方**:内部起后台循环,直到 ctx 结束或者 Close 被调用。
// (写成 `go bus.Subscribe(ctx, hub)` 也一样能用。)
//
// Redis 不可用时不会退出,而是退避重连 —— 唤醒总线是加速器,
// 它自己先倒了不该让 SSE 整个不可用(还有定时轮询兜底)。
func (b *Bus) Subscribe(ctx context.Context, h *Hub) {
	if b == nil || b.sub == nil || h == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go b.loop(ctx, h)
}

// Close 停掉后台订阅循环。
//
// **它不关 Redis 客户端** —— 客户端是调用方给的,归调用方所有;
// 关掉的话业务命令也会一起断。
//
// ★ 尤其不能关 b.pub:它是调用方那个客户端的**克隆**(共享同一个连接池),
// 关掉它等于关掉调用方的连接池。
func (b *Bus) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.done)
	return nil
}

// loop 是后台循环:订阅断了就退避重连,直到 ctx 结束或 Close。
func (b *Bus) loop(ctx context.Context, h *Hub) {
	defer b.markStopped()
	for {
		if ctx.Err() != nil {
			return
		}
		err := b.watch(ctx, h)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		default:
		}
		if err != nil {
			slog.Warn("SSE 唤醒订阅断开,准备重连", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		case <-time.After(reconnectGap):
		}
	}
}

// watch 建立一次订阅,把收到的信号转成 hub 通知,直到出错或 ctx 结束。
func (b *Bus) watch(ctx context.Context, h *Hub) error {
	// 建立订阅的所有步骤都用 estCtx 兜一个上限:
	// 建连(HELLO/AUTH 握手)、发 SUBSCRIBE、等确认。
	//
	// 用 Pub/Sub 专用的客户端(ReadTimeout=-1,没有读超时)时,
	// 握手这一段没有客户端级的读超时兜着,ctx 是唯一的界限 ——
	// 所以这里的超时不是可有可无的。
	estCtx, cancel := context.WithTimeout(ctx, subscribeTimeout)
	defer cancel()

	ps := b.sub.Subscribe(estCtx, wakeChannel)
	defer ps.Close()

	// 等订阅确认。这一步同时确定了两件事:
	//   - 订阅建起来了(可以发信号了)
	//   - Redis 连得上(连不上就在这里返回错误,由外层退避重连)
	//
	// 注意 Client.Subscribe 会把建订阅的错误吞掉,
	// 所以错误只能从 Receive 这里冒出来 —— 不能省掉这一次 Receive。
	if _, err := ps.Receive(estCtx); err != nil {
		return fmt.Errorf("建立 SSE 唤醒订阅: %w", err)
	}
	b.markReady()

	ch := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.done:
			return nil
		case msg, ok := <-ch:
			if !ok {
				return errors.New("SSE 唤醒订阅已关闭")
			}
			sessionID, err := decodeWake(msg.Payload)
			if err != nil {
				// 解不出来就不通知 —— 宁可少一次唤醒,
				// 也不能把信号送到错的会话上(那会把别的事件推给用户)。
				slog.Warn("解析 SSE 唤醒信号失败", "error", err)
				continue
			}
			h.Notify(sessionID)
		}
	}
}

// markReady 标记"订阅已建立"。
func (b *Bus) markReady() {
	b.once.Do(func() { close(b.ready) })
}

// markStopped 标记"后台循环已退出"。
func (b *Bus) markStopped() {
	b.stopOnce.Do(func() { close(b.stopped) })
}

// isClosed 报告总线是否已经关闭。
func (b *Bus) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// wakeMessage 是唤醒信号的消息体。
//
// 用 JSON 而不是裸的 sessionID:消息体自描述,以后要加字段
// (比如"这是第几条")不用改协议。版本不一致的时候解不出来的那一方
// 只是丢一次唤醒,不影响正确性 —— 库里才是权威。
type wakeMessage struct {
	SessionID string `json:"session_id"`
}

// decodeWake 解出信号里的 sessionID。
func decodeWake(payload string) (string, error) {
	var m wakeMessage
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return "", fmt.Errorf("解析唤醒信号: %w", err)
	}
	if m.SessionID == "" {
		return "", errors.New("唤醒信号里没有 session_id")
	}
	return m.SessionID, nil
}
