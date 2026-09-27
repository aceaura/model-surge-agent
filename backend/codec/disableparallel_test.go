package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
)

// anthropic 的 tool_choice.disable_parallel_tool_use 是官方稳定字段，与 OpenAI
// 两系顶层的 parallel_tool_calls 同维反相。此前 decodeToolChoice 只读 type/name，
// 这个位静默蒸发：同族往返丢失、跨族也无从与 parallel_tool_calls 互转，且无注记
//（caps 还误标 anthropic 没有这一维）。归一进 ir.Request.ParallelToolCalls 后，
// 入站/同族/两个跨族方向/缺席不发明五条路径都要对。

const anthropicDisableBody = `{"model":"m","max_tokens":16,` +
	`"messages":[{"role":"user","content":"hi"}],` +
	`"tools":[{"name":"t","input_schema":{"type":"object"}}],` +
	`"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`

// 入站：disable_parallel_tool_use:true 归一成 ParallelToolCalls=false。
func TestDisableParallelDecodesIntoIR(t *testing.T) {
	r := decodeReq(t, codec.ProtocolAnthropic, anthropicDisableBody)
	if r.ParallelToolCalls == nil || *r.ParallelToolCalls {
		t.Fatalf("disable_parallel_tool_use:true 没归一成 ParallelToolCalls=false：%v", r.ParallelToolCalls)
	}
}

// 同族往返：anthropic→anthropic 原样写回 disable_parallel_tool_use:true。
func TestDisableParallelSameFamilyRoundTrip(t *testing.T) {
	r := decodeReq(t, codec.ProtocolAnthropic, anthropicDisableBody)
	body := string(encodeOut(t, codec.ProtocolAnthropic, r))
	if !strings.Contains(body, `"disable_parallel_tool_use":true`) {
		t.Errorf("同族往返丢了 disable_parallel_tool_use：%s", body)
	}
}

// 跨族 anthropic→chat：disable:true 翻成顶层 parallel_tool_calls:false。
func TestDisableParallelCrossToChat(t *testing.T) {
	r := decodeReq(t, codec.ProtocolAnthropic, anthropicDisableBody)
	body := string(encodeOut(t, codec.ProtocolChatCompletions, r))
	if !strings.Contains(body, `"parallel_tool_calls":false`) {
		t.Errorf("anthropic→chat 没把 disable 翻成 parallel_tool_calls:false：%s", body)
	}
}

// 跨族 chat→anthropic：顶层 parallel_tool_calls:false 翻成 disable_parallel_tool_use:true
// （客户端没给 tool_choice，编码侧合成 auto 承载禁止位）。
func TestParallelFalseCrossToAnthropic(t *testing.T) {
	r := decodeReq(t, codec.ProtocolChatCompletions,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}],`+
			`"parallel_tool_calls":false}`)
	if r.ParallelToolCalls == nil || *r.ParallelToolCalls {
		t.Fatalf("chat parallel_tool_calls:false 没进 IR：%v", r.ParallelToolCalls)
	}
	body := string(encodeOut(t, codec.ProtocolAnthropic, r))
	if !strings.Contains(body, `"disable_parallel_tool_use":true`) {
		t.Errorf("chat→anthropic 没把 parallel_tool_calls:false 翻成 disable 位：%s", body)
	}
}

// 缺席不发明：没有 disable 位时 anthropic 出站不写 disable_parallel_tool_use，
// 也不凭空合成 tool_choice。
func TestDisableParallelAbsentNotInvented(t *testing.T) {
	r := decodeReq(t, codec.ProtocolAnthropic,
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"name":"t","input_schema":{"type":"object"}}]}`)
	if r.ParallelToolCalls != nil {
		t.Fatalf("缺席被发明成 %v", *r.ParallelToolCalls)
	}
	body := string(encodeOut(t, codec.ProtocolAnthropic, r))
	if strings.Contains(body, "disable_parallel_tool_use") {
		t.Errorf("客户端没给却发明了 disable_parallel_tool_use：%s", body)
	}
}

// 没有工具时不合成 tool_choice：禁止并行在无工具场景是空操作，合成一个
// tool_choice 反而会被上游拒（tool_choice 需要 tools 配套）。
func TestDisableParallelNoToolsNoSynthesis(t *testing.T) {
	r := decodeReq(t, codec.ProtocolChatCompletions,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":false}`)
	body := string(encodeOut(t, codec.ProtocolAnthropic, r))
	if strings.Contains(body, "disable_parallel_tool_use") || strings.Contains(body, `"tool_choice"`) {
		t.Errorf("无工具却合成了 tool_choice/disable 位：%s", body)
	}
}
