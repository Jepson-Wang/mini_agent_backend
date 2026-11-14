// Package sse 是 SSE(Server-Sent Events)流式输出的基础设施。
//
// 这个包只管三件事:
//
//   - 把事件编码成标准 SSE 帧(frame.go)
//   - 维护本副本的订阅表(hub.go)
//   - 跨副本的唤醒信号(bus.go)
//
// ## 它不发事件
//
// **事件本身在数据库里**,这个包只负责两件事:把库里的事件编码成帧,
// 以及在"有更新了"的时候把连接叫醒。
//
// 收到唤醒信号之后是"去读库里 id > lastID 的事件",不是"拿这条事件"。
// 所以**信号丢了不丢数据** —— 丢的只是实时性,下一次轮询会把它补上。
// Redis 在这里是加速器,不是权威;这个取向和项目里限流、吊销的 fail open 一致。
//
// ## 分层
//
// 这是纯基础设施:**不 import service / handler / model / agentclient**。
// 归属校验、事件落库、限流那些都在别的层,这里只做字节和信号的搬运。
package sse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Frame 是一条待发送的 SSE 事件。
//
// 三行的顺序是固定的:id、event、data。
// 顺序不能变是因为浏览器的 EventSource 解析器按行读,
// 顺序变了不会报错,只会让前端拿到一个没有 type 的事件 ——
// 那种 bug 在日志里看不出来。
type Frame struct {
	ID    int64  // 库里的自增 ID;0 表示不带 id 行
	Event string // 事件类型;空表示不发 event 行
	Data  any    // JSON 编码后放进 data 行
}

// Encode 把 Frame 编码成标准 SSE 帧文本,以两个换行 "\n\n" 结尾。
//
// 帧的形状:
//
//	id: 12345
//	event: thinking
//	data: {"seq":3}
//
// (末尾还有一行空行,这里为了看清结构没画出来。)
//
// ## Data 必须经过 JSON 编码
//
// 这是**帧结构不被 payload 破坏**的唯一保证。
// thinking 事件带的是整轮 assistant.content,里面可能有换行;
// tool_result 带的是工具返回,可能有几百 KB 的文件内容。
// encoding/json 会把字符串里的换行转义成 "\n"(两个字符),
// 于是 data 永远是**一行**,帧结构不会被劈开。
//
// 反过来说:如果直接拼字符串,一条带换行的 payload 会让后面所有行
// 被浏览器当成同一条事件的多行 data,前端看到的是一坨错乱的文本。
//
// ## ID == 0 不发 id 行
//
// 发 `id: 0` 不是"没有 id",而是"把 Last-Event-ID 设成 0"。
// 断线重连时浏览器会带上 Last-Event-ID: 0,服务端会从头重放全部历史 ——
// 和"不续传"完全是两回事。所以 0 表示"这一帧没有 id 行"。
//
// ## 错误
//
// 唯一合理的错误来源是 json.Marshal 失败(比如 Data 里塞了 channel 或 func)。
// 这类错误交给上层处理:丢一条事件比发一条坏帧好。
func Encode(f Frame) ([]byte, error) {
	// event 的值里带换行会破坏帧结构。
	// 实际上 ID 是数字、Event 是我们自己给的常量,不可能有换行;
	// 但 Encode 是导出函数,不能靠"调用方不会这么干"来保证帧是合法的。
	if err := checkField("event", f.Event); err != nil {
		return nil, err
	}

	// json.Marshal 对 nil 返回 "null",所以 Data == nil 时会发出 data: null。
	// 这不是特例分支,是同一条路径的结果。
	data, err := json.Marshal(f.Data)
	if err != nil {
		return nil, fmt.Errorf("编码 SSE 帧的 data 失败: %w", err)
	}

	var b bytes.Buffer
	if f.ID != 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatInt(f.ID, 10))
		b.WriteByte('\n')
	}
	if f.Event != "" {
		b.WriteString("event: ")
		b.WriteString(f.Event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(data)
	// 帧尾必须是**恰好两个**换行:一个结束 data 行,一个空行结束这一帧。
	// 少一个,浏览器会把下一帧的内容当成这一帧的续行。
	b.WriteString("\n\n")
	return b.Bytes(), nil
}

// EncodeComment 编码一行注释(心跳)。浏览器会忽略注释行,但能保活代理连接。
//
// 形状是 ": keepalive\n\n"。
//
// nginx 的 proxy_read_timeout 默认 60s,不心跳连接会被静默掐断,
// 而客户端分不清"服务端不说话"和"连接断了"。
//
// 注释里也不能有换行:一行以冒号开头才被当成注释,
// 中间夹一个换行,后半截就变成了一条**没有 data 的事件**,
// 浏览器会把它当成一个空事件派发出去。
// EncodeComment 没有 error 返回值,所以这里只能把换行去掉 ——
// 心跳文本是我们自己写的常量,真出现换行说明写错了,
// 但一条写错的心跳不该让整条连接断掉。
func EncodeComment(text string) []byte {
	var b bytes.Buffer
	b.WriteString(": ")
	b.WriteString(stripLineBreak(text))
	b.WriteString("\n\n")
	return b.Bytes()
}

// checkField 检查一个 SSE 字段值里有没有会破坏帧结构的字符。
//
// 返回 error 而不是悄悄改写,理由:
//   - 帧结构被破坏**不止影响这一帧**。浏览器把多出来的行当成同一条事件的
//     后续 data 行,于是这条事件的内容被拼坏,后面几帧的解析也可能跟着歪。
//   - event 的取值只有我们自己给的常量,出现换行只可能是调用方的 bug。
//     悄悄改写会把"事件类型拼错了"变成"浏览器收到一个不认识的事件名",
//     问题从"一眼能看出来"变成"前端不显示任何东西"。
//   - Encode 本来就会因为 json.Marshal 失败返回 error,多一个错误来源
//     不增加调用方的分支复杂度。
func checkField(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("SSE 帧的 %s 字段不能含换行: %q", name, value)
	}
	return nil
}

// stripLineBreak 去掉文本里的换行符(CR 和 LF)。
//
// 用于没有 error 返回值的那条路径(心跳注释):
// 与其把一条坏注释发出去让浏览器解析出莫名其妙的事件,
// 不如发一条内容少一点的注释 —— 心跳的全部作用是"让连接别断"。
func stripLineBreak(text string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(text)
}
