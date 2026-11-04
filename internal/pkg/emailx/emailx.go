// Package emailx 放邮箱的规范化。
//
// 单独成包是因为有两处必须用**同一个函数**:账号的注册/登录查库,
// 以及登录限流器的桶键。两边规范化方式不一致的话,换个大小写就是
// 另一个桶,限流形同虚设。service.normalizeEmail 是未导出的,
// 限流中间件够不着,所以挪到这里共用。
package emailx

import "strings"

// Normalize 统一邮箱的书写形式:去首尾空白,再整体转小写。
//
// 不做的话 Alice@Example.com 和 alice@example.com 会是两个账号 ——
// 唯一索引比的是字节,不管大小写。注册和登录必须走同一个函数,
// 只在其中一边规范化比两边都不做更糟:能注册进去,登录时却找不到。
//
// 只小写域名是更严谨的做法(RFC 5321 里 local-part 理论上区分大小写),
// 但现实中没有邮件服务商这么干,而"大小写不同算两个账号"带来的
// 客服成本是实打实的,所以这里务实地整体转小写。
func Normalize(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
