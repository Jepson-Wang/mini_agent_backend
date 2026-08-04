package dto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ptr"
)

func sampleUser() *model.User {
	return &model.User{
		ID:           7,
		Email:        "alice@example.com",
		PasswordHash: "$2a$12$SECRET_HASH_MUST_NOT_LEAK_aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Nickname:     ptr.To("爱丽丝"),
		Status:       "active",
		LastLoginAt:  ptr.To(time.Now()),
		CreatedAt:    time.Now(),
	}
}

// 这条是整个 dto 包存在的理由。
func TestUserResp_NeverCarriesPasswordHash(t *testing.T) {
	u := sampleUser()

	raw, err := json.Marshal(NewUserResp(u))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	body := string(raw)

	if strings.Contains(body, u.PasswordHash) {
		t.Fatalf("散列出现在响应里: %s", body)
	}
	// 连片段都不能有 —— 万一将来有人只截了一半塞进去。
	for _, leak := range []string{"$2a$", "password", "Password", "hash"} {
		if strings.Contains(body, leak) {
			t.Errorf("响应里出现了 %q: %s", leak, body)
		}
	}
}

// 字段必须是显式列出来的。内嵌 model.User 的话,以后加一列敏感字段
// 就会自动出现在所有响应里,这条用例就是那道闸。
func TestUserResp_NoEmbeddedStruct(t *testing.T) {
	rt := reflect.TypeOf(UserResp{})
	for i := range rt.NumField() {
		if f := rt.Field(i); f.Anonymous {
			t.Errorf("UserResp 内嵌了 %s —— 请改成手写字段", f.Type)
		}
	}
}

// 锁死对外的字段集合。加字段是 API 变更,应该是一次显式的、要改测试的动作。
func TestUserResp_JSONKeys(t *testing.T) {
	raw, err := json.Marshal(NewUserResp(sampleUser()))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	want := map[string]bool{
		"id": true, "email": true, "nickname": true,
		"status": true, "created_at": true,
	}
	for k := range got {
		if !want[k] {
			t.Errorf("多出了字段 %q,确认它可以对外", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("缺少字段 %q", k)
	}
}

// 没设置昵称时要输出 null,不是空串 —— 客户端好判断"没填过"和"填了空"。
func TestUserResp_NilNickname(t *testing.T) {
	u := sampleUser()
	u.Nickname = nil

	raw, err := json.Marshal(NewUserResp(u))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(raw), `"nickname":null`) {
		t.Errorf("期望 nickname 为 null: %s", raw)
	}
}

func TestNewUserResp_Nil(t *testing.T) {
	if got := NewUserResp(nil); got != nil {
		t.Errorf("NewUserResp(nil) = %#v, 期望 nil", got)
	}
}

func TestNewAuthResp(t *testing.T) {
	u := sampleUser()
	expiresAt := time.Now().Add(2 * time.Hour)

	got := NewAuthResp(u, "the.jwt.token", expiresAt)

	if got.AccessToken != "the.jwt.token" {
		t.Errorf("AccessToken = %q", got.AccessToken)
	}
	if got.TokenType != "Bearer" {
		t.Errorf("TokenType = %q, 客户端要照着拼 Authorization 头", got.TokenType)
	}
	// 允许一点执行耗时。
	if got.ExpiresIn > 7200 || got.ExpiresIn < 7195 {
		t.Errorf("ExpiresIn = %d, 期望接近 7200", got.ExpiresIn)
	}
	if got.User == nil || got.User.ID != u.ID {
		t.Errorf("User = %#v", got.User)
	}

	// 令牌可以出现在这里(它本来就是要发给客户端的),散列不行。
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), u.PasswordHash) {
		t.Fatalf("散列出现在登录响应里: %s", raw)
	}
}
