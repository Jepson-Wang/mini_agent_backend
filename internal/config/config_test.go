package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 正好 32 字节

// writeYAML 把内容写进临时文件并返回路径。
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时配置: %v", err)
	}
	return p
}

// clearEnv 清掉所有会影响 Load / LoadEnvFile 的环境变量。
// 不清的话,开发机上恰好设了 JWT_SECRET 时测试结果会跟着变。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CONFIG_PATH", "ENV_PATH", "APP_ENV", "HTTP_ADDR", "DB_DSN", "AGENT_ADDR",
		"JWT_ISSUER", "JWT_SECRET", "JWT_ACCESS_TTL",
	} {
		// 两步走。t.Setenv 是为了登记 cleanup,测试结束把原值还回去;
		// Unsetenv 才是真清 —— godotenv 用 LookupEnv 判断"已存在",
		// 只置空的话它会认为变量已经有人给了,跳过不写,新用例就假绿了。
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

const minimalYAML = `
database:
  dsn: "host=localhost dbname=x"
`

func TestLoad_YAMLAndDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	path := writeYAML(t, `
server:
  addr: ":9090"
database:
  dsn: "host=localhost dbname=x"
agent:
  addr: "localhost:60000"
jwt:
  issuer: "custom-issuer"
  access_ttl: "30m"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("Server.Addr = %q", cfg.Server.Addr)
	}
	if cfg.Agent.Addr != "localhost:60000" {
		t.Errorf("Agent.Addr = %q", cfg.Agent.Addr)
	}
	if cfg.JWT.Issuer != "custom-issuer" {
		t.Errorf("JWT.Issuer = %q", cfg.JWT.Issuer)
	}
	if cfg.JWT.AccessTTL != 30*time.Minute {
		t.Errorf("JWT.AccessTTL = %v, 期望 30m", cfg.JWT.AccessTTL)
	}
	if cfg.JWT.Secret != testSecret {
		t.Error("密钥应当来自 JWT_SECRET")
	}
}

func TestLoad_DefaultsWhenYAMLOmitsThem(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	cfg, err := Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("Server.Addr = %q, 期望默认值 :8080", cfg.Server.Addr)
	}
	if cfg.JWT.AccessTTL != 2*time.Hour {
		t.Errorf("JWT.AccessTTL = %v, 期望默认值 2h", cfg.JWT.AccessTTL)
	}
	if cfg.JWT.Issuer != "mini-agent" {
		t.Errorf("JWT.Issuer = %q, 期望默认值 mini-agent", cfg.JWT.Issuer)
	}
}

// 环境变量优先级最高,盖住 YAML。
func TestLoad_EnvOverridesYAML(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("HTTP_ADDR", ":7777")
	t.Setenv("DB_DSN", "host=from-env dbname=y")
	t.Setenv("JWT_ISSUER", "from-env")
	t.Setenv("JWT_ACCESS_TTL", "15m")

	cfg, err := Load(writeYAML(t, `
server:
  addr: ":9090"
database:
  dsn: "host=from-yaml dbname=x"
jwt:
  issuer: "from-yaml"
  access_ttl: "30m"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":7777" {
		t.Errorf("Server.Addr = %q, 期望 :7777", cfg.Server.Addr)
	}
	if cfg.Database.DSN != "host=from-env dbname=y" {
		t.Errorf("Database.DSN = %q", cfg.Database.DSN)
	}
	if cfg.JWT.Issuer != "from-env" {
		t.Errorf("JWT.Issuer = %q", cfg.JWT.Issuer)
	}
	if cfg.JWT.AccessTTL != 15*time.Minute {
		t.Errorf("JWT.AccessTTL = %v, 期望 15m", cfg.JWT.AccessTTL)
	}
}

// 密钥只能从环境变量来。写进 YAML 的必须被忽略,
// 否则"配置文件里不放密钥"这条约定就是空话。
func TestLoad_SecretNeverFromYAML(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	cfg, err := Load(writeYAML(t, `
database:
  dsn: "host=localhost dbname=x"
jwt:
  issuer: "mini-agent"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JWT.Secret != testSecret {
		t.Fatalf("JWT.Secret = %q, 期望环境变量的值", cfg.JWT.Secret)
	}
}

// 缺密钥必须启动失败,不能兜底随机生成 ——
// 那样服务每次重启所有人被登出,而且没人会发现漏配了。
func TestLoad_MissingSecret(t *testing.T) {
	clearEnv(t)

	_, err := Load(writeYAML(t, minimalYAML))
	if err == nil {
		t.Fatal("缺 JWT_SECRET 应当报错")
	}
	if !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("错误信息应当指出缺哪个变量,实际: %v", err)
	}
}

func TestLoad_ShortSecret(t *testing.T) {
	clearEnv(t)
	short := "tooshort"
	t.Setenv("JWT_SECRET", short)

	_, err := Load(writeYAML(t, minimalYAML))
	if err == nil {
		t.Fatal("过短的密钥应当报错")
	}
	// 这条错误会进启动日志,绝不能把密钥本身带出去。
	if strings.Contains(err.Error(), short) {
		t.Errorf("错误信息里出现了密钥内容: %v", err)
	}
}

// 配置项拼错要报出来,不能静默跑默认值。
func TestLoad_UnknownField(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	_, err := Load(writeYAML(t, `
database:
  dsn: "host=localhost dbname=x"
jwt:
  access_tt1: "30m"
`))
	if err == nil {
		t.Fatal("未知字段 access_tt1 应当报错")
	}
}

func TestLoad_BadTTLEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("JWT_ACCESS_TTL", "两小时")

	if _, err := Load(writeYAML(t, minimalYAML)); err == nil {
		t.Fatal("非法的 JWT_ACCESS_TTL 应当报错")
	}
}

// 容器里常常只给环境变量,没有配置文件。这种情况要能起来。
func TestLoad_MissingFileIsOK(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("DB_DSN", "host=localhost dbname=x")

	cfg, err := Load(filepath.Join(t.TempDir(), "不存在.yaml"))
	if err != nil {
		t.Fatalf("配置文件缺失不该报错: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("Server.Addr = %q, 期望默认值", cfg.Server.Addr)
	}
}

func TestLoad_MissingDSN(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	if _, err := Load(writeYAML(t, "server:\n  addr: \":8080\"\n")); err == nil {
		t.Fatal("缺 dsn 应当报错")
	}
}

func TestLoad_ZeroTTL(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	_, err := Load(writeYAML(t, `
database:
  dsn: "host=localhost dbname=x"
jwt:
  access_ttl: "0s"
`))
	if err == nil {
		t.Fatal("access_ttl 为 0 应当报错 —— 签出来的令牌立刻就过期")
	}
}

// CONFIG_PATH 应当在 path 为空时生效。
func TestLoad_ConfigPathEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	path := writeYAML(t, `
server:
  addr: ":6666"
database:
  dsn: "host=localhost dbname=x"
`)
	t.Setenv("CONFIG_PATH", path)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":6666" {
		t.Errorf("Server.Addr = %q, 期望 CONFIG_PATH 指向的文件生效", cfg.Server.Addr)
	}
}

// writeEnv 把内容写成临时 .env 并返回路径。
func writeEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时 .env: %v", err)
	}
	return p
}

// .env 里的键要能进到进程环境变量里,否则 os.Getenv 读不到,整件事就白做了。
// 这些键都在 clearEnv 的列表里,测试结束会被还原。
func TestLoadEnvFile_SetsVars(t *testing.T) {
	clearEnv(t)

	path := writeEnv(t, "JWT_SECRET="+testSecret+"\nJWT_ISSUER=from-dotenv\n")
	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}

	if got := os.Getenv("JWT_SECRET"); got != testSecret {
		t.Errorf("JWT_SECRET = %q, 期望来自 .env", got)
	}
	if got := os.Getenv("JWT_ISSUER"); got != "from-dotenv" {
		t.Errorf("JWT_ISSUER = %q, 期望 from-dotenv", got)
	}
}

// 真实环境变量压过 .env。这条是那个优先级约定的守门人 ——
// 反过来的话,线上误留一份 .env 就能盖掉编排系统注入的密钥。
func TestLoadEnvFile_DoesNotOverrideExisting(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_ISSUER", "from-shell")

	path := writeEnv(t, "JWT_ISSUER=from-dotenv\nJWT_SECRET="+testSecret+"\n")
	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}

	if got := os.Getenv("JWT_ISSUER"); got != "from-shell" {
		t.Errorf("JWT_ISSUER = %q, 已有的环境变量应当胜出", got)
	}
	// 没被占的键还是要正常灌进去。
	if got := os.Getenv("JWT_SECRET"); got != testSecret {
		t.Errorf("JWT_SECRET = %q, 期望来自 .env", got)
	}
}

// 线上本来就不该有 .env,变量由编排系统注入。缺文件必须能正常起来。
func TestLoadEnvFile_MissingFileIsOK(t *testing.T) {
	clearEnv(t)

	if err := LoadEnvFile(filepath.Join(t.TempDir(), "不存在.env")); err != nil {
		t.Fatalf(".env 缺失不该报错: %v", err)
	}
}

// ENV_PATH 应当在 path 为空时生效。
func TestLoadEnvFile_EnvPathEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("ENV_PATH", writeEnv(t, "JWT_ISSUER=from-env-path\n"))

	if err := LoadEnvFile(""); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("JWT_ISSUER"); got != "from-env-path" {
		t.Errorf("JWT_ISSUER = %q, 期望 ENV_PATH 指向的文件生效", got)
	}
}

// 仓库里那份 configs/config.yaml 必须是能解析的,而且不能含 jwt.secret。
func TestRepoConfigYAMLIsValid(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	path := filepath.Join("..", "..", "configs", "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读不到 %s: %v", path, err)
	}
	if strings.Contains(string(raw), "secret:") {
		t.Error("configs/config.yaml 里出现了 secret 字段,密钥不该进仓库")
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("仓库里的配置文件解析失败: %v", err)
	}
	if cfg.JWT.AccessTTL <= 0 {
		t.Errorf("access_ttl = %v", cfg.JWT.AccessTTL)
	}
}
