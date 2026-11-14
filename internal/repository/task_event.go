package repository

import (
	"context"
	"errors"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"gorm.io/gorm"
)

// TaskEventRepo 是任务事件的仓库。
//
// 这一层只管数据访问:写入一行、按会话增量读。业务规则(截断、通知、归属)
// 一律不在这里。
type TaskEventRepo struct{ db *gorm.DB }

// defaultEventListLimit 是增量读一次最多取多少条。
//
// 必须有默认值:调用方迟早会算出 limit=0 或者负数,而 GORM 的
// Limit(负数) 是**不写 LIMIT 子句**(clause/limit.go:16),等于把这个会话的
// 事件历史全捞进内存;Limit(0) 则是 LIMIT 0,静默返回空。两个都得挡。
const defaultEventListLimit = 200

// Append 写入一条事件。写入成功后 ev.ID / ev.CreatedAt 会被回填。
func (r *TaskEventRepo) Append(ctx context.Context, ev *model.TaskEvent) error {
	err := r.db.WithContext(ctx).Create(ev).Error
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		// session_id 指向的会话不存在。事件表目前没建外键,这个分支暂时
		// 不会触发;等加了外键就能直接生效(同 TaskRepo.Create 的做法)。
		return model.ErrSessionNotFound
	}
	// 重复键这里不会发生:事件表除了自增主键没有唯一约束。
	// (task_uid, seq) 那个索引不能是唯一的 —— 事件流断了会原样重发,
	// Agent 那边的 seq 每次从 1 重新数,同一个 (task_uid, seq) 会出现两次。
	return err
}

// ListSince 返回该会话下 id > afterID 的事件,按 id 升序,最多 limit 条。
//
// afterID 是 SSE 的续传游标:客户端记住最后收到的 id,重连时带上,
// 这里只给后面的。id 是全局单调的自增主键,所以"id > afterID"在同一个
// 会话内就是精确的增量 —— 不会漏,也不会重。
//
// limit <= 0 时用默认值,而不是当成 0 或者干脆不写 LIMIT ——
// 不写 LIMIT 就是全表(见 defaultEventListLimit 的注释)。
func (r *TaskEventRepo) ListSince(ctx context.Context, sessionID string,
	afterID int64, limit int) ([]model.TaskEvent, error) {

	if limit <= 0 {
		limit = defaultEventListLimit
	}

	var evs []model.TaskEvent
	err := r.db.WithContext(ctx).
		Where("session_id = ? AND id > ?", sessionID, afterID).
		Order("id ASC").
		Limit(limit).
		Find(&evs).Error
	if err != nil {
		// 出错就返回零值,不把可能半填充的切片一起交出去。
		return nil, err
	}
	return evs, nil
}
