package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 合成 id 与上游原生 id 混在同一请求里：前者必须省略、后者必须保留，
// 而两者的 functionResponse.name 都要填对。
//
// 省略是因为合成的 id 上游从未见过，发回去它有权拒绝或错配；
// 保留是因为原生 id 能让调用标识原样穿过一轮，省掉下一轮的再次合成。
func TestSynthToolIDIsOmittedButNativeIDSurvives(t *testing.T) {
	synth := codec.SynthToolID("resp-1", "grep", 1)
	const native = "call_native_42"

	body, err := EncodeRequest(&ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
					ID: synth, Name: "grep", Input: `{"pattern":"TODO"}`,
				}},
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
					ID: native, Name: "read", Input: `{"path":"a.go"}`,
				}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: synth,
					Content:   []ir.Block{{Type: ir.BlockText, Text: "found 3"}},
				}},
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: native,
					Content:   []ir.Block{{Type: ir.BlockText, Text: "package main"}},
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if strings.Contains(string(body), synth) {
		t.Errorf("合成 id 泄漏进请求体，上游从未见过这个标识符：%s", body)
	}
	if !strings.Contains(string(body), native) {
		t.Errorf("上游原生 id 被丢掉了，下一轮还得重新合成：%s", body)
	}

	calls, responses := toolPartsOf(t, body)
	if len(calls) != 2 {
		t.Fatalf("functionCall 数 = %d，应为 2：%s", len(calls), body)
	}
	if len(responses) != 2 {
		t.Fatalf("functionResponse 数 = %d，应为 2：%s", len(responses), body)
	}

	// 省略 id 的那一格仍要有正确的 name：上游全靠它配对。
	names := map[string]string{}
	for _, r := range responses {
		names[r.Name] = r.ID
	}
	if id, ok := names["grep"]; !ok {
		t.Errorf("合成 id 的调用丢了 functionResponse.name：%s", body)
	} else if id != "" {
		t.Errorf("合成 id 的 functionResponse 仍带 id %q", id)
	}
	if id, ok := names["read"]; !ok {
		t.Errorf("原生 id 的调用丢了 functionResponse.name：%s", body)
	} else if id != native {
		t.Errorf("原生 id 的 functionResponse.id = %q，应为 %q", id, native)
	}
}

// 省略合成 id 不得记进有损说明：这是恢复本协议的原生形态，不是削弱请求。
// gemini 每个无 id 调用都会触发，恒定的说明会把真正的有损信号淹掉。
func TestOmittingSynthToolIDIsNotReportedAsLossy(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: codec.SynthToolID("resp-1", "grep", 1), Name: "grep", Input: `{}`,
			}},
		}}},
	}

	_, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, n := range notes {
		if strings.Contains(n, "tool call id") || strings.Contains(n, "tool_call_id") {
			t.Errorf("省略合成 id 不该报说明，实得 %q", n)
		}
	}
}

type toolPart struct {
	Name string
	ID   string
}

// toolPartsOf 抽出请求体里的 functionCall 与 functionResponse 两类 part。
func toolPartsOf(t *testing.T, body []byte) (calls, responses []toolPart) {
	t.Helper()
	var w struct {
		Contents []struct {
			Parts []struct {
				FunctionCall *struct {
					Name string          `json:"name"`
					ID   string          `json:"id"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
				FunctionResponse *struct {
					Name string `json:"name"`
					ID   string `json:"id"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal body: %v\n%s", err, body)
	}
	for _, c := range w.Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				calls = append(calls, toolPart{Name: p.FunctionCall.Name, ID: p.FunctionCall.ID})
			}
			if p.FunctionResponse != nil {
				responses = append(responses,
					toolPart{Name: p.FunctionResponse.Name, ID: p.FunctionResponse.ID})
			}
		}
	}
	return calls, responses
}
