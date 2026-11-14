// Package redisx 是 go-redis 之上的一层薄封装,只负责一件事:
// 用**显式、偏紧**的超时和"不重试"的策略建出客户端。
//
// 和 breaker 一样,这个包有自己的 Options,不 import internal/config。
package redisx

import (
	"context"
	"runtime"
	"time"

	"github.com/redis/go-redis/v9"
)

// 各项超时的默认值。
const (
	defaultTimeout = 200 * time.Millisecond
)

// Options 是建客户端的参数。除 Addr 外,零值字段一律换成下面的默认值。
type Options struct {
	Addr     string
	Password string
	DB       int

	DialTimeout, ReadTimeout, WriteTimeout time.Duration
	PoolSize                               int
}

// Client 包一个 *redis.Client。**所有方法都对 nil 接收者安全**:
// "这个部署没有 Redis"是合法选择,nil 安全放在方法里,
// 而不是散落在每个调用点(同 service.NewTaskQuota、service.NewReconciler)。
type Client struct {
	rdb *redis.Client
}

// New 建客户端。Addr 为空返回 (nil, nil),表示不启用 Redis。
//
// 超时和重试必须显式设,不能用 go-redis v9 的默认值:它默认
// MaxRetries=3、读写超时 3s、拨号超时 5s,Redis 卡住时每个请求最多
// 白白多等十来秒,才轮得到中间件的 fail-open(放行)逻辑。
// 一个慢的 fail-open 比没有限流器还糟 —— 一层可选的保护,
// 变成了主路径(每次登录)的延迟。所以:不重试,三个超时默认 200ms。
//
// ★ **这里配不出 go-redis 语义的"无超时"(-1)**:`opt.ReadTimeout <= 0`
// 的兜底会把 -1 和 -2 都改写成 200ms。而 go-redis 的语义里
// (options.go):`-1` = 无超时(永久阻塞),`0` = "未设置" → 用 5s 默认。
// 也就是说在这个函数里,ReadTimeout 想要"关掉"是做不到的 ——
// 写 -1 会被悄悄改掉,而且没有任何迹象。
// 需要一个真的没有读超时的客户端,只能用 NewPubSub。
func New(opt Options) (*Client, error) {
	if opt.Addr == "" {
		return nil, nil
	}
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = defaultTimeout
	}
	if opt.ReadTimeout <= 0 {
		opt.ReadTimeout = defaultTimeout
	}
	if opt.WriteTimeout <= 0 {
		opt.WriteTimeout = defaultTimeout
	}
	if opt.PoolSize <= 0 {
		opt.PoolSize = 10 * runtime.NumCPU()
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:         opt.Addr,
		Password:     opt.Password,
		DB:           opt.DB,
		DialTimeout:  opt.DialTimeout,
		ReadTimeout:  opt.ReadTimeout,
		WriteTimeout: opt.WriteTimeout,
		PoolSize:     opt.PoolSize,
		MaxRetries:   -1, // -1 才是关闭重试;0 会被 go-redis 当成"用默认值 3"(生效后 Options() 里读到的是 0)
		// 拨号层另有一套重试(默认 5 次、间隔 100ms),Redis 挂了时每次调用还要白等约 0.4s;<=0 会被换成 5,所以关闭写 1。
		DialerRetries: 1,
	})
	return &Client{rdb: rdb}, nil
}

// NewPubSub 建一个专门给 Pub/Sub 用的客户端。
//
// Addr 为空返回 nil,表示不启用 Redis(和 New 一个路子)。
//
// ★ **它忽略 opt.ReadTimeout / opt.WriteTimeout**:这两个值被强制成
// "无超时"。要一个有界的读写超时,自己用 ctx 兜 —— 见下面的说明。
//
// ## 先说清楚一个实测结论,免得下一个人重复推导
//
// **go-redis v9.23.0 的 PubSub 接收循环不受 Options.ReadTimeout 约束。**
// 调用链:
//
//   - pubsub.go:528  Receive(ctx) → ReceiveTimeout(ctx, 0)
//   - pubsub.go:501  cn.WithReader(ctx, 0, ...) —— 超时是**传进去的 0**,不是 opt.ReadTimeout
//   - internal/pool/conn.go:1166  timeout >= 0 → getEffectiveReadTimeout(0)
//     → 没有 relaxed window 时返回 0(普通超时)
//   - internal/pool/conn.go:1382  deadline(ctx, 0):timeout==0 且 ctx 自己没有 deadline 时
//     返回零值 time.Time{} → SetReadDeadline(time.Time{}) = **清除 deadline,无限期阻塞**
//
// 所以订阅连接的空读**不会**被 200ms 的读超时打断。
// redisx_test.go 里有一条用例把这个结论钉住了 ——
// 哪天 go-redis 改了这条路径,那个用例会红。
//
// (那个用例跑在 miniredis 上。结论本身是**客户端侧**的 ——
// deadline 由 go-redis 算,和对端是不是真 Redis 无关;
// miniredis 只负责"订阅期间不发消息"这件事。)
//
// ## 那为什么还要单独建一个客户端
//
// 上面那个结论把"避开读超时打断订阅"这个理由否掉了,剩下的理由有两条:
//
//  1. **只有这里能把读超时真正配成"无"。** New 的 `opt.ReadTimeout <= 0`
//     兜底会把 -1 改回 200ms;而 go-redis 的语义里 0 是"未设置"→ 5s 默认,
//     不是"无超时"。也就是说普通客户端**配不出**无超时,
//     Pub/Sub 客户端显式写 -1,绕开那个兜底。
//  2. **生命周期独立。** 订阅是长期占着的一条连接,
//     调用方要能单独把它关掉,而不影响业务命令用的那个客户端。
//
// ## 为什么写 -1 而不是 0
//
// go-redis 的 options.go 写得很明确:
//
//	-1 = 无超时(永久阻塞)
//	-2 = 完全不调 SetReadDeadline
//	 0 = 未设置 → 5s 默认
//
// 写 0 会被当成"未设置",于是这个客户端仍然有 5s 的读超时 ——
// 函数叫 NewPubSub,行为却不是它承诺的那个。
//
// ★ **这个客户端上的普通命令(比如 PUBLISH)一个 deadline 都没有。**
//
// 而且**「调用方自己用 ctx 兜超时」是兜不住的** ——
// go-redis 的命令路径是:
//
//	cn.WithReader(c.context(ctx), c.opt.ReadTimeout, ...)
//
// 而 `baseClient.context()`(redis.go:1892)在 `ContextTimeoutEnabled=false`
// (**默认值**)时把调用方的 ctx **换成 context.Background()**。
// 于是 ctx 上的 deadline 根本到不了 socket。
//
// 剩下的 `opt.ReadTimeout` 又是 0(上面的 -1 被 options.init() 归一成的),
// `pool.Conn.deadline(ctx, 0)` 在 ctx 没有 deadline 时返回 `noDeadline`。
//
// **结论:这个客户端只能用来订阅,不能用来发命令。**
// 要发命令就用 New() 那个有界客户端(读/写超时 200ms),
// 或者自己用 `client.WithTimeout(d)` 克隆一份有界视图
// (只改 ReadTimeout/WriteTimeout,共享连接池)。
//
// sse.Bus 就是这么分的:sub 用这个无界客户端,pub 用有界视图 ——
// 见 internal/pkg/sse/bus.go 里 publishTimeout 的注释。
func NewPubSub(opt Options) *Client {
	if opt.Addr == "" {
		return nil
	}
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = defaultTimeout
	}
	if opt.PoolSize <= 0 {
		opt.PoolSize = 10 * runtime.NumCPU()
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:        opt.Addr,
		Password:    opt.Password,
		DB:          opt.DB,
		DialTimeout: opt.DialTimeout,
		// ★ -1 在 go-redis 内部被归一成 0,也就是"没有读超时"。
		//   写 0 会被当成"未设置"→ 5s 默认,那就不是这个函数承诺的东西了。
		ReadTimeout:   -1,
		WriteTimeout:  -1,
		PoolSize:      opt.PoolSize,
		MaxRetries:    -1,
		DialerRetries: 1,
	})
	return &Client{rdb: rdb}
}

// Ping 探一次 Redis。nil 客户端返回 nil。
func (c *Client) Ping(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.rdb.Ping(ctx).Err()
}

// WaitReady 最多 ping attempts 次,每次失败后睡 gap,首次成功即返回 nil,
// 全部失败返回**最后一次**的错误。尊重 ctx:取消后立刻返回 ctx 的错误。
//
// 为什么要重试而不是 ping 一次:调用方把彻底失败当作启动期致命错误
// (地址配错应当像缺 JWT_SECRET 一样大声死掉),但单次 ping 会把
// 一秒钟的 Redis 抖动或滚动重启变成 CrashLoopBackOff,拖垮登录、会话、
// 任务 —— 而 Redis 只守着两个接口。重试既保留"配错即死",又不至于这样小题大做。
func (c *Client) WaitReady(ctx context.Context, attempts int, gap time.Duration) error {
	if c == nil {
		return nil
	}
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if last = c.Ping(ctx); last == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gap):
		}
	}
	return last
}

// Close 关闭连接池。nil 客户端返回 nil。
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	return c.rdb.Close()
}

// Raw 返回底层 *redis.Client,给 ratelimit.New 这类要 redis.Scripter 的地方用。
// nil 客户端返回 nil。
func (c *Client) Raw() *redis.Client {
	if c == nil {
		return nil
	}
	return c.rdb
}
