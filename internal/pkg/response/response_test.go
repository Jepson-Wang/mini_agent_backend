package response

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

func init() { gin.SetMode(gin.TestMode) }

// fire 把 err 交给 FailFromError,返回 HTTP 状态码和响应体。
func fire(err error) (int, Response) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	FailFromError(c, err)

	var body Response
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// 配额超限必须映射成 42900,不能掉进"没登记 → 50000"的默认分支。
//
// 这条值得单独钉住:配额的整条链路(TaskQuota → ErrTaskQuotaExceeded →
// errRules → 42900)只要断在任何一环,表现都是 500 —— 而 500 会让客户端
// 当成"服务坏了"去重试,恰好是超配额时最不该做的事。
func TestFailFromError_QuotaExceeded(t *testing.T) {
	// 包一层 %w:service 那边就是这么抛的(带上真实数字给日志看)。
	err := fmt.Errorf("%w: 200/200 in 24h0m0s", model.ErrTaskQuotaExceeded)

	status, body := fire(err)

	if body.Code != CodeQuotaExceeded {
		t.Fatalf("业务码 = %d, 想要 %d", body.Code, CodeQuotaExceeded)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("HTTP 状态码 = %d, 想要 %d", status, http.StatusTooManyRequests)
	}
	// 具体数字是运营策略,不该回给客户端 —— 逐个探测出来对刷量的人有用。
	if strings.Contains(body.Message, "200") {
		t.Errorf("对外文案里出现了配额数字: %q", body.Message)
	}
}

// TestFailFromError_AgentBusy 盯着"执行器满了"这个码。
//
// 掉进"没登记 → 50000"的默认分支时,客户端会把它当成"服务坏了"去重试 ——
// 而"并发满了"恰恰是最不该重试的时刻(重试不会让槽多出来,只会让更堵)。
// 所以它必须有自己的业务码 50301,不能和 50300(执行器不可用)混成一个。
func TestFailFromError_AgentBusy(t *testing.T) {
	// 包一层 %w:service 那边就是这么抛的。
	err := fmt.Errorf("接收任务事件流: %w", model.ErrAgentBusy)

	status, body := fire(err)

	if body.Code != CodeAgentBusy {
		t.Fatalf("业务码 = %d, 想要 %d", body.Code, CodeAgentBusy)
	}
	if body.Code == CodeInternal {
		t.Fatal("掉进了默认分支(50000),客户端会当成服务坏了去重试")
	}
	if body.Code == CodeUnavailable {
		t.Fatal("和「执行器不可用」共用一个码,前端分不出「满了」和「挂了」")
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("HTTP 状态码 = %d, 想要 %d", status, http.StatusServiceUnavailable)
	}
	if !strings.Contains(body.Message, "并发") {
		t.Errorf("对外文案里没提并发: %q", body.Message)
	}
}

// TestFailFromError_SSECodesAreDistinct 盯着实时推送的两个码不许混。
//
// 50302（连接数满了）和 50303（功能没开）**处置方向相反**：
// 前者该退避重试（等有连接被关掉），后者该改用轮询（重试多少次都没用）。
// 混成一个码，客户端就会对一条根本不存在的路反复重试。
//
// 50300「执行器暂时不可用」更不能混 —— SSE 连接数满了跟执行器没关系。
func TestFailFromError_SSECodesAreDistinct(t *testing.T) {
	_, over := fire(fmt.Errorf("包一层: %w", model.ErrSSEOverloaded))
	_, dis := fire(fmt.Errorf("包一层: %w", model.ErrSSEDisabled))

	if over.Code != CodeSSEOverloaded {
		t.Fatalf("连接数超限的业务码 = %d, 想要 %d", over.Code, CodeSSEOverloaded)
	}
	if dis.Code != CodeSSEDisabled {
		t.Fatalf("功能未启用的业务码 = %d, 想要 %d", dis.Code, CodeSSEDisabled)
	}
	if over.Code == dis.Code {
		t.Fatal("「连接数满了」和「功能没开」共用一个码,客户端分不出该退避重试还是该改用轮询")
	}
	if over.Code == CodeUnavailable || dis.Code == CodeUnavailable {
		t.Fatal("和「执行器暂时不可用」共用一个码 —— SSE 的事跟执行器没关系,混了会让客户端去等执行器")
	}
	// 文案要能直接告诉客户端该做什么。
	if !strings.Contains(dis.Message, "轮询") {
		t.Errorf("「功能未启用」的文案里没说该改用轮询: %q", dis.Message)
	}
	if !strings.Contains(over.Message, "连接") {
		t.Errorf("「连接数超限」的文案里没提连接: %q", over.Message)
	}
}

// 没登记的错误一律 500,而且**绝不**把原始错误文案回给客户端 ——
// 数据库报错的原文可能带表名、SQL 甚至连接串。
func TestFailFromError_UnknownErrorLeaksNothing(t *testing.T) {
	secret := `pq: password authentication failed for user "postgres"`

	status, body := fire(fmt.Errorf("查任务: %s", secret))

	if body.Code != CodeInternal {
		t.Fatalf("业务码 = %d, 想要 %d", body.Code, CodeInternal)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("HTTP 状态码 = %d, 想要 500", status)
	}
	if strings.Contains(body.Message, "postgres") || strings.Contains(body.Message, "pq:") {
		t.Fatalf("原始错误漏进了响应体: %q", body.Message)
	}
}

// errRules 是按顺序匹配的,新增条目时很容易不小心排在一条更宽泛的规则后面。
// 这条把几个关键映射一起钉住,顺手也证明 %w 包装不影响匹配。
func TestFailFromError_KnownErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{model.ErrSessionNotFound, CodeNotFound},
		{model.ErrTaskNotFound, CodeNotFound},
		{model.ErrInvalidCredentials, CodeUnauthorized},
		{model.ErrUserDisabled, CodeForbidden},
		{model.ErrTaskNotCancelable, CodeConflict},
		{model.ErrTaskQuotaExceeded, CodeQuotaExceeded},
		{model.ErrAgentUnavailable, CodeUnavailable},
		{model.ErrAgentTimeout, CodeGatewayTimeout},
		{model.ErrAgentBusy, CodeAgentBusy},
		{model.ErrSSEOverloaded, CodeSSEOverloaded},
		{model.ErrSSEDisabled, CodeSSEDisabled},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			if _, body := fire(fmt.Errorf("包一层: %w", tc.err)); body.Code != tc.code {
				t.Fatalf("业务码 = %d, 想要 %d", body.Code, tc.code)
			}
		})
	}
}
