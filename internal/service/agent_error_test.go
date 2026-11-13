package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// TestTranslateAgentErr 盯着跨包边界的那次翻译。
//
// 两个方向都要断言:认识的哨兵必须翻对,不认识的必须**原样返回**。
// 只测前者的话,"图省事把 default 改成一律翻成不可用"这种改法照样全绿 ——
// 而那会让客户端去重试一件重试也没用的事。
func TestTranslateAgentErr(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
		want error // nil 表示"要求原样返回 in"
	}{
		{"nil 不变", nil, nil},
		{"不可用", agentclient.ErrUnavailable, model.ErrAgentUnavailable},
		{"超时", agentclient.ErrTimeout, model.ErrAgentTimeout},

		// 真实调用链上错误是被 %w 包过的(见 GRPCClient 那几处),
		// 直接用 == 比就会漏。
		{"包过一层仍能认出", fmt.Errorf("RunTask: %w", agentclient.ErrUnavailable), model.ErrAgentUnavailable},

		// 下面这几个都必须原样往上走。
		// ErrAlreadyRunning 尤其不能并进"不可用":它的语义是"等一下就好",
		// 对面健康得很(见 CLAUDE.md 里流中断重发那节)。
		{"任务还在跑,原样", agentclient.ErrAlreadyRunning, nil},
		{"坏请求,原样", agentclient.ErrBadRequest, nil},
		{"对面内部错,原样", agentclient.ErrRemoteInternal, nil},
		{"陌生错误,原样", errors.New("谁也没见过的错误"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translateAgentErr(tc.in)

			if tc.in == nil {
				if got != nil {
					t.Fatalf("translateAgentErr(nil) = %v, 想要 nil", got)
				}
				return
			}

			if tc.want != nil {
				if !errors.Is(got, tc.want) {
					t.Fatalf("translateAgentErr(%v) = %v, 想要 %v", tc.in, got, tc.want)
				}
				return
			}

			// want 为 nil:要求原样返回,而且**不许**被硬翻成业务错误。
			// 后半句才是这条用例的重点 —— 硬翻是在猜,猜错了客户端会照着
			// 一个假的原因去做决定(重试/不重试)。
			if !errors.Is(got, tc.in) {
				t.Fatalf("translateAgentErr(%v) = %v, 想要原样返回", tc.in, got)
			}
			if errors.Is(got, model.ErrAgentUnavailable) || errors.Is(got, model.ErrAgentTimeout) {
				t.Fatalf("认不出来的错误被硬翻成了业务错误: %v -> %v", tc.in, got)
			}
		})
	}
}

// TestTranslateAgentErr_AgentBusy 盯着跨包边界的那一次翻译。
//
// 「执行器满了」必须翻成 model.ErrAgentBusy,而不是落回 ErrAgentUnavailable:
// 两者对客户端的处置方向相反(一个等它腾空位,一个等它恢复)。
// 翻错的后果是双重的 —— 熔断方向没错(那个在 agentclient 那边),
// 但对外文案会把一个健康的执行器说成挂了。
func TestTranslateAgentErr_AgentBusy(t *testing.T) {
	got := translateAgentErr(agentclient.ErrAgentBusy)

	if !errors.Is(got, model.ErrAgentBusy) {
		t.Fatalf("translateAgentErr(ErrAgentBusy) = %v,想要 model.ErrAgentBusy", got)
	}
	if errors.Is(got, model.ErrAgentUnavailable) {
		t.Fatal("归成了 ErrAgentUnavailable,「满了」和「连不上」必须分开")
	}

	// 真实调用链上错误是被 %w 包过的,直接用 == 比会漏。
	wrapped := translateAgentErr(fmt.Errorf("接收任务事件流: %w", agentclient.ErrAgentBusy))
	if !errors.Is(wrapped, model.ErrAgentBusy) {
		t.Fatalf("包过一层的错翻成了 %v,想要 model.ErrAgentBusy", wrapped)
	}

	// 文案也要分开,而且是固定文案:error_msg 会跟着任务详情回显给用户。
	msg := agentFailureReason(got)
	if !strings.Contains(msg, "并发") {
		t.Errorf("派发失败文案里没提并发: %q", msg)
	}
	if strings.Contains(msg, "不可用") {
		t.Errorf("派发失败文案把「满了」说成了「不可用」: %q", msg)
	}
}

// TestAgentFailureReason_DropsRawError 是这个文件里最要紧的一条。
//
// 派发失败的原因会写进 tasks.error_msg,而那一列**跟着任务详情回显给用户**。
// agentclient 那侧已经有 TestTranslateGRPCError_DropsRawMessage 断掉了 gRPC 原文,
// 这条守的是第二道:哪怕上游哪天漏了内网地址过来,也不能拼进对外文案里。
//
// 走的是 session.go 里那句真实调用: agentFailureReason(translateAgentErr(err))。
// 必须覆盖**翻译不掉的**那几个哨兵 —— ErrUnavailable 会被换成光秃秃的
// model.ErrAgentUnavailable,原文在那一步就没了,拿它测泄漏是在测一条
// 不可能发生的路径。真正带着原文走到这儿的是 default 分支原样返回的那些。
func TestAgentFailureReason_DropsRawError(t *testing.T) {
	// 这串就是 Unavailable / Internal 的 status.Message() 的真实样子。
	const rawMsg = "dial tcp 10.0.0.7:50051: connect: connection refused"

	for _, sentinel := range []error{
		agentclient.ErrRemoteInternal, // 原样返回,原文跟着走到 agentFailureReason
		agentclient.ErrBadRequest,     // 同上
		agentclient.ErrAlreadyRunning, // 同上
		agentclient.ErrUnavailable,    // 会被翻掉,顺带也钉一下
		errors.New(rawMsg),            // 连哨兵都不是的陌生错误
	} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			got := agentFailureReason(translateAgentErr(fmt.Errorf("%s: %w", rawMsg, sentinel)))

			for _, leak := range []string{"10.0.0.7", "50051", "dial", "refused", "connect"} {
				if strings.Contains(got, leak) {
					t.Fatalf("派发失败文案里漏出了 %q: %q", leak, got)
				}
			}
		})
	}
}

// TestAgentFailureReason_Messages 锁住两句文案的分工。
//
// 分开写而不是一律"执行器不可用",是因为用户能据此做不同的事:
// 不可用 → 过会儿重发;超时 → 请求可能已经送到了,先去列表看看。
// 混成一句的话这个区别就没了,而测试是唯一能拦住"顺手合并"的东西。
func TestAgentFailureReason_Messages(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       error
		contains string
		absent   string
	}{
		{"超时说超时", model.ErrAgentTimeout, "超时", ""},
		{"不可用说不可用", model.ErrAgentUnavailable, "不可用", "超时"},

		// 兜底的两条:文案可以含糊,但**绝不能是空串** ——
		// 空串会让用户看到一个 failed 却没有任何原因的任务。
		{"nil 也要有话说", nil, "", "超时"},
		{"陌生错误走兜底", errors.New("谁也没见过的错误"), "", "超时"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := agentFailureReason(tc.in)

			if got == "" {
				t.Fatalf("agentFailureReason(%v) 返回了空串", tc.in)
			}
			if tc.contains != "" && !strings.Contains(got, tc.contains) {
				t.Fatalf("agentFailureReason(%v) = %q, 想要含 %q", tc.in, got, tc.contains)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Fatalf("agentFailureReason(%v) = %q, 不该含 %q", tc.in, got, tc.absent)
			}
		})
	}
}

// TestAgentFailureReason_TakesTranslatedErr 把两个函数串起来。
//
// agentFailureReason 的入参语义是"已经被 translateAgentErr 翻过的错误"。
// 分开测各自都对、接起来对不上,是这块最容易漏的形状 ——
// 表现是派发失败时写进库里的原因是错的那一句,而两个包的测试全绿。
func TestAgentFailureReason_TakesTranslatedErr(t *testing.T) {
	for _, tc := range []struct {
		in       error
		contains string
	}{
		{agentclient.ErrTimeout, "超时"},
		{agentclient.ErrUnavailable, "不可用"},
	} {
		t.Run(tc.in.Error(), func(t *testing.T) {
			if got := agentFailureReason(translateAgentErr(tc.in)); !strings.Contains(got, tc.contains) {
				t.Fatalf("%v 一路翻下来 = %q, 想要含 %q", tc.in, got, tc.contains)
			}
		})
	}
}
