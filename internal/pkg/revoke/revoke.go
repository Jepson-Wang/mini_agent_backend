// Package revoke 是 JWT 吊销名单。
//
// ## 缺的到底是什么
//
// 现在登出是**纯前端行为** —— 前端删掉本地 token,但那个 token 在服务端
// 仍然有效,直到 AccessTTL(默认 2 小时)过期。两个真实后果:
//
//  1. 用户发现账号被盗、改了密码之后,攻击者手里的 token 还能用 2 小时。
//     改密码在用户心里等于"把他踢出去了",实际上没有。
//  2. **管理员禁用一个用户之后,他现有的 token 还能用 2 小时。**
//
// 第二条是个实打实的安全洞,而且藏得很深:禁用用户的检查只在 Login 里。
// middleware.Auth → TokenVerifier.Verify 一次库都不查,所以**已经登录的
// 用户被禁用之后,他手里的令牌照样畅通无阻**。
//
// ## 两种粒度
//
// 黑名单单独用不够,因为禁用一个用户时你手里没有他的 jti ——
// jti 只在他自己的 token 里,你没法枚举「这个用户有哪些 token 在外面」。
//
// 用户级单独用也不够,因为它是全有或全无:用它做登出,会把该用户所有设备
// 都踢下线。
//
//	场景                  该吊销什么                          用哪种
//	--------------------  ---------------------------------  ------------------
//	用户点「登出」        只有当前这一个 token                单 token 黑名单
//	改密码 / 管理员禁用   该用户的全部 token,包括你不知道的    用户级 not_before
//
// ## TTL 是这个包最优雅的部分
//
// 黑名单 key 的 TTL **精确等于 token 的剩余有效期**(ttl = exp - now):
//
//   - 更短:token 还没过期,黑名单先没了 → **吊销静默失效**。用户以为登出了,其实没有。
//   - 更长:纯浪费内存。token 自己都过期了,黑名单再拦一次毫无意义。
//   - ttl <= 0:根本不用写 —— 这个 token 已经过期了,JWT 校验会拒它。
//
// 这是 Redis 最漂亮的用法:**让 key 的生命周期和业务对象的生命周期完全对齐,
// 垃圾回收由 Redis 自己做,不需要任何清理任务。**
// 换成数据库表的话,你必须自己写定时任务去删过期行,而那个任务会有它自己的
// 一整套问题(什么时候跑、跑多久、删不完怎么办、多副本会不会打架)——
// 也就是这个项目已经在 Reconciler 上踩过的那些坑。
package revoke

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrDisabled 表示这个部署没有 Redis,吊销功能整体不启用。
//
// 是哨兵不是错误状态 —— 调用方用 errors.Is 判出来之后该走降级分支。
var ErrDisabled = errors.New("redis 未启用")

// key 前缀。
//
// 带上用途前缀:同一个 Redis 实例可能被别的服务共用(限流和选主锁都用它),
// 不加前缀会和别人的 key 撞上。
const (
	tokenKeyPrefix = "jwt:revoked:"
	userKeyPrefix  = "jwt:nbf:"
)

// redisClient 是这个包需要的 Redis 能力。
//
// 自己列一个最小接口而不是直接收 redis.Scripter:这个包只用 MGET / SET /
// SETEX,不需要脚本能力。边界上少露一点第三方类型。
type redisClient interface {
	MGet(ctx context.Context, keys ...string) *redis.SliceCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
}

// Revoker 是吊销名单的读写入口。
//
// **所有方法都对 nil 接收者安全**:没配 REDIS_ADDR 是合法选择,
// nil 安全放在方法上而不是散落在每个调用点(同 pkg/redislock、pkg/ratelimit)。
type Revoker struct {
	rdb redisClient

	// now 可替换,测试里用来伪造时间。
	now func() time.Time

	// accessTTL 是用户级 not_before key 的 TTL。
	// 超过它之后,所有在 not_before 之前签发的 token 都自然过期了,
	// 这个 key 再留着没有作用 —— 不设 TTL 的话每个改过密码的用户
	// 都会永久占一个 key。
	accessTTL time.Duration
}

// New 建一个吊销器。
//
// rdb 为 nil 时返回 (nil, nil),表示"这个部署没有 Redis"。
// accessTTL <= 0 时用两小时兜底 —— 它只影响用户级 key 的存活时间,
// 配错的后果是多留几个 key,不是功能失效。
func New(rdb redisClient, accessTTL time.Duration) *Revoker {
	if rdb == nil {
		return nil
	}
	if accessTTL <= 0 {
		accessTTL = 2 * time.Hour
	}
	return &Revoker{
		rdb:       rdb,
		now:       time.Now,
		accessTTL: accessTTL,
	}
}

// RevokeToken 把单个 token 加进黑名单。
//
// exp 是这个 token 的过期时刻。TTL 取 exp - now。
// **ttl <= 0 时什么都不写** —— token 已经过期了,JWT 校验会拒它,
// 写一条黑名单没有意义(而且 SET k v EX -1 在 Redis 上会直接报错)。
func (r *Revoker) RevokeToken(ctx context.Context, jti string, exp time.Time) error {
	if r == nil {
		return nil
	}
	if jti == "" {
		return errors.New("吊销失败:jti 不能为空")
	}

	ttl := exp.Sub(r.now())
	if ttl <= 0 {
		// 已经过期了,不用吊销。
		return nil
	}

	key := tokenKeyPrefix + jti
	if err := r.rdb.Set(ctx, key, "1", ttl).Err(); err != nil {
		return fmt.Errorf("吊销 token: %w", err)
	}
	return nil
}

// RevokeUser 吊销某个用户的全部 token。
//
// 做法是记一个 not_before 时间戳:之后校验时,凡是 iat 早于它的 token 都算被吊销。
//
// 为什么不用黑名单:禁用用户时你手里没有他的 jti —— jti 只在他自己的 token 里。
// 你没法枚举「这个用户有哪些 token 在外面」,只能按用户一刀切。
//
// ttl 是这个 not_before 的有效期。取 accessTTL:超过它之后,
// 所有在 not_before 之前签发的 token 都自然过期了,这个 key 没用了。
func (r *Revoker) RevokeUser(ctx context.Context, userID int64, ttl time.Duration) error {
	if r == nil {
		return nil
	}
	if userID <= 0 {
		return fmt.Errorf("吊销失败:非法的 userID %d", userID)
	}
	if ttl <= 0 {
		ttl = r.accessTTL
	}

	key := userKeyPrefix + strconv.FormatInt(userID, 10)
	now := r.now().UnixMilli()
	if err := r.rdb.Set(ctx, key, strconv.FormatInt(now, 10), ttl).Err(); err != nil {
		return fmt.Errorf("吊销用户: %w", err)
	}
	return nil
}

// IsRevoked 判断一个 token 是否已被吊销。
//
// 两种检查:
//   - jti 在黑名单里(单 token 吊销,比如登出)
//   - token 的签发时间早于该用户的 not_before(整用户吊销,比如改密码/禁用)
//
// 返回的 err 只表示"Redis 出问题了",**不表示是否被吊销**。
// Redis 挂了的时候调用方应当 fail open(放行):JWT 签名校验已经过了,
// 身份是可信的,吊销只是在一个已验证的身份上再加一层。
func (r *Revoker) IsRevoked(ctx context.Context, jti string, userID int64, issuedAt time.Time) (bool, error) {
	if r == nil {
		return false, nil
	}

	keys := []string{tokenKeyPrefix + jti}
	if userID > 0 {
		keys = append(keys, userKeyPrefix+strconv.FormatInt(userID, 10))
	}

	// MGET 一次拿两个 key,不发两次 GET。
	// 这个方法在**每个私有请求**的关键路径上,两次 RTT 和一次 RTT
	// 是两倍的差别。
	vals, err := r.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return false, fmt.Errorf("查吊销名单: %w", err)
	}
	// 第一个是 token 黑名单。
	if len(vals) > 0 && !isNilValue(vals[0]) {
		return true, nil
	}

	// 第二个是用户级 not_before。
	if len(vals) > 1 && !isNilValue(vals[1]) {
		nbf, err := strconv.ParseInt(toString(vals[1]), 10, 64)
		if err != nil {
			// 值坏了就当没吊销 —— fail open。
			return false, fmt.Errorf("解析 not_before 失败: %w", err)
		}
		// ★ 比较方向不能反。
		//
		// 只有 iat **早于** not_before 的 token 才算被吊销。
		// 写成 <= 或者把两边对调,用户改完密码之后**新登录的 token
		// 也会立刻失效** —— 表现是「登录成功但马上又被踢」的死循环。
		if issuedAt.UnixMilli() < nbf {
			return true, nil
		}
	}

	return false, nil
}

// isNilValue 判断 MGET 的返回值是不是"这个 key 不存在"。
//
// Redis 的 MGET 对不存在的 key 返回 nil(interface{} 里的 nil),
// 对存在的 key 返回字符串。两种 nil 都要认。
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// toString 把 MGET 的返回值转成字符串。
func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(v)
	}
}
