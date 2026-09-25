package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 服务端工具的未建模声明参数——computer 的 display_width_px/display_height_px
// （官方 Required）、web_fetch 的 citations/max_content_tokens——解码即丢会让
// 同族往返编出缺 Required 键的非法定义。逐字段建模跟进永远慢半拍，同族回写
// 整块原文回吐（与 citation.Raw 原文透传同一手法）。
func TestServerToolRawRoundTripKeepsUnmodeledParams(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[` +
		`{"type":"computer_20250124","name":"computer","display_width_px":1024,"display_height_px":768},` +
		`{"type":"web_fetch_20250910","name":"web_fetch","max_uses":2,"citations":{"enabled":true},"max_content_tokens":4096}` +
		`]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("服务端工具没解出来：%+v", req.Tools)
	}
	for i, tl := range req.Tools {
		if len(tl.ServerRaw) == 0 {
			t.Errorf("tools[%d] 没收下原文：%+v", i, tl)
		}
	}
	// 走一遍 Clone（换目标重试的路径）：原文槽位不得丢。
	out, err := EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"display_width_px":1024`, `"display_height_px":768`,
		`"citations":{"enabled":true}`, `"max_content_tokens":4096`, `"max_uses":2`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("未建模声明参数丢了 %s：%s", want, s)
		}
	}
	if strings.Contains(s, `"Raw":`) || strings.Contains(s, `null`) {
		t.Errorf("原文槽位泄漏或伪造 null：%s", s)
	}
	// 类型版本同样是客户端指名的一部分，不得被偷换。
	if !strings.Contains(s, `"type":"computer_20250124"`) || !strings.Contains(s, `"type":"web_fetch_20250910"`) {
		t.Errorf("type 版本丢了：%s", s)
	}
	// 服务端工具不带 input_schema：参数形状由上游那一版工具自己定义。
	if strings.Contains(s, "input_schema") {
		t.Errorf("原文回吐混进了参数形状：%s", s)
	}
}

// 原文回吐不得盖住客户端指名的版本：逐字段路径写回 ServerType 原文，
// 原文路径整块吐出，两条路都不该换成默认版本名。
func TestServerToolRawKeepsClientPinnedVersion(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20260201","name":"web_search","max_uses":1}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"web_search_20260201"`) {
		t.Errorf("客户端指名版本被偷换：%s", out)
	}
	if !strings.Contains(string(out), `"max_uses":1`) {
		t.Errorf("未建模的 max_uses 丢了：%s", out)
	}
}

// 函数工具不得吃原文通道：解码侧 custom/省略 type 不留原文，
// 编码侧没有 ServerType 的 ServerRaw 不认。
func TestServerToolRawNotForFunctionTools(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"custom","name":"f1","input_schema":{"type":"object"}},{"name":"f2"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("函数工具没解出来：%+v", req.Tools)
	}
	for i, tl := range req.Tools {
		if tl.ServerType != "" || len(tl.ServerRaw) != 0 {
			t.Errorf("tools[%d] 函数工具被记成服务端工具：%+v", i, tl)
		}
	}
	// 构造态：只有原文没有 ServerType——必须走逐字段，原文不得泄漏。
	req2 := &ir.Request{
		Model: "claude", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{{
			Name: "f3", Schema: `{"type":"object"}`,
			ServerRaw: json.RawMessage(`{"type":"web_search_20250305","name":"hijack","max_uses":9}`),
		}},
	}
	out, err := EncodeRequest(req2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"max_uses":9`) || strings.Contains(string(out), "hijack") {
		t.Errorf("函数工具吃了原文通道：%s", out)
	}
	if !strings.Contains(string(out), `"input_schema":{"type":"object"}`) {
		t.Errorf("函数工具的正常形状丢了：%s", out)
	}
}

// IR 级序列化纪律：RawMessage 槽位必带 omitempty——没有原文的 Tool 不得
// 伪造 server_raw:null 键，字面量 null 会在下一轮被当成有原文。
func TestServerToolRawOmitEmptyInIR(t *testing.T) {
	raw, err := json.Marshal(ir.Tool{Name: "n", ServerType: "web_search_20250305"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "server_raw") {
		t.Errorf("空 ServerRaw 伪造了键：%s", raw)
	}
	tl := ir.Tool{Name: "n", ServerType: "web_search_20250305",
		ServerRaw: json.RawMessage(`{"type":"web_search_20250305"}`)}
	raw2, err := json.Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw2), `"server_raw":`) {
		t.Errorf("ServerRaw 没随 IR 序列化：%s", raw2)
	}
}
