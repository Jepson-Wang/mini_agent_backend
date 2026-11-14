package sse

import (
	"encoding/json"
	"strings"
	"testing"
)

// frameLines 把一帧拆成行(去掉末尾的空行)。
//
// 用来断言"帧结构没被 payload 破坏":
// 一帧就是 id/event/data 三行,行数不该变。
func frameLines(t *testing.T, raw []byte) []string {
	t.Helper()
	s := string(raw)
	if !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("帧尾不是两个换行: %q", s)
	}
	body := strings.TrimSuffix(s, "\n\n")
	return strings.Split(body, "\n")
}

func TestEncode_ThreeLinesInOrder(t *testing.T) {
	raw, err := Encode(Frame{ID: 12345, Event: "thinking", Data: map[string]any{"a": 1}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := "id: 12345\nevent: thinking\ndata: {\"a\":1}\n\n"
	if got := string(raw); got != want {
		t.Errorf("帧 = %q, 期望 %q", got, want)
	}
}

func TestEncode_NoIDLineWhenIDIsZero(t *testing.T) {
	raw, err := Encode(Frame{ID: 0, Event: "thinking", Data: "x"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	lines := frameLines(t, raw)
	if len(lines) != 2 {
		t.Fatalf("行数 = %d(%v), 期望 2(id=0 时不发 id 行)", len(lines), lines)
	}
	if strings.HasPrefix(lines[0], "id:") {
		t.Errorf("id=0 时不该发 id 行,实际第一行是 %q", lines[0])
	}
	// 特别地:不能发 `id: 0` —— 那等于把 Last-Event-ID 设成 0,
	// 断线重连会从头重放全部历史。
	if strings.Contains(string(raw), "id:") {
		t.Errorf("帧里不该出现 id 行: %q", string(raw))
	}
}

func TestEncode_NoEventLineWhenEventIsEmpty(t *testing.T) {
	raw, err := Encode(Frame{ID: 7, Event: "", Data: "x"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	lines := frameLines(t, raw)
	if len(lines) != 2 {
		t.Fatalf("行数 = %d(%v), 期望 2(event 为空时不发 event 行)", len(lines), lines)
	}
	if strings.Contains(string(raw), "event:") {
		t.Errorf("event 为空时不该发 event 行: %q", string(raw))
	}
}

func TestEncode_EndsWithExactlyTwoNewlines(t *testing.T) {
	raw, err := Encode(Frame{ID: 1, Event: "e", Data: "d"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	s := string(raw)
	if !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("帧尾 = %q, 期望以两个换行结尾", s)
	}
	// 恰好两个:多一个会多出一个空帧,少一个浏览器会把下一帧当成本帧的续行。
	if strings.HasSuffix(s, "\n\n\n") {
		t.Errorf("帧尾多于两个换行: %q", s)
	}
	// 整帧的换行数:id、event、data 各一个行尾,再加帧尾那个空行 = 4。
	// data 里不会有裸换行(JSON 编码保证了),所以这个数是确定的。
	if n := strings.Count(s, "\n"); n != 4 {
		t.Errorf("整帧换行数 = %d, 期望 4(id/event/data 各一个 + 帧尾空行)", n)
	}
}

func TestEncode_NilDataIsNull(t *testing.T) {
	raw, err := Encode(Frame{ID: 1, Event: "e", Data: nil})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), "data: null\n") {
		t.Errorf("Data 为 nil 时应发 data: null, 实际 %q", string(raw))
	}
}

func TestEncode_PayloadWithNewlinesQuotesAndChinese(t *testing.T) {
	payload := "第一行\n第二行\r\n第三行\n"
	raw, err := Encode(Frame{ID: 9, Event: "thinking", Data: map[string]any{
		"payload": payload,
		"quote":   `他说"你好"`,
		"中文":      "值",
	}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// 帧结构不能被 payload 破坏:仍然只有三行。
	lines := frameLines(t, raw)
	if len(lines) != 3 {
		t.Fatalf("行数 = %d(%v), 期望 3 —— payload 里的换行把帧劈开了", len(lines), lines)
	}

	// data 行里的换行必须是转义过的两个字符 "\n",不是真的换行。
	data := lines[2]
	if !strings.HasPrefix(data, "data: ") {
		t.Fatalf("第三行 = %q, 期望 data 行", data)
	}
	if strings.Contains(data, "\n") || strings.Contains(data, "\r") {
		t.Errorf("data 行里出现了裸换行: %q", data)
	}
	if !strings.Contains(data, `\n`) {
		t.Errorf("data 行里没有转义后的换行: %q", data)
	}

	// 再解回来,内容必须一模一样(汉字不能被劈开)。
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &got); err != nil {
		t.Fatalf("data 行不是合法 JSON: %v", err)
	}
	if got["payload"] != payload {
		t.Errorf("payload = %q, 期望 %q", got["payload"], payload)
	}
	if got["quote"] != `他说"你好"` {
		t.Errorf("quote = %v", got["quote"])
	}
	if got["中文"] != "值" {
		t.Errorf("中文 = %v", got["中文"])
	}
}

func TestEncode_EventWithNewlineIsRejected(t *testing.T) {
	for _, event := range []string{"a\nb", "a\rb", "\n"} {
		raw, err := Encode(Frame{ID: 1, Event: event, Data: "x"})
		if err == nil {
			t.Errorf("event = %q 应当报错, 实际编出了 %q", event, string(raw))
			continue
		}
		if raw != nil {
			t.Errorf("event = %q 报错时不该同时返回帧: %q", event, string(raw))
		}
	}
}

func TestEncode_UnmarshalableDataReturnsError(t *testing.T) {
	// func 和 channel 都没法 JSON 编码。
	// 这种错误要能传上去 —— 丢一条事件比发一条坏帧好。
	if _, err := Encode(Frame{ID: 1, Event: "e", Data: func() {}}); err == nil {
		t.Error("Data 不可编码时应当返回 error")
	}
	if _, err := Encode(Frame{ID: 1, Event: "e", Data: make(chan int)}); err == nil {
		t.Error("Data 不可编码时应当返回 error")
	}
}

func TestEncodeComment_IsCommentLine(t *testing.T) {
	got := string(EncodeComment("keepalive"))
	want := ": keepalive\n\n"
	if got != want {
		t.Errorf("心跳 = %q, 期望 %q", got, want)
	}
	if !strings.HasPrefix(got, ":") {
		t.Errorf("心跳必须以冒号开头: %q", got)
	}
}

func TestEncodeComment_StripsLineBreaks(t *testing.T) {
	// 注释里夹一个换行,后半截就会变成一条没有 data 的事件。
	got := string(EncodeComment("a\nb"))
	if strings.Contains(strings.TrimSuffix(got, "\n\n"), "\n") {
		t.Errorf("心跳里不该有裸换行: %q", got)
	}
	if !strings.HasPrefix(got, ":") {
		t.Errorf("心跳必须以冒号开头: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("心跳必须以两个换行结尾: %q", got)
	}
}
