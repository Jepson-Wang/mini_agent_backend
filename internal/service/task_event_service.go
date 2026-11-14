package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// maxEventPayload 是事件 payload 的长度上限(按字符数)。
//
// 必须有:tool_result 的 content 可能是**整个文件的内容**(read_file 工具),
// 几百 KB;事件会落库(payload 是一列 text,数据库自己不设上限),
// 也会原样推给浏览器。
//
// 8KiB 对"让用户看懂 Agent 在干什么"足够。理由同 maxEventErrMsg 和
// maxEventOutput:凡是会跟着响应回到用户眼前的内容,写入方就得限长。
const maxEventPayload = 8 * 1024

// TaskEventRecord 是一条待记录的事件。
//
// 合成一个结构而不是继续往参数表上加字符串:相邻的同类型参数
// 是典型的"传反了编译器也不吭声"的形状(同 service.CallbackResult)。
type TaskEventRecord struct {
	TaskUID   string
	SessionID string
	Seq       int64
	Type      string
	Payload   string
}

// EventNotifier 通知「某个会话有新事件了」。
//
// **它传的只是"有更新了"这个信号,不传事件本身。** 事件在库里,信号丢了
// 只是延迟,不会丢事件 —— 这是整个 SSE 设计的地基(见
// docs/plans/2026-11-13-sse-streaming.md 决策 1)。
//
// 定成接口而不是具体类型,有两个理由:service 不该知道唤醒信号是怎么发的
// (Redis Pub/Sub?本地 hub?),而且没配 Redis 时这一层要能整个退化成轮询。
type EventNotifier interface {
	Publish(ctx context.Context, sessionID string)
}

// TaskEventRecorder 记录任务事件。实现方是 *TaskEventService。
//
// 定在使用方(service 自己),和 taskStatusWriter 一个道理:
// 事件处理器的测试塞个假的就能跑,不连库。
type TaskEventRecorder interface {
	RecordEvent(ctx context.Context, ev TaskEventRecord) error
}

// eventStore 是事件表需要的最小能力,生产实现是 *repository.TaskEventRepo。
//
// 定成接口:service 层不碰 gorm,更重要的是测试能塞一个内存实现 ——
// 截断、通知这些逻辑不该为了验一行代码就去连库。
type eventStore interface {
	Append(ctx context.Context, ev *model.TaskEvent) error
	ListSince(ctx context.Context, sessionID string, afterID int64, limit int) ([]model.TaskEvent, error)
}

// sessionLookup 是取会话需要的能力,生产实现是 *repository.SessionRepo。
type sessionLookup interface {
	GetByID(ctx context.Context, sessionID string) (*model.Session, error)
}

// TaskEventService 负责任务事件的记录与读取。
//
// 它是"事件落库 = 权威"这条地基的落地点:事件先写进 task_events,
// 再通知订阅方。订阅方收到信号后去读库拿 id > lastID 的那几条,
// 所以信号丢了只会让推送晚一点,不会让事件消失。
//
// ## nil 接收者也是安全的
//
// 三个方法都先判 `s == nil` 再干活。原因是接线方有可能把一个
// **值为 nil 的 *TaskEventService** 塞进接口字段 ——
// 那时候接口本身不是 nil,调用方的 `== nil` 判不出来,
// 一调方法就在 nil 指针上炸。这里返回错误而不是 panic:
// 「代码接错了」和「用户的数据有问题」必须长得不一样。
type TaskEventService struct {
	events   eventStore
	sessions sessionLookup
	notifier EventNotifier
}

// NewTaskEventService 组装事件服务。
//
// notifier 传 nil 表示这个部署不做实时唤醒(比如没配 Redis):
// 事件照样落库,读的那侧退化成定时轮询 —— 事件是权威,信号只是加速器。
// 这和限流"没配 REDIS_ADDR 就不启用"的取向一致。
//
// **参数用的是未导出的接口(eventStore / sessionLookup),这不是笔误。**
// Go 允许导出函数用未导出的接口作参数,而且外部包**不需要引用接口名**:
// 传 *repository.TaskEventRepo 进去,编译器自己做隐式接口满足检查。
// 所以接线方写起来还是 `service.NewTaskEventService(repo.TaskEvent, repo.Session, notifier)`。
//
// 这样定的理由:service 层不碰 gorm,更重要的是测试能塞一个内存实现 ——
// 截断、通知、归属这些逻辑不该为了验一行代码就去连库。
func NewTaskEventService(events eventStore, sessions sessionLookup,
	notifier EventNotifier) *TaskEventService {

	s := &TaskEventService{notifier: notifier}
	// 显式判空再赋值:传进来的 nil 指针会变成一个**非 nil 的接口**
	// (接口里装着 nil 指针),后面的 s.events == nil 判断就不成立了,
	// 一调方法直接 panic(里面是 nil 的 db)。
	if !isNilPointer(events) {
		s.events = events
	}
	if !isNilPointer(sessions) {
		s.sessions = sessions
	}
	return s
}

// isNilPointer 报告一个依赖是不是"没接上"。
//
// 光判 `x == nil` 对接口参数不够用:调用方传一个 nil 的 *repository.TaskEventRepo
// 时,接口本身不是 nil(里面装着一个 nil 指针),普通的判空全部失效。
// 所以这里把"接口里的 nil 指针"也认出来,让依赖没接上时走的是
// "返回错误"而不是"panic"。
func isNilPointer(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Ptr && rv.IsNil()
}

// RecordEvent 记录一条事件,并通知订阅方。
//
// 返回的 error **只**表示"事件没进库"。通知失败不算错误 ——
// 事件已经在库里了,订阅方下一次唤醒(或者兜底轮询)就能拿到;
// 让它冒泡只会让调用方以为整条链路都坏了。
//
// 写库失败则原样交出去:调用方(TaskEventHandler)会记一条 Warn 就继续,
// 不让一条事件的失败把后面的事件(尤其是 finished)一起拖掉。
func (s *TaskEventService) RecordEvent(ctx context.Context, ev TaskEventRecord) error {
	if s == nil {
		return errors.New("事件服务未配置,无法记录事件")
	}
	if s.events == nil {
		return errors.New("事件表未配置,无法记录事件")
	}
	if ev.SessionID == "" {
		// 没有 session_id 就归不到任何会话上,这条事件谁都读不到。
		// 这是接线错误(session_id 没有一路带下来),不是正常输入。
		return errors.New("事件缺少 session_id,无法记录")
	}

	row := &model.TaskEvent{
		TaskUID:   ev.TaskUID,
		SessionID: ev.SessionID,
		Seq:       ev.Seq,
		Type:      ev.Type,
		// 截断在这里做,而不是在调用方:这里是 payload 进入库的**唯一入口**,
		// 放在这儿才能保证"库里躺着的每一条都已经被限长"。
		Payload: truncateEventPayload(ev.Payload, maxEventPayload),
	}
	if err := s.events.Append(ctx, row); err != nil {
		return fmt.Errorf("记录任务事件(type=%s, seq=%d): %w", ev.Type, ev.Seq, err)
	}

	// 先落库再通知:反过来的话,订阅方被唤醒时去读库可能什么都读不到。
	if s.notifier != nil {
		s.notify(ctx, ev.SessionID)
	}
	return nil
}

// notify 通知订阅方,不让它的失败影响事件流。
//
// Publish 没有错误返回值 —— 它只发一个"有更新了"的信号,发不出去也只是延迟。
// 但它是个外部依赖:实现方 panic 的话,这条事件流就断在这儿了,
// 后面那些事件(尤其是 finished)全都收不到,任务会永远停在 running。
// 所以这里自己兜住,记一条 Warn 就继续。
func (s *TaskEventService) notify(ctx context.Context, sessionID string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("通知事件订阅方时 panic", "session_id", sessionID, "panic", r)
		}
	}()
	s.notifier.Publish(ctx, sessionID)
}

// MustOwnSession 校验会话归属。不是这个用户的,返回 model.ErrSessionNotFound。
//
// **不返回"无权限"是有意的**:403 和 404 分开的话,这个接口就成了一台
// session_id 探测器 —— 拿到 403 说明这个 ID 真实存在,拿到 404 说明不存在。
// 对外必须是同一个结果。理由同 service.SessionService.mustOwn。
//
// 参数非法也归到 NotFound:单独给一个"参数错误"会让调用方能区分
// "ID 格式不对"和"ID 不属于我",而格式那关 handler 已经把过了。
func (s *TaskEventService) MustOwnSession(ctx context.Context, userID int64, sessionID string) error {
	if s == nil {
		return errors.New("事件服务未配置,无法校验归属")
	}
	if userID <= 0 || sessionID == "" {
		return model.ErrSessionNotFound
	}
	if s.sessions == nil {
		return errors.New("会话仓库未配置,无法校验归属")
	}

	sess, err := s.sessions.GetByID(ctx, sessionID)
	if err != nil {
		return err // 仓库层已翻译成 model.ErrSessionNotFound
	}
	if sess.UserID != userID {
		return model.ErrSessionNotFound
	}
	return nil
}

// ListEvents 读该会话的增量事件:id > afterID 的,按 id 升序,最多 limit 条。
//
// **调用方必须先调 MustOwnSession。** 这个方法按 session_id 读,
// 它自己不认用户身份 —— 事件表里没存 user_id(少一列、少一个可能填错的字段),
// 归属只能靠会话表推。鉴权在 service 层的 MustOwnSession 里做,
// handler 里出现 if sess.UserID != userID 就是层错了。
func (s *TaskEventService) ListEvents(ctx context.Context, sessionID string,
	afterID int64, limit int) ([]model.TaskEvent, error) {

	if s == nil {
		return nil, errors.New("事件服务未配置,无法读取事件")
	}
	if s.events == nil {
		return nil, errors.New("事件表未配置,无法读取事件")
	}
	evs, err := s.events.ListSince(ctx, sessionID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("读取会话 %s 的事件: %w", sessionID, err)
	}
	return evs, nil
}

// truncateEventPayload 按**字符数**截断事件 payload。
//
// 按 []rune 而不是 []byte:按字节切会把一个汉字劈成两半,落库就是一段乱码。
//
// 不复用 service.truncate:那个是给 tasks.error_msg / output 用的,
// 返回 *string(空值落 NULL)而且会去掉首尾空白。事件 payload 要**原样**保留 ——
// 消费方拿到的是 Agent 发的那一份,悄悄去掉首尾空白等于改了内容,
// 而且"解不出来就让事件失败"的约定要求消费方能自己面对各种形状的 payload。
func truncateEventPayload(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}
