package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeRevoker 是 TokenRevoker 的假实现。
type fakeRevoker struct {
	calls int
	jti   string
	exp   time.Time
	err   error
}

func (f *fakeRevoker) RevokeToken(_ context.Context, jti string, exp time.Time) error {
	f.calls++
	f.jti = jti
	f.exp = exp
	return f.err
}

// 登出要吊销当前这一个令牌。
func TestLogout_RevokesToken(t *testing.T) {
	rev := &fakeRevoker{}
	s := NewAuthService(nil, nil, rev)

	exp := time.Now().Add(time.Hour)
	if err := s.Logout(context.Background(), "JTI-1", exp); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if rev.calls != 1 {
		t.Errorf("吊销了 %d 次,期望 1 次", rev.calls)
	}
	if rev.jti != "JTI-1" {
		t.Errorf("吊销的 jti = %q,期望 %q", rev.jti, "JTI-1")
	}
	if !rev.exp.Equal(exp) {
		t.Errorf("吊销的 exp = %v,期望 %v", rev.exp, exp)
	}
}

// 没有吊销器(没启用吊销的部署)时,登出是**成功的空操作**。
//
// 返回 error 反而会让前端弹一个"登出失败",而实际上什么都没出错 ——
// 前端照样该清掉本地 token。
func TestLogout_NilRevokerIsNoop(t *testing.T) {
	s := NewAuthService(nil, nil, nil)
	if err := s.Logout(context.Background(), "JTI-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("没有吊销器时不该报错: %v", err)
	}
}

// 取不到 jti 时也是成功的空操作 —— 那个部署没启用吊销,没有可吊销的东西。
func TestLogout_EmptyJTIIsNoop(t *testing.T) {
	rev := &fakeRevoker{}
	s := NewAuthService(nil, nil, rev)

	if err := s.Logout(context.Background(), "", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("取不到 jti 时不该报错: %v", err)
	}
	if rev.calls != 0 {
		t.Errorf("没有 jti 却发了 %d 次吊销请求", rev.calls)
	}
}

// 吊销失败要往上抛 —— 前端需要知道这次登出没生效。
func TestLogout_RevokeErrorPropagates(t *testing.T) {
	rev := &fakeRevoker{err: errors.New("redis 连不上")}
	s := NewAuthService(nil, nil, rev)

	if err := s.Logout(context.Background(), "JTI-1", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("吊销失败必须报错")
	}
}
