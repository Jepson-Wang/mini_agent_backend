// Package config 加载运行配置。
//
// 四层优先级,后面的盖前面的:内置默认值 < YAML 文件 < .env 文件 < 真实环境变量。
// 密钥、口令这类东西只走环境变量,不进 YAML —— 配置文件是要提交到仓库的。
package config

import (
	"fmt"
	"os"
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
	Server   Server   `yaml:"server"`
	Database Database `yaml:"database"`
	Agent    Agent    `yaml:"agent"`
	JWT      JWT      `yaml:"jwt"`
}

type Server struct {
	Addr string `yaml:"addr"`
}

type Database struct {
	// DSN 里带口令,别往 YAML 里写,用 DB_DSN 环境变量传。
	// 现在 config.yaml 里还留着一份开发用的明文,上生产前必须清掉。
	DSN string `yaml:"dsn"`
}

type Agent struct {
	Addr string `yaml:"addr"`
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
		Agent:  Agent{Addr: "localhost:50051"},
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
	setString(&c.JWT.Issuer, "JWT_ISSUER")
	setString(&c.JWT.Secret, "JWT_SECRET")

	if v := strings.TrimSpace(os.Getenv("JWT_ACCESS_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("JWT_ACCESS_TTL=%q 不是合法的时长(例:2h、30m): %w", v, err)
		}
		c.JWT.AccessTTL = d
	}
	return nil
}

func setString(dst *string, env string) {
	if v := os.Getenv(env); v != "" {
		*dst = v
	}
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
	return nil
}
