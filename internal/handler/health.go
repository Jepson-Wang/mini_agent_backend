package handler

import (
	"context"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

// pingTimeout 探数据库等多久。
const pingTimeout = 2 * time.Second

// 探测结果的取值。做成常量是因为监控告警会按这几个串配规则,
// 手抖打错一个字母,告警就永远不触发,而且没人会发现。
const (
	probeUp      = "up"
	probeDown    = "down"
	probeSkipped = "skipped" // 没接这个依赖,不是"探失败了"
)

// 整体状态。
const (
	statusOK       = "ok"
	statusDegraded = "degraded"
)

// AgentProber 是健康检查需要的 Agent 能力。
//
// 接口定在使用方(这个包),不在 agentclient —— handler 测试塞个假的就能跑,
// 不用起 gRPC server。返回类型还是 agentclient 的本地类型,
// 那个包本来就只暴露本地类型,不会把 proto 顶到这儿来。
type AgentProber interface {
	Health(ctx context.Context) (agentclient.HealthStatus, error)
	BreakerState() string
}

// RedisProber 是健康检查需要的 Redis 能力。
//
// 同 AgentProber 定在使用方:*redisx.Client 天然满足它,这里不必 import redisx,
// 测试塞个假的就能跑。
type RedisProber interface {
	Ping(ctx context.Context) error
}

// redisProbeTimeout 探 Redis 等多久。redisx 自己已经有紧超时,
// 这里再包一层是兜底:/health 被按秒轮询,不能被一个卡住的依赖拖住。
const redisProbeTimeout = 1 * time.Second

type HealthHandler struct {
	db    *gorm.DB
	agent AgentProber
	redis RedisProber
}

func NewHealthHandler(db *gorm.DB, agent AgentProber, redis RedisProber) *HealthHandler {
	return &HealthHandler{db: db, agent: agent, redis: redis}
}

type healthData struct {
	// Status 是整体结论:ok / degraded。
	Status string `json:"status"`

	// Database up / skipped。探不通的话整个接口直接 500,不会走到这儿。
	Database string `json:"database"`

	// Agent up / down / skipped。
	Agent string `json:"agent"`

	// AgentVersion 是 Agent 自报的版本,探不通时为空。
	// 两边行为对不上时,先看这个 —— 十次里有八次是版本没对齐。
	AgentVersion string `json:"agent_version,omitempty"`

	// Breaker 是熔断器状态:closed / open / half_open。
	//
	// 和 Agent 字段分开报是刻意的,两个字段回答的是不同的问题:
	// agent 是"现在探得通吗",breaker 是"业务流量现在放不放行"。
	// agent=up + breaker=open 表示对面已经恢复、再过一个冷却期
	// 业务也会恢复 —— 这时候去重启服务反而是帮倒忙。
	Breaker string `json:"breaker,omitempty"`

	// Redis up / down / skipped。
	// 挂了只降级:Redis 在这里只守着登录注册和建任务的限流,
	// 它没了是少一层保护,不是业务不可用。
	Redis string `json:"redis"`
}

// Check 探活。
//
// **Agent 挂了不返回 500,只把 status 降级成 degraded。**
// 这两个依赖的失败半径差得很远:
//
//   - 数据库没了,登录、列会话、看任务详情——几乎每个接口都废了,
//     这个实例继续接流量没有意义,该让编排系统把它摘掉,所以回 500。
//   - Agent 没了,只有"新建任务"这一条路受影响;历史会话、任务详情、
//     登录注册全都正常。这时候回 500 会让 k8s 把所有实例一起重启,
//     把一个局部故障放大成整站不可用 —— 而重启并不能让 Agent 回来。
//
// Redis 同理,也只降级:它没了只是少一层限流保护,登录、会话、任务全都照常,
// 回 500 会让编排系统重启所有实例,而重启并不能让 Redis 回来。
//
// 需要"Agent 不好就别给我流量"的场景(比如灰度切流),
// 让调用方自己读 agent 字段判断,不要改这里的状态码。
func (h *HealthHandler) Check(c *gin.Context) {
	data := healthData{
		Status:   statusOK,
		Database: probeSkipped,
		Agent:    probeSkipped,
		Redis:    probeSkipped,
	}

	// 三个依赖并行探。串行的话最坏要把几个超时加起来,
	// 而 /health 是被按秒轮询的接口 —— 探活本身不该成为负担。
	var (
		wg     sync.WaitGroup
		dbErr  error
		agentH agentclient.HealthStatus
		agtErr error
		rdsErr error
	)

	if h.db != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dbErr = h.pingDB(c.Request.Context())
		}()
	}
	if h.agent != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// agentclient.Health 自己带 healthTimeout,这里不再叠一层。
			agentH, agtErr = h.agent.Health(c.Request.Context())
		}()
	}
	if h.redis != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c.Request.Context(), redisProbeTimeout)
			defer cancel()
			rdsErr = h.redis.Ping(ctx)
		}()
	}
	wg.Wait()

	if h.db != nil {
		if dbErr != nil {
			_ = c.Error(dbErr) // 真实原因进日志,不回给客户端
			response.Fail(c, response.CodeInternal, "数据库不可用")
			return
		}
		data.Database = probeUp
	}

	if h.agent != nil {
		data.Breaker = h.agent.BreakerState()
		switch {
		case agtErr != nil:
			// 探不通。原因进日志 —— 那里面是 gRPC 状态码,
			// 不是给客户端看的。
			_ = c.Error(agtErr)
			data.Agent = probeDown
		case !agentH.OK:
			// 探通了,但 Agent 自己说不好(比如它连不上模型后端)。
			// 对使用者来说结果一样:现在别派任务。
			data.Agent = probeDown
			data.AgentVersion = agentH.Version
		default:
			data.Agent = probeUp
			data.AgentVersion = agentH.Version
		}
		if data.Agent != probeUp {
			data.Status = statusDegraded
		}
	}

	if h.redis != nil {
		if rdsErr != nil {
			// 原因只进日志:Redis 地址是内网拓扑,/health 通常不鉴权。
			_ = c.Error(rdsErr)
			data.Redis = probeDown
			data.Status = statusDegraded
		} else {
			data.Redis = probeUp
		}
	}

	response.OK(c, data)
}

// pingDB 真发一次查询,不只是看连接池里有没有连接。
func (h *HealthHandler) pingDB(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	sqlDB, err := h.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}
