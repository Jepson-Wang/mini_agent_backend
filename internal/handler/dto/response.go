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
