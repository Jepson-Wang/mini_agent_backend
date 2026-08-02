// Package middleware 存放 HTTP 中间件。
//
// 挂载顺序有讲究,见 handler.NewRouter:
//
//	TraceID → Recovery → Logger → (私有组再加 Auth)
//
// TraceID 必须最外层,因为 Recovery 和 Logger 都要用它;
// Recovery 在 Logger 外面,这样连 Logger 自己 panic 都能兜住。
package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
)

// maxInboundTraceID 是接受上游 trace id 的长度上限。
const maxInboundTraceID = 64

// TraceID 给每个请求分配一个 id,写进 context 和响应头。
//
// 上游(网关、调用方服务)如果已经带了 X-Trace-ID 就沿用,好把一次调用串起来;
// 但那是外部输入,要先过一遍校验再用 —— 否则别人往头里塞换行符,
// 就能在日志里伪造出一整行假记录。
func TraceID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(trace.Header)
		if !validTraceID(id) {
			id = newTraceID()
		}
		trace.Set(c, id)
		c.Header(trace.Header, id)
		c.Next()
	}
}

// newTraceID 用 ULID,前 10 位是毫秒时间戳,日志按 id 排序即时间序。
// 熵源出问题时退回纳秒时间戳,宁可 id 弱一点也不能让请求挂掉。
func newTraceID() string {
	if id, err := ulid.New(); err == nil {
		return id
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// validTraceID 只放行可见 ASCII,把控制字符(尤其是 \n \r)挡在日志外面。
func validTraceID(s string) bool {
	if s == "" || len(s) > maxInboundTraceID {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '!' || s[i] > '~' {
			return false
		}
	}
	return true
}
