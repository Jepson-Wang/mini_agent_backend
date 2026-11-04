package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ratelimit"
)

// 编译期断言:Raw() 的结果能当 redis.Scripter 交给限流器。
var _ redis.Scripter = (*redis.Client)(nil)

func TestNew_EmptyAddrIsDisabled(t *testing.T) {
	c, err := New(Options{})
	if c != nil || err != nil {
		t.Fatalf("New(空地址) = (%v, %v), 期望 (nil, nil)", c, err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("nil 客户端 Ping = %v, 期望 nil", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("nil 客户端 Close = %v, 期望 nil", err)
	}
	if c.Raw() != nil {
		t.Error("nil 客户端 Raw 应返回 nil")
	}
}

func TestNew_SetsExplicitTimeouts(t *testing.T) {
	s := miniredis.RunT(t)
	c, err := New(Options{Addr: s.Addr()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	o := c.Raw().Options()
	if o.MaxRetries != 0 || o.DialerRetries != 1 {
		t.Errorf("MaxRetries = %d, DialerRetries = %d, 期望 0(go-redis 把 -1 归一成 0,默认是 3)和 1", o.MaxRetries, o.DialerRetries)
	}
	if o.DialTimeout == 0 || o.ReadTimeout == 0 || o.WriteTimeout == 0 || o.PoolSize == 0 {
		t.Errorf("存在未显式设置的项: %+v", o)
	}
}

func TestNew_RespectsProvidedOptions(t *testing.T) {
	s := miniredis.RunT(t)
	c, err := New(Options{
		Addr:         s.Addr(),
		DialTimeout:  time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     7,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	o := c.Raw().Options()
	if o.DialTimeout != time.Second || o.ReadTimeout != 2*time.Second ||
		o.WriteTimeout != 3*time.Second || o.PoolSize != 7 {
		t.Errorf("显式传入的参数被覆盖了: %+v", o)
	}
}

func TestPing_Succeeds(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestPing_FailsWhenServerGone(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	s.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("服务端已关闭,Ping 应当失败")
	}
}

func TestWaitReady_SucceedsFirstTry(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	if err := c.WaitReady(context.Background(), 3, time.Hour); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestWaitReady_ReturnsLastErrorAfterAttempts(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	s.Close()

	const gap = 50 * time.Millisecond
	start := time.Now()
	err := c.WaitReady(context.Background(), 3, gap)
	if err == nil {
		t.Fatal("地址不通,WaitReady 应当返回错误")
	}
	// 3 次尝试之间有 2 段间隔;只试一次的话会远小于这个下限。
	if elapsed := time.Since(start); elapsed < 2*gap {
		t.Errorf("耗时 %v 少于 %v,说明没有真的重试多次", elapsed, 2*gap)
	}
}

func TestWaitReady_StopsOnContextCancel(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()
	s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := c.WaitReady(ctx, 100, time.Second)
	if err == nil {
		t.Fatal("ctx 已取消,应当返回错误")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("取消后仍耗时 %v,没有及时返回", elapsed)
	}
}

func TestRaw_IsUsableByRatelimit(t *testing.T) {
	s := miniredis.RunT(t)
	c, _ := New(Options{Addr: s.Addr()})
	defer c.Close()

	var sc redis.Scripter = c.Raw()
	if lim := ratelimit.New(sc); lim == nil {
		t.Fatal("ratelimit.New 返回 nil")
	}
}
