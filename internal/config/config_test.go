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
		"CONFIG_PATH", "ENV_PATH", "APP_ENV", "HTTP_ADDR", "DB_DSN",
		"AGENT_ADDR", "AGENT_CALLBACK_SECRET", "AGENT_GRPC_TOKEN", "AGENT_TASK_TIMEOUT",
		"AGENT_BREAKER_THRESHOLD", "AGENT_BREAKER_COOLDOWN",
		"RECONCILE_INTERVAL", "RECONCILE_STALE_AFTER", "RECONCILE_BATCH",
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

// 回调密钥不是必填 —— 缺了只是 /internal 那组接口用不了(一律 500),
// 不该把整个服务拦在门外。
func TestLoad_CallbackSecretOptional(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	cfg, err := Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("缺 AGENT_CALLBACK_SECRET 不该阻止启动: %v", err)
	}
	if cfg.Agent.CallbackSecret != "" {
		t.Errorf("Agent.CallbackSecret = %q, 期望空", cfg.Agent.CallbackSecret)
	}
}

// 但填了就得够长。"填了个 8 位的"比"没填"更危险 —— 前者会让人以为已经防住了。
func TestLoad_ShortCallbackSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	short := "tooshort"
	t.Setenv("AGENT_CALLBACK_SECRET", short)

	_, err := Load(writeYAML(t, minimalYAML))
	if err == nil {
		t.Fatal("过短的回调密钥应当报错")
	}
	// 这条会进启动日志。
	if strings.Contains(err.Error(), short) {
		t.Errorf("错误信息里出现了密钥内容: %v", err)
	}
}

// 两个密钥必须是两个值:一个泄露了不该顺带把另一个也交出去。
func TestLoad_CallbackSecretMustDifferFromJWT(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("AGENT_CALLBACK_SECRET", testSecret)

	_, err := Load(writeYAML(t, minimalYAML))
	if err == nil {
		t.Fatal("两个密钥相同应当报错")
	}
	if strings.Contains(err.Error(), testSecret) {
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

// ---- 熔断与回收器 ----

func TestLoad_BreakerAndReconcileDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	cfg, err := Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.BreakerThreshold != 5 {
		t.Errorf("BreakerThreshold = %d, 期望 5", cfg.Agent.BreakerThreshold)
	}
	if cfg.Agent.BreakerCooldown != 30*time.Second {
		t.Errorf("BreakerCooldown = %v, 期望 30s", cfg.Agent.BreakerCooldown)
	}
	if cfg.Reconcile.Interval != time.Minute {
		t.Errorf("Reconcile.Interval = %v, 期望 1m", cfg.Reconcile.Interval)
	}
	if cfg.Reconcile.StaleAfter != 15*time.Minute {
		t.Errorf("Reconcile.StaleAfter = %v, 期望 15m", cfg.Reconcile.StaleAfter)
	}
	if cfg.Reconcile.Batch != 100 {
		t.Errorf("Reconcile.Batch = %d, 期望 100", cfg.Reconcile.Batch)
	}
}

func TestLoad_BreakerAndReconcileFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("AGENT_BREAKER_THRESHOLD", "9")
	t.Setenv("AGENT_BREAKER_COOLDOWN", "45s")
	t.Setenv("RECONCILE_INTERVAL", "2m")
	t.Setenv("RECONCILE_STALE_AFTER", "20m")
	t.Setenv("RECONCILE_BATCH", "7")

	cfg, err := Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.BreakerThreshold != 9 || cfg.Agent.BreakerCooldown != 45*time.Second {
		t.Errorf("熔断参数没被环境变量覆盖: %+v", cfg.Agent)
	}
	if cfg.Reconcile.Interval != 2*time.Minute ||
		cfg.Reconcile.StaleAfter != 20*time.Minute || cfg.Reconcile.Batch != 7 {
		t.Errorf("回收器参数没被环境变量覆盖: %+v", cfg.Reconcile)
	}
}

// 环境变量写错了要报错,不能静默忽略 —— 那会让人以为自己改了配置,
// 而服务其实还跑着默认值。
func TestLoad_BadBreakerAndReconcileEnv(t *testing.T) {
	for _, tc := range []struct{ env, val string }{
		{"AGENT_BREAKER_THRESHOLD", "五次"},
		{"AGENT_BREAKER_COOLDOWN", "30秒"},
		{"RECONCILE_INTERVAL", "1min"}, // 合法写法是 1m
		{"RECONCILE_BATCH", "一百"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("JWT_SECRET", testSecret)
			t.Setenv(tc.env, tc.val)

			if _, err := Load(writeYAML(t, minimalYAML)); err == nil {
				t.Errorf("%s=%q 该报错", tc.env, tc.val)
			}
		})
	}
}

// 启用熔断却把冷却配成 0:打开之后下一次调用立刻又放行,等于没熔断。
// 这种"看起来启用了、实际什么也没做"的配置比明确关掉更危险。
func TestLoad_BreakerEnabledNeedsCooldown(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("AGENT_BREAKER_COOLDOWN", "0s")

	if _, err := Load(writeYAML(t, minimalYAML)); err == nil {
		t.Fatal("启用熔断但冷却为 0 应当报错")
	}
}

// 关掉熔断是合法的部署选择(本地开发),不该顺带校验冷却。
func TestLoad_BreakerDisabledSkipsCooldownCheck(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("AGENT_BREAKER_THRESHOLD", "0")
	t.Setenv("AGENT_BREAKER_COOLDOWN", "0s")

	if _, err := Load(writeYAML(t, minimalYAML)); err != nil {
		t.Fatalf("关掉熔断不该报错: %v", err)
	}
}

// TestLoad_StaleAfterMustExceedInterval 挡的是两个值写反。
//
// 判定窗口比扫描间隔还短的话,一个刚建出来、还没来得及跑的任务
// 会在下一轮就被判成"卡住了"并标记失败 —— 而且是**所有**任务,
// 上线之后没人会立刻反应过来是配置的问题。
func TestLoad_StaleAfterMustExceedInterval(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("RECONCILE_INTERVAL", "15m")
	t.Setenv("RECONCILE_STALE_AFTER", "1m")

	if _, err := Load(writeYAML(t, minimalYAML)); err == nil {
		t.Fatal("stale_after 小于 interval 应当报错")
	}
}

// interval <= 0 表示关掉回收器,这时其余字段没意义,不该拦。
func TestLoad_ReconcileDisabledSkipsChecks(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("RECONCILE_INTERVAL", "0s")
	t.Setenv("RECONCILE_STALE_AFTER", "0s")
	t.Setenv("RECONCILE_BATCH", "0")

	cfg, err := Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("关掉回收器不该报错: %v", err)
	}
	if cfg.Reconcile.Interval > 0 {
		t.Errorf("Reconcile.Interval = %v, 期望 <= 0", cfg.Reconcile.Interval)
	}
}

// ---------------------------------------------------------------------------
// AGENT_GRPC_TOKEN:调 Agent 时带的凭证。和 CallbackSecret 方向相反。
// ---------------------------------------------------------------------------

// 不填不该拦住启动:Agent 绑在回环地址上(它的默认值)时本来就不鉴权。
func TestLoad_GRPCTokenOptional(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	cfg, err := Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("缺 AGENT_GRPC_TOKEN 不该阻止启动: %v", err)
	}
	if cfg.Agent.Token != "" {
		t.Errorf("Agent.Token = %q, 期望空", cfg.Agent.Token)
	}
}

// 但填了就得够长,理由同 CallbackSecret。
func TestLoad_ShortGRPCToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)
	short := "tooshort"
	t.Setenv("AGENT_GRPC_TOKEN", short)

	_, err := Load(writeYAML(t, minimalYAML))
	if err == nil {
		t.Fatal("过短的 gRPC 凭证应当报错")
	}
	// 这条会进启动日志。
	if strings.Contains(err.Error(), short) {
		t.Errorf("错误信息里出现了密钥内容: %v", err)
	}
}

// 三把密钥两两不同。共用一个的话,任何一条路径泄露就等于三条全丢 ——
// 而它们的暴露面完全不一样(JWT 发给每个用户,另外两个只在服务之间走)。
func TestLoad_GRPCTokenMustDifferFromOtherSecrets(t *testing.T) {
	other := "another-32-byte-long-secret-value-xx"

	for _, tc := range []struct{ name, jwt, callback, token string }{
		{"和 JWT_SECRET 相同", testSecret, other, testSecret},
		{"和 AGENT_CALLBACK_SECRET 相同", testSecret, other, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("JWT_SECRET", tc.jwt)
			t.Setenv("AGENT_CALLBACK_SECRET", tc.callback)
			t.Setenv("AGENT_GRPC_TOKEN", tc.token)

			_, err := Load(writeYAML(t, minimalYAML))
			if err == nil {
				t.Fatal("密钥重复使用应当报错")
			}
			if strings.Contains(err.Error(), tc.token) {
				t.Errorf("错误信息里出现了密钥内容: %v", err)
			}
		})
	}
}

// 和 JWT.Secret 一样:哪怕有人往 YAML 里写了,也不会被加载进来。
func TestLoad_GRPCTokenNeverFromYAML(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", testSecret)

	// yaml:"-" 的字段在 Strict 模式下是未知字段,写了会直接报错 ——
	// 这本身就是我们要的:悄悄忽略比报错更糟。
	_, err := Load(writeYAML(t, minimalYAML+"\nagent:\n  token: leaked-from-yaml\n"))
	if err == nil {
		t.Fatal("往 YAML 里写 agent.token 应当被 Strict 模式拒绝")
	}
}

// 密钥要走 HTTP 头 / gRPC metadata,非 ASCII 会让每一次调用都在传输层被拒,
// 而启动时看不出任何异常 —— 必须在配置校验就挡下来。
//
// 实测过的失败形态:一个 40 汉字的 AGENT_GRPC_TOKEN(len()=120 字节,长度这关
// 轻松通过)会让 gRPC 客户端连请求都发不出去,报
// "Metadata value for key x-agent-token is invalid: Illegal header value",
// 客户端侧表现为 INTERNAL,日志里没有任何东西指向密钥格式。
func TestLoad_WireSecretsMustBeASCII(t *testing.T) {
	const (
		// 不能和 testSecret 取同一个值 —— 三把密钥必须两两不同,
		// 否则这条用例会被那条规则拦下,看起来像 ASCII 检查误伤了。
		ok      = "ZZZZ56789abcdef0123456789abcdefZ"
		chinese = "密钥密钥密钥密钥密钥密钥密钥密钥密钥密钥"             // 20 字 = 60 字节,长度够
		spaced  = "0123456789abcdef 0123456789abcde" // 含空格:头部里合法但会被修剪吃掉
	)

	for _, tc := range []struct{ name, env, val string }{
		{"回调密钥含中文", "AGENT_CALLBACK_SECRET", chinese},
		{"gRPC 凭证含中文", "AGENT_GRPC_TOKEN", chinese},
		{"回调密钥含空格", "AGENT_CALLBACK_SECRET", spaced},
		{"gRPC 凭证含空格", "AGENT_GRPC_TOKEN", spaced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("JWT_SECRET", testSecret)
			t.Setenv(tc.env, tc.val)

			_, err := Load(writeYAML(t, minimalYAML))
			if err == nil {
				t.Fatal("非可见 ASCII 的密钥应当在启动时就被拒绝")
			}
			// 这条会进启动日志。
			if strings.Contains(err.Error(), tc.val) {
				t.Errorf("错误信息里出现了密钥内容: %v", err)
			}
		})
	}

	// 反面:纯 ASCII 的照常放行,别把正常配置也误伤了。
	t.Run("纯 ASCII 放行", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("JWT_SECRET", testSecret)
		t.Setenv("AGENT_GRPC_TOKEN", ok)
		if _, err := Load(writeYAML(t, minimalYAML)); err != nil {
			t.Fatalf("合法的 ASCII 凭证不该被拒: %v", err)
		}
	})
}

// JWT_SECRET 故意**不**受这条规则约束:它只用来签名,从不出现在任何
// 头部或 metadata 里,限制它的字符集没有收益,却会打断已有部署。
func TestLoad_JWTSecretMayBeNonASCII(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", "密钥密钥密钥密钥密钥密钥密钥密钥密钥密钥")

	if _, err := Load(writeYAML(t, minimalYAML)); err != nil {
		t.Fatalf("JWT_SECRET 允许非 ASCII: %v", err)
	}
}

