package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/handler"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/sse"
)

// sseSID 是这条流对应的会话。
const sseSID = "01J8ZQXK7P2222222222222222"

// sseOwner 是这个会话的主人(user id)。
const sseOwner = 7

// sseNilServiceCalls 记「在 nil 接收者上被调用过」的次数。
//
// 用来区分两件在响应体上**一模一样**的事:
//   - handler 干净地挡住了没接好的依赖(回 50000 + 一条「未注入 service」的日志);
//   - handler 放它过去,依赖在 nil 指针上炸,被 Recovery 兜成 50000。
//
// 两者的响应体完全一样(Recovery 和 notWired 都调 response.Fail(CodeInternal)),
// 光看响应分不出来 —— 所以只能从「有没有真的调到那个依赖」上分。
var sseNilServiceCalls atomic.Int32

// fakeSSEStream 是 handler.SSEStreamService 的假实现。
//
// 它按 afterID 过滤、按 limit 截断,行为和仓库层一致 ——
// 续传那条用例靠的就是这个语义。
type fakeSSEStream struct {
	mu sync.Mutex

	// ownerUserID 是会话的主人。别的人来访问一律 NotFound。
	ownerUserID int64
	// ownErr 非空时归属校验直接返回它(模拟仓库层故障)。
	ownErr error
	// listErr 非空时读事件直接返回它。
	listErr error

	// brokenRepo 模拟「仓库层的增量读坏了」:无视 afterID,总是返回最早那条。
	//
	// 用来验那个「游标没推进就停」的守卫。没有它,仓库一坏就是查库死循环。
	brokenRepo bool

	events []model.TaskEvent

	// listCalls 记 ListEvents 被调了几次。
	//
	// 用来看**查询次数**而不只是查询结果 —— 兜底轮询的开销模型是
	// 「连接数 × 频率」,它是个性能特征,不看次数看不出来。
	listCalls int
}

// listCount 返回 ListEvents 的调用次数。
func (f *fakeSSEStream) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeSSEStream) MustOwnSession(_ context.Context, userID int64, sessionID string) error {
	if f == nil {
		// 走到这里说明 handler 没能挡住「接口里装着一个 nil 指针」这种接线错误,
		// 于是依赖在 nil 指针上被调了 —— 那正是要防的事。
		sseNilServiceCalls.Add(1)
		return errFake
	}
	if f.ownErr != nil {
		return f.ownErr
	}
	if sessionID != sseSID {
		return model.ErrSessionNotFound
	}
	if userID != f.ownerUserID {
		// 不是你的 —— 必须和「不存在」是同一个错误。
		return model.ErrSessionNotFound
	}
	return nil
}

func (f *fakeSSEStream) ListEvents(_ context.Context, sessionID string,
	afterID int64, limit int) ([]model.TaskEvent, error) {

	if f == nil {
		sseNilServiceCalls.Add(1)
		return nil, errFake
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []model.TaskEvent
	for _, ev := range f.events {
		if ev.SessionID != sessionID {
			continue
		}
		if !f.brokenRepo && ev.ID <= afterID {
			continue
		}
		out = append(out, ev)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// add 追加一条事件。
func (f *fakeSSEStream) add(ev model.TaskEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

// lockedRecorder 给 httptest.ResponseRecorder 加一把锁。
//
// httptest.ResponseRecorder **不是并发安全的**:SSE handler 在一个 goroutine 里写,
// 测试在另一个 goroutine 里读,直接用会 data race。
type lockedRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func (r *lockedRecorder) Header() http.Header { return r.rec.Header() }

func (r *lockedRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.Write(p)
}

func (r *lockedRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.WriteHeader(code)
}

func (r *lockedRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.Flush()
}

func (r *lockedRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.Body.String()
}

func (r *lockedRecorder) code() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.Code
}

func (r *lockedRecorder) header() http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.Header().Clone()
}

// sseConn 是一条正在跑的 SSE 连接。
type sseConn struct {
	rec    *lockedRecorder
	done   chan struct{}
	cancel context.CancelFunc
}

// waitBody 等到响应体里出现 want 为止。
func (c *sseConn) waitBody(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.rec.body(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %q 超时,实际收到:\n%s", want, c.rec.body())
}

// waitHub 等到 hub 里的订阅数变成 want。
func waitHub(t *testing.T, hub *sse.Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Count() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待订阅数变成 %d 超时,当前 %d", want, hub.Count())
}

// close 断开连接,并等 handler 真的退出。
func (c *sseConn) close(t *testing.T) {
	t.Helper()
	c.cancel()
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE handler 没有在客户端断开后退出")
	}
}

// sseCfg 是测试用的 SSE 配置:心跳和轮询都调短,免得用例干等。
func sseCfg() config.SSE {
	return config.SSE{
		Enabled:           true,
		PollInterval:      20 * time.Millisecond,
		Heartbeat:         40 * time.Millisecond,
		MaxConnPerSession: 2,
		MaxConnTotal:      100,
		MaxConnPerUser:    20,
		ReplayLimit:       200,
		MaxReplayPerDrain: 1000,
	}
}

// sseRouter 装一条带 SSE 的路由。
func sseRouter(svc handler.SSEStreamService, hub *sse.Hub, cfg config.SSE,
	shutdown <-chan struct{}) *gin.Engine {

	return handler.NewRouter(handler.Deps{
		SSEStream: svc,
		SSEHub:    hub,
		SSEConfig: cfg,
		Shutdown:  shutdown,
		Verifier:  fakeTokenVerifier{userID: sseOwner},
	})
}

// connect 开一条 SSE 连接。
//
// 请求在另一个 goroutine 里跑:SSE handler 不会自己结束,
// 只有客户端断开(cancel)才会。
func connect(t *testing.T, r *gin.Engine, path string, headers map[string]string) *sseConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer whatever")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := &lockedRecorder{rec: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(rec, req)
	}()
	return &sseConn{rec: rec, done: done, cancel: cancel}
}

// sseFrame 是一条解析出来的 SSE 帧。
type sseFrame struct {
	ID    string
	Event string
	Data  string
	// Comment 是注释行(心跳)。有它就说明这一帧是心跳。
	Comment string
}

// parseSSE 把响应体拆成帧。
//
// 帧之间是空行,末尾也是空行 —— 按 "\n\n" 切正好。
func parseSSE(body string) []sseFrame {
	var out []sseFrame
	for _, chunk := range strings.Split(body, "\n\n") {
		if chunk == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(chunk, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				f.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, ":"):
				f.Comment = line
			}
		}
		out = append(out, f)
	}
	return out
}

// eventFrame 找出第一条带指定事件名的帧。
func eventFrame(t *testing.T, body, event string) sseFrame {
	t.Helper()
	for _, f := range parseSSE(body) {
		if f.Event == event {
			return f
		}
	}
	t.Fatalf("没有找到 event=%s 的帧,实际收到:\n%s", event, body)
	return sseFrame{}
}

func sseEvent(id int64, typ, payload string) model.TaskEvent {
	return model.TaskEvent{
		ID:        id,
		TaskUID:   "01J8ZQXK7P3333333333333333",
		SessionID: sseSID,
		Seq:       id,
		Type:      typ,
		Payload:   payload,
		CreatedAt: time.Now(),
	}
}

// ---- 握手阶段:失败必须走统一外壳 ----

// 没带令牌就是 401,而且 service 一次都不能被调用。
func TestSSE_RequiresAuth(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := decode(t, w); got.Code != response.CodeUnauthorized {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeUnauthorized, w.Body.String())
	}
	if hub.Count() != 0 {
		t.Errorf("未认证的请求不该留下订阅,当前 %d", hub.Count())
	}
}

// **别人的会话 → 404,不是 403。**
//
// 分开的话这个接口就成了 session_id 探测器:拿到 403 说明这个 ID 真实存在。
func TestSSE_OtherUsersSessionLooksMissing(t *testing.T) {
	// 路由上的令牌代表 sseOwner,而会话的主人不是他。
	svc := &fakeSSEStream{ownerUserID: 999}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := decode(t, w)
	if got.Code != response.CodeNotFound {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeNotFound, w.Body.String())
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("HTTP 状态 = %d, 期望 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "403") {
		t.Errorf("响应里出现了 403: %s", w.Body.String())
	}
	if hub.Count() != 0 {
		t.Errorf("归属校验没过就不该留下订阅,当前 %d", hub.Count())
	}
}

// session_id 格式不对 → 40000,统一外壳。
func TestSSE_BadSessionID(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/xxx/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := decode(t, w); got.Code != response.CodeInvalidParam {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeInvalidParam, w.Body.String())
	}
}

// 依赖没注入 → 500,不是 404,也不能 panic。
//
// 路由照常注册是有意的:"接口不存在"和"接口没接好"是两回事。
func TestSSE_NilServiceFailsClosed(t *testing.T) {
	r := handler.NewRouter(handler.Deps{
		SSEHub:    sse.NewHub(),
		SSEConfig: sseCfg(),
		Verifier:  fakeTokenVerifier{userID: sseOwner},
	}) // SSEStream 为 nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := decode(t, w); got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeInternal, w.Body.String())
	}
}

// hub 没注入 → 500。hub 没了连接数上限就算不出来,那等于没有上限。
func TestSSE_NilHubFailsClosed(t *testing.T) {
	r := handler.NewRouter(handler.Deps{
		SSEStream: &fakeSSEStream{ownerUserID: sseOwner},
		SSEConfig: sseCfg(),
		Verifier:  fakeTokenVerifier{userID: sseOwner},
	}) // SSEHub 为 nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := decode(t, w); got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeInternal, w.Body.String())
	}
}

// 功能被关掉 → 503,不是 404。
//
// 前端拿到 503 会退回轮询;404 会被当成「接口不存在」,那是误导。
func TestSSE_DisabledReturns503(t *testing.T) {
	cfg := sseCfg()
	cfg.Enabled = false
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	r := sseRouter(svc, sse.NewHub(), cfg, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := decode(t, w)
	// ★ 用自己的码(50303),不是 50300「执行器暂时不可用」。
	//
	// 客户端按业务码做处置时必须能分开:这里该**改用轮询**,
	// 而 50300 会让它以为该等执行器恢复,反复重试一条根本不存在的路。
	if got.Code != response.CodeSSEDisabled {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeSSEDisabled, w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("HTTP 状态 = %d, 期望 503", w.Code)
	}
}

// ---- 流阶段 ----

// 自己的会话 → 200,响应头是 text/event-stream,帧格式完整。
func TestSSE_StreamsEventsWithSSEHeaders(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	svc.add(sseEvent(7, "thinking", "我在想想"))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	conn.waitBody(t, "id: 7")

	if conn.rec.code() != http.StatusOK {
		t.Errorf("HTTP 状态 = %d, 期望 200", conn.rec.code())
	}
	hd := conn.rec.header()
	if got := hd.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if hd.Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control = %q", hd.Get("Cache-Control"))
	}
	// X-Accel-Buffering 少了这条,nginx 会攒着一起发,实时性就没了。
	if hd.Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering = %q, 期望 no", hd.Get("X-Accel-Buffering"))
	}

	body := conn.rec.body()
	frame := eventFrame(t, body, "thinking")

	// 帧的形状:id / event / data 三行齐全。
	if frame.ID != "7" {
		t.Errorf("帧的 id = %q, 期望 7", frame.ID)
	}
	if frame.Event != "thinking" {
		t.Errorf("帧的 event = %q, 期望 thinking", frame.Event)
	}
	if !strings.HasPrefix(frame.Data, "{") {
		t.Errorf("帧的 data 不是 JSON: %q", frame.Data)
	}

	// data 里的字段。
	var payload struct {
		TaskUID   string `json:"task_uid"`
		SessionID string `json:"session_id"`
		Seq       int64  `json:"seq"`
		Type      string `json:"type"`
		Payload   string `json:"payload"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		t.Fatalf("data 不是合法 JSON: %v (%s)", err, frame.Data)
	}
	if payload.TaskUID == "" || payload.SessionID != sseSID {
		t.Errorf("data 里的标识不对: %+v", payload)
	}
	if payload.Type != "thinking" || payload.Payload != "我在想想" {
		t.Errorf("data 里的内容不对: %+v", payload)
	}
	if payload.CreatedAt == "" {
		t.Error("data 里缺 created_at")
	}

	// 帧尾必须是恰好两个换行。
	if !strings.Contains(body, "data: "+frame.Data+"\n\n") {
		t.Errorf("帧尾不是两个换行: %q", body)
	}
}

// 帧的 id 行等于事件的自增 id —— 客户端靠它做续传游标。
func TestSSE_FrameIDIsEventID(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	svc.add(sseEvent(41, "tool_call", `{"name":"read_file"}`))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	conn.waitBody(t, "id: 41")
	if frame := eventFrame(t, conn.rec.body(), "tool_call"); frame.ID != "41" {
		t.Errorf("帧的 id = %q, 期望 41", frame.ID)
	}
}

// 连接建好之后来的事件,要被推出去。
func TestSSE_PushesNewEvents(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	// 等订阅登记上,否则事件可能在订阅之前就到了。
	waitHub(t, hub, 1)

	svc.add(sseEvent(9, "thinking", "新的进度"))
	hub.Notify(sseSID) // 模拟「有人写了新事件」

	conn.waitBody(t, "id: 9")
}

// 收到唤醒信号之后,兜底轮询的计时要被重置。
//
// 兜底轮询是**每连接**一次查询,连接一多就是「连接数 × 频率」的常驻开销
// (1000 条连接、1 秒一轮 ≈ 1000 QPS)。而收到信号时我们**已经**查过库了,
// 那一轮不需要立刻再补一次 —— 把计时推后,信号密的时候兜底轮询就几乎不触发,
// 它只在信号真的丢了(Redis 抖动、副本重启)时才顶上来。
//
// 这条测的是**查询次数**而不是查询结果:结果对不对由别的用例管,
// 这里管的是「有没有白查」。没有它,这个优化会被当成无用代码删掉 ——
// 而删掉之后功能照常,只是开销悄悄涨回去了。
func TestSSE_ResetPollTimerAfterSignal(t *testing.T) {
	cfg := sseCfg()
	// 周期要长到信号能在它之前密集到达,又要短到测试等得起。
	cfg.PollInterval = 300 * time.Millisecond

	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	waitHub(t, hub, 1)

	// 建连本身查了两次:一次校正续传游标,一次首次推送。
	base := svc.listCount()

	// 信号比轮询周期密(50ms < 300ms),而且**用固定节奏发** ——
	// 不能靠 waitBody 来控节奏:它会把所有信号挤在同一个轮询周期里,
	// 于是 ticker 一次都不到点,reset 与不 reset 表现一模一样,
	// 用例就成了摆设(这是本条用例第一版的毛病)。
	const signals = 20
	for i := 1; i <= signals; i++ {
		svc.add(sseEvent(int64(100+i), "thinking", "密集的进度"))
		hub.Notify(sseSID)
		time.Sleep(50 * time.Millisecond)
	}

	// 等最后一次 drain 落地,拿到「所有信号都处理完了」的判据。
	// 从这里到断言之间不能跨过一个轮询周期,否则 ticker 可能插进来。
	conn.waitBody(t, "id: 120")

	// 信号期间,兜底轮询一次都不该触发 ——
	// 每一次 drain 都把计时推后了,而信号间隔(50ms)小于周期(300ms)。
	//
	// 反过来(没有 reset):ticker 独立地每 300ms 触发一次,
	// 这一秒多的循环里它会响三四次,多出几次白查。
	if got := svc.listCount(); got != base+signals {
		t.Errorf("查询次数 = %d, 期望 %d —— 收到信号后兜底轮询的计时没被重置,白查了 %d 次",
			got, base+signals, got-base-signals)
	}
}

// Last-Event-ID 能续传:只收到后面的那几条。
func TestSSE_LastEventIDResumes(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	svc.add(sseEvent(5, "thinking", "第一条"))
	svc.add(sseEvent(6, "thinking", "第二条"))
	svc.add(sseEvent(7, "thinking", "第三条"))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream",
		map[string]string{"Last-Event-ID": "6"})
	defer conn.close(t)

	conn.waitBody(t, "id: 7")
	body := conn.rec.body()
	if strings.Contains(body, "id: 5\n") || strings.Contains(body, "id: 6\n") {
		t.Errorf("续传不该重发已经收到的事件:\n%s", body)
	}
}

// **带着别的会话的 lastID 来连 → 归零从头发,不能跳过本会话的事件。**
//
// 库的 id 是全局自增的:别的会话的游标可能比本会话的所有事件都大,
// 照着它读「id > 游标」会一条都读不到 —— 用户看到的是一段空白的流。
func TestSSE_ForeignLastIDRestartsFromBeginning(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	svc.add(sseEvent(5, "thinking", "第一条"))
	svc.add(sseEvent(6, "thinking", "第二条"))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	// 100 是「别的会话」的游标:比本会话的所有事件都大。
	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream",
		map[string]string{"Last-Event-ID": "100"})
	defer conn.close(t)

	conn.waitBody(t, "id: 5")
	conn.waitBody(t, "id: 6")
}

// 心跳是注释行,浏览器会忽略它,但能保活代理连接。
func TestSSE_HeartbeatIsComment(t *testing.T) {
	cfg := sseCfg()
	cfg.Heartbeat = 20 * time.Millisecond
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	conn.waitBody(t, ": keepalive")

	for _, f := range parseSSE(conn.rec.body()) {
		if f.Comment != "" && !strings.HasPrefix(f.Comment, ":") {
			t.Errorf("心跳不是注释行: %q", f.Comment)
		}
		if f.Comment == "" && f.Data == "" {
			t.Errorf("收到一条既没有 data 也不是注释的帧: %+v", f)
		}
	}
}

// 客户端断开后订阅必须摘干净,否则连接数上限会误判、内存也还占着。
func TestSSE_DisconnectReleasesSubscription(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	waitHub(t, hub, 1)

	conn.close(t)

	waitHub(t, hub, 0)
}

// 每会话的连接数上限 → 50302。
func TestSSE_PerSessionConnectionLimit(t *testing.T) {
	cfg := sseCfg()
	cfg.MaxConnPerSession = 1
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	first := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer first.close(t)
	waitHub(t, hub, 1)

	// 第二条连接就该被拒。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := decode(t, w)
	if got.Code != 50302 {
		t.Fatalf("code = %d, 期望 50302 (body=%s)", got.Code, w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("HTTP 状态 = %d, 期望 503", w.Code)
	}
}

// 全局连接数上限 → 50302。
func TestSSE_GlobalConnectionLimit(t *testing.T) {
	cfg := sseCfg()
	cfg.MaxConnTotal = 1
	cfg.MaxConnPerSession = 5
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	first := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer first.close(t)
	waitHub(t, hub, 1)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := decode(t, w); got.Code != 50302 {
		t.Fatalf("code = %d, 期望 50302 (body=%s)", got.Code, w.Body.String())
	}
}

// **归属校验在连接数上限之前**。
//
// 顺序反过来(先看上限)的话,别人的会话在被占满时会回 503 而不是 404 ——
// 那等于告诉别人这个 session_id 真实存在。
func TestSSE_OwnershipCheckedBeforeConnectionLimit(t *testing.T) {
	cfg := sseCfg()
	cfg.MaxConnPerSession = 1
	// 会话的主人是 999,而令牌代表 7。
	svc := &fakeSSEStream{ownerUserID: 999}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	// 先占掉唯一的那个名额(用会话主人自己的令牌)。
	owner := handler.NewRouter(handler.Deps{
		SSEStream: &fakeSSEStream{ownerUserID: 999},
		SSEHub:    hub,
		SSEConfig: cfg,
		Verifier:  fakeTokenVerifier{userID: 999},
	})
	first := connect(t, owner, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer first.close(t)
	waitHub(t, hub, 1)

	// 现在用另一个用户的身份来连:必须是 404,不能是 503。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := decode(t, w)
	if got.Code != response.CodeNotFound {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeNotFound, w.Body.String())
	}
}

// 关停信号 → 发一帧 shutdown,然后正常收尾。
func TestSSE_ShutdownFrameOnClose(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	shutdown := make(chan struct{})
	r := sseRouter(svc, hub, sseCfg(), shutdown)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	waitHub(t, hub, 1)

	close(shutdown)

	conn.waitBody(t, "event: shutdown")
	// 关停帧不带 id 行:它不是一条事件,不该推进客户端的续传游标。
	for _, f := range parseSSE(conn.rec.body()) {
		if f.Event == "shutdown" && f.ID != "" {
			t.Errorf("关停帧不该带 id 行: %+v", f)
		}
	}

	select {
	case <-conn.done:
	case <-time.After(3 * time.Second):
		t.Fatal("收到关停信号后 handler 没有退出")
	}
	waitHub(t, hub, 0)
}

// 读事件失败(库挂了)不能让整条流变成一个坏帧。
//
// 流阶段的错误不能走统一外壳:响应头已经写出去了。
// 这里锁住的是「连接正常结束,不往流里混 JSON」。
func TestSSE_ReadFailureEndsStreamCleanly(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner, listErr: errFake}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	select {
	case <-conn.done:
	case <-time.After(3 * time.Second):
		t.Fatal("读库失败后 handler 没有退出")
	}

	body := conn.rec.body()
	if strings.Contains(body, `"code"`) || strings.Contains(body, `"message"`) {
		t.Errorf("流里混进了 JSON 外壳: %s", body)
	}
	if hub.Count() != 0 {
		t.Errorf("连接结束了但订阅还留着: %d", hub.Count())
	}
}

// errFake 是一个假的库错误。
var errFake = errors.New("库炸了")

// 帧里的 payload 带换行时,帧结构不能被破坏。
//
// 这是 JSON 编码的唯一保证:换行被转义成 "\n" 两个字符,data 永远只有一行。
func TestSSE_PayloadWithNewlinesKeepsFrameIntact(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	svc.add(sseEvent(3, "thinking", "第一行\n第二行\n第三行"))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	conn.waitBody(t, "id: 3")

	frames := parseSSE(conn.rec.body())
	var thinking *sseFrame
	for i := range frames {
		if frames[i].Event == "thinking" {
			thinking = &frames[i]
		}
	}
	if thinking == nil {
		t.Fatalf("没找到 thinking 帧:\n%s", conn.rec.body())
	}
	if strings.Contains(thinking.Data, "\n") {
		t.Errorf("data 里出现了裸换行,帧结构会被破坏: %q", thinking.Data)
	}
	var payload struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal([]byte(thinking.Data), &payload); err != nil {
		t.Fatalf("data 不是合法 JSON: %v (%s)", err, thinking.Data)
	}
	if payload.Payload != "第一行\n第二行\n第三行" {
		t.Errorf("payload 内容不对: %q", payload.Payload)
	}
}

// 字节数检查:确认响应体不是空的(防止上面那些用例被"什么都没发生"骗过)。
func TestSSE_ResponseIsNotEmpty(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	svc.add(sseEvent(1, "started", ""))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	conn.waitBody(t, "id: 1")
	if len(conn.rec.body()) == 0 {
		t.Fatal("响应体是空的")
	}
	if !bytes.Contains([]byte(conn.rec.body()), []byte("data: ")) {
		t.Errorf("响应体里没有 data 行:\n%s", conn.rec.body())
	}
}

// ---- 审阅补上的用例 ----
//
// 下面这几条锁的都是「代码里写了、但没有任何用例碰过」的不变量。
// 它们的共同点:**破坏之后功能照常,只是行为悄悄变了** ——
// 所以不被测试盯着的代码,活不过下一次重构。

// **外来游标落在本会话两条事件的空隙里 → 必须归零重发。**
//
// 这条和 TestSSE_ForeignLastIDRestartsFromBeginning 是两回事:
// 那条用的游标(100)比本会话**所有**事件都大,走的是「游标指在末尾之后」;
// 这条用的游标(7)正好卡在 id=5 和 id=9 中间,走的是「游标指在空隙里」。
//
// 两条分支必须都在:判据是「游标必须落在本会话一条事件的 id 上」。
//
// 破坏后果:把「空隙里」那条分支的 `return 0, nil` 改成 `return lastID, nil`,
// 本会话 id=5 的事件**永远收不到**,前端只看到后半段 ——
// 症状是「进度从中间开始」,仓库里其余用例全绿。
func TestSSE_ForeignLastIDInGapRestartsFromBeginning(t *testing.T) {
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	svc.add(sseEvent(5, "thinking", "第一条"))
	svc.add(sseEvent(9, "thinking", "第二条"))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, sseCfg(), nil)

	// 7 落在 5 和 9 中间的空隙里:它多半来自别的会话。
	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream",
		map[string]string{"Last-Event-ID": "7"})
	defer conn.close(t)

	conn.waitBody(t, "id: 5")
	conn.waitBody(t, "id: 9")

	body := conn.rec.body()
	if !strings.Contains(body, "id: 5\n") {
		t.Errorf("事件 id=5 被静默丢弃了 —— 外来游标落在空隙里也必须归零重发:\n%s", body)
	}
}

// **一条事件编码失败不许卡住整条流。**
//
// 编码失败(事件类型里带换行)时,那条事件被丢掉,但**游标照样往前推**。
// 不推的话:每轮 drain 都重新读到它、重新失败,后面所有事件再也发不出去 ——
// 一条坏事件把整条流毒死了。
//
// 破坏后果:把编码失败分支里的 `*lastID = ev.ID` 删掉。
//
// ★ 为什么这条用例要把 ReplayLimit 设成 1:
// 坏事件必须**正好卡在批次末尾**才看得出差别。
// 同一批里坏事件后面还跟着好事件的话,后面那条照样会把游标推过去,
// 看起来一切正常 —— 破坏之后行为其实变了,但这条用例抓不到。
// 一批一条时:坏事件是第一批的全部,游标推不动 → drain 停在守卫上 →
// 后面那条**永远发不出去**,症状是「进度卡在第一条」。
func TestSSE_BadEventDoesNotStallTheStream(t *testing.T) {
	cfg := sseCfg()
	cfg.ReplayLimit = 1 // 一批一条,坏事件正好卡在批次末尾

	svc := &fakeSSEStream{ownerUserID: sseOwner}
	// 类型里带换行 → sse.Encode 报错(它拒绝会破坏帧结构的字段)。
	svc.add(sseEvent(1, "bad\ntype", "这一条编码不过"))
	svc.add(sseEvent(2, "thinking", "这一条是好的"))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	// 坏的那条被丢掉是应该的;但它**后面**那条必须照发。
	conn.waitBody(t, "id: 2")

	body := conn.rec.body()
	if strings.Contains(body, "bad") {
		t.Errorf("编码失败的那条不该被发出去(坏帧会把浏览器的解析器带歪):\n%s", body)
	}
}

// **仓库层的增量读坏了 → 不能变成查库死循环。**
//
// 守卫在 drain 里:一轮读完,游标没推进就停。
//
// 这里让仓库无视 afterID,永远返回同一条 —— 真仓库不会这样,
// 但「仓库坏了」正是这个守卫要防的场景:
//
//   - 没有守卫:drain 里 len(evs) == ReplayLimit,于是循环回去再读,
//     读到的是同一条,游标还是不动,永远转下去 —— 一次请求就能把库打满。
//   - 有守卫:第二轮就发现游标没推进,停。
//
// 断言用「查询次数」:没有守卫时它会指数级往上飘。
func TestSSE_BrokenRepoDoesNotSpinForever(t *testing.T) {
	cfg := sseCfg()
	// 批次大小设成 1,仓库每次返回一条,于是「读满」的条件最容易成立。
	cfg.ReplayLimit = 1
	// 轮询周期拉长,免得兜底轮询的查询混进来干扰计数。
	cfg.PollInterval = time.Second

	svc := &fakeSSEStream{ownerUserID: sseOwner, brokenRepo: true}
	svc.add(sseEvent(3, "thinking", "仓库坏了,永远返回这一条"))
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	conn.waitBody(t, "id: 3")

	// 给它一点时间去转。守卫在的话,查询次数是常数(每次 drain 读两轮就停)。
	time.Sleep(200 * time.Millisecond)

	if got := svc.listCount(); got > 10 {
		t.Errorf("查询次数 = %d,期望是个常数 —— 仓库坏了之后 drain 在原地打转,那是一次请求就能把库打满", got)
	}
}

// **单用户连接数上限 → 50302。**
//
// 只有「每会话」和「全局」两个闸的话,一个账号拥有 N 个会话就能开
// N × MaxConnPerSession 条连接,把全局预算吃光,别的用户全部连不上。
//
// 这条把「每会话」的上限调大,好让拒绝的的确来自单用户那一级。
func TestSSE_PerUserConnectionLimit(t *testing.T) {
	cfg := sseCfg()
	cfg.MaxConnPerSession = 5
	cfg.MaxConnPerUser = 1
	cfg.MaxConnTotal = 100
	svc := &fakeSSEStream{ownerUserID: sseOwner}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	first := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer first.close(t)
	waitHub(t, hub, 1)

	// 同一个用户再来一条:每会话(5)和全局(100)都还有余量,
	// 所以拒绝只可能来自单用户那一级。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := decode(t, w)
	if got.Code != response.CodeSSEOverloaded {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeSSEOverloaded, w.Body.String())
	}
}

// **单轮推送有上限,但超出的事件不会丢。**
//
// 一个攒了很多事件的会话,一次重连补发历史就会把整段历史一口气写出去 ——
// 那是一次性的放大。上限按住的是**单轮的突发量**。
//
// 超出部分**不会丢**:游标推过去了,剩下的下一轮(兜底轮询)继续发。
// 这条用例同时锁住这两件事:该停的时候要停,该补的时候要补。
func TestSSE_DrainCapsEventsPerRoundWithoutLosingThem(t *testing.T) {
	cfg := sseCfg()
	cfg.MaxReplayPerDrain = 2
	cfg.ReplayLimit = 200
	// 轮询周期拉长,好把「第一轮」和「第二轮」分开看。
	cfg.PollInterval = 500 * time.Millisecond

	svc := &fakeSSEStream{ownerUserID: sseOwner}
	for i := 1; i <= 5; i++ {
		svc.add(sseEvent(int64(i), "thinking", "第几条"))
	}
	hub := sse.NewHub()
	r := sseRouter(svc, hub, cfg, nil)

	conn := connect(t, r, "/api/v1/sessions/"+sseSID+"/stream", nil)
	defer conn.close(t)

	conn.waitBody(t, "id: 2")

	// 第一轮只该发两条。
	if body := conn.rec.body(); strings.Contains(body, "id: 3\n") {
		t.Errorf("第一轮就发了超过上限的事件:\n%s", body)
	}

	// 剩下的不能丢:下一轮补上。
	conn.waitBody(t, "id: 3")
	conn.waitBody(t, "id: 5")
}

// **接口里装着一个 nil 指针 → 500,不是 panic。**
//
// Deps.SSEStream 是接口字段。接线方写
// `SSEStream: (*service.TaskEventService)(nil)` 时,接口本身不是 nil,
// `== nil` 判不出来 —— 一调方法就在 nil 指针上炸。
//
// 被 Recovery 兜成 500 的话,看起来像服务故障,实际是接线 bug,
// 排查方向一开始就走偏(同 middleware.Auth 对空声明的处理)。
//
// ★ 断言不能只看响应码:Recovery 和 notWired 都回 `response.Fail(CodeInternal)`,
// **响应体一模一样**,光看码分不出「干净地拒绝」和「炸了之后被兜住」。
//
// 所以这条断言的是「那个没接好的依赖一次都不该被调用」——
// 那才是「没接好」和「接好了但运行出错」的分界线。
func TestSSE_TypedNilServiceFailsClosed(t *testing.T) {
	sseNilServiceCalls.Store(0)

	var svc *fakeSSEStream // 有类型、无值
	r := handler.NewRouter(handler.Deps{
		SSEStream: svc,
		SSEHub:    sse.NewHub(),
		SSEConfig: sseCfg(),
		Verifier:  fakeTokenVerifier{userID: sseOwner},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sseSID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := decode(t, w); got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeInternal, w.Body.String())
	}
	if got := sseNilServiceCalls.Load(); got != 0 {
		t.Errorf("没接好的依赖被调用了 %d 次 —— 它会被 nil 指针炸掉,再被 Recovery 兜成 500,"+
			"看起来像服务故障,实际是接线 bug", got)
	}
}
