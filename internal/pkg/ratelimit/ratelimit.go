// Package ratelimit 是基于 Redis ZSET 的滑动窗口日志限流器。
//
// 为什么是滑动窗口而不是固定窗口:固定窗口(INCR + EXPIRE)在窗口边界允许
// 2 倍突发 —— 第 1 秒的最后 1 毫秒打满,第 2 秒的第 1 毫秒再打满,2 毫秒内
// 过了两倍的量。登录接口恰恰最怕这个。
//
// 为什么不是令牌桶:令牌桶刻意保留突发容量,而这里的需求是"不要突发",
// 是反需求。
//
// 为什么必须用 Lua:一次判定是 ZREMRANGEBYSCORE(清掉窗口外的) → ZCARD(数一下)
// → **如果没满**才 ZADD。中间那个"如果"决定了它不能用 MULTI/EXEC ——
// 事务不能根据前一条命令的结果分支,命令是一次性排队、一次性执行的。
// Pipeline 只是把多个往返合成一个,**不保证原子性**,两个并发请求会都读到
// "还差一个"。只剩 Lua:Redis 单线程跑完整个脚本,期间插不进别的命令。
//
// 这个包只认 Redis,不认 HTTP,也不 import internal/config —— 和
// internal/pkg/breaker 一样收自己包里的类型。key 长什么样由调用方决定
// (见 internal/middleware/ratelimit.go),这个包不关心维度的业务含义。
package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// script 是滑动窗口的全部逻辑。
//
// ★ 两个性质是这个脚本存在的理由,改动前先读懂它们:
//
//  1. **两遍循环:第一遍只判定,第二遍才记账。**
//     边查边写的话,考虑这个序列:攻击者的请求先通过全局桶(记了一账),
//     再被 IP 桶拒掉 —— 他被拒了,但全局额度已经被他消耗了。于是他只要
//     维持住请求速率就能占满全局名额,让所有正常用户都被拒,而日志上看
//     全是"正常拦截"。限流器自己成了 DoS 放大器。
//     先全查后全写之后,被窄桶拒掉的请求一个全局名额都不占。
//
//  2. **时钟只有 Redis 这一个,不从 Go 传。**
//     ZREMRANGEBYSCORE 是对共享状态的**删除**。由 Go 传时间的话,一个
//     快 Δ 的副本不只是误判自己这次请求,它会删掉其他所有副本还认为在
//     窗口内的成员。Δ 大到一个窗口以上时(VM 从挂起恢复、容器 ntpd 坏掉、
//     CLOCK_REALTIME 跳变),那个副本每来一个请求就把全部 ZSET 清空一次,
//     **全集群的限流静默失效,日志里一行都没有**。
//     TIME 在 Redis 5 之后的脚本里是合法的(效果复制),不需要
//     redis.replicate_commands()。测试侧用 miniredis.SetTime 控时钟。
const script = `
-- KEYS[i]      第 i 个桶的 key
-- ARGV[1]      member 后缀,保证同毫秒的多个请求各算一次
-- ARGV[2i]     第 i 个桶的 window_ms
-- ARGV[2i+1]   第 i 个桶的 limit
-- 返回 {allowed, idx, retry_after_ms}

local t = redis.call('TIME')
local now_ms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local n = #KEYS

-- 第一遍:只判定,一个桶的额度都不消耗。
for i = 1, n do
  local window = tonumber(ARGV[2 * i])
  local limit  = tonumber(ARGV[2 * i + 1])
  redis.call('ZREMRANGEBYSCORE', KEYS[i], 0, now_ms - window)
  if redis.call('ZCARD', KEYS[i]) >= limit then
    -- retry_after 用**固定的窗口长度**,不用精确剩余时间:精确值会泄露
    -- "桶里最早那次尝试发生在什么时候",而邮箱桶是攻击者和账号主人
    -- 共享的 —— 那等于从一个免鉴权接口读出受害者上次登录的时刻。
    return {0, i, window}
  end
end

-- 第二遍:全部通过才记账。
local member = now_ms .. '-' .. ARGV[1]
for i = 1, n do
  local window = tonumber(ARGV[2 * i])
  redis.call('ZADD', KEYS[i], now_ms, member)
  redis.call('PEXPIRE', KEYS[i], window)
end
return {1, 0, 0}
`

// Rule 是一个桶的限额。
type Rule struct {
	// Window 统计窗口。滑动窗口:数的是"此刻往前 Window 之内来了几个"。
	Window time.Duration

	// Limit 窗口内的上限。**<= 0 表示这一层不限**,由 Limiter 负责滤掉。
	//
	// 滤掉这件事不能忘:这种桶要是进了脚本,第一遍会算 ZCARD >= 0 ——
	// **恒真,拒绝一切请求**。而配置是允许单独某一层为 0 的,所以这种桶
	// 一定会出现。
	Limit int
}

// Bucket 是一次判定里的一个维度。
type Bucket struct {
	// Key 是 Redis 里的键名。由调用方拼,这个包不关心它的业务含义。
	//
	// 同一次 Allow 里的所有 Key 应当带同一个 hash tag(形如 rl:{login}:...):
	// Redis Cluster 要求一个脚本的所有 key 落在同一个 slot。
	Key string

	Rule Rule

	// Scope 是这个维度的名字,只用于日志和指标。
	// **绝不能回给客户端** —— 让攻击者知道是哪个维度拦的,他就知道该换
	// IP 还是换邮箱了。
	Scope string
}

// Result 是一次判定的结果。
type Result struct {
	Allowed bool

	// RetryAfter 是被拒桶的**窗口长度**,不是精确剩余时间。见 script 里的注释。
	RetryAfter time.Duration

	// Scope 是被哪个桶拒的。只进日志,不进响应体。
	Scope string
}

// Limiter 是滑动窗口限流器。零值不可用,必须走 New。
type Limiter struct {
	rdb    redis.Scripter
	script *redis.Script

	// member 生成 ZSET 成员名的唯一后缀。可替换,给测试用。
	//
	// 为什么必须唯一:ZSET 的成员会去重。两个请求落在同一毫秒、成员名又
	// 一样的话,第二次 ZADD 只是更新了第一个成员的 score,ZCARD 还是 1 ——
	// **一次尝试凭空消失**。这不是边缘情况,是这个量级下的常态。
	member func() string
}

// New 建一个限流器。
//
// 收 redis.Scripter 这个第三方类型是刻意的:这个包的职责就是 Redis,它是
// **边界包**,和 internal/agentclient 是全仓库唯一能 import proto 的包同
// 一个道理。约束在于边界不能往上漏 —— middleware 及以上只许出现本包的
// 类型,go-redis 的类型一个都不许出现。
func New(rdb redis.Scripter) *Limiter {
	return &Limiter{
		rdb:    rdb,
		script: redis.NewScript(script),
		member: defaultMember,
	}
}

// Allow 判定这一组桶是否全部放行。
//
// 三条硬性语义:
//
//   - **接收者为 nil 时一律放行**。那表示这个部署没启用限流(没配 REDIS_ADDR),
//     调用方就不用到处写 if l != nil,少一处能忘的地方。同 TaskQuota.Check。
//   - **先滤掉 Rule.Limit <= 0 的桶**,理由见 Rule.Limit 的注释。
//   - **滤完为空就直接放行**,不发 EVAL(0 个 key 的 EVAL 没有意义)。
//
// 返回 error 时调用方应当**放行**(fail-open):限流拦的是滥用,而口令校验
// 那道防线还在 —— Redis 挂了只是失去了"慢一点"这一层,不是失去了认证。
// 反过来 fail-closed 会让一次 Redis 抖动变成全站登不进去,把局部故障放大
// 成整站不可用。
func (l *Limiter) Allow(ctx context.Context, buckets []Bucket) (Result, error) {
	// TODO(阶段 1):实现。
	return Result{}, nil
}

// defaultMember 返回一个进程内唯一、跨副本也几乎不可能相撞的后缀。
//
// 进程启动时取一次随机前缀,之后只做原子自增 —— 每次都调 crypto/rand
// 的话,限流这条路上每个请求都要摸一次熵源,没必要。
func defaultMember() string {
	return fmt.Sprintf("%s%d", memberPrefix, memberSeq.Add(1))
}

var (
	memberPrefix = newMemberPrefix()
	memberSeq    atomic.Uint64
)

func newMemberPrefix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源拿不到也不能让进程起不来:这个前缀只需要"不同",不需要
		// 不可预测。退化成时间戳,同一毫秒内启动的两个副本仍然靠下面的
		// 原子自增区分不开 —— 但那要求两个副本在同一毫秒启动且 ZADD 落在
		// 同一毫秒,代价只是极少数计数偏少,不值得为它拒绝启动。
		return fmt.Sprintf("%x-", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:]) + "-"
}
