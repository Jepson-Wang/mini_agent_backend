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
		TaskUID:    t.TaskUID,
		SessionID:  t.SessionID,
		Status:     t.Status,
		Input:      t.Input,
		ErrorMsg:   t.ErrorMsg,
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
