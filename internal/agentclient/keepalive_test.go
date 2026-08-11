package agentclient

import (
	"testing"
	"time"
)

// defaultServerMinPingInterval 是服务端默认允许的最小 ping 间隔。
//
// grpc-go: keepalive.EnforcementPolicy.MinTime 默认 5 分钟。
// grpcio(Python): grpc.http2.min_ping_interval_without_data_ms 默认 300000。
const defaultServerMinPingInterval = 5 * time.Minute

// TestKeepaliveRespectsDefaultServerPolicy 是一条绊线,不是行为测试。
//
// 它挡的是这么一次改动:有人觉得"5 分钟才发现掉线太慢了",
// 顺手把 keepaliveTime 调成 30 秒、把 PermitWithoutStream 打开 ——
// 看起来更积极,实际上服务端会回 GOAWAY ENHANCE_YOUR_CALM
// 把连接踢掉,在途的事件流全断,那些任务被 OnStreamError 标成 failed。
// 现象是"跑着跑着一批任务集体失败",很难联想到 keepalive。
//
// 这个坑真踩过一次(日志里是 too_many_pings)。
//
// 真要调快,两边一起改 —— 见 client.go 里那段 Python 侧的 options。
func TestKeepaliveRespectsDefaultServerPolicy(t *testing.T) {
	if keepaliveTime < defaultServerMinPingInterval {
		t.Errorf("keepaliveTime = %v,短于服务端默认下限 %v。"+
			"服务端会回 GOAWAY ENHANCE_YOUR_CALM 踢掉连接 —— "+
			"要调快必须同时改 Python 侧的 grpc.http2.min_ping_interval_without_data_ms",
			keepaliveTime, defaultServerMinPingInterval)
	}

	if keepalivePermitWithoutStream {
		t.Error("keepalivePermitWithoutStream = true,但服务端的 " +
			"grpc.keepalive_permit_without_calls 默认是关的:" +
			"没有在途调用还 ping,对面会当成违规。" +
			"这个客户端大部分时间空闲,开着它等于持续送违规计数")
	}

	// ping 发出去之后等回应的时间必须短于 ping 的间隔,
	// 否则上一轮还没判定超时,下一轮就发了。
	if keepaliveTimeout >= keepaliveTime {
		t.Errorf("keepaliveTimeout(%v)不该大于等于 keepaliveTime(%v)",
			keepaliveTimeout, keepaliveTime)
	}
}
