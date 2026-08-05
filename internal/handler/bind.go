package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/validate"
)

// bindJSON 解析并校验请求体。
//
// 返回 false 表示已经写好失败响应,调用方直接 return 即可。
// 整个 handler 层只走这一个入口,免得有人图省事写成
// response.Fail(c, CodeInvalidParam, err.Error()) —— 那会把
// validator 的原文和 Go 结构体名一起送给客户端。
func bindJSON(c *gin.Context, req any) bool {
	err := c.ShouldBindJSON(req)
	if err == nil {
		return true
	}

	// 原始错误挂到 c.Errors,由 Logger 中间件连着 trace_id 打出来。
	// 排查时看日志,客户端只拿到翻译过的版本。
	_ = c.Error(err)

	if fields := validate.Translate(err); len(fields) > 0 {
		response.FailWithData(c, response.CodeInvalidParam, "参数校验失败", gin.H{"fields": fields})
		return false
	}

	// 走到这里说明压根没进校验:JSON 语法错、字段类型对不上、body 是空的。
	// 同样不回 err.Error() —— 它形如
	//   json: cannot unmarshal number into Go struct field RegisterReq.password of type string
	// 带着内部结构体名和字段路径。
	response.Fail(c, response.CodeInvalidParam, "请求体格式不正确")
	return false
}

// bindQuery 解析并校验 URL 查询参数,规则同 bindJSON。
//
// 和 bindJSON 分开而不是加个参数,是因为兜底文案不一样:
// 查询参数出错时说"请求体格式不正确"会让人对着 body 找半天。
// 字段用 form tag,不是 json tag。
func bindQuery(c *gin.Context, req any) bool {
	err := c.ShouldBindQuery(req)
	if err == nil {
		return true
	}

	_ = c.Error(err)

	if fields := validate.Translate(err); len(fields) > 0 {
		response.FailWithData(c, response.CodeInvalidParam, "参数校验失败", gin.H{"fields": fields})
		return false
	}

	// 类型转换失败,比如 ?limit=abc。原文形如
	//   strconv.ParseInt: parsing "abc": invalid syntax
	// 不算敏感,但也没必要给出去。
	response.Fail(c, response.CodeInvalidParam, "查询参数格式不正确")
	return false
}
