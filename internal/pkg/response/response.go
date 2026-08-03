// Package response 定义整个 HTTP 层的统一响应外壳。
//
// 所有出口都从这里走,包括 404 / 405 兜底和 panic 兜底,
// 客户端永远只需要解析同一个结构。
package response

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// Response 是统一外壳。
//
// Data 上没加 omitempty:失败时也要输出 "data": null,
// 这样成功和失败的字段集完全一致,客户端不用判断字段存不存在。
type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	TraceID string `json:"trace_id"`
}

// bizCodeKey 记录本次请求最终返回的业务码,给 Logger 中间件读。
const bizCodeKey = "biz_code"

// BizCode 返回本次请求写出的业务码。没写过响应时 ok 为 false。
func BizCode(c *gin.Context) (int, bool) {
	v, exists := c.Get(bizCodeKey)
	if !exists {
		return 0, false
	}
	code, ok := v.(int)
	return code, ok
}

// OK 返回成功。data 传 nil 就是 "data": null。
func OK(c *gin.Context, data any) {
	write(c, CodeOK, Text(CodeOK), data)
}

// Fail 返回失败。message 传空串则用该错误码的默认文案。
//
// 注意 Fail 会 Abort:响应已经写出去了,后面的 handler 不该再跑,
// 否则会重复写 body。中间件里调完直接 return 即可。
func Fail(c *gin.Context, code int, message string) {
	if message == "" {
		message = Text(code)
	}
	write(c, code, message, nil)
	c.Abort()
}

// errRule 把一个业务错误映射到 (业务码, 对外文案)。
// HTTP 状态码由业务码算出来,不用单独写。
type errRule struct {
	err     error
	code    int
	message string
}

// errRules 是业务错误 → 响应的映射表。
//
// 用切片配 errors.Is 而不是 map[error],因为业务错误经常被 fmt.Errorf("%w")
// 包一层再往上抛,map 直接查会漏。顺序即优先级,先匹配到的先用。
//
// 没登记的错误一律走默认分支:对外只说"内部错误",真实错误进日志。
// 这条很重要 —— 数据库报错的原文可能带表名、SQL 甚至连接串,不能回给客户端。
var errRules = []errRule{
	{model.ErrUserNotFound, CodeNotFound, "用户不存在"},
	{model.ErrSessionNotFound, CodeNotFound, "会话不存在"},
	{model.ErrTaskNotFound, CodeNotFound, "任务不存在"},

	// 注册时邮箱被占用。这里刻意不说"该邮箱已注册",
	// 否则接口就成了账号探测器。model.ErrEmailOccupied 的文案也是同样考虑。
	{model.ErrEmailOccupied, CodeInvalidParam, "邮箱或密码不可用"},

	// 登录失败。文案里绝不能透露到底是邮箱不存在还是密码错,
	// service 层也已经把这两种情况合并成同一个错误了。
	{model.ErrInvalidCredentials, CodeUnauthorized, "邮箱或密码不正确"},

	// model.ErrSessionExists / ErrTaskExists 故意不登记:
	// 它们意味着 ULID 撞了,属于内部故障,不该让客户端看见细节。
}

// FailFromError 按错误查表返回。err 为 nil 时按内部错误处理(说明调用方逻辑错了)。
//
// 原始 err 会挂到 c.Errors 上,Logger 中间件负责把它连同 trace_id 一起打出来,
// 所以这里不用再写日志。
func FailFromError(c *gin.Context, err error) {
	if err == nil {
		Fail(c, CodeInternal, "")
		return
	}
	_ = c.Error(err)
	for _, r := range errRules {
		if errors.Is(err, r.err) {
			Fail(c, r.code, r.message)
			return
		}
	}
	Fail(c, CodeInternal, "")
}

func write(c *gin.Context, code int, message string, data any) {
	c.Set(bizCodeKey, code)
	c.JSON(httpStatus(code), Response{
		Code:    code,
		Message: message,
		Data:    data,
		TraceID: trace.FromGin(c),
	})
}
