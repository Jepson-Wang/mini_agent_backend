package agentclient

import (
	"context"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"google.golang.org/grpc/credentials"
)

// staticToken 必须实现 grpc 的 PerRPCCredentials。
// 少了这条,把它改坏之后编译器只会在 NewGRPCClient 那一行报错,
// 而那行藏在一堆 dial option 中间。
var _ credentials.PerRPCCredentials = staticToken{}

func TestStaticToken_UsesLowercaseMetadataKey(t *testing.T) {
	// gRPC 会把 metadata 的 key 统一小写。写成 X-Agent-Token 的话本地看着对,
	// 到线上和 Agent 那边的比较就对不上 —— 而那是个只在部署时才暴露的错误。
	md, err := (staticToken{"s3cret"}).GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if got := md[agentTokenMetadataKey]; got != "s3cret" {
		t.Fatalf("metadata[%q] = %q,想要 %q", agentTokenMetadataKey, got, "s3cret")
	}
	if agentTokenMetadataKey != "x-agent-token" {
		t.Fatalf("键必须是小写的 x-agent-token,当前 %q —— Agent 那侧写死的就是它",
			agentTokenMetadataKey)
	}
}

func TestStaticToken_DoesNotRequireTransportSecurity(t *testing.T) {
	// 连接是明文的(insecure.NewCredentials)。这里返回 true 的话 grpc-go 会
	// 直接拒绝拨号,客户端连建都建不出来。
	if (staticToken{"x"}).RequireTransportSecurity() {
		t.Fatal("RequireTransportSecurity 必须是 false,否则明文连接会被 grpc-go 拒绝")
	}
}

func TestNewGRPCClient_TokenIsOptional(t *testing.T) {
	// 没配 token 也要能建出客户端:Agent 绑回环时本来就不鉴权。
	// 建不出来的话本地开发直接起不来。
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"没有 token", ""},
		{"有 token", "0123456789abcdef0123456789abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, err := NewGRPCClient(
				config.Agent{Addr: "localhost:50051", Token: tc.token},
				noopHandler{},
			)
			if err != nil {
				t.Fatalf("NewGRPCClient: %v", err)
			}
			t.Cleanup(func() { _ = cli.Close() })
		})
	}
}

// noopHandler 满足 EventHandler,什么也不做。
// NewGRPCClient 拒绝 nil handler,所以这里得给一个。
type noopHandler struct{}

func (noopHandler) OnEvent(context.Context, Event) error         { return nil }
func (noopHandler) OnStreamError(context.Context, string, error) {}
