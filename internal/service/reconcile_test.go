package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// fakeLister 是 staleLister 的假实现。回收器的逻辑跟 SQL 没关系,
// 这一层测的是"捞出来之后怎么处置",不连库。
type fakeLister struct {
	tasks []model.Task
	err   error

	// before / limit 记下最后一次查询的参数,用来验时间窗口算对了没有。
	before time.Time
	limit  int
	calls  int
}

func (f *fakeLister) ListStale(_ context.Context, before time.Time, limit int) ([]model.Task, error) {
	f.calls++
	f.before = before
	f.limit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.tasks, nil
}

func testReconcileCfg() config.Reconcile {
	return config.Reconcile{Interval: time.Minute, StaleAfter: 15 * time.Minute, Batch: 100}
}

func newTestReconciler(t *testing.T, l *fakeLister, w *fakeStatusWriter) *Reconciler {
	t.Helper()
	r, err := NewReconciler(l, w, testReconcileCfg())
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	if r == nil {
		t.Fatal("NewReconciler 返回了 nil,但配置里是启用的")
	}
	return r
}

func TestReconcile_MarksStaleTasksFailed(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{
		{TaskUID: "A", Status: model.TaskRunning},
		{TaskUID: "B", Status: model.TaskPending},
	}}
	w := newFakeStatusWriter()

	n, err := newTestReconciler(t, l, w).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("回收了 %d 个,期望 2", n)
	}
	if len(w.calls) != 2 {
		t.Fatalf("写了 %d 次状态,期望 2", len(w.calls))
	}
	for _, c := range w.calls {
		if c.status != model.TaskFailed {
			t.Errorf("%s 被写成了 %q,期望 %q", c.taskUID, c.status, model.TaskFailed)
		}
		if c.errMsg == nil || *c.errMsg == "" {
			t.Errorf("%s 没写失败原因,用户看不到任何解释", c.taskUID)
		}
	}
}

// 回收器只能写 failed。写 canceled 的话,用户会以为是自己取消的,
// 一个本该被排查的故障就这么被藏起来了 —— HandleCallback 那边也挡着,
// 这里是第二道。
func TestReconcile_NeverWritesCanceled(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{{TaskUID: "A", Status: model.TaskRunning}}}
	w := newFakeStatusWriter()

	if _, err := newTestReconciler(t, l, w).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for _, c := range w.calls {
		if c.status == model.TaskCanceled {
			t.Fatal("回收器把任务写成了 canceled —— 用户会以为是自己取消的,不会去查")
		}
	}
}

// TestReconcile_WindowIsStaleAfterAgo 验时间窗口的方向。
//
// 写成 now.Add(+staleAfter) 的话,回收器会把**所有**中间态任务一次全收掉,
// 包括刚建出来还没跑的。这个 bug 不会报错,只会在某天早上让所有人的任务
// 都变成 failed。
func TestReconcile_WindowIsStaleAfterAgo(t *testing.T) {
	l := &fakeLister{}
	r := newTestReconciler(t, l, newFakeStatusWriter())

	start := time.Now()
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !l.before.Before(start) {
		t.Fatalf("查询窗口 before=%v 不早于现在(%v)—— 方向反了,会把刚建的任务也收掉",
			l.before, start)
	}
	if d := start.Sub(l.before); d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("查询窗口距今 %v,期望约 15m", d)
	}
	if l.limit != 100 {
		t.Errorf("limit = %d,期望 100(不夹住的话 GORM 会捞全表)", l.limit)
	}
}

// 单个任务写失败不能让整轮停下 —— 一条坏记录不该让后面几十个都收不掉。
func TestReconcile_OneFailureDoesNotStopTheBatch(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{
		{TaskUID: "A", Status: model.TaskRunning},
		{TaskUID: "B", Status: model.TaskRunning},
	}}
	w := &fakeStatusWriter{applied: true, err: errors.New("库挂了")}

	n, err := newTestReconciler(t, l, w).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("单个任务失败不该让 RunOnce 报错: %v", err)
	}
	if n != 0 {
		t.Errorf("全都失败了却报收了 %d 个", n)
	}
	if len(w.calls) != 2 {
		t.Errorf("只试了 %d 个,期望两个都试过", len(w.calls))
	}
}

// 捞出来之后它自己完成了(cas 落空)。正常竞态,不算收掉,也不算错误。
func TestReconcile_LateFinishIsNotCounted(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{{TaskUID: "A", Status: model.TaskRunning}}}
	w := &fakeStatusWriter{applied: false}

	n, err := newTestReconciler(t, l, w).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("cas 没改到行却报收了 %d 个", n)
	}
}

func TestReconcile_ListErrorIsReported(t *testing.T) {
	l := &fakeLister{err: errors.New("连接超时")}

	if _, err := newTestReconciler(t, l, newFakeStatusWriter()).RunOnce(context.Background()); err == nil {
		t.Error("查询失败该往上报,否则回收器静默停摆没人知道")
	}
}

// ctx 取消后不该继续写库:进程正在退出,连接池随时会关。
func TestReconcile_StopsOnCanceledContext(t *testing.T) {
	l := &fakeLister{tasks: []model.Task{
		{TaskUID: "A", Status: model.TaskRunning},
		{TaskUID: "B", Status: model.TaskRunning},
	}}
	w := newFakeStatusWriter()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := newTestReconciler(t, l, w).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(w.calls) != 0 {
		t.Errorf("ctx 已取消还写了 %d 次库", len(w.calls))
	}
}

func TestNewReconciler_DisabledReturnsNil(t *testing.T) {
	cfg := testReconcileCfg()
	cfg.Interval = 0

	r, err := NewReconciler(&fakeLister{}, newFakeStatusWriter(), cfg)
	if err != nil {
		t.Fatalf("关掉回收器不该报错: %v", err)
	}
	if r != nil {
		t.Error("interval <= 0 表示不启用,该返回 nil")
	}
}

func TestNewReconciler_RejectsBadConfig(t *testing.T) {
	bad := map[string]config.Reconcile{
		"stale_after 为 0": {Interval: time.Minute, StaleAfter: 0, Batch: 10},
		"batch 为 0":       {Interval: time.Minute, StaleAfter: time.Minute, Batch: 0},
	}
	for name, cfg := range bad {
		if _, err := NewReconciler(&fakeLister{}, newFakeStatusWriter(), cfg); err == nil {
			t.Errorf("%s:该报错", name)
		}
	}
}

// Run 必须能被 ctx 停下,否则进程退不出去。
func TestReconciler_RunStopsOnContextCancel(t *testing.T) {
	cfg := testReconcileCfg()
	cfg.Interval = 10 * time.Millisecond

	r, err := NewReconciler(&fakeLister{}, newFakeStatusWriter(), cfg)
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 没退出,进程会关不掉")
	}
}
