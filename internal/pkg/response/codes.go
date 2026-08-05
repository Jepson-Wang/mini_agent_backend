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
	CodeInternal         = 50000 // 内部错误
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
	CodeInternal:         "内部错误",
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
