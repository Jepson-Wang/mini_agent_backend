package sse

import "sync"

// Subscriber 是一个订阅者。
//
// C 的容量是 1:它只承载"有更新了"这个**信号**,不承载事件本身。
// 事件在数据库里,醒了之后去读增量。
type Subscriber struct {
	// SessionID 是它订阅的会话。
	//
	// 导出但**不是权威**:Hub 自己记了一份(见 Hub.owner),
	// 调用方改了这里,Hub 照样能把它摘干净。
	SessionID string

	// UserID 是它属于哪个用户。用来算「单用户连接数」那个闸。
	//
	// 同样不是权威,理由同 SessionID。
	UserID int64

	C chan struct{} // 有新事件时被唤醒。容量 1,非阻塞投递
}

// ownerEntry 是「订阅者 → 它属于哪个会话、哪个用户」这条反向索引的值。
//
// 为什么要有这张表:订阅者身上的 SessionID / UserID 是**导出字段**,
// 调用方改了它,Hub 就再也找不到这个订阅者,注销不掉 ——
// 泄漏一条连接、它的 goroutine,还有它在 users 计数里占的那一份。
// Hub 自己记一份,就不依赖别人不改导出字段。
type ownerEntry struct {
	sessionID string
	userID    int64
}

// Hub 是本地订阅表:sessionID -> 订阅者集合。
//
// 它只管**本副本**的连接。跨副本的唤醒走 Bus(Redis Pub/Sub)。
//
// ## 为什么 Notify 绝对不能阻塞
//
// Notify 是被事件发布方调用的 —— 也就是被 Agent 的事件流调用的。
// 那里面的 goroutine 还连着 Agent 的 gRPC 长流,一卡就是整个任务卡住。
// 所以 Notify 满了就丢:丢的是一次唤醒,不是数据
// (下一轮读库会把这一批事件都补上)。
//
// 反过来,订阅方那边有定时轮询兜底,信号丢了最多延迟一轮。
//
// ## 并发
//
// Hub 会被两类 goroutine 同时碰:
//   - HTTP handler 的 goroutine(注册/注销订阅)
//   - 事件发布方的 goroutine(Notify)
//
// 所以每个方法都加锁。锁的粒度是整个 Hub —— 订阅者数量是几十到几千的量级,
// 临界区只有几十纳秒,不值得为了并发度再拆一把。
type Hub struct {
	mu sync.Mutex
	// subs 是 sessionID -> 订阅者集合。
	// 用 map 而不是 slice:摘掉一个订阅者是 O(1),而且天然幂等 ——
	// 摘两次不会留下空 slice,也不会 panic。
	subs map[string]map[*Subscriber]struct{}
	// owner 记着每个订阅者属于哪个会话、哪个用户。
	//
	// 为什么不能直接读 sub.SessionID:那是导出字段,调用方改了它,
	// Hub 就再也找不到这个订阅者,注销不掉 —— 泄漏一条连接和它的 goroutine。
	// Hub 自己记一份,就不依赖别人不改导出字段。
	owner map[*Subscriber]ownerEntry
	// users 记着每个用户当前有几条连接。
	//
	// 为什么单独一张表:「每会话」和「全局」两个闸挡不住
	// 「一个账号有 N 个会话,于是开 N × MaxConnPerSession 条连接,
	// 把全局预算吃光,别的用户全部连不上」。
	// 单用户这个闸必须能按 user_id 计数,而 subs 是按会话分组的。
	users map[int64]int
}

// NewHub 建一个空的本地订阅表。
func NewHub() *Hub {
	return &Hub{
		subs:  make(map[string]map[*Subscriber]struct{}),
		owner: make(map[*Subscriber]ownerEntry),
		users: make(map[int64]int),
	}
}

// Subscribe 注册一个订阅者,返回它的唤醒通道。
//
// userID 是订阅者的用户 id,用来算单用户连接数那个闸。
// 传 0 表示"不知道是谁"(不影响正确性:只是这个订阅不计入任何用户的计数)。
//
// 容量 1 是刻意的:这个通道只表达"有更新了",不表达"有几条更新"。
// 读的人醒来之后是去读库里的增量,读到几条读几条。
// 容量大于 1 只会让"攒了 3 次唤醒"变成一种假象 ——
// 读的人会醒来 3 次,后两次读到空,白跑三趟。
func (h *Hub) Subscribe(sessionID string, userID int64) *Subscriber {
	sub := &Subscriber{
		SessionID: sessionID,
		UserID:    userID,
		C:         make(chan struct{}, 1),
	}
	if h == nil {
		// nil Hub 是"没有这个功能",不是程序 bug。
		// 返回一个可用的订阅者(调用方照样能 select),只是没人会通知它。
		return sub
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = make(map[string]map[*Subscriber]struct{})
	}
	if h.owner == nil {
		h.owner = make(map[*Subscriber]ownerEntry)
	}
	if h.users == nil {
		h.users = make(map[int64]int)
	}
	set := h.subs[sessionID]
	if set == nil {
		set = make(map[*Subscriber]struct{})
		h.subs[sessionID] = set
	}
	set[sub] = struct{}{}
	h.owner[sub] = ownerEntry{sessionID: sessionID, userID: userID}
	h.users[userID]++
	return sub
}

// Unsubscribe 摘掉一个订阅者。
//
// 幂等:摘两次不 panic(第二次找不到就是找不到)。
// 摘干净是这个方法存在的全部意义 —— 留在表里的订阅者会让 Count 虚高,
// 连接数上限会误判,内存也不会释放。
//
// **用户计数也在同一处减掉。** 减和加必须成对:
// 减漏了 → 那个用户的额度被慢慢吃光;减多了 → 额度虚高,闸形同虚设。
// 而且必须只减一次 —— 摘两次不能减两次。
// 幂等由 owner 这张表保证:第二次摘的时候 owner 里已经没这条了,直接返回。
//
// **不 close 订阅者的 channel。** close 和并发的 Notify 撞上会 panic
// ("send on closed channel"),而 Notify 是被别的 goroutine 调用的,
// 我们没法保证摘订阅的那一刻没有人在投递。
// 不 close 的代价是:读通道的一方不会收到"通道已关闭"的信号,
// 它得靠自己的 ctx 感知连接结束 —— SSE handler 本来就该靠 ctx 结束。
func (h *Hub) Unsubscribe(sub *Subscriber) {
	if h == nil || sub == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	owner, ok := h.owner[sub]
	if !ok {
		// 已经摘过了(或者压根不是这个 Hub 注册的)。幂等返回。
		return
	}
	delete(h.owner, sub)

	// 用户计数减一。减到 0 就把这一格删掉,不留 userID → 0 的残条目 ——
	// users 表是按用户长起来的,不清的话用得越久条目越多。
	if owner.userID != 0 {
		h.users[owner.userID]--
		if h.users[owner.userID] <= 0 {
			delete(h.users, owner.userID)
		}
	}

	set := h.subs[owner.sessionID]
	if set == nil {
		return
	}
	delete(set, sub)
	if len(set) == 0 {
		// 会话下没人了就把这一格整个删掉,不留空 map。
		delete(h.subs, owner.sessionID)
	}
}

// Notify 通知某个会话的所有本地订阅者:有更新了。
//
// 非阻塞:订阅者的通道满了就丢弃这次唤醒。
//
// 丢唤醒不丢数据 —— 订阅方醒来之后是去读库里的增量,
// 而且还有定时轮询兜底。反过来,阻塞在 Notify 里会让
// 事件发布方(Agent 事件流那条链路)卡住,那是更大的事故。
func (h *Hub) Notify(sessionID string) {
	if h == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[sessionID] {
		// 通道容量是 1:满的时候这次唤醒没有新信息可带,
		// 订阅方醒来之后照样能从库里读到全部增量。
		select {
		case sub.C <- struct{}{}:
		default:
		}
	}
}

// Count 返回当前所有订阅者的总数(跨会话)。
//
// 给**全局**连接数上限用(计划里的 SSE_MAX_CONN_TOTAL)。
// 单会话的上限用 CountSession,别拿这个去比。
func (h *Hub) Count() int {
	if h == nil {
		return 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, set := range h.subs {
		n += len(set)
	}
	return n
}

// CountSession 返回某个会话当前的订阅者数量。
//
// 给**单会话**连接数上限用(计划里的 SSE_MAX_CONN_PER_SESSION)。
// 和 Count() 的区别:Count() 是跨会话的总数(全局上限),
// CountSession 是单个会话的。
//
// ## 这是软限制,不是精确锁
//
// 调用方通常是"先查 CountSession,没超就 Subscribe"。
// **先查后订中间有缝**:两个并发请求可能都查到"还差一个",
// 然后都订阅成功,于是超了一个。
//
// 这个不要试图做成精确的 —— 理由和 service.TaskQuota 完全一样:
// 要精确就得把计数和插入放进同一个事务再锁用户行,
// 为一个防滥用的粗粒度阈值不值得。
//
// 而且超限一个的后果也就多占一个 goroutine 和一点内存,不是安全洞。
// 真正要防的是"一个脚本开一万个连接",那个量级下粗粒度阈值完全够用。
//
// **所以 Hub 有意不提供「原子地订阅且不超过上限」的方法。**
// 有了它,调用方会以为那个上限是硬的;而它其实不是。
// 一个没有文档的硬上限,比一个文档清楚的软上限更危险。
func (h *Hub) CountSession(sessionID string) int {
	if h == nil {
		return 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[sessionID])
}

// CountUser 返回某个用户当前有几条连接(跨会话)。
//
// 给**单用户**连接数上限用。
//
// ## 为什么需要这一层
//
// CountSession 按会话分组,Count 是全进程总数 —— 这两个都挡不住
// 「一个账号拥有 N 个会话」这件事:那个账号能开 N × MaxConnPerSession 条连接,
// 把全进程的预算吃光,于是**别的用户全部连不上**。
// 那等于一个账号能替所有人做决定,而 SSE 是给用户看自己的任务进度的。
//
// 和另外两个闸一样是软限制(先查后订中间有缝)。
func (h *Hub) CountUser(userID int64) int {
	if h == nil {
		return 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.users[userID]
}
