// Package dto 是 HTTP 层的请求/响应结构。
//
// 和 model 刻意分成两套。合用一套的话有两个后果:
//  1. 往 model 加一列(PasswordHash、手机号、实名信息)就会顺着 JSON 漏出去,
//     而且没有任何地方会提醒你;
//  2. 改数据库字段名会直接变成一次 API 的破坏性变更。
//
// 校验规则写在 binding tag 里,由 gin 在 ShouldBindJSON 时执行;
// 失败信息由 internal/pkg/validate 翻译成人话,不会把 validator 的原文吐出去。
package dto

import (
	"strings"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// RegisterReq 注册请求。
type RegisterReq struct {
	// max=255 对齐 users.email 的 varchar(255)。
	// 这个 max 按字符数算,而 varchar(n) 在 Postgres 里也是按字符算,正好对得上。
	Email string `json:"email" binding:"required,email,max=255"`

	// bmax 是自定义规则,按字节数算 —— 内置的 max 对字符串按字符数算,
	// 而 bcrypt 只认前 72 字节,多出来的会被悄悄丢掉(意味着
	// "前 72 字节相同的两个长口令" 可以互相登录)。详见 pkg/validate。
	//
	// min=8 是强度下限,只在注册时要求,登录时不要求,理由见 LoginReq。
	Password string `json:"password" binding:"required,min=8,bmax=72"`

	// 可选。omitempty 的意思是「没填就不校验」,填了才走 max=64。
	// 不写 omitempty 的话空串会被 max 放过、被 required 拦下,语义容易搞混。
	Nickname string `json:"nickname" binding:"omitempty,max=64"`
}

// LoginReq 登录请求。
//
// 密码这里只限最大长度,故意不带 min=8:
//  1. 注册时的强度规则不该追认到已有账号上,老用户的短口令得能登进来;
//  2. 「密码太短」返回 40000、「密码不对」返回 40100,
//     等于免费把口令策略告诉了对面。
//
// bmax=72 还是要留:超过 bcrypt 上限的输入应该在入口就被挡掉,
// 而不是丢给散列函数去截断。
type LoginReq struct {
	Email    string `json:"email" binding:"required,email,max=255"`
	Password string `json:"password" binding:"required,bmax=72"`
}

// CreateSessionReq 创建会话。会话和它的首个任务是一起建的(一个事务)。
//
// 两个字段都可空:允许先开一个空会话,标题等首轮对话出来再回填。
type CreateSessionReq struct {
	// max=128 对齐 sessions.title 的 varchar(128)。
	// Postgres 的 varchar(n) 也按字符算,和 validator 的 max 正好对得上。
	Title *string `json:"title" binding:"omitempty,max=128"`

	// tasks.input 是 text,数据库自己不设上限,所以必须在入口挡。
	// 不挡的话一次请求就能往库里塞几 MB,而且它还会跟着任务详情原样发回去。
	Input *string `json:"input" binding:"omitempty,max=8000"`
}

// CancelTaskReasonReq
type CancelTaskReasonReq struct {
	Reason string `json:"reason" binding:"max=20"`
}

// UpdateSessionTitleReq 改标题。
//
// Title 是 *string 且 required,意思是"这个字段必须出现在 body 里"。
// 少了它的话 {} 和 {"title": null} 在 Go 这边都是 nil,分不出
// "没提这个字段"和"要求清空" —— 而这两件事的语义完全相反。
//
// 清空标题的写法是 {"title": ""},空白串会被 NormalizedTitle 归成 nil。
// 不接受 null 是刻意的:让"清空"有一个明确的、可校验的写法。
type UpdateSessionTitleReq struct {
	Title *string `json:"title" binding:"required,max=128"`
}

// NormalizedTitle 返回该写进库的标题。空白串归成 nil,对应库里的 NULL。
// 和 normalizeNickname 是同一个理由:不这么做的话库里会同时存在
// NULL 和 "" 两种"没有标题",查询时得写两个条件。
func (r UpdateSessionTitleReq) NormalizedTitle() *string {
	if r.Title == nil {
		return nil
	}
	if s := strings.TrimSpace(*r.Title); s != "" {
		return &s
	}
	return nil
}

// CreateTaskReq 在已有会话下追加一个任务。
//
// 这里的 input 是 required,和 CreateSessionReq 不一样:
// 建空会话说得通(等用户想好再说),建一个没有输入的任务说不通。
type CreateTaskReq struct {
	Input *string `json:"input" binding:"required,max=8000"`
}

// PageQuery 是列表接口的分页参数,用 form tag 从 query string 里取。
//
// limit 的上限写死 100,和 repository 的 maxListLimit 对齐。两边都夹是刻意的:
// 这里回 40000 让调用方知道自己传错了,仓库层那道则保证"哪怕有人绕过 handler
// 直接调 service 也读不出全表"。
//
// limit=0 走 omitempty(int 的零值)不校验,落到 service 变成默认页大小;
// limit=-1 不是零值,会被 min=1 拦下 —— 这个值很关键,
// GORM 的 Limit(负数) 是"整个不写 LIMIT 子句",也就是全表。
type PageQuery struct {
	Limit  int `form:"limit" binding:"omitempty,min=1,max=100"`
	Offset int `form:"offset" binding:"omitempty,min=0"`
}

// TaskCallbackReq 是 Agent 回报任务状态的请求体。
//
// 只放行 running / succeeded / failed 三个值。canceled 不在里面是刻意的:
// 取消只能由用户发起,让 Agent 也能把任务改成"已取消",等于给一个
// 被攻破的 Agent 一个悄悄吞任务的开关 —— 用户看到的是自己取消的,不会去查。
type TaskCallbackReq struct {
	Status string `json:"status" binding:"required,oneof=running succeeded failed"`

	// ErrorMsg 失败原因,只在 status=failed 时有意义。
	//
	// 必须限长:这个值会原样落进 tasks.error_msg,而那是一列 text,
	// 数据库自己不设上限。不挡的话对面一次能往库里塞几 MB,
	// 而且它还会跟着任务详情接口发回给用户。
	ErrorMsg *string `json:"error_msg" binding:"omitempty,max=2000"`
}

// NormalizedErrorMsg 返回该写进库的失败原因。
//
// 只有 failed 才带原因 —— 成功的任务不该留着上一次的错误信息。
// 空白串归一成 nil,免得库里同时存在 NULL 和 "" 两种"没有原因",
// 查询时得写两个条件。
func (r TaskCallbackReq) NormalizedErrorMsg() *string {
	if r.Status != model.TaskFailed || r.ErrorMsg == nil {
		return nil
	}
	if msg := strings.TrimSpace(*r.ErrorMsg); msg != "" {
		return &msg
	}
	return nil
}
