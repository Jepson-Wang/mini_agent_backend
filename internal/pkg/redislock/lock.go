// Package redislock 是基于 Redis 的带租约互斥锁。
//
// ## 这把锁不是正确性依赖
//
// 这是整个包存在的第一句话,展开说:
//
//   - 没有锁:多个副本重复扫同一批任务。下游的 cas 在兜着,**不会写错任何数据**,
//     只是白烧几次 SELECT。
//   - 有锁:省掉那几次查询。
//
// 所以锁失效的最坏后果 = 退化回没有锁的行为。这个性质直接决定了三个设计选择:
//
//  1. **不用 Redlock。** Redlock(多个独立 Redis 实例投票)是为「锁丢了会破坏正确性」
//     的场景设计的。如果你的正确性依赖锁,Redlock 也救不了你(GC 停顿、时钟漂移会让
//     持锁方在自己不知情的情况下失去锁),你需要的是 fencing token;而如果你的正确性
//     不依赖锁,单实例 SET NX PX 就够了。
//     这里属于后者 —— 下游的 cas 本身就是 fencing 机制。
//  2. **不做续租(watchdog)。** 续租解决的是「不知道要跑多久」的场景。
//     这里的操作有硬超时(见 service.reconcileTimeout),上界是已知的。
//  3. **拿不到锁就跳过,不重试。** 见 Acquire 的注释。
//
// ## 为什么解锁必须用 Lua
//
// 一个反例:
//
//	副本 A 拿到锁 → A 卡住(GC / 网络抖动)超过 TTL → 锁自动过期
//	→ 副本 B 拿到锁 → A 恢复过来,执行解锁 → **A 把 B 的锁删了**
//	→ 副本 C 也拿到锁 → 现在 B 和 C 同时在跑。
//
// 用 GET 判断再 DEL 也不行 —— GET 返回自己的 token 之后、DEL 执行之前,
// 锁仍然可能过期并被别人拿走。**判断和删除必须在同一个原子操作里**,
// 和 pkg/ratelimit 那个滑动窗口脚本是同一个道理。
//
// token 的作用就是证明「这把锁是我的」,所以它必须每次都不同 ——
// 用固定值(比如 hostname)的话,同一台机器上的两次加锁就分不开了。
package redislock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
)

// releaseScript 是解锁的全部逻辑。
//
// KEYS[1] 锁的 key
// ARGV[1] 本次加锁拿到的 token
//
// 返回 1 表示删掉了(锁本来就是自己的),0 表示没删(锁已经不是自己的了)。
const releaseScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
    return redis.call('DEL', KEYS[1])
else
    return 0
end
`

// ErrDisabled 表示这个部署没有 Redis,锁功能整体不启用。
//
// 是哨兵不是错误状态 —— 调用方用 errors.Is 判出来之后该走降级分支。
var ErrDisabled = errors.New("redis 未启用")

// redisClient 是这把锁需要的 Redis 能力。
//
// 不直接用 redis.Scripter:那个接口只有脚本相关的方法(Eval / ScriptLoad 等),
// 不含 SetNX —— 而加锁靠的正是 SET NX PX。
//
// 自己列一个最小接口有两个好处:
//
//   - **边界清楚**:go-redis 的类型不出现在这个包的 API 上,和 pkg/ratelimit
//     一样的取向 —— 那个包收 redis.Scripter,是因为它只用脚本。
//   - **测试能塞假的**:想测「Redis 挂了会怎样」时,不用真的把 Redis 关掉。
//
// *redis.Client 和 *redis.ClusterClient 都满足它。
type redisClient interface {
	redis.Scripter
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) *redis.BoolCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// Lock 是一把带租约的分布式锁。
//
// **所有方法都对 nil 接收者安全**:没配 REDIS_ADDR 是合法选择,
// nil 安全放在方法上而不是散落在每个调用点(同 service.NewTaskQuota)。
type Lock struct {
	rdb    redisClient
	script *redis.Script

	// token 生成锁的持有者标识。可替换,给测试用。
	token func() (string, error)
}

// New 建一把锁。
//
// rdb 为 nil 时返回 (nil, nil),表示"这个部署没有 Redis"。
// 和 redisx.New 一样,空地址是合法选择而不是配置错误。
func New(rdb redisClient) *Lock {
	if rdb == nil {
		return nil
	}
	return &Lock{
		rdb:    rdb,
		script: redis.NewScript(releaseScript),
		token:  defaultToken,
	}
}

// defaultToken 生成一个跨副本几乎不可能相撞的持有者标识。
//
// 用 ULID(仓库已有 pkg/ulid):它是单调递增的 26 位字符串,
// 看日志时能直接看出先后顺序,排查「谁拿着锁」比看一串随机 hex 方便。
func defaultToken() (string, error) {
	return ulid.New()
}

// Acquire 尝试拿锁。
//
// 返回三个值,是因为「拿到没」和「Redis 好着吗」是**两件事**:
//
//   - token 非空:拿到了。token 要留着,Release 时用。
//   - ok == false:锁被别人持有着,没拿到。
//   - err != nil:Redis 出问题了。
//
// **nil 锁和 Redis 挂了都返回 ok == true。** 这是刻意的 fail open:
//
// 回收器本身就是兜底机制,让兜底依赖另一个会挂的组件是反的。
// 代价只是退化回"多副本重复扫" —— 不会写错数据。
// 记 err 是为了能告警:Redis 挂了这事运维该知道,只是不该让回收器停摆。
func (l *Lock) Acquire(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	if l == nil {
		return "", true, nil
	}
	if ttl <= 0 {
		// TTL 必须为正。<=0 的 SET NX PX 意味着**永不过期** ——
		// 持锁方崩了之后锁就永远不放,回收器从此再也不会跑。
		// 这是接线错误,大声返回而不是默默放行。
		return "", false, fmt.Errorf("锁 %q 的 TTL 必须大于 0,实际为 %v", key, ttl)
	}

	token, err := l.token()
	if err != nil {
		return "", false, fmt.Errorf("生成锁 token: %w", err)
	}

	// SET key token NX PX ttl
	//
	// NX = 只在 key 不存在时设置,这就是互斥。
	// PX = 毫秒级过期,这就是租约 —— 持锁方崩了锁会自己消失。
	//
	// 这两条**必须都在**:少了 NX 就成了「最后写的人赢」,两个副本同时拿到;
	// 少了 PX 就成了永久锁,第一次进程崩溃就永久死锁。
	ok, err := l.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		// Redis 挂了 → fail open,当作拿到了。err 照传,让调用方记日志。
		return "", true, fmt.Errorf("加锁 %q: %w", key, err)
	}
	if !ok {
		// 锁被别人持有着。这是**正常的并发**,不是错误。
		return "", false, nil
	}
	return token, true, nil
}

// Release 释放锁。
//
// **只删自己的锁。** 脚本里先比对 token,不是自己的就不动 ——
// 见包注释里那个「A 把 B 的锁删了」的反例。
//
// 幂等:锁已经不在了(自己过期了、或被别人释放了)不会报错。
// `defer release()` 和显式 release 并存时不该炸。
func (l *Lock) Release(ctx context.Context, key, token string) error {
	if l == nil {
		return nil
	}
	if token == "" {
		// 没拿到锁就没锁可放。直接返回,不去 DEL 别人的锁。
		return nil
	}
	if _, err := l.script.Run(ctx, l.rdb, []string{key}, token).Result(); err != nil {
		return fmt.Errorf("释放锁 %q: %w", key, err)
	}
	return nil
}
