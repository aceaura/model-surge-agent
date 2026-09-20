package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

const probePNG = "iVBORw0KGgoAAAANSUhEUg=="

func toolResultWithScreenshot() *ir.Request {
	return &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "shot", Input: `{}`}},
		}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{
				{Type: ir.BlockText, Text: "screenshot:"},
				{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: probePNG}},
			}}},
		}},
	}}
}

// 本轮的第二条入口证据：responses 与 gemini 的工具结果载荷是单个字符串，
// 图片原来被 joinText 静默碾掉——模型看不到截图却被要求据此答题，答错
// 且无人知道为什么。chat_completions 更糟：role:tool 带 image_url part
// 会被上游按格式错误拒收整个请求。
func TestToolResultMediaSurvivesEveryProtocol(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			oc, _ := codec.Outbound(name)
			body, err := oc.EncodeRequest(toolResultWithScreenshot())
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if !strings.Contains(string(body), probePNG) {
				t.Errorf("图片载荷在出站请求体里消失了: %s", body)
			}
			// 文本也要留着：抽走媒体不该顺手把工具的文本输出弄丢。
			if !strings.Contains(string(body), "screenshot:") {
				t.Errorf("工具结果的文本消失了: %s", body)
			}
		})
	}
}

// 挪位置是有损的：模型看到的材料位置与调用方发的不同，要能看见这件事。
func TestMovedToolResultMediaIsReportedAsLossy(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			oc, _ := codec.Outbound(name)
			caps := oc.Caps()
			lo, ok := oc.(codec.LossyEncoder)
			if !ok {
				t.Skipf("%s 没有有损说明出口", name)
			}
			_, notes, err := lo.EncodeRequestLossy(toolResultWithScreenshot())
			if err != nil {
				t.Fatalf("EncodeRequestLossy: %v", err)
			}
			mentioned := anyNoteMentions(notes, "media moved into a following user turn")
			if caps.ToolResultTextOnly && !mentioned {
				t.Errorf("挪走了媒体却没报有损说明: %v", notes)
			}
			if !caps.ToolResultTextOnly && mentioned {
				t.Errorf("原生装得下媒体却报了挪位置: %v", notes)
			}
		})
	}
}

// anthropic 的 tool_result.content 原生装得下媒体。对它动手会把一个正确的
// 形态改坏，也会凭空多出一条用户消息。
func TestNativeToolResultMediaIsLeftInPlace(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if oc.Caps().ToolResultTextOnly {
		t.Fatal("anthropic 被标成工具结果只装文本，与探针实测矛盾")
	}
	req := toolResultWithScreenshot()
	before := len(req.Messages)
	body, err := oc.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// 首条不是 user，anthropic 会补一条前导占位，所以是 before+1 条。
	if got := strings.Count(string(body), `"role":"user"`); got != 2 {
		t.Errorf(`user 消息数 = %d，想要 2（前导占位 + 工具结果那条）: %s`, got, body)
	}
	if len(req.Messages) != before {
		t.Errorf("调用方的请求被改写了：%d → %d 条", before, len(req.Messages))
	}
	// 图片必须还在 tool_result 里面，而不是被挪到一条独立消息里。
	idx := strings.Index(string(body), `"tool_result"`)
	if idx < 0 {
		t.Fatalf("没有 tool_result 块: %s", body)
	}
	if !strings.Contains(string(body)[idx:], probePNG) {
		t.Errorf("图片不在 tool_result 内部: %s", body)
	}
}

// 同一条消息里多个工具结果的媒体合成一条用户消息：拆成多条会让模型看到
// 一串没有上下文的图，也会多出几轮空洞的角色交替。
func TestMediaFromSeveralToolResultsCollapseIntoOneTurn(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "shot", Input: `{}`}},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t2", Name: "shot", Input: `{}`}},
		}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{
				{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AAAA"}},
			}}},
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "t2", Content: []ir.Block{
				{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "BBBB"}},
			}}},
		}},
	}}
	// 只看一个协议的消息条数就够：抽出逻辑在整形层是共享的，
	// 各协议的差别只在怎么把那条消息编出来。
	shaped := req.Clone()
	caps := codec.Capabilities{ToolResultTextOnly: true, Tools: true, Images: true}
	codec.ShapeRequest(shaped, "probe", caps)
	if len(shaped.Messages) != 3 {
		t.Fatalf("消息数 = %d，想要 3（助手 + 工具结果 + 一条合并的媒体）", len(shaped.Messages))
	}
	media := shaped.Messages[2]
	if media.Role != ir.RoleUser {
		t.Errorf("媒体那条的角色 = %q，想要 user", media.Role)
	}
	if len(media.Content) != 2 {
		t.Errorf("媒体块数 = %d，想要 2（两个工具结果的图并进一条）", len(media.Content))
	}
}

// 位置必须在工具结果之后：排在前面等于让模型先看到材料再看到它来自哪次调用。
func TestMediaTurnFollowsTheToolResult(t *testing.T) {
	shaped := toolResultWithScreenshot()
	caps := codec.Capabilities{ToolResultTextOnly: true, Tools: true, Images: true}
	codec.ShapeRequest(shaped, "probe", caps)
	if len(shaped.Messages) != 3 {
		t.Fatalf("消息数 = %d，想要 3", len(shaped.Messages))
	}
	if shaped.Messages[1].Content[0].Type != ir.BlockToolResult {
		t.Errorf("第二条应当还是工具结果，实际是 %q", shaped.Messages[1].Content[0].Type)
	}
	if !shaped.Messages[2].Content[0].Type.IsMedia() {
		t.Errorf("第三条应当是媒体，实际是 %q", shaped.Messages[2].Content[0].Type)
	}
}

// 纯文本的工具结果不该产生任何额外消息：无条件插入会改变正常请求的形状。
func TestTextOnlyToolResultProducesNoExtraTurn(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f", Input: `{}`}},
		}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{
				{Type: ir.BlockText, Text: "ok"},
			}}},
		}},
	}}
	caps := codec.Capabilities{ToolResultTextOnly: true, Tools: true, Images: true}
	notes := codec.ShapeRequest(req, "probe", caps)
	if len(req.Messages) != 2 {
		t.Errorf("消息数 = %d，想要 2（不该多出媒体那条）", len(req.Messages))
	}
	if anyNoteMentions(notes, "media moved") {
		t.Errorf("没有媒体却报了挪位置: %v", notes)
	}
}

// 编码不得改到调用方那份请求：那份要留着换目标重试，而换到 anthropic 之后
// 图片若已不在 tool_result 里，anthropic 这条路径不插媒体消息，图片就彻底没了。
//
// 守的是编码入口这一层的不变量（各 codec 先 Clone 再整形），不是
// moveToolResultMedia 内部是否再复制一次——ir.Request.Clone 已深拷到
// ToolResult.Content，那里再复制一层是冗余。
func TestMovingMediaDoesNotMutateTheCallersToolResult(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			oc, _ := codec.Outbound(name)
			if !oc.Caps().ToolResultTextOnly {
				t.Skipf("%s 的工具结果原生装得下媒体，不走抽出这条路", name)
			}
			req := toolResultWithScreenshot()
			if _, err := oc.EncodeRequest(req); err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if len(req.Messages) != 2 {
				t.Fatalf("调用方的消息数被改成了 %d，想要 2", len(req.Messages))
			}
			kept := req.Messages[1].Content[0].ToolResult.Content
			hasMedia := false
			for _, b := range kept {
				if b.Type.IsMedia() {
					hasMedia = true
				}
			}
			if !hasMedia {
				t.Errorf("调用方那份 tool_result 里的媒体被摘走了：%+v", kept)
			}
		})
	}
}

// SplitToolResultMedia 是纯函数：没有媒体时原样返回，不做无谓的拷贝，
// 也不让调用方误以为发生了改写。
func TestSplitReturnsInputWhenThereIsNoMedia(t *testing.T) {
	in := []ir.Block{{Type: ir.BlockText, Text: "a"}, {Type: ir.BlockText, Text: "b"}}
	kept, media := codec.SplitToolResultMedia(in)
	if media != nil {
		t.Errorf("没有媒体却抽出了 %d 块", len(media))
	}
	if len(kept) != len(in) {
		t.Errorf("保留块数 = %d，想要 %d", len(kept), len(in))
	}
}

// 四类媒体块都要抽：图片之外的音频、文档、文件在那三个协议的工具结果里
// 同样装不下。
func TestSplitPullsEveryMediaKind(t *testing.T) {
	in := []ir.Block{
		{Type: ir.BlockText, Text: "a"},
		{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "A"}},
		{Type: ir.BlockAudio, Media: &ir.Media{MediaType: "audio/wav", Data: "B"}},
		{Type: ir.BlockDocument, Media: &ir.Media{MediaType: "application/pdf", Data: "C"}},
		{Type: ir.BlockFile, Media: &ir.Media{MediaType: "text/plain", Data: "D"}},
	}
	kept, media := codec.SplitToolResultMedia(in)
	if len(media) != 4 {
		t.Errorf("抽出块数 = %d，想要 4", len(media))
	}
	if len(kept) != 1 || kept[0].Type != ir.BlockText {
		t.Errorf("保留的应当只剩那个文本块，实际 %+v", kept)
	}
}
