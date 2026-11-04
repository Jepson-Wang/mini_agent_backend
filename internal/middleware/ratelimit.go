package middleware

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/emailx"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ratelimit"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// Limiter 是限流中间件依赖的判定接口,同 TokenVerifier 定在使用方。
//
// 签名里只许出现 ratelimit 包的类型 —— go-redis 的类型不能漏到这一层。
type Limiter interface {
	Allow(ctx context.Context, buckets []ratelimit.Bucket) (ratelimit.Result, error)
}

// AuthLimits 是登录/注册入口的三层限额。
type AuthLimits struct {
	Global, IP, Email ratelimit.Rule

	// EmailKey 是给邮箱做 HMAC 的子密钥,由 main.go 用 DeriveEmailKey 算好传进来,
	// 这样 JWT_SECRET 的原文不进路由层。
	EmailKey []byte
}

// ChatLimits 是发起任务入口的两层限额。
type ChatLimits struct {
	Global, User ratelimit.Rule
}

const (
	// maxAuthBody 是限流中间件愿意读的请求体上限。合法的登录/注册体不到 1KB。
	maxAuthBody = 4 << 10

	// limiterTimeout 是一次限流判定的时限。限流在每个请求的关键路径上,
	// Redis 慢的时候宁可放行,也不能让它拖慢登录。
	limiterTimeout = 50 * time.Millisecond

	// failOpenLogEvery 是"限流降级"警告日志的最小间隔。
	failOpenLogEvery = 10 * time.Second

	// rejectLogEvery 是"限流生效中"汇总日志的最小间隔。
	// 被拒的速率就是攻击的速率,逐条打日志等于把刷日志的能力也交给攻击者。
	rejectLogEvery = 10 * time.Second
)

// DeriveEmailKey 从 JWT_SECRET 派生出一把只用于限流桶键的子密钥。
//
// HMAC-SHA256(jwtSecret, "mini-agent:ratelimit:email"),标准的密钥分离:
// 限流器永远见不到 JWT_SECRET 本身,桶键泄露一个也不会顺带交出签名密钥。
func DeriveEmailKey(jwtSecret string) []byte {
	m := hmac.New(sha256.New, []byte(jwtSecret))
	m.Write([]byte("mini-agent:ratelimit:email"))
	return m.Sum(nil)
}

// emailBucketID 把邮箱变成桶键里的标识:规范化 → HMAC → 取十六进制前 32 位。
//
// 必须先规范化:桶键用原始邮箱的话,换个大小写就是一个全新的桶,
// 按邮箱的限流形同虚设。
//
// 必须是 HMAC 而不是明文或裸 SHA-256:Redis 的 key 会出现在 SLOWLOG、
// MONITOR 和监控面板里,邮箱是个人信息。裸 SHA-256 也不算脱敏 ——
// 邮箱的取值空间可枚举,拿字典一撞就还原了。
func emailBucketID(key []byte, rawEmail string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(emailx.Normalize(rawEmail)))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// AuthRateLimit 给登录/注册这类免鉴权入口限流:全局 + 按 IP + 按邮箱。
func AuthRateLimit(l Limiter, limits AuthLimits) gin.HandlerFunc {
	return func(c *gin.Context) {
		if l == nil {
			// 这个部署没启用限流,是合法的部署选择。
			c.Next()
			return
		}

		var raw []byte
		if c.Request.Body != nil {
			raw, _ = io.ReadAll(io.LimitReader(c.Request.Body, maxAuthBody+1))
			if len(raw) > maxAuthBody {
				// 超长体必须拒绝,不能"跳过邮箱桶然后放行"。
				// dto 没开 DisallowUnknownFields,塞一个 4KB 的垃圾字段照样能
				// 绑定成功、照样能登录 —— 放行的话,攻击者靠一个字节数就关掉了
				// 唯一针对单个账号的防线。合法体不到 1KB,拒掉没有误伤。
				response.Fail(c, response.CodeInvalidParam, "请求体过大")
				return
			}
			// 读走了就得还回去,下游 handler 还要绑定。
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		}

		// 只解邮箱。解不出来就没有邮箱维度,让 handler 自己回 40000,这里不替它拒。
		var probe struct {
			Email string `json:"email"`
		}
		_ = json.Unmarshal(raw, &probe)

		var buckets []ratelimit.Bucket

		// login 和 register 的全局桶故意分开:注册刷量不能把老用户锁在登录之外。
		if action := lastSegment(c.FullPath()); action != "" {
			buckets = append(buckets, ratelimit.Bucket{
				Key: "rl:{login}:global:" + action, Rule: limits.Global, Scope: "login_global",
			})
		}

		// ClientIP 的可信度取决于引擎的 TrustedProxies 配置;生产上应置空,
		// 只认 RemoteAddr,否则 X-Forwarded-For 可以随便伪造出新桶。
		if ip := c.ClientIP(); ip != "" {
			buckets = append(buckets, ratelimit.Bucket{
				Key: "rl:{login}:ip:" + ip, Rule: limits.IP, Scope: "ip",
			})
		}

		if emailx.Normalize(probe.Email) != "" {
			if len(limits.EmailKey) > 0 {
				buckets = append(buckets, ratelimit.Bucket{
					Key:   "rl:{login}:email:" + emailBucketID(limits.EmailKey, probe.Email),
					Rule:  limits.Email,
					Scope: "email",
				})
			} else if limits.Email.Limit > 0 {
				// 配了邮箱限额却没给子密钥是接线错误:大声报,只丢这一个维度。
				slog.Error("邮箱限流已配置但缺少 EmailKey,跳过邮箱维度",
					"trace_id", trace.FromGin(c),
				)
			}
		}

		admit(c, l, buckets)
	}
}

// ChatRateLimit 给发起任务的入口限流:全局 + 按登录用户。必须挂在 Auth 之后。
// 不碰请求体。
func ChatRateLimit(l Limiter, limits ChatLimits) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := UserID(c)
		if !ok {
			// 这里 fail closed,和下面 admit 里的 fail open 方向相反,而且必须相反:
			// "这个部署没启用限流"是合法的部署选择,"代码接线接错了"是 bug。
			// 合成一个分支的话,bug 就被伪装成了配置。
			// 走到这里只可能是这条路由忘了挂 Auth。
			slog.Error("限流中间件取不到登录用户,路由可能漏挂了 Auth,拒绝请求",
				"trace_id", trace.FromGin(c),
				"path", c.Request.URL.Path,
			)
			response.Fail(c, response.CodeInternal, "")
			return
		}
		if l == nil {
			c.Next()
			return
		}

		buckets := []ratelimit.Bucket{
			{Key: "rl:{chat}:global", Rule: limits.Global, Scope: "chat_global"},
			{Key: "rl:{chat}:user:" + strconv.FormatInt(uid, 10), Rule: limits.User, Scope: "user"},
		}
		admit(c, l, buckets)
	}
}

// admit 做判定并决定放行还是拒绝,两个中间件共用。
func admit(c *gin.Context, l Limiter, buckets []ratelimit.Bucket) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), limiterTimeout)
	defer cancel()

	res, err := l.Allow(ctx, buckets)
	if err != nil {
		// fail open:限流拦的是滥用,而口令校验那道防线还在 —— Redis 挂了
		// 只是失去"慢一点"这一层,不是失去认证。fail closed 会让一次 Redis
		// 抖动变成全站登不进去,把局部故障放大成整站不可用。
		recordFailOpen(c, err)
		c.Next()
		return
	}
	if res.Allowed {
		c.Next()
		return
	}

	// Retry-After 必须在 Fail 之前设:Fail → write → c.JSON 会写出头部块,
	// 之后再设头不会生效。向上取整,最少 1 秒。
	secs := int64((res.RetryAfter + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	c.Header("Retry-After", strconv.FormatInt(secs, 10))

	// 被哪个桶拒的只进日志。各个维度对客户端必须完全一样 —— 同码、同文案、
	// 同头部;也不发 X-RateLimit-Remaining 之类的头,那是给刷量的人看的余额表。
	recordReject(c, res.Scope)
	response.Fail(c, response.CodeRateLimited, "")
}

// failOpenCount 累计限流器出错而被放行的次数。
var failOpenCount atomic.Uint64

// lastFailOpenLog 是上次打降级警告的 unix 纳秒,用来节流。
var lastFailOpenLog atomic.Int64

// RateLimitFailOpenCount 返回因限流器出错而放行的累计次数,
// 留给将来的指标或健康检查接口读取。
func RateLimitFailOpenCount() uint64 { return failOpenCount.Load() }

// recordFailOpen 记录一次降级。
//
// 降级必须"可数",而不只是"可读":Redis 挂了的时候逐请求打 Warn 会把日志
// 刷爆,真正有用的信息(在降级、降了多少次)反而淹没。所以计数器每次都加,
// 日志最多每 10 秒一条,第一条必打。
func recordFailOpen(c *gin.Context, err error) {
	failOpenCount.Add(1)

	now := time.Now().UnixNano()
	last := lastFailOpenLog.Load()
	if last != 0 && now-last < int64(failOpenLogEvery) {
		return
	}
	if !lastFailOpenLog.CompareAndSwap(last, now) {
		return // 别的请求抢到了这次日志名额
	}
	slog.Warn("限流器出错,按 fail-open 放行",
		"trace_id", trace.FromGin(c),
		"error", err,
		"fail_open_total", failOpenCount.Load(),
	)
}

// rejectCounts 按维度累计被限流拒掉的请求数。
//
// 做成 map[string]*atomic.Uint64 而不是一个总数:运维最需要回答的问题是
// "现在是谁在被拦",而 login_global 被打满(过载/被打)和 email 被打满
// (有人在撞某个账号)是两件完全不同的事,该触发的告警也不一样。
//
// 这张表只在启动时写一次(init 里把所有已知维度建好),之后只读不写,
// 所以不用加锁 —— 维度是编译期就定下来的常量集合,不会在运行时长出新的。
var rejectCounts = map[string]*atomic.Uint64{
	"login_global": new(atomic.Uint64),
	"ip":           new(atomic.Uint64),
	"email":        new(atomic.Uint64),
	"chat_global":  new(atomic.Uint64),
	"chat_user":    new(atomic.Uint64),
}

// lastRejectLog 是上次打限流汇总日志的 unix 纳秒,用来节流。
var lastRejectLog atomic.Int64

// RateLimitRejectCount 返回某个维度累计拒掉的请求数,维度名未知时返回 0。
// 留给将来的指标接口读取。
func RateLimitRejectCount(scope string) uint64 {
	if c, ok := rejectCounts[scope]; ok {
		return c.Load()
	}
	return 0
}

// recordReject 记一次拒绝。
//
// **这里刻意不逐请求打日志**,理由和 recordFailOpen 一样但更要紧:被拒的
// 速率就是攻击的速率,撞库的时候逐条打 Warn 等于让攻击者顺手把日志刷爆,
// 真正有用的信息反而被埋掉。
//
// 不打也不会丢记录:response.Fail → write 会 c.Set("biz_code", 42901),
// Logger 中间件本来就会把**每一条**被拒的请求连着 trace_id 记进访问日志。
// 所以这里只需要一条周期性的汇总,用来回答"现在是哪个维度在被打"。
func recordReject(c *gin.Context, scope string) {
	if n, ok := rejectCounts[scope]; ok {
		n.Add(1)
	}

	now := time.Now().UnixNano()
	last := lastRejectLog.Load()
	if last != 0 && now-last < int64(rejectLogEvery) {
		return
	}
	if !lastRejectLog.CompareAndSwap(last, now) {
		return // 别的请求抢到了这次日志名额
	}
	slog.Warn("限流生效中",
		"trace_id", trace.FromGin(c),
		"scope", scope,
		"path", c.Request.URL.Path,
		"login_global_total", RateLimitRejectCount("login_global"),
		"ip_total", RateLimitRejectCount("ip"),
		"email_total", RateLimitRejectCount("email"),
		"chat_global_total", RateLimitRejectCount("chat_global"),
		"chat_user_total", RateLimitRejectCount("chat_user"),
	)
}

// lastSegment 取路径最后一段;空路径或以斜杠结尾返回空串。
func lastSegment(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}
