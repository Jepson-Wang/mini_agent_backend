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
