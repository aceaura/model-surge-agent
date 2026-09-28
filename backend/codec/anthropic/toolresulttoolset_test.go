package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次44：toolset_name 是 anthropic tool_result 块上的 beta toolsets 归属名（官方
// tool_result_block_param.toolset_name：「配对 tool_use 所属的 toolset 家族」，可选）。
// 此前 wireBlock 已建模该键、tool_use 侧（轮次28）也解码回吐，但 tool_result 的解码
// 分支从不读它——被 json.Unmarshal 静默吞掉，同族多轮历史里客户端回传上一轮的
// tool_result（带 toolset_name）时往返不再逐字，与 tool_use 侧不对称。这组测试钉住
// 解码入 IR、同族编码回吐、缺席不写键，以及不经解码器的 IR 直构路径。

func TestToolResultToolsetDecodedIntoIR(t *testing.T) {
	body := `{"type":"tool_result","tool_use_id":"toolu_1","content":"ok","toolset_name":"ts_beta"}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	tr := req.Messages[0].Content[0].ToolResult
	if tr == nil {
		t.Fatalf("no ToolResult payload: %#v", req.Messages[0].Content[0])
	}
	if tr.ToolsetName != "ts_beta" {
		t.Fatalf("tool_result toolset_name = %q，没读进 IR", tr.ToolsetName)
	}
}

func TestToolResultToolsetRoundTripsVerbatim(t *testing.T) {
	body := `{"type":"tool_result","tool_use_id":"toolu_1","content":"ok","toolset_name":"ts_beta"}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"toolset_name":"ts_beta"`) {
		t.Fatalf("tool_result toolset_name lost on re-encode: %s", wire)
	}
}

func TestNoToolResultToolsetWhenAbsent(t *testing.T) {
	body := `{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(wire), "toolset_name") {
		t.Fatalf("absent tool_result toolset_name should not emit key: %s", wire)
	}
}

// 编码侧单测：直接从 IR 造带 toolset_name 的 tool_result 块，验证 encodeBlock 逐字
// 写出（覆盖不经解码器的构造路径，例如聚合后的流式响应块）。
func TestEncodeBlockWritesToolResultToolsetFromIR(t *testing.T) {
	wb, ok, err := encodeBlock(ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
		ToolUseID:   "toolu_1",
		Content:     []ir.Block{{Type: ir.BlockText, Text: "ok"}},
		ToolsetName: "ts_beta",
	}})
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	b, _ := json.Marshal(wb)
	if !strings.Contains(string(b), `"toolset_name":"ts_beta"`) {
		t.Fatalf("tool_result toolset_name not written: %s", b)
	}
}

// toolset_name 必须熬过 Clone：请求侧编码会 Clone 整份请求，字符串字段随结构体
// 值拷贝自动带上，此测钉住 cloneBlocks 的 `v := *b.ToolResult` 不漏新字段。
func TestToolResultToolsetSurvivesClone(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
			ToolUseID: "toolu_1", ToolsetName: "ts_beta",
		}}}},
	}}
	cl := req.Clone()
	tr := cl.Messages[0].Content[0].ToolResult
	if tr == nil || tr.ToolsetName != "ts_beta" {
		t.Fatalf("tool_result toolset_name 没熬过 Clone：%#v", tr)
	}
}
