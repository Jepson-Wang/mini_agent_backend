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
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			if _, body := fire(fmt.Errorf("包一层: %w", tc.err)); body.Code != tc.code {
				t.Fatalf("业务码 = %d, 想要 %d", body.Code, tc.code)
			}
		})
	}
}
