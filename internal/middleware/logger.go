package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// slowThreshold 超过这个耗时的请求按 warn 级别记,方便捞慢接口。
const slowThreshold = time.Second

// Logger 记访问日志。每行都带 trace_id,跟响应体里的是同一个,
// 用户报障时拿着响应里的 trace_id 就能直接捞到这条。
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		// Logger 挂在 Recovery 里层,handler panic 时这个函数是被 panic
		// 展开顺带执行的,c.Next() 后面的代码根本不会跑。
		// 用一个标志位判断有没有正常走完,这样 panic 的请求也能留下访问日志。
		// 注意此刻 Recovery 还没写响应,c.Writer.Status() 仍是默认的 200,
		// 所以状态码要在这里自己补成 500。
		finished := false

		defer func() {
			status := c.Writer.Status()
			if !finished {
				status = http.StatusInternalServerError
			}

			attrs := []any{
				"trace_id", trace.FromGin(c),
				"method", c.Request.Method,
				"path", path,
				"status", status,
				"latency_ms", time.Since(start).Milliseconds(),
				"ip", c.ClientIP(),
			}
			if query != "" {
				attrs = append(attrs, "query", query)
			}
			if code, ok := response.BizCode(c); ok {
				attrs = append(attrs, "code", code)
			}
			// FailFromError 把原始错误挂在这儿。对外只回了"内部错误",
			// 真正的原因只在这行日志里,别删。
			if errs := c.Errors.Errors(); len(errs) > 0 {
				attrs = append(attrs, "errors", errs)
			}

			switch {
			case !finished:
				// panic 已经由 Recovery 打过 error 日志和栈,这里只补一行访问记录
				slog.Warn("access(panic)", attrs...)
			case status >= http.StatusInternalServerError:
				slog.Error("access", attrs...)
			case status >= http.StatusBadRequest, time.Since(start) > slowThreshold:
				slog.Warn("access", attrs...)
			default:
				slog.Info("access", attrs...)
			}
		}()

		c.Next()
		finished = true
	}
}
