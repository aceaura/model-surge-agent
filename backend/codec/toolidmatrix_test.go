package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件是标识符形态的矩阵：合成 id 的可识别性与它在四个出站协议上的落位。
//
// 长度收敛与字节预算的矩阵不在这里——它们要注入能力位，而四协议的相应能力位
// 全为零值（无实测证据），从注册表取真值只能测到「跳过」这一格。
// 那两组在 codec 包内测（shape_toolid_test.go / payload_test.go），
// 这里只测能用真协议驱动的部分。

// --- 合成 id 前缀矩阵：三处合成点 ---

// synthSites 是三处会合成调用 id 的解码路径。
//
// 上游不给 id 时必须合成：另外三个协议都要 id 才能把结果回指到调用。
// 三处必须产出同一形态，漏改一处就会让出站侧的前缀判定在那条路上静默失效。
var synthSites = map[string]struct {
	protocol string
	raw      string
}{
	"chat_completions/stream": {
		protocol: codec.ProtocolChatCompletions,
		// 不给 id，只给 name 与 arguments。
		raw: strings.Join([]string{
			`data: {"id":"m","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"grep","arguments":"{}"}}]}}]}`,
			``,
			`data: {"id":"m","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"),
	},
	"gemini/stream": {
		protocol: codec.ProtocolGemini,
		raw: strings.Join([]string{
			`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"grep","args":{}}}]}}]}`,
			``,
		}, "\n"),
	},
	"gemini/non-stream": {
		protocol: codec.ProtocolGemini,
		// 非流式响应聚合走另一条代码路径，同样会合成。
		raw: `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"grep","args":{}}}]},"finishReason":"STOP"}]}`,
	},
}

// TestSynthIDMatrixCoversEverySite 断言三处合成点都给出带前缀、可判定的 id。
func TestSynthIDMatrixCoversEverySite(t *testing.T) {
	for site, fx := range synthSites {
		t.Run(site, func(t *testing.T) {
			resp := decodeAnyBody(t, fx.protocol, fx.raw)
			calls := toolUses(resp)
			if len(calls) != 1 {
				t.Fatalf("解出 %d 个工具块，应为 1", len(calls))
			}
			id := calls[0].ID
			if id == "" {
				t.Fatal("上游没给 id 时必须合成一个，否则结果无从回指到调用")
			}
			if !codec.IsSynthToolID(id) {
				t.Errorf("合成的 id %q 缺前缀，出站侧判不出来源、无从决定该不该省略", id)
			}
		})
	}
}

// 上游给了 id 就不许加前缀：误判会让一个上游认得的 id 被省略掉。
func TestUpstreamIDIsNotMarkedSynth(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_upstream","name":"grep","args":{}}}]}}]}`,
		``,
	}, "\n")
	resp := decodeAnyBody(t, codec.ProtocolGemini, raw)
	calls := toolUses(resp)
	if len(calls) != 1 {
		t.Fatalf("解出 %d 个工具块，应为 1", len(calls))
	}
	if got := calls[0].ID; got != "call_upstream" {
		t.Errorf("上游 id 被改成了 %q", got)
	}
	if codec.IsSynthToolID(calls[0].ID) {
		t.Error("上游 id 被误判成合成 id，出站会把它省略掉")
	}
}

// --- 合成 id 出站省略矩阵：4 出站 × 合成/原生 ---

// TestSynthIDOmissionMatrix 是能力位两档 × 四出站的矩阵。
//
// id 字段可选的协议（gemini）省略合成 id，交由上游按调用顺序消歧；
// 必填的协议保留，因为省略会让上游彻底无法配对——比发一个陌生 id 更糟。
func TestSynthIDOmissionMatrix(t *testing.T) {
	synth := codec.SynthToolID("grep", 1)

	for _, out := range outboundNames() {
		oc, ok := codec.Outbound(out)
		if !ok {
			t.Fatalf("outbound %q not registered", out)
		}
		optional := oc.Caps().ToolIDOptional

		t.Run("synth/"+out, func(t *testing.T) {
			body, notes := lossyOf(t, out, toolPairRequest(synth))
			leaked := strings.Contains(string(body), synth)
			if optional && leaked {
				t.Errorf("%s 的 id 可选，合成 id 不该泄漏：%s", out, body)
			}
			if !optional && !leaked {
				t.Errorf("%s 的 id 必填，省略会让上游无法配对：%s", out, body)
			}
			// 省略是恢复协议原生形态，不是削弱请求：不记说明。
			for _, n := range notes {
				if strings.Contains(n, "tool call id") {
					t.Errorf("%s 省略合成 id 报了说明 %q", out, n)
				}
			}
		})

		t.Run("native/"+out, func(t *testing.T) {
			const native = "call_native_7"
			body, _ := lossyOf(t, out, toolPairRequest(native))
			if !strings.Contains(string(body), native) {
				t.Errorf("%s 丢了上游原生 id，下一轮还得重新合成：%s", out, body)
			}
		})
	}
}

// toolPairRequest 造一轮「调用 + 结果」的历史。
// 两侧都要有：省略与改写都必须同时作用于调用与结果，只改一侧配对就断了。
func toolPairRequest(id string) *ir.Request {
	req := probeRequest(ir.Block{Type: ir.BlockText, Text: "ok"})
	req.Tools = []ir.Tool{{Name: "grep", Description: "search",
		Schema: `{"type":"object","properties":{"pattern":{"type":"string"}}}`}}
	req.Messages = append(req.Messages,
		ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: id, Name: "grep", Input: `{"pattern":"TODO"}`,
			}},
		}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: id,
				Content:   []ir.Block{{Type: ir.BlockText, Text: "found 3"}},
			}},
		}},
	)
	return req
}

// decodeAnyBody 解一段上游内容，SSE 与裸 JSON 都吃。
// gemini 的非流式响应不是 SSE，所以不能一律走帧扫描器。
func decodeAnyBody(t *testing.T, protocol, raw string) *ir.Response {
	t.Helper()
	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		c, ok := codec.Outbound(protocol)
		if !ok {
			t.Fatalf("outbound %q not registered", protocol)
		}
		resp, err := c.DecodeResponse([]byte(raw))
		if err != nil {
			t.Fatalf("%s DecodeResponse: %v", protocol, err)
		}
		return resp
	}
	resp, _ := decodeStreamWithNotes(t, protocol, raw)
	return resp
}
