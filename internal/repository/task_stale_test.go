// task_stale_test.go 测回收器要用的两个查询:ListStale 和 Touch。
// 共用的 withTx / mustUser / mustSession / mustTask 在 repository_test.go。
package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// setUpdatedAt 把 updated_at 改到指定时刻。
//
// 只能走裸 SQL:UpdatedAt 上有 GORM 的自动更新语义,
// 用 Model().Update() 去写它,写进去的往往还是 now()。
func setUpdatedAt(t *testing.T, r *Repository, taskUID string, ts time.Time) {
	t.Helper()
	err := r.db.WithContext(context.Background()).
		Exec("UPDATE tasks SET updated_at = ? WHERE task_uid = ?", ts, taskUID).Error
	if err != nil {
		t.Fatalf("改 updated_at: %v", err)
	}
}

// setStatus 直接改状态,绕开 cas —— 造数据用,不是被测路径。
func setStatus(t *testing.T, r *Repository, taskUID, status string) {
	t.Helper()
	err := r.db.WithContext(context.Background()).
		Exec("UPDATE tasks SET status = ? WHERE task_uid = ?", status, taskUID).Error
	if err != nil {
		t.Fatalf("改 status: %v", err)
	}
}

// TestTaskRepo_ListStale 一次把三条规则钉住:
// 只捞中间态、只捞够旧的、按最旧优先。
func TestTaskRepo_ListStale(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)
		sess := mustSession(t, r, u.ID)
		now := time.Now()

		// 卡住的:running,一小时没动静。
		old := mustTask(t, r, sess, u.ID, now)
		setStatus(t, r, old.TaskUID, model.TaskRunning)
		setUpdatedAt(t, r, old.TaskUID, now.Add(-time.Hour))

		// 也卡住了,但没那么久 —— 用来验排序。
		mid := mustTask(t, r, sess, u.ID, now)
		setStatus(t, r, mid.TaskUID, model.TaskPending)
		setUpdatedAt(t, r, mid.TaskUID, now.Add(-30*time.Minute))

		// 刚刚才更新过,不该被捞。
		fresh := mustTask(t, r, sess, u.ID, now)
		setStatus(t, r, fresh.TaskUID, model.TaskRunning)

		// 已经是终态,再旧也不该被捞 —— 捞出来也改不动(cas 会落空),
		// 每一轮重复捞同一批就是白烧数据库。
		done := mustTask(t, r, sess, u.ID, now)
		setStatus(t, r, done.TaskUID, model.TaskSucceeded)
		setUpdatedAt(t, r, done.TaskUID, now.Add(-2*time.Hour))

		got, err := r.Task.ListStale(ctx, now.Add(-15*time.Minute), 10)
		if err != nil {
			t.Fatalf("ListStale: %v", err)
		}

		var uids []string
		for _, x := range got {
			uids = append(uids, x.TaskUID)
		}
		if len(uids) != 2 {
			t.Fatalf("捞到 %d 个 %v,期望 2 个(old / mid)", len(uids), uids)
		}
		if uids[0] != old.TaskUID {
			t.Errorf("第一个是 %s,期望卡得最久的 %s —— 排序反了的话,"+
				"积压超过 batch 时最老的那批永远轮不上", uids[0], old.TaskUID)
		}
		if uids[1] != mid.TaskUID {
			t.Errorf("第二个是 %s,期望 %s", uids[1], mid.TaskUID)
		}
	})
}

// limit <= 0 必须返回空,不能退化成全表扫。
// GORM 的 Limit(0) 是**不写 LIMIT 子句**,而回收器跑在后台,
// 真把整张 tasks 捞进内存也没人会看见。
func TestTaskRepo_ListStale_NonPositiveLimitReturnsNothing(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)
		sess := mustSession(t, r, u.ID)

		task := mustTask(t, r, sess, u.ID, time.Now())
		setStatus(t, r, task.TaskUID, model.TaskRunning)
		setUpdatedAt(t, r, task.TaskUID, time.Now().Add(-time.Hour))

		for _, limit := range []int{0, -1} {
			got, err := r.Task.ListStale(ctx, time.Now(), limit)
			if err != nil {
				t.Fatalf("limit=%d: %v", limit, err)
			}
			if len(got) != 0 {
				t.Errorf("limit=%d 捞到了 %d 条,期望 0 —— 不夹住就是全表", limit, len(got))
			}
		}
	})
}

func TestTaskRepo_Touch_RefreshesUpdatedAt(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)
		sess := mustSession(t, r, u.ID)

		task := mustTask(t, r, sess, u.ID, time.Now())
		setStatus(t, r, task.TaskUID, model.TaskRunning)
		stale := time.Now().Add(-time.Hour)
		setUpdatedAt(t, r, task.TaskUID, stale)

		ok, err := r.Task.Touch(ctx, task.TaskUID, time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("Touch: %v", err)
		}
		if !ok {
			t.Fatal("一小时没动静的 running 任务该被续上")
		}

		after, err := r.Task.GetByUID(ctx, task.TaskUID)
		if err != nil {
			t.Fatalf("重查: %v", err)
		}
		if !after.UpdatedAt.After(stale) {
			t.Errorf("updated_at 还是 %v,没被刷新", after.UpdatedAt)
		}
		if after.Status != model.TaskRunning {
			t.Errorf("Touch 把状态改成了 %q,它只该动 updated_at", after.Status)
		}
	})
}

// TestTaskRepo_Touch_ThrottledInSQL 验节流。
//
// 节流放在 WHERE 里(而不是内存里)是刻意的:多副本部署时,
// 每个副本各记各的内存状态,节流就形同虚设。
func TestTaskRepo_Touch_ThrottledInSQL(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)
		sess := mustSession(t, r, u.ID)

		task := mustTask(t, r, sess, u.ID, time.Now())
		setStatus(t, r, task.TaskUID, model.TaskRunning)
		// updated_at 就是刚才,离"30 秒前"这条线还差得远。
		setUpdatedAt(t, r, task.TaskUID, time.Now())

		ok, err := r.Task.Touch(ctx, task.TaskUID, time.Now().Add(-30*time.Second))
		if err != nil {
			t.Fatalf("Touch: %v", err)
		}
		if ok {
			t.Error("刚更新过的任务又被写了一次 —— 一个话痨 Agent 会把这一行的行锁打满")
		}
	})
}

// 只续 running。pending 意味着还没交接成功,那正是回收器该来看的状态,
// 给它续命等于把交接失败永远藏起来。终态更不能碰。
func TestTaskRepo_Touch_OnlyRunning(t *testing.T) {
	for _, status := range []string{
		model.TaskPending, model.TaskSucceeded, model.TaskFailed, model.TaskCanceled,
	} {
		t.Run(status, func(t *testing.T) {
			withTx(t, func(t *testing.T, r *Repository) {
				ctx := context.Background()
				u := mustUser(t, r)
				sess := mustSession(t, r, u.ID)

				task := mustTask(t, r, sess, u.ID, time.Now())
				setStatus(t, r, task.TaskUID, status)
				setUpdatedAt(t, r, task.TaskUID, time.Now().Add(-time.Hour))

				ok, err := r.Task.Touch(ctx, task.TaskUID, time.Now())
				if err != nil {
					t.Fatalf("Touch: %v", err)
				}
				if ok {
					t.Errorf("状态是 %s 却被续了命", status)
				}
			})
		})
	}
}

func TestTaskRepo_Touch_UnknownTaskIsNotAnError(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ok, err := r.Task.Touch(context.Background(), "01J8ZQXK7P0000000000000000", time.Now())
		if err != nil {
			t.Fatalf("不存在的任务不该报错: %v", err)
		}
		if ok {
			t.Error("不存在的任务不该报告续命成功")
		}
	})
}
