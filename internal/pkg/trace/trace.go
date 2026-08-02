// Package trace 存放贯穿一次请求的 trace id。
//
// 单独拆一个包是为了断开 import 环:middleware 要写 trace id,
// response 要把它塞进响应体,两边都 import 这里就不会互相依赖。
// service / repository 层想打日志时也可以从 context.Context 里取。
package trace

import (
	"context"

	"github.com/gin-gonic/gin"
)

// Header 是 trace id 在 HTTP 头里的名字,请求和响应用同一个。
const Header = "X-Trace-ID"

// ginKey 是 trace id 在 gin.Context 里的键。
const ginKey = "trace_id"

// ctxKey 用私有类型做键,不会跟别的包撞。
type ctxKey struct{}

// Set 把 trace id 同时写进 gin.Context 和 请求的 context.Context。
// 前者给 handler / middleware 用,后者会一路传到 service 和 repository。
func Set(c *gin.Context, id string) {
	c.Set(ginKey, id)
	c.Request = c.Request.WithContext(
		context.WithValue(c.Request.Context(), ctxKey{}, id),
	)
}

// FromGin 取 trace id,没有就返回空串。
func FromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(ginKey); ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// FromContext 取 trace id,没有就返回空串。
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(ctxKey{}).(string); ok {
		return id
	}
	return ""
}
