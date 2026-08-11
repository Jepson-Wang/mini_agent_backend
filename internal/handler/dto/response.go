package dto

import (
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// tokenTypeBearer 是固定值,客户端照着拼 "Authorization: Bearer <token>"。
// 写死在响应里而不是让客户端自己记,少一处口头约定。
const tokenTypeBearer = "Bearer"

// UserResp 是对外的用户视图。
//
// 字段是一个个手写出来的,没有内嵌 model.User。这是这个结构体存在的全部意义:
// 内嵌的话,以后谁往 model.User 上加一列敏感字段,它就会自动出现在
// 每一个返回用户的接口里,而 code review 很难注意到。
// 手写一遍,等于把「哪些字段可以对外」变成一个必须显式修改的决定。
//
// PasswordHash 永远不在这里,dto_test.go 里有一条断言盯着它。
type UserResp struct {
	ID       int64   `json:"id"`
	Email    string  `json:"email"`
	Nickname *string `json:"nickname"` // 没设置时是 null,不是空串
	Status   string  `json:"status"`

	// LastLoginAt 没放进来:用户自己看不出它有什么用,
	// 而它能反推出账号的活跃情况。真要展示就单开一个个人中心接口。
	CreatedAt time.Time `json:"created_at"`
}

// NewUserResp 从 model 转成对外视图。u 为 nil 时返回 nil。
func NewUserResp(u *model.User) *UserResp {
	if u == nil {
		return nil
	}
	return &UserResp{
		ID:        u.ID,
		Email:     u.Email,
		Nickname:  u.Nickname,
		Status:    u.Status,
		CreatedAt: u.CreatedAt,
	}
}

// AuthResp 是注册/登录成功后的返回。
type AuthResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`

	// ExpiresAt 是绝对时刻,ExpiresIn 是剩余秒数,两个都给:
	// 只给前者客户端得和服务器对时,只给后者跨时区调试很难受。
	// 客户端当然可以自己解 JWT 拿 exp —— 但客户端解出来的东西不该被信任,
	// 这两个字段是服务端给的同一份数据,拿来做 UI 是安全的。
	ExpiresAt time.Time `json:"expires_at"`
	ExpiresIn int64     `json:"expires_in"`

	User *UserResp `json:"user"`
}

// SessionResp 是对外的会话视图。
//
// 和 UserResp 一样手写字段,不内嵌 model.Session。漏掉的两个:
//   - UserID:能看到这条记录就说明它是你的,再回一遍没有信息量,
//     而且会把内部的用户主键散播到每一个会话响应里;
//   - DeletedAt:软删除是实现细节。列表里本来就查不到已删的,
//     给出去只会引出"能不能恢复"这类接口不支持的期待。
type SessionResp struct {
	SessionID string  `json:"session_id"`
	Status    string  `json:"status"`
	Title     *string `json:"title"` // 还没定标题时是 null,不是空串

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewSessionResp 从 model 转成对外视图。s 为 nil 时返回 nil。
func NewSessionResp(s *model.Session) *SessionResp {
	if s == nil {
		return nil
	}
	return &SessionResp{
		SessionID: s.SessionID,
		Status:    s.Status,
		Title:     s.Title,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

// SessionWithTaskResp 是"建会话"的返回:会话和它的首个任务一起给。
// 少一次往返 —— 客户端建完就要拿 task_uid 去轮询状态。
type SessionWithTaskResp struct {
	Session *SessionResp `json:"session"`
	Task    *TaskResp    `json:"task"`
}

// ListResp 是所有分页列表的统一外形。
//
// 没有 total。offset 分页的 total 要多打一次 COUNT,而它对客户端的用处
// 只有渲染页码 —— 这个产品是"往下滚"的形态,用不上。将来换成游标分页
// (WHERE (updated_at, session_id) < (?, ?))时 total 更是没有意义。
type ListResp[T any] struct {
	// Items 永远是数组,空的时候是 [] 而不是 null。
	// 少了 NewListResp 里那句初始化,nil 切片会被序列化成 null,
	// 客户端一个 items.map 就炸 —— 而且只在"这个用户一条数据都没有"时炸。
	Items []T `json:"items"`

	// 回显生效后的分页参数。客户端传 limit=999 时会被夹到 100,
	// 不回显的话它会以为自己拿到了 999 条里的全部。
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// NewListResp 组装列表响应。items 为 nil 时给一个空数组。
func NewListResp[T any](items []T, limit, offset int) ListResp[T] {
	if items == nil {
		items = []T{}
	}
	return ListResp[T]{Items: items, Limit: limit, Offset: offset}
}

// NewSessionListResp 把 model 列表转成对外视图。
func NewSessionListResp(ss []model.Session, limit, offset int) ListResp[*SessionResp] {
	items := make([]*SessionResp, 0, len(ss))
	for i := range ss {
		// 取 &ss[i] 而不是 range 的值变量:后者是一份拷贝,
		// 虽然这里 NewSessionResp 只读不存地址,写成这样更不容易出错。
		items = append(items, NewSessionResp(&ss[i]))
	}
	return NewListResp(items, limit, offset)
}

// NewTaskListResp 把 model 列表转成对外视图。
func NewTaskListResp(ts []model.Task, limit, offset int) ListResp[*TaskResp] {
	items := make([]*TaskResp, 0, len(ts))
	for i := range ts {
		items = append(items, NewTaskResp(&ts[i]))
	}
	return NewListResp(items, limit, offset)
}

// TaskResp 是对外的任务视图。
//
// 和 UserResp 一样手写字段、不内嵌 model.Task,理由也一样。
// 两个字段是刻意漏掉的:
//   - ID:自增主键。给出去等于把全站任务总量和增速一起送人
//     (下两次单,一减就知道这中间平台接了多少活)。
//   - UserID:能看到这条记录就说明它是你的,再回一遍没有信息量。
type TaskResp struct {
	TaskUID   string `json:"task_uid"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`

	Input *string `json:"input"`

	// ErrorMsg 是 Agent 报上来的失败原因,会原样展示给用户。
	// 写这个值的地方(dto.TaskCallbackReq)必须限长,否则这里会跟着一起爆。
	ErrorMsg *string `json:"error_msg"`

	// CancelReason 是用户自己填的取消理由,原样回显。
	// 不是终态 canceled 的任务这里恒为 null。
	CancelReason *string `json:"cancel_reason"`

	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// NewTaskResp 从 model 转成对外视图。t 为 nil 时返回 nil。
func NewTaskResp(t *model.Task) *TaskResp {
	if t == nil {
		return nil
	}
	return &TaskResp{
		TaskUID:      t.TaskUID,
		SessionID:    t.SessionID,
		Status:       t.Status,
		Input:        t.Input,
		ErrorMsg:     t.ErrorMsg,
		CancelReason: t.CancelReason,

		StartedAt:  t.StartedAt,
		FinishedAt: t.FinishedAt,
		CreatedAt:  t.CreatedAt,
		UpdatedAt:  t.UpdatedAt,
	}
}

// TaskCallbackResp 告诉回调方这次到底改没改到状态。
//
// applied=false 时接口仍然返回 200(见 handler.InternalHandler.TaskCallback),
// 所以必须有这么个字段 —— 否则"任务已经被取消了"和"状态成功推进"
// 在回调方眼里长得一模一样,出问题时查不出来。
type TaskCallbackResp struct {
	Applied bool `json:"applied"`
}

// NewAuthResp 组装登录态响应。expiresAt 由签发方给出,剩余秒数在这里算。
func NewAuthResp(u *model.User, token string, expiresAt time.Time) *AuthResp {
	return &AuthResp{
		AccessToken: token,
		TokenType:   tokenTypeBearer,
		ExpiresAt:   expiresAt,
		ExpiresIn:   int64(time.Until(expiresAt).Seconds()),
		User:        NewUserResp(u),
	}
}
