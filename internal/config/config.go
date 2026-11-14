// Package config 加载运行配置。
//
// 四层优先级,后面的盖前面的:内置默认值 < YAML 文件 < .env 文件 < 真实环境变量。
// 密钥、口令这类东西只走环境变量,不进 YAML —— 配置文件是要提交到仓库的。
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/joho/godotenv"
)

// DefaultPath 默认配置文件路径,可用 CONFIG_PATH 覆盖。
const DefaultPath = "configs/config.yaml"

// DefaultEnvPath 默认的环境变量文件路径,可用 ENV_PATH 覆盖。
const DefaultEnvPath = ".env"

// minSecretLen HS256 密钥的最小长度。
//
// RFC 7518 要求 HMAC 的密钥不短于哈希输出(HS256 就是 32 字节)。
// 比这短的话签名强度由密钥长度决定而不是算法本身,暴力破解成本掉一大截。
const minSecretLen = 32

type Config struct {
	Server    Server    `yaml:"server"`
	Database  Database  `yaml:"database"`
	Agent     Agent     `yaml:"agent"`
	Quota     Quota     `yaml:"quota"`
	Reconcile Reconcile `yaml:"reconcile"`
	Redis     Redis     `yaml:"redis"`
	RateLimit RateLimit `yaml:"rate_limit"`
	JWT       JWT       `yaml:"jwt"`
	SSE       SSE       `yaml:"sse"`
}

type Server struct {
	Addr string `yaml:"addr"`

	// TrustedProxies 可信反向代理(IP 或 CIDR)。nil 表示谁都不信。
	// 启动时逐项校验,见 validateTrustedProxies。
	TrustedProxies []string `yaml:"trusted_proxies"`
}

type Database struct {
	// DSN 里带口令,别往 YAML 里写,用 DB_DSN 环境变量传。
	// 现在 config.yaml 里还留着一份开发用的明文,上生产前必须清掉。
	DSN string `yaml:"dsn"`
}

// DefaultAgentTaskTimeout 一个任务的事件流最多跑多久。
//
// 给得比 HTTP 那边宽松得多:Agent 跑一轮 LLM 循环几分钟很正常,
// 这个上限只是兜底,防止对面不吭声时消费协程永远挂着(Close 会等它)。
const DefaultAgentTaskTimeout = 30 * time.Minute

type Agent struct {
	Addr string `yaml:"addr"`

	// TaskTimeout 单个任务事件流的上限,支持 "30m" / "1h" 这种写法。
	// 到点整条流会被掐断,任务在库里停在最后一次收到的状态。
	TaskTimeout time.Duration `yaml:"task_timeout"`

	// CallbackSecret 是 Agent 回调本服务时用的共享密钥,
	// 只从 AGENT_CALLBACK_SECRET 读。yaml:"-" 的理由和 JWT.Secret 一样。
	//
	// 必须和 JWT_SECRET 取不同的值:一个泄露了,不该顺带把另一个也交出去。
	//
	// 和 JWT.Secret 不同,它**不是启动必填**。缺了的话 /internal 那组接口
	// 一律 500(见 middleware.InternalAuth),服务其余部分照常工作 ——
	// 这是"接口用不了",不是"接口没防护",所以不用把整个进程拦在门外。
	// 但只要填了,就得够长。
	CallbackSecret string `yaml:"-"`

	// Token 是**反方向**的共享密钥:本服务调 Agent 的 gRPC 接口时带上它,
	// 放在 metadata 的 x-agent-token 里。Agent 那侧对应 MINI_AGENT_GRPC_TOKEN。
	//
	// 和 CallbackSecret 分开是刻意的,虽然两者都是"服务间凭证":
	// 它们保护的是两条相反方向的路径,泄露一个不该把另一个也交出去。
	// 出于同样的理由,它也不能等于 JWT_SECRET。
	//
	// 也不是启动必填 —— Agent 绑在回环地址上时不需要鉴权(见它的 serve())。
	// 但只要填了,就得够长。
	//
	// yaml:"-" 的理由和 JWT.Secret 一样:哪怕有人往配置文件里写了也不会被加载。
	Token string `yaml:"-"`

	// BreakerThreshold 连续多少次"下游有病"就熔断,0 表示不启用熔断。
	//
	// 数的是连续失败而不是失败率:这个客户端调用量很低(一次任务一两个 RPC),
	// 失败率在小样本上抖得厉害。什么样的错误算"有病"由 agentclient 判断,
	// 参数错之类的不算 —— 见 agentclient.countsAsUnhealthy。
	BreakerThreshold int `yaml:"breaker_threshold"`

	// BreakerCooldown 熔断打开后要等多久才放一个探针出去。
	// BreakerThreshold > 0 时必须大于 0。
	BreakerCooldown time.Duration `yaml:"breaker_cooldown"`
}

// Quota 限制单个用户在一个时间窗口内能建多少任务。
//
// 存在的理由见 service.TaskQuota:每个任务都会驱动一整轮 LLM 循环,
// 不限额的话注册一个号就等于拿到一张无限额度的卡。
type Quota struct {
	// Window 统计窗口,支持 "24h" / "1h" 这种写法。滑动窗口,
	// 数的是"此刻往前 Window 之内建了几个",不是自然日。
	Window time.Duration `yaml:"window"`

	// MaxTasks 窗口内的上限。**<= 0 表示不限额。**
	//
	// 默认是开着的(见 defaults())。默认关掉的话,这个配置项就成了
	// "知道的人才会去打开"的摆设,而不知道的人正是最需要它的人。
	MaxTasks int `yaml:"max_tasks"`
}

// Reconcile 是任务回收器的参数。
//
// 它存在的理由:同步 Agent 只有事件流这一条汇报通道。进程重启、
// 流被中间设备掐断、Agent 被 kill —— 这些情况下没有任何人会来报结果,
// 任务就永远停在 pending / running,前端界面上一直转圈。
type Reconcile struct {
	// Interval 多久扫一次。<= 0 表示**关掉回收器**。
	Interval time.Duration `yaml:"interval"`

	// StaleAfter 一个中间态任务多久没动静就算卡住了。
	//
	// 这里要对比的是「Agent 两次事件之间的最长间隔」,**不是任务总时长**:
	// 任务只要还在推进度事件(thinking / tool_call / ...),
	// TaskEventHandler 就会给它续一次 updated_at(见 TaskService.Heartbeat)。
	// 所以一个跑两小时的任务不会被误杀,一个静默 15 分钟的任务才会。
	StaleAfter time.Duration `yaml:"stale_after"`

	// Batch 一轮最多处理多少个。夹住是为了别让一次积压把库打满。
	// 处理不完没关系,下一轮接着来。
	Batch int `yaml:"batch"`
}

// Redis 是限流用的 Redis 连接参数。Addr 为空表示不启用(没有默认值)。
type Redis struct {
	Addr string `yaml:"addr"`

	// Password 只从 REDIS_PASSWORD 读。yaml:"-" 的理由同 JWT.Secret。
	Password string `yaml:"-"`

	DB int `yaml:"db"`
}

// RateLimit 是各层限流的阈值与窗口。仅在配置了 Redis.Addr 时才会被检查和使用。
// 某层 Limit 为 0 表示关掉这一层。
type RateLimit struct {
	AuthGlobalLimit  int           `yaml:"auth_global_limit"`
	AuthGlobalWindow time.Duration `yaml:"auth_global_window"`
	IPLimit          int           `yaml:"ip_limit"`
	IPWindow         time.Duration `yaml:"ip_window"`
	EmailLimit       int           `yaml:"email_limit"`
	EmailWindow      time.Duration `yaml:"email_window"`
	ChatGlobalLimit  int           `yaml:"chat_global_limit"`
	ChatGlobalWindow time.Duration `yaml:"chat_global_window"`
	ChatUserLimit    int           `yaml:"chat_user_limit"`
	ChatUserWindow   time.Duration `yaml:"chat_user_window"`
}

type JWT struct {
	// Secret HS256 的签名密钥,只从 JWT_SECRET 环境变量读。
	// yaml:"-" 是刻意的:哪怕有人往配置文件里写了,也不会被加载进来。
	Secret string `yaml:"-"`

	// Issuer 签发方标识,会写进 iss 并在校验时核对。
	// 多个服务共用一个密钥时,靠它防止 A 服务的令牌被拿去 B 服务用。
	Issuer string `yaml:"issuer"`

	// AccessTTL 访问令牌有效期,支持 "2h" / "30m" 这种写法。
	//
	// 这里没有吊销机制:令牌一旦签出去,过期前都算有效,
	// 改密码、封号都拦不住它。所以别配太长。
	AccessTTL time.Duration `yaml:"access_ttl"`
}

// SSE 是实时推送(Server-Sent Events)那一段的参数。
//
// 事件本身在数据库里,Redis Pub/Sub 只做「有更新了」的唤醒信号:
// 信号丢了不丢数据,只丢实时性(还有定时轮询兜底)。
//
// ## 零值是"关掉"
//
// 别的配置段零值是"用默认值",这里不一样:SSE.Enabled 的零值(false)是"关"。
// 这样更安全 —— 忘了接线只是没有这个功能,不会凭空开出一堆长连接。
// 从配置文件加载的路径不受影响:默认值是开着的(见 defaults)。
type SSE struct {
	// Enabled 是不是开着这个功能。
	//
	// 关掉时路由**照常注册**,接口回 50300:前端拿到 503 会退回轮询,
	// 而 404 会被当成「接口不存在」—— 那是误导,接口明明在,只是没开。
	Enabled bool `yaml:"enabled"`

	// PollInterval 兜底轮询的间隔。
	//
	// 唤醒信号可能丢(Redis 抖动、副本重启),所以除了信号还要定时读一次库。
	// **没配 Redis 时,这是唯一的推送来源** —— SSE 仍然完全可用,
	// 只是延迟变成一个轮询周期。
	//
	// 代价要知道:每条连接每个周期查一次库,总开销是**连接数乘出来的**
	// (1000 条连接就是每秒 1000 次查询)。有 Redis 时唤醒信号是主路径,
	// 这一层只是兜底;没 Redis 时它就是主路径,连接数得看着点。
	PollInterval time.Duration `yaml:"poll_interval"`

	// Heartbeat 心跳间隔。
	//
	// nginx 的 proxy_read_timeout 默认 60s,不心跳连接会被静默掐断,
	// 而客户端分不清「服务端不说话」和「连接断了」。
	Heartbeat time.Duration `yaml:"heartbeat"`

	// MaxConnPerSession 单个会话最多几条 SSE 连接。
	//
	// 默认 2:同一个用户开两个标签页够用。
	// 这是**软限制**(先查后订中间有缝),超限一个只是多占一个 goroutine。
	MaxConnPerSession int `yaml:"max_conn_per_session"`

	// MaxConnTotal 全进程最多几条 SSE 连接。
	//
	// 无限 SSE 连接会打爆 backend 内存(每条至少一个 goroutine + 缓冲),
	// 所以这个闸必须有。它同样是软限制。
	//
	// **这是每进程的,不是全集群的**:多副本部署时实际是「副本数 × 这个值」。
	MaxConnTotal int `yaml:"max_conn_total"`

	// MaxConnPerUser 单个用户最多几条 SSE 连接。
	//
	// 为什么不能只有「每会话」和「全局」两个闸:一个账号拥有 N 个会话,
	// 就能开 N × MaxConnPerSession 条连接,把这个进程的全局预算吃光,
	// 于是**别的用户全部连不上**。那等于一个账号能替所有人做决定。
	//
	// 和另外两个闸一样是软限制(先查后订中间有缝)。
	MaxConnPerUser int `yaml:"max_conn_per_user"`

	// ReplayLimit 一次读多少条事件。
	//
	// 一批读满就接着读下一批,所以这个值只影响单次查询的大小,
	// 不影响一条事件都不会漏。
	ReplayLimit int `yaml:"replay_limit"`

	// MaxReplayPerDrain 一轮推送最多下发多少条事件。
	//
	// ReplayLimit 管的是「一次查询读多少条」,这个管的是「一轮推送发多少条」。
	// 两者分开是因为:一轮推送可能读好几批(读到读不满为止),
	// 而一个攒了几万条事件的会话,一次重连就会把整段历史一口气写出去 ——
	// 那是一次性的放大,单次查询上限拦不住。
	//
	// **超出的部分不会丢**:游标已经推进了,剩下的会在下一轮(兜底轮询或唤醒信号)
	// 继续发。所以这个值限制的是**单轮的突发量**,不是总量。
	MaxReplayPerDrain int `yaml:"max_replay_per_drain"`
}

// SSE 这一段的默认值。
const (
	// DefaultSSEPollInterval 兜底轮询间隔。
	DefaultSSEPollInterval = time.Second

	// DefaultSSEHeartbeat 心跳间隔。
	//
	// 15s:比 nginx 的 proxy_read_timeout(默认 60s)小得多,
	// 留足了余量,中间还隔了好几层代理也不会被静默掐断。
	DefaultSSEHeartbeat = 15 * time.Second

	// DefaultSSEMaxConnPerSession 单会话连接数上限。
	DefaultSSEMaxConnPerSession = 2

	// DefaultSSEMaxConnTotal 全局连接数上限。
	DefaultSSEMaxConnTotal = 1000

	// DefaultSSEMaxConnPerUser 单用户连接数上限。
	//
	// 20:一个正常使用的人顶多同时开几个页面看任务流。
	// 定得比「每会话 2 条」宽,因为一个人本来就可以有好几个会话;
	// 但比「全局 1000 条」窄得多 —— 不然一个账号有 500 个会话就能吃光全局预算。
	DefaultSSEMaxConnPerUser = 20

	// DefaultSSEReplayLimit 一次读多少条事件。
	//
	// 和 repository 那边一次读的默认上限一样。
	DefaultSSEReplayLimit = 200

	// DefaultSSEMaxReplayPerDrain 一轮推送最多下发多少条。
	//
	// 1000:一轮最多读 5 批(每批 200 条)。
	// 一次重连补发整段历史时,单轮突发量被它按住;
	// 剩下的下一轮继续发,不会丢。
	DefaultSSEMaxReplayPerDrain = 1000
)

// WithDefaults 返回一份把非法值换回默认值的副本。
//
// **SSE 这一段的 0 全都不是开关**,和限流那五层不一样:
//
//   - poll_interval=0 → 兜底轮询停摆。没配 Redis 时 SSE 就永远不推事件。
//   - heartbeat=0 → 不发心跳,nginx 的 proxy_read_timeout(默认 60s)会静默掐断连接。
//   - max_conn_*=0 → **不是「不限流」**。按字面理解会让一条连接都连不上;
//     按「关掉这一层」理解又等于开了一个资源耗尽的口子 ——
//     而无限 SSE 连接正是这个接口最该防的那件事。
//   - replay_limit=0 → 一条事件都发不出去。
//   - max_replay_per_drain=0 → 一轮推送都发不出去(游标推不动,连接等于白开)。
//
// 任何一个 0 都只会让这个功能「看起来配了,实际不工作」,
// 所以一律换回默认值。真正想关掉整个功能,用 sse.enabled=false。
//
// Enabled 不在这里动:false 就是一个有意义的开关(关掉),不能替人做主。
func (s SSE) WithDefaults() SSE {
	if s.PollInterval <= 0 {
		s.PollInterval = DefaultSSEPollInterval
	}
	if s.Heartbeat <= 0 {
		s.Heartbeat = DefaultSSEHeartbeat
	}
	if s.MaxConnPerSession <= 0 {
		s.MaxConnPerSession = DefaultSSEMaxConnPerSession
	}
	if s.MaxConnTotal <= 0 {
		s.MaxConnTotal = DefaultSSEMaxConnTotal
	}
	if s.MaxConnPerUser <= 0 {
		s.MaxConnPerUser = DefaultSSEMaxConnPerUser
	}
	if s.ReplayLimit <= 0 {
		s.ReplayLimit = DefaultSSEReplayLimit
	}
	if s.MaxReplayPerDrain <= 0 {
		s.MaxReplayPerDrain = DefaultSSEMaxReplayPerDrain
	}
	return s
}

// defaults 返回内置默认值。密钥没有默认值 —— 必须显式提供。
func defaults() Config {
	return Config{
		Server: Server{Addr: ":8080"},
		Agent: Agent{
			Addr:             "localhost:50051",
			TaskTimeout:      DefaultAgentTaskTimeout,
			BreakerThreshold: 5,
			BreakerCooldown:  30 * time.Second,
		},
		Quota: Quota{
			// 给得很宽松:这道闸防的是脚本按秒刷,不是正常人手速。
			// 真人一天点两百次对话已经很多了,而一个循环一秒就能打满。
			Window:   24 * time.Hour,
			MaxTasks: 200,
		},
		Reconcile: Reconcile{
			Interval:   time.Minute,
			StaleAfter: 15 * time.Minute,
			Batch:      100,
		},
		JWT: JWT{
			Issuer:    "mini-agent",
			AccessTTL: 2 * time.Hour,
		},
		// Redis 不给默认地址:空表示本部署没有 Redis、不启用限流。
		RateLimit: RateLimit{
			AuthGlobalLimit: 10, AuthGlobalWindow: time.Second,
			IPLimit: 30, IPWindow: time.Minute,
			EmailLimit: 5, EmailWindow: time.Minute,
			ChatGlobalLimit: 5, ChatGlobalWindow: time.Second,
			ChatUserLimit: 10, ChatUserWindow: time.Minute,
		},
		// SSE 默认开着:它是增量能力,前端不用它就退回轮询,不影响主流程。
		// 关掉它的理由应该是"这个部署的 nginx 配不对 SSE",而不是"先留着以后再说"。
		SSE: SSE{
			Enabled:           true,
			PollInterval:      DefaultSSEPollInterval,
			Heartbeat:         DefaultSSEHeartbeat,
			MaxConnPerSession: DefaultSSEMaxConnPerSession,
			MaxConnTotal:      DefaultSSEMaxConnTotal,
			MaxConnPerUser:    DefaultSSEMaxConnPerUser,
			ReplayLimit:       DefaultSSEReplayLimit,
			MaxReplayPerDrain: DefaultSSEMaxReplayPerDrain,
		},
	}
}

// LoadEnvFile 把 .env 里的键值灌进进程环境变量,好让后面的 os.Getenv 读得到。
//
// Go 标准库不认 .env,os.Getenv 只看进程自己的环境;这个函数就是那座桥。
// 调用点在 cmd/server,不在 Load 里 —— 这个包其余部分只读环境变量、不改它,
// 测试才能靠 t.Setenv 精确控制输入。
//
// 已存在的环境变量不会被覆盖 —— 真实环境(容器、CI、shell)优先于文件。
// 反过来的话,线上误留一份 .env 就能盖掉编排系统注入的密钥。
//
// path 传空则取 ENV_PATH,再没有就用 DefaultEnvPath。
// 文件不存在不算错误:线上本来就不该有这个文件,变量由编排系统给。
func LoadEnvFile(path string) error {
	if path == "" {
		path = os.Getenv("ENV_PATH")
	}
	if path == "" {
		path = DefaultEnvPath
	}

	if err := godotenv.Load(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("加载环境变量文件 %s: %w", path, err)
	}
	return nil
}

// Load 按 默认值 → YAML → 环境变量 的顺序装配配置,最后做一次校验。
//
// path 传空则取 CONFIG_PATH,再没有就用 DefaultPath。
// 文件不存在不算错误(容器里常常只给环境变量),但格式错、字段名拼错要报出来。
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path == "" {
		path = os.Getenv("CONFIG_PATH")
	}
	if path == "" {
		path = DefaultPath
	}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Strict 会把未知字段报成错误。少了它,配置项拼错(access_tt1)
		// 会被静默忽略,然后你对着一个跑着默认值的服务查半天。
		if err := yaml.UnmarshalWithOptions(raw, &cfg, yaml.Strict()); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// 允许纯环境变量启动。
	default:
		return nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}

	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// SSE 那一段的非法值换回默认值。
	//
	// 放在 Validate 之后:Validate 负责"能不能用",归一化是另一件事。
	// 归一化放在这儿而不是 Validate 里,是为了让 Validate 保持只读 ——
	// 一个会改参数的"校验函数"总有一天会被人误用。
	cfg.SSE = cfg.SSE.WithDefaults()
	return &cfg, nil
}

// applyEnv 用环境变量覆盖已有值。只覆盖非空的,空字符串等于没设。
func (c *Config) applyEnv() error {
	setString(&c.Server.Addr, "HTTP_ADDR")
	setString(&c.Database.DSN, "DB_DSN")
	setString(&c.Agent.Addr, "AGENT_ADDR")
	setString(&c.Agent.CallbackSecret, "AGENT_CALLBACK_SECRET")
	setString(&c.Agent.Token, "AGENT_GRPC_TOKEN")
	setString(&c.JWT.Issuer, "JWT_ISSUER")
	setString(&c.JWT.Secret, "JWT_SECRET")

	if err := setDuration(&c.JWT.AccessTTL, "JWT_ACCESS_TTL"); err != nil {
		return err
	}
	if err := setDuration(&c.Agent.TaskTimeout, "AGENT_TASK_TIMEOUT"); err != nil {
		return err
	}
	if err := setDuration(&c.Agent.BreakerCooldown, "AGENT_BREAKER_COOLDOWN"); err != nil {
		return err
	}
	if err := setInt(&c.Agent.BreakerThreshold, "AGENT_BREAKER_THRESHOLD"); err != nil {
		return err
	}
	if err := setDuration(&c.Reconcile.Interval, "RECONCILE_INTERVAL"); err != nil {
		return err
	}
	if err := setDuration(&c.Reconcile.StaleAfter, "RECONCILE_STALE_AFTER"); err != nil {
		return err
	}
	if err := setInt(&c.Reconcile.Batch, "RECONCILE_BATCH"); err != nil {
		return err
	}
	if err := setDuration(&c.Quota.Window, "TASK_QUOTA_WINDOW"); err != nil {
		return err
	}
	// 注意 setInt 对空值是"不覆盖",所以 TASK_QUOTA_MAX=0 是可以关掉配额的
	// (0 不是空串)。这一点和其它几个"0 表示关掉"的开关一致。
	if err := setInt(&c.Quota.MaxTasks, "TASK_QUOTA_MAX"); err != nil {
		return err
	}
	// Redis 没有默认地址:空表示不启用限流。口令只走环境变量。
	setString(&c.Redis.Addr, "REDIS_ADDR")
	setString(&c.Redis.Password, "REDIS_PASSWORD")
	if err := setInt(&c.Redis.DB, "REDIS_DB"); err != nil {
		return err
	}
	rl := &c.RateLimit
	for _, it := range []struct {
		dst *int
		env string
	}{
		{&rl.AuthGlobalLimit, "RATELIMIT_AUTH_GLOBAL_LIMIT"},
		{&rl.IPLimit, "RATELIMIT_IP_LIMIT"},
		{&rl.EmailLimit, "RATELIMIT_EMAIL_LIMIT"},
		{&rl.ChatGlobalLimit, "RATELIMIT_CHAT_GLOBAL_LIMIT"},
		{&rl.ChatUserLimit, "RATELIMIT_CHAT_USER_LIMIT"},
	} {
		if err := setInt(it.dst, it.env); err != nil {
			return err
		}
	}
	for _, it := range []struct {
		dst *time.Duration
		env string
	}{
		{&rl.AuthGlobalWindow, "RATELIMIT_AUTH_GLOBAL_WINDOW"},
		{&rl.IPWindow, "RATELIMIT_IP_WINDOW"},
		{&rl.EmailWindow, "RATELIMIT_EMAIL_WINDOW"},
		{&rl.ChatGlobalWindow, "RATELIMIT_CHAT_GLOBAL_WINDOW"},
		{&rl.ChatUserWindow, "RATELIMIT_CHAT_USER_WINDOW"},
	} {
		if err := setDuration(it.dst, it.env); err != nil {
			return err
		}
	}
	setStringSlice(&c.Server.TrustedProxies, "TRUSTED_PROXIES")

	// SSE。enabled 是唯一的开关,其余几个是阈值和间隔。
	//
	// 注意 setInt 对空值是"不覆盖",所以 SSE_MAX_CONN_TOTAL=0 是能传进来的 ——
	// 而 0 在这一段**不是**"关掉这一层"(见 SSE.WithDefaults),
	// Load 之后会被换回默认值。
	if err := setBool(&c.SSE.Enabled, "SSE_ENABLED"); err != nil {
		return err
	}
	if err := setDuration(&c.SSE.PollInterval, "SSE_POLL_INTERVAL"); err != nil {
		return err
	}
	if err := setDuration(&c.SSE.Heartbeat, "SSE_HEARTBEAT"); err != nil {
		return err
	}
	if err := setInt(&c.SSE.MaxConnPerSession, "SSE_MAX_CONN_PER_SESSION"); err != nil {
		return err
	}
	if err := setInt(&c.SSE.MaxConnTotal, "SSE_MAX_CONN_TOTAL"); err != nil {
		return err
	}
	if err := setInt(&c.SSE.MaxConnPerUser, "SSE_MAX_CONN_PER_USER"); err != nil {
		return err
	}
	if err := setInt(&c.SSE.ReplayLimit, "SSE_REPLAY_LIMIT"); err != nil {
		return err
	}
	if err := setInt(&c.SSE.MaxReplayPerDrain, "SSE_MAX_REPLAY_PER_DRAIN"); err != nil {
		return err
	}
	return nil
}

func setString(dst *string, env string) {
	if v := os.Getenv(env); v != "" {
		*dst = v
	}
}

// setDuration 解析 "30m" / "1h" 这种写法。变量没设或是空串就不动 dst。
//
// 解析失败要报错、不能静默跳过:RECONCILE_INTERVAL=1min(不是合法写法)
// 被忽略的话,服务会带着默认值跑起来,而运维以为自己改了。
func setDuration(dst *time.Duration, env string) error {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s=%q 不是合法的时长(例:30s、15m、1h): %w", env, v, err)
	}
	*dst = d
	return nil
}

func setInt(dst *int, env string) error {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s=%q 不是合法的整数: %w", env, v, err)
	}
	*dst = n
	return nil
}

// setBool 读布尔型环境变量。没设或是空串就不动 dst。
//
// 真值认 true/1/yes/on,假值认 false/0/no/off,不分大小写。
//
// **认不出来要报错**:一个拼错的开关比一个没配的开关危险得多 ——
// 前者会让运维以为 SSE 关掉了(其实还开着),或者反过来以为开着(其实没开)。
// 静默当成 false 的话,第二种情况会一直没人发现。
func setBool(dst *bool, env string) error {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		*dst = true
	case "0", "false", "no", "off":
		*dst = false
	default:
		return fmt.Errorf("%s=%q 不是合法的布尔值(例:true / false / 1 / 0)", env, v)
	}
	return nil
}

// setStringSlice 解析逗号分隔的列表。变量没设或是空串就不动 dst;
// 否则按逗号切开,每项去首尾空白,丢掉空项("a, b ," 得到 [a b])。
func setStringSlice(dst *[]string, env string) {
	v := os.Getenv(env)
	if v == "" {
		return
	}
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	*dst = out
}

// checkWireSecret 校验一把**要在链路上传输**的共享密钥。
//
// 长度之外还要卡字符集,理由是这两把密钥都不是"随便什么字节都行"的:
//   - AGENT_CALLBACK_SECRET 走 HTTP 头 X-Internal-Token;
//   - AGENT_GRPC_TOKEN 走 gRPC metadata 的 x-agent-token。
//
// 两者都只允许 ASCII。塞非 ASCII 进去的后果特别难查:配置校验放行、服务
// 正常启动,然后**每一次调用**都在传输层就被打回来 —— gRPC 那边是
// "Illegal header value" + INTERNAL,日志里没有任何东西指向密钥格式。
//
// 顺带也让两侧对"多长算够"有同一个答案:Python 那边 len() 数的是字符,
// Go 这边数的是字节,只要限定 ASCII,两者就恒等。
//
// 取 0x21..0x7E(可见字符,不含空格):空格在头部里合法但会被两端的
// 修剪逻辑吃掉,是另一个同样难查的坑。openssl rand -base64 的输出全在这个范围内。
func checkWireSecret(name, secret string) error {
	if len(secret) < minSecretLen {
		// 不把密钥本身写进错误里 —— 这条会进启动日志。
		return fmt.Errorf("%s 至少 %d 字节,当前 %d 字节", name, minSecretLen, len(secret))
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] < 0x21 || secret[i] > 0x7E {
			// 只说位置,不说是什么字符,更不打印整串。
			return fmt.Errorf(
				"%s 第 %d 个字节不是可见 ASCII 字符;它要放进 HTTP 头 / gRPC metadata,"+
					"非 ASCII 会让每一次调用都在传输层被拒。用 openssl rand -base64 48 生成",
				name, i+1)
		}
	}
	return nil
}

// Validate 检查配置能不能撑起一次正常启动。
//
// 这里宁可启动失败也不给"能跑但不安全"的兜底 ——
// 缺密钥就随机生成一个的话,服务每次重启所有人被登出,而且没人会发现漏配了。
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr 不能为空")
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn 不能为空(可用 DB_DSN 环境变量提供)")
	}
	if c.JWT.Issuer == "" {
		return fmt.Errorf("jwt.issuer 不能为空")
	}
	if c.JWT.AccessTTL <= 0 {
		return fmt.Errorf("jwt.access_ttl 必须大于 0,当前为 %v", c.JWT.AccessTTL)
	}
	if c.JWT.Secret == "" {
		return fmt.Errorf("缺少 JWT_SECRET 环境变量")
	}
	if len(c.JWT.Secret) < minSecretLen {
		// 不把密钥本身写进错误里 —— 这条会进启动日志。
		return fmt.Errorf("JWT_SECRET 至少 %d 字节,当前 %d 字节", minSecretLen, len(c.JWT.Secret))
	}
	if c.Agent.Addr == "" {
		return fmt.Errorf("agent.addr 不能为空(可用 AGENT_ADDR 环境变量提供)")
	}
	if c.Agent.TaskTimeout <= 0 {
		return fmt.Errorf("agent.task_timeout 必须大于 0,当前为 %v", c.Agent.TaskTimeout)
	}
	// 空表示不启用 /internal(那组接口会一律 500),但填了就必须够长、且可传输。
	// "填了个 8 位的" 比 "没填" 危险得多:前者会让人以为已经防住了。
	if s := c.Agent.CallbackSecret; s != "" {
		if err := checkWireSecret("AGENT_CALLBACK_SECRET", s); err != nil {
			return err
		}
	}
	if c.Agent.CallbackSecret != "" && c.Agent.CallbackSecret == c.JWT.Secret {
		return fmt.Errorf("AGENT_CALLBACK_SECRET 不能和 JWT_SECRET 相同")
	}
	// 同 CallbackSecret:空表示不带凭证(只在 Agent 绑回环时说得通),填了就得够长、且可传输。
	if s := c.Agent.Token; s != "" {
		if err := checkWireSecret("AGENT_GRPC_TOKEN", s); err != nil {
			return err
		}
	}
	// 三个密钥两两不同。共用一个的话,任何一条路径泄露就等于三条全丢 ——
	// 而它们的暴露面完全不一样(JWT 发给每个用户,另外两个只在服务之间走)。
	if c.Agent.Token != "" && c.Agent.Token == c.JWT.Secret {
		return fmt.Errorf("AGENT_GRPC_TOKEN 不能和 JWT_SECRET 相同")
	}
	if c.Agent.Token != "" && c.Agent.Token == c.Agent.CallbackSecret {
		return fmt.Errorf("AGENT_GRPC_TOKEN 不能和 AGENT_CALLBACK_SECRET 相同")
	}
	if c.Agent.BreakerThreshold < 0 {
		return fmt.Errorf("agent.breaker_threshold 不能为负,当前为 %d(0 表示不启用熔断)",
			c.Agent.BreakerThreshold)
	}
	// 阈值 > 0 但冷却为 0 的话,熔断器一打开、下一次调用立刻又放行,等于没熔断。
	// 这种配置看起来是启用了,实际什么也没做 —— 比明确关掉危险。
	if c.Agent.BreakerThreshold > 0 && c.Agent.BreakerCooldown <= 0 {
		return fmt.Errorf("启用熔断时 agent.breaker_cooldown 必须大于 0,当前为 %v",
			c.Agent.BreakerCooldown)
	}
	if err := c.Quota.validate(); err != nil {
		return err
	}
	if err := c.Reconcile.validate(); err != nil {
		return err
	}
	if err := c.Redis.validate(); err != nil {
		return err
	}
	if err := c.RateLimit.validate(c.Redis.Addr != ""); err != nil {
		return err
	}
	return validateTrustedProxies(c.Server.TrustedProxies)
}

// validate 校验配额配置。MaxTasks <= 0 表示不限额,这时窗口随便填。
func (q Quota) validate() error {
	if q.MaxTasks <= 0 {
		return nil
	}
	if q.Window <= 0 {
		return fmt.Errorf("启用配额时 quota.window 必须大于 0,当前为 %v", q.Window)
	}
	return nil
}

// validate 校验回收器配置。Interval <= 0 表示关掉,这时其余字段随便填。
func (r Reconcile) validate() error {
	if r.Interval <= 0 {
		return nil
	}
	if r.StaleAfter <= 0 {
		return fmt.Errorf("reconcile.stale_after 必须大于 0,当前为 %v", r.StaleAfter)
	}
	// 判定窗口比扫描间隔还短,几乎一定是把两个值写反了。放过去的话,
	// 一个刚建出来还没来得及跑的任务会在下一轮就被判为卡住并标记失败。
	if r.StaleAfter < r.Interval {
		return fmt.Errorf("reconcile.stale_after(%v)不能小于 reconcile.interval(%v)",
			r.StaleAfter, r.Interval)
	}
	if r.Batch <= 0 {
		return fmt.Errorf("reconcile.batch 必须大于 0,当前为 %d", r.Batch)
	}
	return nil
}

// validate 校验限流配置。redisEnabled 为 false(没配 Redis 地址)时**整体跳过**:
// 限流器根本不会被创建,这时窗口、阈值写成什么都不会被用到 ——
// 和 Quota.MaxTasks <= 0、Reconcile.Interval <= 0 是同一种短路。
func (r RateLimit) validate(redisEnabled bool) error {
	if !redisEnabled {
		return nil
	}
	layers := []struct {
		name   string
		limit  int
		window time.Duration
	}{
		{"auth_global", r.AuthGlobalLimit, r.AuthGlobalWindow},
		{"ip", r.IPLimit, r.IPWindow},
		{"email", r.EmailLimit, r.EmailWindow},
		{"chat_global", r.ChatGlobalLimit, r.ChatGlobalWindow},
		{"chat_user", r.ChatUserLimit, r.ChatUserWindow},
	}
	enabled := 0
	for _, l := range layers {
		if l.limit < 0 {
			return fmt.Errorf("rate_limit.%s_limit 不能为负,当前为 %d(0 表示关掉这一层)", l.name, l.limit)
		}
		if l.limit > 0 {
			enabled++
			if l.window <= 0 {
				return fmt.Errorf("启用限流时 rate_limit.%s_window 必须大于 0,当前为 %v", l.name, l.window)
			}
		}
	}
	// 配了 Redis 却把五层全关成 0:看起来限流已经启用(启动时还会去连 Redis),
	// 实际一个请求都不会拦。和 BreakerThreshold > 0 配 BreakerCooldown = 0 同理,
	// 这种"看似启用、实则无效"的配置比明确关掉危险 —— 要关就别配 REDIS_ADDR。
	if enabled == 0 {
		return fmt.Errorf("已配置 redis.addr 但 rate_limit 五层上限全为 0,限流形同虚设;" +
			"要关掉限流请不要配置 redis.addr")
	}
	return nil
}

// validate 校验 Redis 配置。Addr 为空表示不启用,这时其余字段随便填。
func (r Redis) validate() error {
	if r.Addr == "" {
		return nil
	}
	if r.DB < 0 {
		return fmt.Errorf("redis.db 不能为负,当前为 %d", r.DB)
	}
	return nil
}

// validateTrustedProxies 逐项检查可信代理(单个 IP 或 CIDR)。
//
// 必须在启动时就校验:NewRouter 会把它交给 gin 的 SetTrustedProxies,
// 而那个函数的 error 没法从只返回 *gin.Engine 的 NewRouter 里传出去。
// 拼错一项被吞掉的后果是 gin 谁都不信 —— 部署在反向代理后面时,
// 所有客户端的 ClientIP 都变成代理自己的地址,按 IP 的限流悄悄退化成
// 全体共用一份额度,而日志里没有任何异常。所以宁可启动失败。
func validateTrustedProxies(list []string) error {
	for _, p := range list {
		if strings.Contains(p, "/") {
			if _, _, err := net.ParseCIDR(p); err != nil {
				return fmt.Errorf("server.trusted_proxies 里的 %q 不是合法的 CIDR: %w", p, err)
			}
			continue
		}
		if net.ParseIP(p) == nil {
			return fmt.Errorf("server.trusted_proxies 里的 %q 不是合法的 IP 或 CIDR", p)
		}
	}
	return nil
}
