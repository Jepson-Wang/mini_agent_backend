package response

import "net/http"

// 业务错误码。前三位刻意跟 HTTP 状态码对齐,后两位留给同一类错误下的细分,
// 这样 httpStatus() 直接除以 100 就能得到该返回的 HTTP 状态,不用再维护一张表。
//
// 想细分的时候在对应区间里往后排,比如 40001 表示"手机号格式不对"。
const (
	CodeOK               = 0     // 成功
	CodeInvalidParam     = 40000 // 参数校验失败
	CodeUnauthorized     = 40100 // 未认证
	CodeForbidden        = 40300 // 无权限
	CodeNotFound         = 40400 // 资源不存在
	CodeMethodNotAllowed = 40500 // 请求方法不被支持(NoMethod 兜底用,见 handler.NewRouter)
	CodeConflict         = 40900 // 和资源当前状态冲突(比如去取消一个已经跑完的任务)
	CodeQuotaExceeded    = 42900 // 超出配额

	// CodeRateLimited 请求被限流器拦下(登录等入口的频率限制)。
	//
	// 排在 42900 后面:同属 429 区间,但 CodeQuotaExceeded 是"建任务的额度用完了",
	// 这个是"短时间请求太密"。文案刻意不说是哪个维度触发的(IP、邮箱还是全局),
	// 也不带任何数字和窗口长度:那是运营策略,会改,而且让调用方逐个探测出来
	// 对刷量的人是有用信息。
	//
	// 注意:这个码**没有**对应的 errRules 行,也没有 model.ErrRateLimited。
	// 限流器是中间件,直接 response.Fail(c, CodeRateLimited, "") 出去,
	// 不经过 FailFromError,加规则只是死代码,别"补上"。
	CodeRateLimited = 42901
	CodeInternal    = 50000 // 内部错误

	// CodeUnavailable 依赖的下游暂时不可用(现在只有 Agent)。
	//
	// 和 CodeInternal 分开是有实际意义的:503 告诉客户端"这次不算数,
	// 过会儿重试有用",而 500 意味着"重试大概率还是这个结果"。
	// 全都回 500 的话,前端只能一律弹"系统错误",用户也就不会再试了。
	CodeUnavailable = 50300

	// CodeAgentBusy 执行器接不了活:并发任务数已达上限。
	//
	// 和 CodeUnavailable(50300)分开的理由是**处置方向相反**:50300 是
	// 「执行器连不上」,该退避、该等它恢复;50301 是「执行器好得很、只是
	// 排满了」,过一会儿腾出空位就行。合成一个码的话,客户端只能一律
	// 当成「执行器挂了」,于是一个健康的执行器也一起被放弃。
	//
	// 同样落在 503 区间:两者对客户端都是「过会儿再试有用」,区别在
	// 要不要隔离执行器。
	CodeAgentBusy = 50301

	// CodeSSEOverloaded 实时推送的连接数已达上限。
	//
	// 50300 和 50301 说的都是**执行器**,而这一条说的是**本服务自己**:
	// SSE 连接数满了,新的订阅请求被挡在门外。这两件事的处置方向不一样 ——
	// 执行器不可用该等执行器恢复;连接数满了该等**有连接被关掉**,
	// 而那一刻可能比执行器恢复早得多也可能晚得多。
	//
	// 混进 50300 的话,客户端会把它当成「执行器挂了」去等执行器,
	// 而真正该做的是「退避一下再连 SSE」。
	CodeSSEOverloaded = 50302

	// CodeSSEDisabled 这个部署没开实时推送。
	//
	// **和 50302 分开是有意的**:50302 是「等一会儿就能连上」,
	// 重试有用;这一条是「这条路在这个部署上不存在」,重试多少次都没用 ——
	// 客户端该做的是**改用轮询**,而不是退避重试 SSE。
	//
	// 所以它的文案直接告诉客户端该换条路。
	CodeSSEDisabled = 50303

	// CodeGatewayTimeout 下游在时限内没回话。
	//
	// 特意和 CodeUnavailable 分开:超时意味着请求**可能已经送到并执行了**,
	// 所以对非幂等的操作,客户端不该无脑重试。
	CodeGatewayTimeout = 50400
)

// codeText 是每个码的兜底文案,Fail 传空 message 时用。
// 注意这是给人看的提示,不要把内部错误细节写进来。
var codeText = map[int]string{
	CodeOK:               "ok",
	CodeInvalidParam:     "参数校验失败",
	CodeUnauthorized:     "未认证",
	CodeForbidden:        "无权限",
	CodeNotFound:         "资源不存在",
	CodeMethodNotAllowed: "请求方法不被支持",
	CodeConflict:         "当前状态下无法执行该操作",
	CodeQuotaExceeded:    "超出配额",
	CodeRateLimited:      "请求过于频繁,请稍后再试",
	CodeInternal:         "内部错误",
	CodeUnavailable:      "服务暂时不可用,请稍后重试",
	CodeAgentBusy:        "执行器并发任务数已达上限,请稍后重试",
	CodeSSEOverloaded:    "实时连接数已达上限,请稍后重试",
	CodeSSEDisabled:      "实时推送未启用,请改用轮询获取进度",
	CodeGatewayTimeout:   "服务响应超时,请稍后重试",
}

// Text 返回错误码的默认文案,未登记的码返回内部错误的文案。
func Text(code int) string {
	if s, ok := codeText[code]; ok {
		return s
	}
	return codeText[CodeInternal]
}

// httpStatus 把业务码换算成 HTTP 状态码。
// 换算不出合法状态码的一律当 500 —— 与其返回一个奇怪的状态,不如老实说内部错了。
func httpStatus(code int) int {
	if code == CodeOK {
		return http.StatusOK
	}
	status := code / 100
	if status < 100 || status > 599 {
		return http.StatusInternalServerError
	}
	return status
}
