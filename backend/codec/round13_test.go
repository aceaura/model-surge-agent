package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次13 三个真缺口的全覆盖测试。三处丢弃此前都无人报（rule a：丢弃必须有注记）：
//
//	A-1 responses/gemini 出站把工具结果用 joinText 折成纯文本，嵌在 tool_result
//	    里的拒绝/不透明子块被静默吞掉（顶层同类块跨族会硬报错，嵌套的却被折平）。
//	    anthropic（encodeBlocks）与 chat（encodeContent）递归编码工具结果子块，
//	    拒绝另有处置、不透明跨族硬报错，都不经 joinText，故只报 responses/gemini。
//	A-2 消息级发送者名（chat 的 message.name）只由 chat_completions 一族读写，
//	    跨到其余族整条丢失，而目标协议没有逐消息作者名字段。
//	A-4 responses 条目原号（item id）只由 responses 一族读写，跨族丢弃后客户端
//	    稍后凭 store=true 发来的 item_reference 无处解析。ir.ToolUse.ItemID 注释
//	    已明确承诺此丢弃由有损诊断报出，此前却无人兑现。

const (
	flattenNote = "flattens tool output to a plain string"
	senderNote  = "sender name"
	itemIDNote  = "will not resolve"
)

func toolResultReq(nested ...ir.Block) *ir.Request {
	return &ir.Request{Messages: msg(ir.Block{
		Type:       ir.BlockToolResult,
		ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: nested},
	})}
}

// ---- A-1: 工具结果被折平的非文本子块 ----

func TestDescribeLossyToolResultFlattenNotedForResponses(t *testing.T) {
	req := toolResultReq(
		ir.Block{Type: ir.BlockText, Text: "ok"},
		ir.Block{Type: ir.BlockOpaque, Opaque: &ir.Opaque{WireType: "mystery"}},
	)
	got := strings.Join(DescribeLossy(req, ProtocolResponses, fullCaps()), "\n")
	if !strings.Contains(got, flattenNote) {
		t.Errorf("responses 折平工具结果时嵌套不透明块被静默丢弃，应报注记:\n%s", got)
	}
}

func TestDescribeLossyToolResultFlattenNotedForGemini(t *testing.T) {
	req := toolResultReq(
		ir.Block{Type: ir.BlockText, Text: "ok"},
		ir.Block{Type: ir.BlockRefusal, Text: "抱歉"},
	)
	got := strings.Join(DescribeLossy(req, ProtocolGemini, fullCaps()), "\n")
	if !strings.Contains(got, flattenNote) {
		t.Errorf("gemini 折平工具结果时嵌套拒绝块被静默丢弃，应报注记:\n%s", got)
	}
}

// anthropic/chat 递归编码工具结果子块（不经 joinText），折平注记不得误报到它们头上。
func TestDescribeLossyToolResultFlattenSilentForRecursiveEncoders(t *testing.T) {
	req := toolResultReq(
		ir.Block{Type: ir.BlockText, Text: "ok"},
		ir.Block{Type: ir.BlockOpaque, Opaque: &ir.Opaque{WireType: "mystery"}},
		ir.Block{Type: ir.BlockRefusal, Text: "抱歉"},
	)
	for _, name := range []string{ProtocolAnthropic, ProtocolChatCompletions} {
		got := strings.Join(DescribeLossy(req, name, fullCaps()), "\n")
		if strings.Contains(got, flattenNote) {
			t.Errorf("%s 递归编码工具结果子块、不经 joinText 折平，被误报折平注记:\n%s", name, got)
		}
	}
}

// 只含文本的工具结果没有被折掉的东西，即便目标是 responses/gemini 也不得报。
func TestDescribeLossyToolResultTextOnlySilent(t *testing.T) {
	req := toolResultReq(ir.Block{Type: ir.BlockText, Text: "ok"})
	for _, name := range []string{ProtocolResponses, ProtocolGemini} {
		got := strings.Join(DescribeLossy(req, name, fullCaps()), "\n")
		if strings.Contains(got, flattenNote) {
			t.Errorf("%s 纯文本工具结果无可折内容，被误报折平注记:\n%s", name, got)
		}
	}
}

// ---- A-2: 消息级发送者名 ----

func namedMsgReq() *ir.Request {
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Name: "alice", Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
	}}
}

func TestDescribeLossyMessageNameNotedForNonChat(t *testing.T) {
	for _, name := range []string{ProtocolAnthropic, ProtocolResponses, ProtocolGemini} {
		got := strings.Join(DescribeLossy(namedMsgReq(), name, fullCaps()), "\n")
		if !strings.Contains(got, senderNote) {
			t.Errorf("%s 没有逐消息作者名字段，丢弃 message.name 应报注记:\n%s", name, got)
		}
	}
}

// chat→chat 同族原样回写 message.name，报了就是谎报。
func TestDescribeLossyMessageNameSilentForChat(t *testing.T) {
	got := strings.Join(DescribeLossy(namedMsgReq(), ProtocolChatCompletions, fullCaps()), "\n")
	if strings.Contains(got, senderNote) {
		t.Errorf("chat 同族保留 message.name，被误报丢弃:\n%s", got)
	}
}

// 无名消息不得触发注记。
func TestDescribeLossyMessageNameSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Messages: msg(ir.Block{Type: ir.BlockText, Text: "hi"})}
	got := strings.Join(DescribeLossy(req, ProtocolAnthropic, fullCaps()), "\n")
	if strings.Contains(got, senderNote) {
		t.Errorf("无 message.name 的请求被误报丢弃发送者名:\n%s", got)
	}
}

// ---- A-4: responses 条目原号 ----

func itemIDReq() *ir.Request {
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, ItemID: "msg_1", Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f", ItemID: "fc_1"}},
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{ItemID: "rs_1"}},
		}},
	}}
}

func TestDescribeLossyItemIDNotedForNonResponses(t *testing.T) {
	for _, name := range []string{ProtocolAnthropic, ProtocolChatCompletions, ProtocolGemini} {
		got := strings.Join(DescribeLossy(itemIDReq(), name, fullCaps()), "\n")
		if !strings.Contains(got, itemIDNote) {
			t.Errorf("%s 没有条目 id 槽位，丢弃 item id 应报注记:\n%s", name, got)
		}
	}
}

// responses→responses 同族原号回写，报了就是谎报。
func TestDescribeLossyItemIDSilentForResponses(t *testing.T) {
	got := strings.Join(DescribeLossy(itemIDReq(), ProtocolResponses, fullCaps()), "\n")
	if strings.Contains(got, itemIDNote) {
		t.Errorf("responses 同族保留 item id，被误报丢弃:\n%s", got)
	}
}

// 无 item id 的请求（chat/anthropic 入站）不得触发注记。
func TestDescribeLossyItemIDSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Messages: msg(ir.Block{Type: ir.BlockText, Text: "hi"})}
	got := strings.Join(DescribeLossy(req, ProtocolAnthropic, fullCaps()), "\n")
	if strings.Contains(got, itemIDNote) {
		t.Errorf("无 item id 的请求被误报丢弃条目号:\n%s", got)
	}
}
