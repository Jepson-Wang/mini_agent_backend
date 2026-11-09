package middleware_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ratelimit"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

// fakeLimiter 记录收到的桶,返回预设结果。
type fakeLimiter struct {
	mu    sync.Mutex
	calls [][]ratelimit.Bucket
	res   ratelimit.Result
	err   error
}

func (f *fakeLimiter) Allow(_ context.Context, b []ratelimit.Bucket) (ratelimit.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, b)
	return f.res, f.err
}

func (f *fakeLimiter) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// keyOf 取最近一次调用里 scope 对应的桶键,没有则返回空串。
func (f *fakeLimiter) keyOf(t *testing.T, scope string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("限流器一次都没被调用")
	}
	for _, b := range f.calls[len(f.calls)-1] {
		if b.Scope == scope {
			return b.Key
		}
	}
	return ""
}

func allow() *fakeLimiter { return &fakeLimiter{res: ratelimit.Result{Allowed: true}} }

var testLimits = middleware.AuthLimits{
	Global:   ratelimit.Rule{Window: time.Minute, Limit: 100},
	IP:       ratelimit.Rule{Window: time.Minute, Limit: 10},
	Email:    ratelimit.Rule{Window: time.Minute, Limit: 5},
	EmailKey: middleware.DeriveEmailKey("0123456789abcdef0123456789abcdef"),
}

// rlEngine 装登录/注册两条路由,handler 记录是否被执行以及读到的 body。
func rlEngine(l middleware.Limiter, limits middleware.AuthLimits, reached *bool, gotBody *[]byte) *gin.Engine {
	r := gin.New()
	_ = r.SetTrustedProxies(nil)
	r.Use(middleware.TraceID())
	g := r.Group("/api/v1/auth")
	g.Use(middleware.AuthRateLimit(l, limits))
	h := func(c *gin.Context) {
		*reached = true
		b, _ := io.ReadAll(c.Request.Body)
		if gotBody != nil {
			*gotBody = b
		}
		c.String(http.StatusOK, "ok")
	}
	g.POST("/login", h)
	g.POST("/register", h)
	return r
}

func post(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-Trace-ID", "fixedtrace")
	req.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const loginPath = "/api/v1/auth/login"

func TestAuthRateLimit_AllowedPassesThrough(t *testing.T) {
	var reached bool
	l := allow()
	w := post(rlEngine(l, testLimits, &reached, nil), loginPath, `{"email":"a@b.com","password":"x"}`)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("status=%d reached=%v", w.Code, reached)
	}
	if l.n() != 1 {
		t.Fatalf("Allow 调用 %d 次, 期望 1", l.n())
	}
	for _, s := range []string{"login_global", "ip", "email"} {
		if l.keyOf(t, s) == "" {
			t.Errorf("缺少 %s 维度的桶", s)
		}
	}
}

func TestAuthRateLimit_RejectedCarriesRetryAfter(t *testing.T) {
	var reached bool
	l := &fakeLimiter{res: ratelimit.Result{Allowed: false, RetryAfter: 1500 * time.Millisecond, Scope: "ip"}}
	w := post(rlEngine(l, testLimits, &reached, nil), loginPath, `{"email":"a@b.com"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, 期望 429", w.Code)
	}
	if got := decode(t, w); got.Code != response.CodeRateLimited {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeRateLimited)
	}
	// 用 Result() 的快照:Recorder 的 Header() 是活 map,写出后再设的头也读得到,
	// 而真实连接上那样的头根本发不出去。
	if got := w.Result().Header.Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, 期望 2 (1.5s 向上取整)", got)
	}
	if reached {
		t.Fatal("被限流的请求进了 handler")
	}
}

func TestAuthRateLimit_RejectionDoesNotRevealDimension(t *testing.T) {
	run := func(scope string) *httptest.ResponseRecorder {
		var reached bool
		l := &fakeLimiter{res: ratelimit.Result{RetryAfter: time.Minute, Scope: scope}}
		return post(rlEngine(l, testLimits, &reached, nil), loginPath, `{"email":"a@b.com"}`)
	}
	a, b := run("ip"), run("email")
	if a.Code != b.Code {
		t.Fatalf("状态码不同: %d vs %d", a.Code, b.Code)
	}
	if a.Body.String() != b.Body.String() {
		t.Fatalf("响应体不同:\n%s\n%s", a.Body.String(), b.Body.String())
	}
	if !reflect.DeepEqual(a.Header(), b.Header()) {
		t.Fatalf("响应头不同:\n%v\n%v", a.Header(), b.Header())
	}
	for name := range a.Header() {
		if strings.Contains(strings.ToLower(name), "ratelimit") {
			t.Errorf("出现了余额类响应头 %q", name)
		}
	}
}

func TestAuthRateLimit_RedisErrorFailsOpen(t *testing.T) {
	var reached bool
	l := &fakeLimiter{err: errors.New("redis 挂了")}
	w := post(rlEngine(l, testLimits, &reached, nil), loginPath, `{"email":"a@b.com"}`)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("status=%d reached=%v, 期望 fail-open 放行", w.Code, reached)
	}
}

func TestAuthRateLimit_FailOpenIsCounted(t *testing.T) {
	var reached bool
	l := &fakeLimiter{err: errors.New("redis 挂了")}
	before := middleware.RateLimitFailOpenCount()
	post(rlEngine(l, testLimits, &reached, nil), loginPath, `{"email":"a@b.com"}`)
	post(rlEngine(l, testLimits, &reached, nil), loginPath, `{"email":"a@b.com"}`)
	if got := middleware.RateLimitFailOpenCount(); got != before+2 {
		t.Fatalf("计数 = %d, 期望 %d", got, before+2)
	}
}

// 本文件最重要的一条:超长体必须被拒,而不是跳过邮箱桶放行。
func TestAuthRateLimit_OversizedBodyIsRejected(t *testing.T) {
	var reached bool
	l := allow()
	body := `{"email":"victim@example.com","password":"hunter2hunter2","junk":"` + strings.Repeat("x", 5<<10) + `"}`
	w := post(rlEngine(l, testLimits, &reached, nil), loginPath, body)
	if reached {
		t.Fatal("超长体进了 handler —— 攻击者可以用字节数关掉邮箱限流")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400", w.Code)
	}
	if got := decode(t, w); got.Code != response.CodeInvalidParam {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInvalidParam)
	}
}

func TestAuthRateLimit_BodyIsRestoredForHandler(t *testing.T) {
	var reached bool
	var got []byte
	body := `{"email":"a@b.com","password":"secret123"}`
	post(rlEngine(allow(), testLimits, &reached, &got), loginPath, body)
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("handler 读到 %q, 期望 %q", got, body)
	}
}

func TestAuthRateLimit_MalformedJSONReachesHandler(t *testing.T) {
	var reached bool
	l := allow()
	w := post(rlEngine(l, testLimits, &reached, nil), loginPath, `{"email":`)
	if !reached || w.Code != http.StatusOK {
		t.Fatalf("reached=%v status=%d, 坏 JSON 应该交给 handler 处理", reached, w.Code)
	}
	if l.keyOf(t, "email") != "" {
		t.Error("解析不出邮箱却建了邮箱桶")
	}
}

func TestAuthRateLimit_UsesRemoteAddrNotXFF(t *testing.T) {
	var reached bool
	l := allow()
	r := rlEngine(l, testLimits, &reached, nil)
	var keys []string
	for _, xff := range []string{"1.1.1.1", "2.2.2.2"} {
		req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(`{"email":"a@b.com"}`))
		req.RemoteAddr = "203.0.113.7:5555"
		req.Header.Set("X-Forwarded-For", xff)
		r.ServeHTTP(httptest.NewRecorder(), req)
		keys = append(keys, l.keyOf(t, "ip"))
	}
	if keys[0] != keys[1] {
		t.Fatalf("XFF 改变了 ip 桶: %q vs %q", keys[0], keys[1])
	}
	if keys[0] != "rl:{login}:ip:203.0.113.7" {
		t.Fatalf("ip 桶键 = %q", keys[0])
	}
}

func TestAuthRateLimit_EmailNormalizedBeforeKeying(t *testing.T) {
	var reached bool
	l := allow()
	r := rlEngine(l, testLimits, &reached, nil)
	post(r, loginPath, `{"email":" Alice@Example.COM "}`)
	k1 := l.keyOf(t, "email")
	post(r, loginPath, `{"email":"alice@example.com"}`)
	k2 := l.keyOf(t, "email")
	if k1 == "" || k1 != k2 {
		t.Fatalf("规范化前后的桶键不同: %q vs %q", k1, k2)
	}
}

func TestAuthRateLimit_EmailKeyIsHMACNotPlaintext(t *testing.T) {
	const email = "alice@example.com"
	var reached bool
	l := allow()
	post(rlEngine(l, testLimits, &reached, nil), loginPath, `{"email":"`+email+`"}`)
	k1 := l.keyOf(t, "email")

	sum := sha256.Sum256([]byte(email))
	plain := hex.EncodeToString(sum[:])
	if strings.Contains(k1, email) || strings.Contains(k1, "alice") {
		t.Errorf("桶键含邮箱明文: %q", k1)
	}
	if strings.Contains(k1, plain[:32]) {
		t.Errorf("桶键是裸 SHA-256: %q", k1)
	}
	if !strings.HasPrefix(k1, "rl:{login}:email:") || len(strings.TrimPrefix(k1, "rl:{login}:email:")) != 32 {
		t.Errorf("桶键形状不对: %q", k1)
	}

	other := testLimits
	other.EmailKey = middleware.DeriveEmailKey("ffffffffffffffffffffffffffffffff")
	post(rlEngine(l, other, &reached, nil), loginPath, `{"email":"`+email+`"}`)
	if k2 := l.keyOf(t, "email"); k2 == k1 {
		t.Error("换了 EmailKey 桶键没变")
	}
}

func TestAuthRateLimit_LoginAndRegisterHaveSeparateGlobalBuckets(t *testing.T) {
	var reached bool
	l := allow()
	r := rlEngine(l, testLimits, &reached, nil)
	post(r, "/api/v1/auth/login", `{"email":"a@b.com"}`)
	kl := l.keyOf(t, "login_global")
	post(r, "/api/v1/auth/register", `{"email":"a@b.com"}`)
	kr := l.keyOf(t, "login_global")
	if kl != "rl:{login}:global:login" || kr != "rl:{login}:global:register" {
		t.Fatalf("全局桶键 = %q / %q", kl, kr)
	}
}

// chatEngine 装一条聊天路由。withAuth 为真时用真正的 Auth 中间件挂一个假校验器,
// 这样 userID 走的是生产里同一条 c.Set 路径。
func chatEngine(l middleware.Limiter, withAuth bool, uid int64, reached *bool, gotBody *[]byte) *gin.Engine {
	r := gin.New()
	r.Use(middleware.TraceID())
	if withAuth {
		r.Use(middleware.Auth(fakeVerifier{uid: uid}, nil))
	}
	r.Use(middleware.ChatRateLimit(l, middleware.ChatLimits{
		Global: ratelimit.Rule{Window: time.Minute, Limit: 100},
		User:   ratelimit.Rule{Window: time.Minute, Limit: 5},
	}))
	r.POST("/chat", func(c *gin.Context) {
		*reached = true
		if gotBody != nil {
			*gotBody, _ = io.ReadAll(c.Request.Body)
		}
		c.String(http.StatusOK, "ok")
	})
	return r
}

type fakeVerifier struct{ uid int64 }

func (f fakeVerifier) Verify(context.Context, string) (int64, error) { return f.uid, nil }

func chatPost(r *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestChatRateLimit_KeyedByUserID(t *testing.T) {
	var reached bool
	l := allow()
	chatPost(chatEngine(l, true, 42, &reached, nil), `{}`)
	if got := l.keyOf(t, "user"); got != "rl:{chat}:user:42" {
		t.Fatalf("用户桶键 = %q", got)
	}
	if got := l.keyOf(t, "chat_global"); got != "rl:{chat}:global" {
		t.Fatalf("全局桶键 = %q", got)
	}
	if !reached {
		t.Fatal("放行后 handler 没执行")
	}
}

func TestChatRateLimit_MissingUserIDFailsClosed(t *testing.T) {
	var reached bool
	l := allow()
	w := chatPost(chatEngine(l, false, 0, &reached, nil), `{}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, 期望 500", w.Code)
	}
	if reached {
		t.Fatal("没挂 Auth 却放行进了 handler")
	}
	if l.n() != 0 {
		t.Fatal("取不到用户时不该调用限流器")
	}
}

func TestChatRateLimit_DoesNotReadBody(t *testing.T) {
	var reached bool
	var got []byte
	body := strings.Repeat("y", 8<<10) // 比 4KiB 上限大,读了就会截断或拒绝
	chatPost(chatEngine(allow(), true, 1, &reached, &got), body)
	if string(got) != body {
		t.Fatalf("handler 读到 %d 字节, 期望 %d", len(got), len(body))
	}
}

func TestDeriveEmailKey_DiffersFromSecretAndIsStable(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	a, b := middleware.DeriveEmailKey(secret), middleware.DeriveEmailKey(secret)
	if !bytes.Equal(a, b) {
		t.Fatal("同一密钥派生结果不稳定")
	}
	if len(a) != 32 {
		t.Fatalf("长度 = %d, 期望 32", len(a))
	}
	if bytes.Equal(a, []byte(secret)) || bytes.Contains(a, []byte(secret)) {
		t.Fatal("派生结果等于或包含原密钥")
	}
	if bytes.Equal(a, middleware.DeriveEmailKey(secret+"x")) {
		t.Fatal("不同密钥派生出相同结果")
	}
}
