package repository

import (
	"context"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// 事件表由包里的 migrateTestDB 建(和 User / Session / Task 一起),
// 表结构只有 model 这一处定义 —— 生产库的 AutoMigrate 用的也是同一个模型。
//
// 这里不再现建一张:两处各建一次的话,"测试库里的表结构"和"生产库里的
// 表结构"可能不一样,而测试照样绿着 —— 那是最坏的一种不一致。
func withEventRepo(t *testing.T, fn func(t *testing.T, r *Repository)) {
	t.Helper()
	withTx(t, fn)
}

// seedEvents 造 n 条事件,按 seq 递增。返回它们。
func seedEvents(t *testing.T, r *Repository, sessionID string, n int) []model.TaskEvent {
	t.Helper()
	ctx := context.Background()

	out := make([]model.TaskEvent, 0, n)
	for i := 1; i <= n; i++ {
		ev := &model.TaskEvent{
			TaskUID:   "01J8ZQXK7P0000000000000000",
			SessionID: sessionID,
			Seq:       int64(i),
			Type:      "thinking",
			Payload:   "内容",
		}
		if err := r.TaskEvent.Append(ctx, ev); err != nil {
			t.Fatalf("写事件 %d: %v", i, err)
		}
		out = append(out, *ev)
	}
	return out
}

const (
	evSessionA = "01J8ZQXK7P0000000000000001"
	evSessionB = "01J8ZQXK7P0000000000000002"
)

// TestTaskEventRepo_ListSince 增量读:SSE 的续传全靠它。
//
// 三件事一起验:只给 afterID 之后的、按 id 升序、不串别的会话。
func TestTaskEventRepo_ListSince(t *testing.T) {
	withEventRepo(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		seeded := seedEvents(t, r, evSessionA, 3)

		// 另一个会话的事件不许串进来。
		seedEvents(t, r, evSessionB, 2)

		got, err := r.TaskEvent.ListSince(ctx, evSessionA, 0, 10)
		if err != nil {
			t.Fatalf("ListSince: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("拿到 %d 条,期望 3 条", len(got))
		}
		for i, ev := range got {
			if ev.ID != seeded[i].ID {
				t.Errorf("第 %d 条的 id = %d,期望 %d", i, ev.ID, seeded[i].ID)
			}
			if ev.SessionID != evSessionA {
				t.Errorf("第 %d 条串了别的会话: %q", i, ev.SessionID)
			}
		}

		// afterID 之后的两条。
		tail, err := r.TaskEvent.ListSince(ctx, evSessionA, seeded[0].ID, 10)
		if err != nil {
			t.Fatalf("ListSince: %v", err)
		}
		if len(tail) != 2 {
			t.Fatalf("afterID 之后拿到 %d 条,期望 2 条", len(tail))
		}
		if tail[0].ID != seeded[1].ID || tail[1].ID != seeded[2].ID {
			t.Errorf("afterID 之后拿到的不是后两条: %d, %d", tail[0].ID, tail[1].ID)
		}

		// 已经是最后一条 → 空列表,不是错误。
		last, err := r.TaskEvent.ListSince(ctx, evSessionA, seeded[2].ID, 10)
		if err != nil {
			t.Fatalf("ListSince: %v", err)
		}
		if len(last) != 0 {
			t.Errorf("已经是最新的一条了,却还拿到 %d 条", len(last))
		}
	})
}

// limit 要真的生效:SSE 那边是"读完还有剩余就继续读",
// 一次给太多会把整个会话的历史一次灌进连接。
func TestTaskEventRepo_ListSince_RespectsLimit(t *testing.T) {
	withEventRepo(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		seedEvents(t, r, evSessionA, 5)

		got, err := r.TaskEvent.ListSince(ctx, evSessionA, 0, 2)
		if err != nil {
			t.Fatalf("ListSince: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("limit=2 却拿到 %d 条", len(got))
		}
	})
}

// limit <= 0 时用默认值,**不是**全表。
//
// GORM 的 Limit(负数) 是不写 LIMIT 子句 —— 等于把整个会话的事件历史
// 全捞进内存;Limit(0) 则是 LIMIT 0,静默返回空。两种都得挡。
//
// 这条特意造了 defaultEventListLimit+1 条:默认值真的生效的话拿回
// defaultEventLimit 条;写成 0 会拿回 0 条;不写 LIMIT 会拿回全部。
func TestTaskEventRepo_ListSince_NonPositiveLimitUsesDefault(t *testing.T) {
	withEventRepo(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		seedEvents(t, r, evSessionA, defaultEventListLimit+1)

		for _, limit := range []int{0, -1, -100} {
			got, err := r.TaskEvent.ListSince(ctx, evSessionA, 0, limit)
			if err != nil {
				t.Fatalf("ListSince(limit=%d): %v", limit, err)
			}
			if len(got) != defaultEventListLimit {
				t.Errorf("limit=%d 拿到 %d 条,期望 %d 条(默认值)",
					limit, len(got), defaultEventListLimit)
			}
		}
	})
}

// Append 要把自增 id 回填 —— SSE 的 id: 行就是它,
// 客户端拿它当 Last-Event-ID 续传。
func TestTaskEventRepo_Append_FillsID(t *testing.T) {
	withEventRepo(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()

		ev := &model.TaskEvent{
			TaskUID:   "01J8ZQXK7P0000000000000000",
			SessionID: evSessionA,
			Seq:       1,
			Type:      "thinking",
			Payload:   "内容",
		}
		if err := r.TaskEvent.Append(ctx, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if ev.ID <= 0 {
			t.Errorf("id 没回填: %d", ev.ID)
		}
		if ev.CreatedAt.IsZero() {
			t.Error("created_at 没回填")
		}
	})
}
