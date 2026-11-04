package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTest 起一个内存 Redis 和接在它上面的限流器。
//
// 用 miniredis 而不是真 Redis,是刻意的:它自带 gopher-lua,这个脚本在它
// 上面是真跑的。把一条防滥用规则押在"没有外部服务就会被跳过"的用例上,
// 等于没测 —— 同 internal/service/quota_test.go 的取向。
//
// 返回的 *miniredis.Miniredis 用来控时钟(SetTime)和推进过期(FastForward):
//
//   - **时间一律用 SetTime 推,不要 Sleep。** 脚本的时钟是 Redis 的 TIME,
//     而 miniredis 的 TIME 听 SetTime 的。Sleep 的用例又慢又飘。
//   - **过期只在 FastForward 时发生。** 验 PEXPIRE 的用例必须 FastForward
//     之后再看 PTTL / Exists,否则"去掉 PEXPIRE"这个变异不会让测试变红。
func newTest(t *testing.T) (*miniredis.Miniredis, *Limiter) {
	t.Helper()
	s := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return s, New(c)
}

// 固定一个基准时刻,让用例之间互不干扰也可读。
var base = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// 接收者为 nil = 这个部署没启用限流,必须放行。
//
// 这条让调用方不用到处写 if l != nil —— nil 安全在方法上,不在调用方,
// 同 service.TaskQuota.Check。
func TestAllow_NilLimiterAllows(t *testing.T) {
	var l *Limiter
	res, err := l.Allow(context.Background(), []Bucket{
		{Key: "k", Rule: Rule{Window: time.Minute, Limit: 1}, Scope: "ip"},
	})
	if err != nil {
		t.Fatalf("nil 接收者不该报错: %v", err)
	}
	if !res.Allowed {
		t.Error("nil 接收者必须放行")
	}
}
