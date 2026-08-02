package middleware

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// Recovery 兜住 handler 里的 panic:进程不倒,客户端拿到统一外壳的 50000。
//
// 没用 gin.Recovery() 是因为它直接写 500 空 body,外壳就不统一了。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}

			traceID := trace.FromGin(c)

			// 连接已经断了,再怎么写都是白费,只记一行然后收工。
			// 这种情况很常见(客户端超时主动断开),不该按 500 报警。
			if isBrokenPipe(p) {
				slog.Warn("客户端连接已断开",
					"trace_id", traceID,
					"path", c.Request.URL.Path,
					"error", fmt.Sprint(p),
				)
				c.Abort()
				return
			}

			// 栈要在 defer 里现取 —— 此时 panic 的帧还在栈上,拿到的才是出事现场。
			slog.Error("handler panic",
				"trace_id", traceID,
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"panic", fmt.Sprint(p),
				"stack", string(debug.Stack()),
			)

			// 已经写过响应头就没法再改状态码了,只能截断。
			if c.Writer.Written() {
				c.Abort()
				return
			}
			response.Fail(c, response.CodeInternal, "")
		}()

		c.Next()
	}
}

// isBrokenPipe 判断 panic 是不是"对端把连接关了"引起的写失败。
// 各平台文案不一样(Linux 是 broken pipe,Windows 是 connection was aborted),
// 所以只能按字符串匹配。
func isBrokenPipe(p any) bool {
	err, ok := p.(error)
	if !ok {
		return false
	}
	var ne *net.OpError
	if !errors.As(err, &ne) {
		return errors.Is(err, http.ErrAbortHandler)
	}
	var se *os.SyscallError
	if !errors.As(ne.Err, &se) {
		return false
	}
	msg := strings.ToLower(se.Error())
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "connection was aborted") ||
		strings.Contains(msg, "connection was forcibly closed")
}
