// Package config 加载运行配置。
//
// 四层优先级,后面的盖前面的:内置默认值 < YAML 文件 < .env 文件 < 真实环境变量。
// 密钥、口令这类东西只走环境变量,不进 YAML —— 配置文件是要提交到仓库的。
package config

import (
	"fmt"
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
	Reconcile Reconcile `yaml:"reconcile"`
	JWT       JWT       `yaml:"jwt"`
}

type Server struct {
	Addr string `yaml:"addr"`
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
		Reconcile: Reconcile{
			Interval:   time.Minute,
			StaleAfter: 15 * time.Minute,
			Batch:      100,
		},
		JWT: JWT{
			Issuer:    "mini-agent",
			AccessTTL: 2 * time.Hour,
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
	return c.Reconcile.validate()
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
