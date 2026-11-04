package response

import (
	"net/http"
	"testing"
)

func TestCodeRateLimited(t *testing.T) {
	if got := httpStatus(CodeRateLimited); got != http.StatusTooManyRequests {
		t.Fatalf("httpStatus(CodeRateLimited) = %d, 想要 %d", got, http.StatusTooManyRequests)
	}
	txt := Text(CodeRateLimited)
	if txt == "" {
		t.Fatal("CodeRateLimited 的文案为空")
	}
	if txt == Text(CodeInternal) {
		t.Fatalf("CodeRateLimited 没有登记到 codeText,落到了内部错误文案 %q", txt)
	}
	if CodeRateLimited == CodeQuotaExceeded {
		t.Fatal("CodeRateLimited 不能和 CodeQuotaExceeded 同码")
	}
}
