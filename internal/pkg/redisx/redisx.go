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
