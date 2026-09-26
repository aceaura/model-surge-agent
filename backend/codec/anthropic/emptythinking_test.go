package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 空壳 thinking 块：正文为空且没有可写回的签名。marshal 出来是
// {"type":"thinking"}（thinking 键随 omitempty 蒸发），Anthropic 拒收
// 缺 thinking 字段的块——一个空块会让整份请求/响应 400。编码器必须整块跳过。

func TestEncodeSkipsEmptyUnsignedThinking(t *testing.T) {
	_, ok, err := encodeBlock(ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	if ok {
		t.Error("无签名空正文的 thinking 块应整块跳过")
	}
}

func TestEncodeSkipsEmptyForeignSignedThinking(t *testing.T) {
	// 别家签名会被剥离，剥完就是空壳：同样跳过，不能写出只有
	// {"type":"thinking"} 的残缺块。
	_, ok, err := encodeBlock(ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
		Signature: "enc-xyz", SignatureFrom: codec.ProtocolResponses,
	}})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	if ok {
		t.Error("空正文+异族签名剥完是空壳，应跳过")
	}
}

func TestEncodeKeepsEmptySignedThinking(t *testing.T) {
	// 空正文但签名可写回不是空壳：签名本身就是载荷，
	// 扩展思考续话校验要靠它，必须原样保留。
	w, ok, err := encodeBlock(ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
		Text: "", Signature: "sig-abc", SignatureFrom: codec.ProtocolAnthropic,
	}})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	if !ok {
		t.Fatal("带签名的空正文 thinking 块不该被跳过")
	}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"signature":"sig-abc"`) {
		t.Errorf("签名丢了: %s", raw)
	}
}

func TestEncodeRequestDropsEmptyThinkingShell(t *testing.T) {
	req := &ir.Request{
		Model: "claude-opus-5",
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{}},
			{Type: ir.BlockText, Text: "ok"},
		}}},
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(wire), `"type":"thinking"`) {
		t.Errorf("空壳 thinking 块混进了请求体: %s", wire)
	}
	if !strings.Contains(string(wire), `"text":"ok"`) {
		t.Errorf("同消息的正文块被连坐丢了: %s", wire)
	}
}

func TestEncodeResponseLossyReportsEmptyThinking(t *testing.T) {
	resp := &ir.Response{ID: "msg_1", Model: "m", Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{}},
		{Type: ir.BlockText, Text: "hi"},
	}}
	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if strings.Contains(string(body), `"type":"thinking"`) {
		t.Errorf("空壳 thinking 块混进了响应体: %s", body)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "empty thinking block") {
		t.Errorf("跳过空壳却没有有损说明: %v", notes)
	}
}

func TestEncodeResponseLossySilentOnSignedEmptyThinking(t *testing.T) { // 带签名的空正文块原样写回，不是丢弃，不得报说明。
	resp := &ir.Response{ID: "msg_1", Model: "m", Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Signature: "sig-abc", SignatureFrom: codec.ProtocolAnthropic,
		}},
	}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	for _, n := range notes {
		if strings.Contains(n, "empty thinking") {
			t.Errorf("合法形态被误报: %v", notes)
		}
	}
}

// 流式开块是空壳跳过的唯一例外：thinking 块的起始快照必然正文为空、
// 签名未到，照跳会让后续 thinking_delta 无块可落。官方起始帧本就是
// 空壳形状，必须照常发出 content_block_start。
func TestStreamStillOpensEmptyThinkingBlock(t *testing.T) {
	enc := newStreamEncoder()
	frames, err := enc.Encode(ir.Event{
		Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	joined := ""
	for _, f := range frames {
		joined += string(f)
	}
	if !strings.Contains(joined, "content_block_start") ||
		!strings.Contains(joined, `"type":"thinking"`) {
		t.Errorf("流式 thinking 块没有正常开启: %s", joined)
	}
}
