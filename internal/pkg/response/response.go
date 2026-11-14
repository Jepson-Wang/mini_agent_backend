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
	FailWithData(c, code, message, nil)
}

// FailWithData 同 Fail,但额外带一份结构化数据。
//
// 目前只有参数校验用它,带的是 字段名 → 提示语 的映射,让前端能直接
// 高亮到出错的输入框。data 里只能放已经翻译好的、给人看的内容,
// 别把原始 err 塞进来 —— 那是 Fail 存在的原因。
func FailWithData(c *gin.Context, code int, message string, data any) {
	if message == "" {
		message = Text(code)
	}
	write(c, code, message, data)
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

	// 账号被禁用。能看到这条说明口令已经对了(Login 是先验口令再看状态),
	// 所以这里说清楚不算泄露,还能少一轮客服往返。
	{model.ErrUserDisabled, CodeForbidden, "账号已被禁用,请联系管理员"},

	// 任务已经是终态,取消不了。能看到这条说明归属校验已经过了
	// (service.TaskService.mustOwn 先跑),所以说清楚不算泄露。
	// 用 40900 而不是 40000:请求本身没毛病,是资源的状态不允许。
	{model.ErrTaskNotCancelable, CodeConflict, "任务已经结束,无法取消"},

	// 建任务超出配额。用 42900 而不是 40000:请求本身没毛病,是"现在不行" ——
	// 同样的请求过一个窗口就会成功,客户端应该退避重试而不是改参数。
	//
	// 文案里不带具体数字和窗口长度:那是运营策略,会改,而且逐个探测出来
	// 对刷量的人是有用信息。真实数字在服务端日志里(TaskQuota.Check 的错误里带着)。
	{model.ErrTaskQuotaExceeded, CodeQuotaExceeded, "任务创建过于频繁,请稍后再试"},

	// 回调带来的状态不认识。这条基本只会在内部回调接口上出现,
	// binding 的 oneof 已经挡过一层了。
	{model.ErrTaskStatusInvalid, CodeInvalidParam, "任务状态不合法"},

	// 执行器不可用 / 超时。用 503、504 而不是 500:这两种"过会儿再试有用",
	// 而 500 的含义是"再试也一样"。混在一起的话,前端只能一律弹"系统错误"。
	//
	// 文案里不带地址、端口和 gRPC 状态码 —— 那些是内部拓扑,
	// 翻译在 agentclient 那一层就已经把原文断掉了(见 translateGRPCError)。
	{model.ErrAgentUnavailable, CodeUnavailable, "执行器暂时不可用,请稍后重试"},
	{model.ErrAgentTimeout, CodeGatewayTimeout, "执行器响应超时,请稍后重试"},

	// 执行器满了(并发任务数已达上限)。用 50301 而不是 50300:
	// 两者对外都是"过会儿再试",但含义相反 —— 50300 是执行器连不上,
	// 50301 是执行器好得很、只是排满了。前端要能分开:一个是等执行器
	// 恢复,一个是等它腾出空位。
	//
	// **现在这条规则还轮不到建任务的 HTTP 响应上**:本次这个错误是
	// **异步**到达的(任务已经建好,流里才报错),用户看到的是任务详情里的
	// error_msg。登记它是为了 (a) 将来万一有同步路径返回它,不会掉进
	// CodeInternal(50000 —— 那会让客户端以为服务坏了去重试,恰好是
	// 并发满时最不该做的事);(b) 前端能区分「并发满了」和「Agent 挂了」。
	{model.ErrAgentBusy, CodeAgentBusy, "执行器并发任务数已达上限,请稍后重试"},

	// 实时推送(SSE)这两条。
	//
	// 50302 和 50303 都是**本服务自己**的容量/开关,不是执行器的问题 ——
	// 客户端按业务码做处置时必须能分开:50302 该退避重试(等有连接被关掉),
	// 50303 该改用轮询(重试多少次都没用)。
	{model.ErrSSEOverloaded, CodeSSEOverloaded, "实时连接数已达上限,请稍后重试"},
	{model.ErrSSEDisabled, CodeSSEDisabled, "实时推送未启用,请改用轮询获取进度"},

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
