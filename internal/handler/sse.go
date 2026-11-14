package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/sse"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
)

// SSEStreamService 是 SSE handler 需要的能力。生产实现是 *service.TaskEventService。
//
// 定在 handler 包(使用方)而不是 service 包:handler 的测试塞个假的就能跑,
// 不连库不签 JWT。同 handler.TaskService、handler.AuthService 的做法。
//
// 只列真要用的方法 —— 这是最小暴露面:以后谁往 *service.TaskEventService 上
// 加一个方法,HTTP 层够不着它,除非有人显式在这儿再敲一行。
type SSEStreamService interface {
	// MustOwnSession 校验会话归属。不是这个用户的,返回 model.ErrSessionNotFound。
	//
	// **归属校验只在这里做**,handler 拿到什么传什么,不自己比对 ——
	// handler 里出现 if sess.UserID != userID 就是层错了。
	MustOwnSession(ctx context.Context, userID int64, sessionID string) error

	// ListEvents 读该会话的增量事件(id > afterID),按 id 升序。
	//
	// **它不认用户身份**,调用方必须先调 MustOwnSession。
	ListEvents(ctx context.Context, sessionID string, afterID int64, limit int) ([]model.TaskEvent, error)
}

// SSEHandler 把任务事件以 SSE(Server-Sent Events)的形式推给浏览器。
//
// 挂 GET /api/v1/sessions/:id/stream,订阅粒度是**会话**:
// 一个会话里可能先后有多个任务,一个连接看全这个会话的任务流。
type SSEHandler struct {
	svc SSEStreamService

	// hub 是本副本的订阅表。它只管本机的连接 ——
	// 跨副本的唤醒走 sse.Bus(Redis Pub/Sub),而 bus 收到信号之后
	// 通知的仍然是本地的 hub。
	hub *sse.Hub

	cfg config.SSE

	// shutdown 是关停信号。收到之后这条连接发一帧 shutdown 然后正常收尾。
	//
	// nil 是安全的默认值:select 一个 nil channel 会永久阻塞,那一路等于没有。
	shutdown <-chan struct{}
}

// NewSSEHandler 组装 SSE handler。
//
// cfg 里的非法值(<= 0)在这里就换回默认值(config.SSE.WithDefaults),
// 免得"忘了填"和"填错了"变成同一个故障 —— 后者更难查。
func NewSSEHandler(svc SSEStreamService, hub *sse.Hub, cfg config.SSE,
	shutdown <-chan struct{}) *SSEHandler {

	return &SSEHandler{
		svc:      svc,
		hub:      hub,
		cfg:      cfg.WithDefaults(),
		shutdown: shutdown,
	}
}

// sseEventData 是帧里 data 行的形状。
//
// 字段名定下来就别改:前端按这些名字解析,改名字等于改协议。
//
// payload 是**原始的事件内容**:thinking 是整段文本,tool_call / tool_result 是 JSON。
// 服务端不做二次加工 —— 消费方按 type 自己解,两套解析逻辑迟早会漂。
// 长度截断在 service 层做(那是 payload 进入库的**唯一入口**),handler 不再改一遍。
type sseEventData struct {
	TaskUID   string `json:"task_uid"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

// shutdownData 是关停帧的 data。
type shutdownData struct {
	// Reason 是个固定字符串,不是错误原文 —— 这一帧会进浏览器的日志,
	// 而关停原因里可能带着内部地址。
	Reason string `json:"reason"`
}

// Stream 处理 GET /api/v1/sessions/:id/stream。
//
// ## 两个阶段,边界很硬
//
// **握手阶段**(还没写任何 SSE 响应头):失败走统一 JSON 外壳
// (response.Fail / response.FailFromError),客户端能解析出状态码和业务码。
//
// **流阶段**(响应头已经写出去了):**绝对不能再调 response.Fail / response.OK**。
// 状态码已经发出去了,改不了;再往流里写 JSON 就是往 SSE 流里混垃圾,
// 浏览器会把它解析成一个莫名其妙的事件。
//
// 所以顺序是硬的:**先把所有可能失败的检查做完,再写 SSE 响应头。**
//
// ## 可能的结果
//
//	40000  session_id 不是 26 位 ULID
//	40100  没带令牌 / 令牌无效(middleware.Auth 拦下,到不了这里)
//	40400  会话不存在,**或者不是你的** —— 两种必须长得一模一样,
//	       分开的话这个接口就成了 session_id 探测器
//	50302  实时连接数已达上限(每会话 / 单用户 / 全进程)
//	50303  实时推送被关掉了(sse.enabled=false)
//	50000  依赖没注入,或者别的一切
func (h *SSEHandler) Stream(c *gin.Context) {
	// ==================== 握手阶段 ====================
	// 这里的任何失败都走统一 JSON 外壳:响应头还没写出去,
	// 状态码和业务码都还能改。
	if !h.ready(c) {
		return
	}

	sessionID, ok := ulidParam(c, "id", "会话")
	if !ok {
		return
	}

	// 归属校验在 service 层(handler 里出现 if sess.UserID != userID 就是层错了)。
	//
	// 放在连接数上限**之前**:归属没过就该是 404,不该因为「这个会话现在满了」
	// 回一个 503 —— 那等于告诉别人这个 session_id 真实存在。
	userID := middleware.MustUserID(c)
	if err := h.svc.MustOwnSession(c.Request.Context(), userID, sessionID); err != nil {
		response.FailFromError(c, err)
		return
	}

	// 连接数上限。三级:每会话、单用户、全进程。
	//
	// 这是**软限制**:先查后订中间有缝,两个并发请求可能都通过,
	// 于是超限一个。超限一个的后果只是多占一个 goroutine 和一点内存,
	// 不是安全洞 —— 真正要防的是"一个脚本开一万个连接"。
	// 详见 sse.Hub.CountSession 的注释。
	//
	// 单用户这一级是**必须有**的:只有「每会话」和「全局」两个闸的话,
	// 一个账号拥有 N 个会话就能开 N × MaxConnPerSession 条连接,
	// 把全局预算吃光,于是别的用户全部连不上 —— 那等于一个账号替所有人做决定。
	if h.hub.CountSession(sessionID) >= h.cfg.MaxConnPerSession ||
		h.hub.CountUser(userID) >= h.cfg.MaxConnPerUser ||
		h.hub.Count() >= h.cfg.MaxConnTotal {
		response.FailFromError(c, model.ErrSSEOverloaded)
		return
	}

	// 续传游标。解析和校正都在写响应头之前做完:
	// 校正要查一次库,而查库可能失败 —— 失败了还能回一个正经的 JSON 错误,
	// 等响应头写出去了就只剩"流断在半路"了。
	lastID, err := h.resolveLastID(c.Request.Context(), sessionID,
		parseLastEventID(c.GetHeader("Last-Event-ID")))
	if err != nil {
		response.FailFromError(c, err)
		return
	}

	// ==================== 流阶段 ====================
	// 从这里开始响应头就写出去了。**不能再调 response.Fail / response.OK**。

	// 注册订阅。defer 摘掉:客户端断开时必须摘干净,
	// 否则订阅者会一直留在表里 —— 连接数上限会误判,内存也还占着。
	//
	// userID 一起登记:单用户那个闸要靠它计数。
	sub := h.hub.Subscribe(sessionID, userID)
	defer h.hub.Unsubscribe(sub)

	h.writeHeaders(c)

	// 连接刚建立时先补一次历史:
	//   - 客户端带着 lastID 来 → 补上它错过的那一段;
	//   - 客户端是新来的   → 补上「它连上之前就发生的事」,
	//     否则前端会以为自己打开会话时任务还没开始。
	//
	// 单轮补发有上限(见 drain 的 max 参数):一个攒了几万条事件的会话,
	// 一次重连就把整段历史一口气写出去,那是一次性的放大。
	// 超出的部分不会丢 —— 游标推过去了,下一轮继续发。
	if err := h.drain(c, sessionID, &lastID, h.cfg.MaxReplayPerDrain); err != nil {
		h.abort(c, err)
		return
	}

	h.loop(c, sessionID, sub, &lastID)
}

// loop 是流的主循环。
//
// 醒来有四种原因,四种都得有:
//
//	sub.C       本副本有人写了新事件(或者 bus 收到跨副本的信号)
//	poll.C      兜底轮询 —— 信号可能丢(Redis 抖动、副本重启),
//	            没配 Redis 时这是**唯一**的推送来源
//	hb.C        心跳,保活
//	ctx.Done()  客户端断开
//
// **不用 gin 的 c.Stream**:它依赖 w.CloseNotify(),而
// httptest.ResponseRecorder 根本不实现 http.CloseNotifier,
// 那个类型断言会 panic —— 测试里根本没法用。HTTP/1.1 下 CloseNotify
// 返回的还是懒创建的 channel,对 GET 请求不可靠。
//
// 改用 c.Request.Context().Done():Go 文档明确写了,对入站的 server 请求,
// 「客户端连接关闭时 context 被取消」。HTTP/1.1 和 HTTP/2 都可靠,
// 而且测试里可以自己 cancel。
func (h *SSEHandler) loop(c *gin.Context, sessionID string, sub *sse.Subscriber, lastID *int64) {
	ctx := c.Request.Context()

	poll := time.NewTicker(h.cfg.PollInterval)
	defer poll.Stop()
	hb := time.NewTicker(h.cfg.Heartbeat)
	defer hb.Stop()

	for {
		select {
		case <-ctx.Done():
			// 客户端断开。连接就此结束,defer 会把订阅摘掉。
			return
		case <-h.shutdown:
			// 服务端主动关停。发一帧 shutdown,让客户端能分清
			// 「服务端要退了」和「连接莫名其妙断了」。
			h.writeShutdown(c)
			return
		case <-sub.C:
			// 有人写了新事件。往下走去读库。
		case <-poll.C:
			// 兜底轮询。往下走去读库。
		case <-hb.C:
			// 心跳。写完继续等,不查库。
			if err := h.writeHeartbeat(c); err != nil {
				h.abort(c, err)
				return
			}
			continue
		}

		// 被唤醒:去读库里的增量。
		//
		// **信号只说"有更新了",不带事件本身** —— 事件在库里,
		// 收到信号之后是"去读 id > lastID 的那些",不是"拿这条事件"。
		// 所以信号丢了不丢数据,只丢实时性。
		//
		// 单轮上限:live 这一轮同样有上限。一个攒了很多事件的会话,
		// 一次唤醒就把整段历史写出去也是放大 —— 上限按住的正是这个突发量。
		// 超出的部分下一轮继续(游标已经推过去了,不会丢)。
		if err := h.drain(c, sessionID, lastID, h.cfg.MaxReplayPerDrain); err != nil {
			h.abort(c, err)
			return
		}

		// 刚查过库,把兜底轮询的计时重置。
		//
		// 为什么:兜底轮询是**每连接**一次查询,连接一多就是
		// 「连接数 × 频率」的常驻开销(1000 条连接 ≈ 1000 QPS)。
		// 而收到唤醒信号时我们**已经**查过库了 —— 这一轮不需要
		// 立刻再补一次查询,等下一个完整周期就行。
		//
		// 效果:有 Redis 时信号和事件几乎同步到达,轮询查询几乎归零,
		// 兜底只在信号真的丢了(Redis 抖动、副本重启)时才起作用;
		// 没配 Redis 时没有信号,ticker 照常走,行为不变。
		poll.Reset(h.cfg.PollInterval)
	}
}

// drain 把库里 id > *lastID 的事件写成帧发出去,并推进游标。
//
// **一批没读满就接着读**:读满 limit 条说明后面可能还有,
// 而 SSE 的语义是「从 lastID 之后的一条都不能漏」。
//
// ## max 是「这一轮最多下发多少条」
//
// max <= 0 表示不限。超过上限就**停下**,但**不丢**:
// 游标已经推进到最后一个发出去的事件,剩下的下一轮(兜底轮询或唤醒信号)继续发。
//
// 为什么要有它:一轮推送可能读好几批(读到读不满为止),而一个攒了几万条事件的
// 会话,一次重连补发历史就会把整段历史一口气写出去 ——
// 那是一次性的放大(一次查库 + 一次巨量写),单次查询的上限(ReplayLimit)拦不住。
//
// 上限按住的是**单轮的突发量**,不是总量:总量仍然是「这个会话自己的历史」,
// 会被完整送达,只是分摊到几轮里。
func (h *SSEHandler) drain(c *gin.Context, sessionID string, lastID *int64, max int) error {
	sent := 0
	for {
		// 上一轮已经发满了:剩下的等下一轮。
		//
		// 游标已经推进到最后一个发出去的事件,所以下一轮接着发就行,
		// 不会重发,也不会丢。
		if max > 0 && sent >= max {
			return nil
		}

		before := *lastID
		evs, err := h.svc.ListEvents(c.Request.Context(), sessionID, *lastID, h.cfg.ReplayLimit)
		if err != nil {
			return fmt.Errorf("读取会话 %s 的事件: %w", sessionID, err)
		}
		if len(evs) == 0 {
			return nil
		}

		// 这一批最多发多少条。
		//
		// ★ 上限必须在**批内**也管住,不能只在批与批之间查:
		// 一次查询就能拿回 ReplayLimit 条(默认 200),如果只查批间,
		// 单批就把上限冲过去了 —— 上限形同虚设。
		n := len(evs)
		truncated := false
		if max > 0 && sent+n > max {
			n = max - sent
			truncated = true
		}

		for i := 0; i < n; i++ {
			if err := h.writeEvent(c, &evs[i], lastID); err != nil {
				return err
			}
			sent++
		}

		// 刷一次。net/http 的响应是带缓冲的,不刷的话事件会攒在缓冲区里,
		// 等写满或者连接结束才吐出去 —— 那就不叫流式了。
		//
		// 写失败(客户端断了)在上面 writeEvent 里就报出来了,
		// gin 的 Flush() 本身不返回错误。
		c.Writer.Flush()

		if *lastID <= before {
			// 游标没前进。仓库层的增量读坏了(或者返回了 id <= afterID 的行)。
			// 停在这儿:继续转会变成一个查库的死循环。
			slog.Warn("SSE 增量读没有推进游标,停止本轮推送",
				"trace_id", trace.FromGin(c),
				"session_id", sessionID,
				"last_id", before)
			return nil
		}
		if truncated {
			// 撞上这一轮的上限了。剩下的下一轮继续 ——
			// 游标已经推进了,所以不会重发,也不会丢。
			return nil
		}
		if len(evs) < h.cfg.ReplayLimit {
			// 没读满,后面没有了。
			return nil
		}
	}
}

// writeEvent 把一条事件写成帧写出去,并把游标推到这条事件之后。
//
// 编码失败(理论上只有事件类型里带换行才会)不算致命:丢这一条,
// 游标照样往前推 —— 反复重试同一条坏事件只会把连接拖死,而
// 「丢一条」比「发一条坏帧」好:坏帧会把浏览器的解析器带歪,后面所有帧都可能跟着错。
//
// 写失败(客户端断了)才终止这条流。
func (h *SSEHandler) writeEvent(c *gin.Context, ev *model.TaskEvent, lastID *int64) error {
	frame, err := sse.Encode(sse.Frame{
		// 库的全局自增 id 直接当 SSE 的 id 行:客户端记住它,
		// 重连时作为 Last-Event-ID 带回来,就是精确的续传游标。
		ID:    ev.ID,
		Event: ev.Type,
		Data: sseEventData{
			TaskUID:   ev.TaskUID,
			SessionID: ev.SessionID,
			Seq:       ev.Seq,
			Type:      ev.Type,
			Payload:   ev.Payload,
			CreatedAt: ev.CreatedAt.UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		slog.Warn("编码 SSE 帧失败,跳过这条事件",
			"trace_id", trace.FromGin(c),
			"event_id", ev.ID,
			"error", err)
		*lastID = ev.ID
		return nil
	}
	if _, err := c.Writer.Write(frame); err != nil {
		return err
	}
	*lastID = ev.ID
	return nil
}

// writeHeaders 写出 SSE 的响应头。
//
// **X-Accel-Buffering: no 必须有**:nginx 默认会缓冲上游响应,
// 事件会被攒着一起发,实时性就没了。
//
// Connection: keep-alive 是 HTTP/1.1 的默认行为,写出来是为了让
// 中间的代理明白这是一条长连接,而不是一个一次性响应。
func (h *SSEHandler) writeHeaders(c *gin.Context) {
	hd := c.Writer.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	// 立刻刷一次:响应头要先到客户端,浏览器才知道这是一条流,
	// 后面才有地方放事件。
	c.Writer.Flush()
}

// writeHeartbeat 发一条心跳注释。
//
// nginx 的 proxy_read_timeout 默认 60s,不心跳连接会被静默掐断,
// 而客户端分不清「服务端不说话」和「连接断了」。
func (h *SSEHandler) writeHeartbeat(c *gin.Context) error {
	if _, err := c.Writer.Write(sse.EncodeComment("keepalive")); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

// writeShutdown 发一帧「服务端要关停」。
//
// 有了这一帧,客户端能分清「服务端主动收尾」和「连接莫名其妙断了」——
// 前者可以马上重连(或者干脆去做别的),后者只能干等超时。
//
// 这一帧**不带 id 行**:它不是一条事件,不该推进客户端的续传游标。
func (h *SSEHandler) writeShutdown(c *gin.Context) {
	frame, err := sse.Encode(sse.Frame{
		Event: "shutdown",
		Data:  shutdownData{Reason: "server_shutting_down"},
	})
	if err != nil {
		return
	}
	if _, err := c.Writer.Write(frame); err != nil {
		return
	}
	c.Writer.Flush()
}

// abort 结束一条已经开始的流。
//
// 流阶段的错误**不能**再走统一外壳:响应头已经发出去了,状态码改不了,
// 再写 JSON 就是往 SSE 流里混垃圾,浏览器会解析出莫名其妙的事件。
//
// 所以这里只记一条 Warn 然后收工 —— 客户端会看到流断掉,
// 而它下一步多半是重连(带 Last-Event-ID 续传)。
func (h *SSEHandler) abort(c *gin.Context, err error) {
	slog.Warn("SSE 流中断",
		"trace_id", trace.FromGin(c),
		"session_id", c.Param("id"),
		"error", err)
}

// ready 挡住两种"没法正常服务"的情况,它们的对外表现不一样。
//
// **依赖没注入 → 500**:"接口不存在(404)"和"接口没接好(500)"是两回事,
// 混在一起会让排查走错方向(同 handler.notWired)。
//
// **功能被关掉 → 503**:前端拿到 503 会退回轮询,
// 而 404 会被当成"接口不存在" —— 那是误导,接口明明在,只是没开。
func (h *SSEHandler) ready(c *gin.Context) bool {
	// ★ 用 isNilDep 而不是 `h.svc == nil`。
	//
	// 后者对「接口里装着一个 nil 指针」判不出来:Deps.SSEStream 是接口字段,
	// 接线方写 `SSEStream: (*service.TaskEventService)(nil)` 时,
	// 接口本身不是 nil,`== nil` 判假,后面一调方法就在 nil 指针上炸。
	// 被 Recovery 兜成 500 的话,看起来像服务故障,实际是接线 bug ——
	// 排查方向一开始就走偏(同 middleware.Auth 对空声明的处理)。
	if h == nil || isNilDep(h.svc) {
		notWired(c, "SSE 接口")
		return false
	}
	if h.hub == nil {
		// hub 没接上 → 连接数上限永远算成 0(等于没有上限),
		// 而无限 SSE 连接正是这个接口最该防的那件事。
		notWired(c, "SSE 接口")
		return false
	}
	if !h.cfg.Enabled {
		// 用自己的码(50303)而不是 50300「执行器暂时不可用」——
		// 客户端按业务码做处置时必须能分开:这里该**改用轮询**,
		// 而 50300 会让它以为该等执行器恢复,反复重试一条根本不存在的路。
		response.FailFromError(c, model.ErrSSEDisabled)
		return false
	}
	return true
}

// parseLastEventID 解析请求头里的 Last-Event-ID。
//
// 浏览器的 EventSource 和手写的 fetch 客户端都会带这个头,
// 它是客户端记住的「最后收到的事件 id」。
//
// **解析失败或者为负一律当 0**(从头开始)。它只是个续传游标,
// 不是安全相关的输入 —— 为一个格式不对的游标拒绝整个请求没有意义。
func parseLastEventID(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// resolveLastID 校正客户端带来的 lastID。
//
// ## 要解决的坑
//
// 库的 ID 是**全局**自增的,而 lastID 是客户端按**会话**记住的。
// 客户端如果带着「别的会话的 lastID」来连(比如前后端各存了一份游标,
// 或者它复用了同一条连接的游标),这个 lastID 可能比本会话的**所有**事件都大 ——
// 于是本会话的事件会被**整个跳过**,用户看到的是一段空白的流。
//
// ## 判据:游标必须落在本会话的一条事件上
//
// 客户端记的 lastID 永远是「它收到过的某一帧的 id」,而那一帧属于本会话。
// 所以**本会话里一定存在一条 id == lastID 的事件**。
// 反过来,如果本会话里找不到这个 id,那这个游标就是别的会话带来的,
// 本会话的事件会被整个跳过 —— 只能归零,从头发。
//
// 怎么在现有 API 上问这个问题:ListEvents 读的是「id > afterID」,
// 所以查 afterID = lastID-1、limit = 1,拿回来的是「本会话里第一条
// id >= lastID 的事件」。
//
//   - 拿不到 → 游标指在本会话**末尾之后** → 别的会话带来的 → 归零。
//   - 拿到的 id > lastID → 游标指在本会话事件之间的**空隙**里 → 也是别的会话带来的 → 归零。
//   - 拿到的 id == lastID → 游标落在本会话的一条事件上 → 保留。
//
// ## 为什么不是「拿最早那条事件的 id 来比」
//
// 拿本会话最早那条事件(minID)比,只能发现「游标比本会话还早」,
// 而那种情况**本来就不会漏发**:读「id > 游标」拿到的仍然是全部。
// 真正会漏发的是「游标比本会话还晚」,minID 根本看不出来 ——
// 本会话的事件是 5、6、7,游标是 100(minID=5 < 100),比出来"没问题",
// 但读「id > 100」一条都拿不到。
//
// ## 为什么不做得更精确
//
// 要完全精确就得问「这个会话里有没有 id <= lastID 的事件」,
// 那要在 repository 里加方法(不在本轮的文件范围内)。
// 上面这个判据用现有的 ListEvents 就够了,而且已经能堵住「漏发」这个真实后果。
//
// 剩下的边角情况(游标恰好等于本会话某条事件的 id,但那个 id 是巧合撞上的)
// 后果是「少发一条历史事件」,不是安全洞 —— **多发比漏发安全**。
//
// ★ **前端不能按 id 去重。**
//
// id 是库的全局自增序号,和「哪一条逻辑事件」不是一回事:
// 任务流断线重发时(agentclient 的 retry),Agent 会从 seq=1 重新数,
// 于是**同一条逻辑事件会以不同的 id 再写一行、再推一帧**
// (model.TaskEvent 的注释也承认 (task_uid, seq) 会重复)。
//
// 所以前端的去重键必须是 **(task_uid, seq)**,不是 id。
// 帧里的 data 同时带了 task_uid 和 seq,就是为了让前端能这么去重。
func (h *SSEHandler) resolveLastID(ctx context.Context, sessionID string, lastID int64) (int64, error) {
	if lastID <= 0 {
		return 0, nil
	}
	// 本会话里第一条 id >= lastID 的事件。
	atOrAfter, err := h.svc.ListEvents(ctx, sessionID, lastID-1, 1)
	if err != nil {
		return 0, fmt.Errorf("校正会话 %s 的续传游标: %w", sessionID, err)
	}
	if len(atOrAfter) == 0 {
		// 这个会话还没有 id >= lastID 的事件 → 游标指在末尾之后。
		// 没配 Redis、或者这个会话一条事件都没有时也走这里,结果一样:归零。
		return 0, nil
	}
	if atOrAfter[0].ID > lastID {
		// 游标指在本会话事件之间的空隙里 → 它多半来自别的会话 → 归零,从头发。
		return 0, nil
	}
	// 游标落在本会话的一条事件上 → 合法,保留。
	return lastID, nil
}
