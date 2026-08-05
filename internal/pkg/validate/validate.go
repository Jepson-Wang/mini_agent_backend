// Package validate 把 gin binding 的校验失败翻译成「字段名 → 人话」。
//
// 存在的理由只有一个:validator 的原文没法给用户看,也不该给用户看。
// 它长这样:
//
//	Key: 'RegisterReq.Password' Error:Field validation for 'Password' failed on the 'min' tag
//
// 里面是 Go 结构体名和 tag 名 —— 用户看不懂,前端没法定位到表单项,
// 同时还白送了一份内部结构。所以整个项目里不允许出现
// response.Fail(c, ..., err.Error()) 这种写法。
package validate

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

// init 在包被引入时就把定制项装到 gin 那个全局校验器上。
//
// 必须在第一次 ShouldBind 之前完成,所以不能做成懒加载 ——
// 等到翻译错误的时候再注册就晚了,那会儿字段名早就定死成 Go 名了。
// 用 init 而不是导出一个 Setup():少一个「忘了调」的失败模式。
func init() {
	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		// gin 换了底层校验器。不 panic:这是可降级的,
		// 顶多字段名退回 Go 名,接口本身照常工作。
		return
	}
	v.RegisterTagNameFunc(jsonTagName)
	_ = v.RegisterValidation("bmax", byteMax)
}

// jsonTagName 让校验错误里的字段名跟 JSON 里的一致。
//
// 不注册的话,前端发的是 {"password": ...},拿回来的错误却指向 "Password",
// 对不上就没法高亮到具体输入框。这也是「不要泄露内部命名」的一部分:
// Go 字段名改成 Pwd 不该让 API 的错误结构跟着变。
func jsonTagName(f reflect.StructField) string {
	name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
	if name == "-" {
		// 明确不参与序列化的字段。返回空串,validator 会退回 Go 字段名。
		return ""
	}
	return name
}

// byteMax 是自定义规则 bmax=N:按**字节数**限制字符串长度。
//
// 内置的 max 对字符串按 rune 计数(见 validator 的 isLte),
// 而 bcrypt 的 72 是字节上限 —— "口令口令口令口令" 只有 8 个字符却是 24 字节。
// 只写 max=72 的话,72 个汉字(216 字节)能过校验,到 hash.Hash 才炸,
// 用户看到的就是一个 500 而不是「密码太长」。
func byteMax(fl validator.FieldLevel) bool {
	n, err := strconv.Atoi(fl.Param())
	if err != nil {
		// tag 写错了(bmax=abc)。全拒而不是全放:
		// 校验规则失效必须是显性的,不能悄悄变成没有校验。
		return false
	}
	return len(fl.Field().String()) <= n
}

// labels 是字段名 → 中文名。
// 没登记的字段直接用它的 json 名,话说得不顺但不会出错。
var labels = map[string]string{
	"email":    "邮箱",
	"password": "密码",
	"nickname": "昵称",
	"title":    "标题",
	"input":    "内容",
}

func label(field string) string {
	if s, ok := labels[field]; ok {
		return s
	}
	return field
}

// Translate 把 ShouldBind 的错误翻成 字段名 → 提示语。
//
// 只认 validator.ValidationErrors。JSON 语法错、类型对不上、body 为空
// 这些不是校验错误,一律返回 nil,由调用方回一句笼统的「请求体格式不正确」——
// 那类错误的原文里带着 Go 结构体名,同样不能直接吐出去。
func Translate(err error) map[string]string {
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		return nil
	}

	out := make(map[string]string, len(ve))
	for _, fe := range ve {
		field := fe.Field()
		// 同名字段(嵌套结构里可能出现)保留第一条,
		// 一次只让用户改一个地方。
		if _, dup := out[field]; dup {
			continue
		}
		out[field] = message(fe)
	}
	return out
}

// message 生成单条提示。
//
// 注意通篇没有用 fe.Value() —— 提示语里绝不能回显用户填的值,
// 否则密码会顺着错误响应和日志一起漏出去。
func message(fe validator.FieldError) string {
	name := label(fe.Field())
	param := fe.Param()
	isText := fe.Kind() == reflect.String

	switch fe.Tag() {
	case "required":
		return name + "不能为空"
	case "email":
		return "请填写正确的" + name + "地址"
	case "min":
		if isText {
			return fmt.Sprintf("%s至少 %s 个字符", name, param)
		}
		return fmt.Sprintf("%s不能小于 %s", name, param)
	case "max":
		if isText {
			return fmt.Sprintf("%s最多 %s 个字符", name, param)
		}
		return fmt.Sprintf("%s不能大于 %s", name, param)
	case "bmax":
		return fmt.Sprintf("%s最长 %s 字节(一个汉字算 3 字节)", name, param)
	case "len":
		if isText {
			return fmt.Sprintf("%s必须是 %s 个字符", name, param)
		}
		return fmt.Sprintf("%s必须等于 %s", name, param)
	case "gt", "gte":
		return fmt.Sprintf("%s必须大于%s %s", name, orEqual(fe.Tag()), param)
	case "lt", "lte":
		return fmt.Sprintf("%s必须小于%s %s", name, orEqual(fe.Tag()), param)
	case "oneof":
		return fmt.Sprintf("%s只能是 %s 之一", name, strings.ReplaceAll(param, " ", "、"))
	case "eqfield":
		return fmt.Sprintf("%s和%s不一致", name, label(strings.ToLower(param)))
	case "url", "uri":
		return name + "必须是合法的链接"
	case "uuid", "uuid4":
		return name + "格式不正确"
	case "alphanum":
		return name + "只能包含字母和数字"
	case "numeric":
		return name + "只能是数字"
	default:
		// 兜底。哪怕来了个没登记的 tag,也绝不退回 fe.Error()。
		return name + "格式不正确"
	}
}

func orEqual(tag string) string {
	if strings.HasSuffix(tag, "e") { // gte / lte
		return "或等于"
	}
	return ""
}
