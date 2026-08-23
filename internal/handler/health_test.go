// health_test.go 只测 /health。走完整 router round-trip,不连库
// (Deps.DB 传 nil,数据库那一路就是 skipped)。
package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/handler"
)

// fakeProber 是 handler.AgentProber 的假实现。
type fakeProber struct {
	status  agentclient.HealthStatus
	err     error
	breaker string
}

func (f *fakeProber) Health(context.Context) (agentclient.HealthStatus, error) {
	if f.err != nil {
		return agentclient.HealthStatus{}, f.err
	}
	return f.status, nil
}

func (f *fakeProber) BreakerState() string {
	if f.breaker == "" {
		return "closed"
	}
	return f.breaker
}

// healthBody 是 /health 的 data 部分。手写一份而不是复用 handler 里那个
// 未导出的结构:这里要锁的正是**对外的 JSON 字段名**,
// 内部结构改了字段名而 json tag 忘了跟上时,这个测试才会红。
type healthBody struct {
	Status       string `json:"status"`
	Database     string `json:"database"`
	Agent        string `json:"agent"`
	AgentVersion string `json:"agent_version"`
	Breaker      string `json:"breaker"`
}

func getHealth(t *testing.T, prober handler.AgentProber) (int, healthBody) {
	t.Helper()

	r := handler.NewRouter(handler.Deps{Agent: prober})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	var resp struct {
		Code int        `json:"code"`
		Data healthBody `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v,body=%s", err, w.Body.String())
	}
	return w.Code, resp.Data
}

func TestHealth_AgentUp(t *testing.T) {
	code, body := getHealth(t, &fakeProber{
		status: agentclient.HealthStatus{OK: true, Version: "0.3.1"},
	})

	if code != http.StatusOK {
		t.Fatalf("状态码 = %d,期望 200", code)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q,期望 ok", body.Status)
	}
	if body.Agent != "up" {
		t.Errorf("agent = %q,期望 up", body.Agent)
	}
	if body.AgentVersion != "0.3.1" {
		t.Errorf("agent_version = %q,期望 0.3.1 —— 两边行为对不上时先看这个字段", body.AgentVersion)
	}
	if body.Database != "skipped" {
		t.Errorf("database = %q,没传 DB 时期望 skipped", body.Database)
	}
}

// TestHealth_AgentDownIsNot500 是这个接口最要紧的一条。
//
// Agent 挂了只降级,不 500。回 500 的话 k8s 会把**所有**实例一起重启,
// 而重启并不能让 Agent 回来 —— 一个只影响"新建任务"的局部故障
// 就这么被放大成整站不可用(登录、历史会话、任务详情本来都是好的)。
func TestHealth_AgentDownIsNot500(t *testing.T) {
	code, body := getHealth(t, &fakeProber{
		err:     errors.New("connection refused"),
		breaker: "open",
	})

	if code != http.StatusOK {
		t.Fatalf("状态码 = %d,期望 200 —— Agent 挂了不该把整个实例判死", code)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q,期望 degraded", body.Status)
	}
	if body.Agent != "down" {
		t.Errorf("agent = %q,期望 down", body.Agent)
	}
	if body.Breaker != "open" {
		t.Errorf("breaker = %q,期望 open", body.Breaker)
	}
}

// 探通了,但 Agent 自己说不好(比如它连不上模型后端)。
// 对使用者来说结论一样:现在别派任务。
func TestHealth_AgentSaysNotOK(t *testing.T) {
	code, body := getHealth(t, &fakeProber{
		status: agentclient.HealthStatus{OK: false, Version: "0.3.1"},
	})

	if code != http.StatusOK {
		t.Fatalf("状态码 = %d,期望 200", code)
	}
	if body.Agent != "down" || body.Status != "degraded" {
		t.Errorf("agent=%q status=%q,Agent 自报不健康时期望 down / degraded", body.Agent, body.Status)
	}
	// 版本还是要报:这时候最可能的原因就是版本不对。
	if body.AgentVersion != "0.3.1" {
		t.Errorf("agent_version = %q,自报不健康时也该带上版本", body.AgentVersion)
	}
}

// 没接 Agent 客户端的部署(比如只跑读接口的实例)不该被报成 degraded。
func TestHealth_NoAgentConfiguredIsSkippedNotDown(t *testing.T) {
	code, body := getHealth(t, nil)

	if code != http.StatusOK {
		t.Fatalf("状态码 = %d,期望 200", code)
	}
	if body.Agent != "skipped" {
		t.Errorf("agent = %q,没接客户端时期望 skipped —— 报 down 会让人去查一个根本不存在的依赖", body.Agent)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q,没接 Agent 不算降级,期望 ok", body.Status)
	}
}

// TestHealth_ProbeErrorNotEchoed:探测失败的原文不能进响应体。
// 那里面是 gRPC 状态和内网地址,而 /health 通常是不鉴权的。
func TestHealth_ProbeErrorNotEchoed(t *testing.T) {
	r := handler.NewRouter(handler.Deps{
		Agent: &fakeProber{err: errors.New("dial tcp 10.0.0.7:50051: connect: connection refused")},
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	body := w.Body.String()
	for _, leak := range []string{"10.0.0.7", "50051", "dial tcp"} {
		if strings.Contains(body, leak) {
			t.Errorf("响应体里出现了 %q,/health 通常不鉴权,等于把内网拓扑挂在公网上:%s", leak, body)
		}
	}
}
