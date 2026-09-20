package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这个文件守的是工具失败态的**重入**。没有原生失败字段的协议
// （chat_completions、responses）里，失败态是我们出站时用
// PrefixToolResultError 写进正文的前缀。客户端把整段历史回传后若不认这个
// 前缀，标记位读作 false：再路由到 anthropic 或 gemini 时模型被告知工具
// 调用成功，而正文写着 [tool error] connection refused。多跳还会把前缀
// 叠成 [tool error] [tool error] …。全程 200、计费照常。

// errResult 是一个失败的工具结果。
func errResult(text string) *ir.ToolResult {
	return &ir.ToolResult{
		ToolUseID: "call_1",
		IsError:   true,
		Content:   []ir.Block{{Type: ir.BlockText, Text: text}},
	}
}

// noNativeErrorField 返回没有原生失败字段的出站协议。
func noNativeErrorField(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, name := range outboundNames() {
		if !capsOf(t, name).ToolResultError {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		t.Fatal("没有任何出站协议缺失原生失败字段，本文件的前提不成立")
	}
	return out
}

// TestToolErrorPrefixSurvivesRoundTrip 是核心往返：出站写进前缀，
// 同协议入站必须把它读回成标记位，且正文恢复原样。
func TestToolErrorPrefixSurvivesRoundTrip(t *testing.T) {
	for _, proto := range noNativeErrorField(t) {
		in, ok := codec.Inbound(proto)
		if !ok {
			// gemini 没有入站解码器，往返在它身上无从构造。
			continue
		}
		t.Run(proto, func(t *testing.T) {
			req := shapeRequest()
			req.Messages = append(req.Messages,
				ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{
					Type:    ir.BlockToolUse,
					ToolUse: &ir.ToolUse{ID: "call_1", Name: "fetch", Input: "{}"},
				}}},
				ir.Message{Role: ir.RoleUser, Content: []ir.Block{{
					Type:       ir.BlockToolResult,
					ToolResult: errResult("connection refused"),
				}}})

			body, _ := lossyOf(t, proto, req)
			if !strings.Contains(string(body), codec.ToolErrorPrefix()) {
				t.Fatalf("%s 出站未写出失败前缀：%s", proto, body)
			}

			back, err := in.DecodeRequest(body)
			if err != nil {
				t.Fatalf("%s 回读失败：%v\n%s", proto, err, body)
			}
			res := findToolResult(back)
			if res == nil {
				t.Fatalf("%s 回读后找不到工具结果块", proto)
			}
			if !res.IsError {
				t.Errorf("%s 回读后 is_error = false，模型会把失败当成功", proto)
			}
			got := blockText(res.Content)
			if strings.Contains(got, codec.ToolErrorPrefix()) {
				t.Errorf("%s 回读后前缀未剥掉，下一跳会叠加：%q", proto, got)
			}
			if got != "connection refused" {
				t.Errorf("%s 回读后正文 = %q，想要原样", proto, got)
			}
		})
	}
}

// TestToolErrorPrefixDoesNotAccumulateAcrossHops 钉住多跳不叠加：
// 出站→入站→再出站后仍只有一层前缀。
func TestToolErrorPrefixDoesNotAccumulateAcrossHops(t *testing.T) {
	prefix := codec.ToolErrorPrefix()
	for _, proto := range noNativeErrorField(t) {
		in, ok := codec.Inbound(proto)
		if !ok {
			continue
		}
		t.Run(proto, func(t *testing.T) {
			req := shapeRequest()
			req.Messages = append(req.Messages,
				ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{
					Type:    ir.BlockToolUse,
					ToolUse: &ir.ToolUse{ID: "call_1", Name: "fetch", Input: "{}"},
				}}},
				ir.Message{Role: ir.RoleUser, Content: []ir.Block{{
					Type:       ir.BlockToolResult,
					ToolResult: errResult("boom"),
				}}})

			first, _ := lossyOf(t, proto, req)
			back, err := in.DecodeRequest(first)
			if err != nil {
				t.Fatalf("%s 第一跳回读失败：%v", proto, err)
			}
			second, _ := lossyOf(t, proto, back)
			if n := strings.Count(string(second), prefix); n != 1 {
				t.Errorf("%s 第二跳前缀出现 %d 次，想要 1 次：%s", proto, n, second)
			}
		})
	}
}

// TestToolErrorPrefixMidContentIsNotAdopted 钉住只认块首：正文中间出现同样
// 的字面量是工具自己打的内容（比如它在转述一条日志），改写它会篡改工具输出。
func TestToolErrorPrefixMidContentIsNotAdopted(t *testing.T) {
	text := "log line: " + codec.ToolErrorPrefix() + "something"
	blocks := []ir.Block{{Type: ir.BlockText, Text: text}}
	got, adopted := codec.AdoptToolResultError(blocks)
	if adopted {
		t.Errorf("正文中间的字面量被当成了标记")
	}
	if blockText(got) != text {
		t.Errorf("正文被改写：%q", blockText(got))
	}
}

// TestAdoptToolResultErrorLeavesInputUntouched 钉住不改入参：调用方的 IR
// 要留着换目标重试，原地改会让重试用上被剥过的内容。
func TestAdoptToolResultErrorLeavesInputUntouched(t *testing.T) {
	in := []ir.Block{{Type: ir.BlockText, Text: codec.ToolErrorPrefix() + "boom"}}
	out, adopted := codec.AdoptToolResultError(in)
	if !adopted {
		t.Fatal("块首前缀未被识别")
	}
	if in[0].Text != codec.ToolErrorPrefix()+"boom" {
		t.Errorf("入参被原地改写：%q", in[0].Text)
	}
	if blockText(out) != "boom" {
		t.Errorf("返回值 = %q", blockText(out))
	}
}

// TestAdoptToolResultErrorDropsPrefixOnlyBlock 钉住前缀独占一块时整块去掉：
// PrefixToolResultError 写出的正是这个形态，留一个空文本块会让下游协议
// 多出一个无内容的 part。
func TestAdoptToolResultErrorDropsPrefixOnlyBlock(t *testing.T) {
	in := []ir.Block{
		{Type: ir.BlockText, Text: codec.ToolErrorPrefix()},
		{Type: ir.BlockText, Text: "boom"},
	}
	out, adopted := codec.AdoptToolResultError(in)
	if !adopted {
		t.Fatal("块首前缀未被识别")
	}
	if len(out) != 1 || out[0].Text != "boom" {
		t.Errorf("out = %#v，想要只剩内容块", out)
	}
}

// TestNativeErrorFieldProtocolsKeepUsingIt 钉住有原生字段的协议不走前缀：
// 加前缀等于把同一件事说两遍，其中一遍还混在工具的真实输出里。
func TestNativeErrorFieldProtocolsKeepUsingIt(t *testing.T) {
	for _, proto := range outboundNames() {
		caps := capsOf(t, proto)
		if !caps.ToolResultError {
			continue
		}
		out := codec.PrefixToolResultError(errResult("boom"), caps)
		if strings.Contains(blockText(out), codec.ToolErrorPrefix()) {
			t.Errorf("%s 有原生失败字段却加了前缀：%q", proto, blockText(out))
		}
	}
}

func blockText(blocks []ir.Block) string {
	var s string
	for _, b := range blocks {
		if b.Type == ir.BlockText {
			s += b.Text
		}
	}
	return s
}
