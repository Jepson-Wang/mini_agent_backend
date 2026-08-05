package handler

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// ulidLen 是 ULID 的字符串长度。session_id / task_uid 落库都是 char(26)。
const ulidLen = 26

// ulidParam 取出并粗校验路径里的 ULID 参数。
//
// 只看长度,不看字符集:后面是一次等值查询,格式再怪也只是查不到,
// 不存在注入。它挡的是"有人拿 3MB 的字符串当 id"这类请求 ——
// 那种东西不该走到数据库。
//
// 放 handler 是因为这是纯格式判断:不查库就能下结论的归 handler。
// 归属校验则相反,那个必须在 service(mustOwn)。
//
// label 是给用户看的名字("会话"/"任务"),别把 gin 的参数名漏出去。
func ulidParam(c *gin.Context, key, label string) (string, bool) {
	v := c.Param(key)
	if len(v) != ulidLen {
		response.Fail(c, response.CodeInvalidParam, label+" ID 格式不正确")
		return "", false
	}
	return v, true
}

// notWired 处理"路由注册了但依赖没注入"的接线错误。
//
// 和 middleware.Auth 遇到 nil Verifier 时是一个思路:宁可 500,
// 也不能拿一个 nil service 往下跑。路由照常注册是刻意的 ——
// 把"接口不存在(404)"和"接口没接好(500)"混在一起会让排查走错方向。
func notWired(c *gin.Context, what string) {
	slog.Error(what+"未注入 service",
		"trace_id", trace.FromGin(c),
		"path", c.Request.URL.Path,
	)
	response.Fail(c, response.CodeInternal, "")
}
