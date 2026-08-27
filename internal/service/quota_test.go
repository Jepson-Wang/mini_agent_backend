package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// fakeCounter 记下被问到的参数,并按脚本回答。
// 有它这个包的配额用例才**不需要 TEST_DSN** —— 配额逻辑跟 SQL 无关,
// 而需要库的用例在没有 DSN 时会被跳过(见 main_test.go),
// 把一条防滥用的规则押在那种用例上等于没测。
type fakeCounter struct {
	n     int64
	err   error
	since time.Time // 最后一次被问到的窗口起点
	calls int
}

func (f *fakeCounter) CountByUserSince(_ context.Context, _ int64, since time.Time) (int64, error) {
	f.calls++
	f.since = since
	return f.n, f.err
}

func quotaWith(t *testing.T, c taskCounter, max int, window time.Duration) *TaskQuota {
	t.Helper()
	q, err := NewTaskQuota(c, config.Quota{Window: window, MaxTasks: max})
	if err != nil {
		t.Fatalf("NewTaskQuota: %v", err)
	}
	return q
}

func TestTaskQuota_AllowsUnderLimit(t *testing.T) {
	c := &fakeCounter{n: 9}
	if err := quotaWith(t, c, 10, time.Hour).Check(context.Background(), 1); err != nil {
		t.Fatalf("9/10 应当放行,却拿到: %v", err)
	}
}

// 边界取 >=,不是 >:已经建了 10 个、上限 10,再建就是第 11 个。
func TestTaskQuota_RejectsAtLimit(t *testing.T) {
	c := &fakeCounter{n: 10}
	err := quotaWith(t, c, 10, time.Hour).Check(context.Background(), 1)
	if !errors.Is(err, model.ErrTaskQuotaExceeded) {
		t.Fatalf("10/10 应当拒绝并给 ErrTaskQuotaExceeded,却拿到: %v", err)
	}
}

// nil 接收者 = 这个部署没开配额,一律放行。
// 调用方靠这个约定省掉 if s.quota != nil,所以它是接口的一部分,不是巧合。
func TestTaskQuota_NilAllowsEverything(t *testing.T) {
	var q *TaskQuota
	if err := q.Check(context.Background(), 1); err != nil {
		t.Fatalf("nil 配额应当放行,却拿到: %v", err)
	}
}

// MaxTasks <= 0 表示不限额,构造函数返回 nil 而不是一个"上限为 0"的检查器。
// 返回后者的话所有请求都会被拒 —— 把"不限额"实现成"全禁"是最糟的失败方向。
func TestNewTaskQuota_DisabledReturnsNil(t *testing.T) {
	for _, max := range []int{0, -1} {
		q, err := NewTaskQuota(&fakeCounter{}, config.Quota{Window: time.Hour, MaxTasks: max})
		if err != nil {
			t.Fatalf("max=%d: %v", max, err)
		}
		if q != nil {
			t.Fatalf("max=%d 应当返回 nil(不限额),却拿到 %#v", max, q)
		}
	}
}

// 数不出来就放行。配额是软限制,而这条路径上的库故障会让紧接着的 INSERT
// 自己失败 —— 在这儿拦下来只会把"建任务失败"变成一个指向错误方向的"配额超限"。
func TestTaskQuota_CountErrorDoesNotBlock(t *testing.T) {
	c := &fakeCounter{n: 999, err: errors.New("db down")}
	if err := quotaWith(t, c, 1, time.Hour).Check(context.Background(), 1); err != nil {
		t.Fatalf("计数失败时应当放行,却拿到: %v", err)
	}
}

// 滑动窗口:问的是"此刻往前 window 之内",不是自然日。
func TestTaskQuota_UsesSlidingWindow(t *testing.T) {
	c := &fakeCounter{n: 0}
	q := quotaWith(t, c, 10, 24*time.Hour)

	fixed := time.Date(2026, 8, 23, 13, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return fixed }

	if err := q.Check(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	want := fixed.Add(-24 * time.Hour)
	if !c.since.Equal(want) {
		t.Fatalf("窗口起点 = %v, 想要 %v", c.since, want)
	}
}

// 错误里带上真实数字,方便运维在日志里看清是谁在刷。
// 但它不能出现在对外文案里 —— 那条由 response.errRules 单独写死。
func TestTaskQuota_ErrorCarriesNumbersForLogs(t *testing.T) {
	err := quotaWith(t, &fakeCounter{n: 200}, 200, 24*time.Hour).Check(context.Background(), 1)
	if err == nil {
		t.Fatal("应当拒绝")
	}
	if got := err.Error(); got == model.ErrTaskQuotaExceeded.Error() {
		t.Fatal("错误里应当带上 数量/上限/窗口,便于日志定位")
	}
}
