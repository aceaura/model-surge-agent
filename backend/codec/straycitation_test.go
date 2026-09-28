package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次71：可移植引用（有 URL）挂在非 assistant 消息上时，chat 出站编码器只在
// assistant 消息写 annotations（encode_request.go 的 role 门控），这类引用被静默
// 丢弃。CountNonPortableCitations 只数「形态装不下」的文档类引用，看不见「形态没
// 问题、挂错了角色」的这一类，DescribeLossy 必须按角色单独报。
//
// 分账纪律：
//   - 只有 chat 丢这类引用（responses 不分角色一律写 annotations、anthropic 同族
//     原样往返、gemini 已由「无槽位」分支整体报过），故仅 chat 报 stray 注记。
//   - stray 注记措辞必须与文档类 CitationDropNote 区分：前者是「挂错角色」，后者
//     是「文档下标形态渲染不下」。同一请求两类并存时各报各的、互不重复计数。

func r71portable(url string) ir.Citation {
	return ir.Citation{URL: url, CitedText: "x", Start: 0, End: 1}
}

// 文档类引用：无 URL（Portable()==false），只有下标，属「形态装不下」那一类。
func r71nonPortable() ir.Citation {
	return ir.Citation{CitedText: "x", Start: 0, End: 1}
}

func r71req(role ir.Role, cs ...ir.Citation) *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{{
		Role:    role,
		Content: []ir.Block{{Type: ir.BlockText, Text: "x", Citations: cs}},
	}}}
}

func r71lossy(t *testing.T, name string, req *ir.Request) string {
	t.Helper()
	oc, ok := codec.Outbound(name)
	if !ok {
		t.Fatalf("outbound %q not registered", name)
	}
	return strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
}

// chat：user 角色上的可移植引用被丢弃，必须报 stray 注记。
func TestR71ChatUserPortableCitationNoted(t *testing.T) {
	got := r71lossy(t, codec.ProtocolChatCompletions,
		r71req(ir.RoleUser, r71portable("https://wx.test/1")))
	if !strings.Contains(got, "non-assistant message") {
		t.Errorf("chat 应报 user 角色可移植引用丢失：%q", got)
	}
	// 措辞必须是「挂错角色」，不能误用文档类的「document index」措辞。
	if strings.Contains(got, "document index") {
		t.Errorf("stray 注记误用了文档类措辞：%q", got)
	}
}

// chat：门控是按「非 assistant」而非「user 专属」——任何非助手角色都丢、都报。
// IR 实际只产 user/assistant 两种角色（system 是独立字段、tool 结果并在 user
// 消息里），这里用字面角色验证计数器的角色判据本身不写死 user。
func TestR71ChatNonAssistantRolesNoted(t *testing.T) {
	for _, role := range []ir.Role{ir.Role("system"), ir.Role("tool")} {
		got := r71lossy(t, codec.ProtocolChatCompletions,
			r71req(role, r71portable("https://wx.test/1")))
		if !strings.Contains(got, "non-assistant message") {
			t.Errorf("chat 应报 %s 角色可移植引用丢失：%q", role, got)
		}
	}
}

// chat：assistant 角色上的可移植引用能落地（编码器写 annotations），报了就是假阳性。
func TestR71ChatAssistantPortableCitationSilent(t *testing.T) {
	got := r71lossy(t, codec.ProtocolChatCompletions,
		r71req(ir.RoleAssistant, r71portable("https://wx.test/1")))
	if strings.Contains(got, "non-assistant message") {
		t.Errorf("chat assistant 角色可移植引用不应报 stray：%q", got)
	}
}

// chat：计数正确——跨多条非 assistant 消息累加。
func TestR71ChatStrayCountAcrossMessages(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "a",
			Citations: []ir.Citation{r71portable("https://wx.test/1"), r71portable("https://wx.test/2")}}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "b",
			Citations: []ir.Citation{r71portable("https://wx.test/3")}}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "c",
			Citations: []ir.Citation{r71portable("https://wx.test/4")}}}},
	}}
	got := r71lossy(t, codec.ProtocolChatCompletions, req)
	if !strings.Contains(got, "3 source annotation(s) on non-assistant") {
		t.Errorf("应数到 3 条 stray（assistant 的那条不计）：%q", got)
	}
}

// chat：文档类（非可移植）引用走 CitationDropNote，不进 stray 计数；两类并存各报各的。
func TestR71ChatNonPortableUsesDocumentNoteNotStray(t *testing.T) {
	// 只有文档类引用：报 document 措辞，不报 stray。
	got := r71lossy(t, codec.ProtocolChatCompletions,
		r71req(ir.RoleUser, r71nonPortable()))
	if !strings.Contains(got, "document index") {
		t.Errorf("文档类引用应报 CitationDropNote：%q", got)
	}
	if strings.Contains(got, "non-assistant message") {
		t.Errorf("文档类引用不应计入 stray：%q", got)
	}
	// 两类并存（user 角色）：各自计数互不污染。
	both := r71lossy(t, codec.ProtocolChatCompletions,
		r71req(ir.RoleUser, r71portable("https://wx.test/1"), r71nonPortable()))
	if !strings.Contains(both, "1 document citation(s)") ||
		!strings.Contains(both, "1 source annotation(s) on non-assistant") {
		t.Errorf("两类引用应分别计数：%q", both)
	}
}

// responses：不分角色一律写 annotations（input_text 也带），可移植引用不丢，不报 stray。
func TestR71ResponsesPreservesUserCitation(t *testing.T) {
	got := r71lossy(t, codec.ProtocolResponses,
		r71req(ir.RoleUser, r71portable("https://wx.test/1")))
	if strings.Contains(got, "non-assistant message") {
		t.Errorf("responses 保全 user 角色引用，不应报 stray：%q", got)
	}
}

// anthropic：同族原样往返，不报 stray。
func TestR71AnthropicSameFamilySilent(t *testing.T) {
	got := r71lossy(t, codec.ProtocolAnthropic,
		r71req(ir.RoleUser, r71portable("https://wx.test/1")))
	if strings.Contains(got, "non-assistant message") {
		t.Errorf("anthropic 同族不应报 stray：%q", got)
	}
}

// gemini：无标注槽位，所有引用（含 user 角色可移植）由「no slot」分支整体报，
// 不走 stray（否则同一引用被报两次）。
func TestR71GeminiUsesNoSlotNoteNotStray(t *testing.T) {
	got := r71lossy(t, codec.ProtocolGemini,
		r71req(ir.RoleUser, r71portable("https://wx.test/1")))
	if !strings.Contains(got, "no slot for source annotations") {
		t.Errorf("gemini 应报无槽位丢弃：%q", got)
	}
	if strings.Contains(got, "non-assistant message") {
		t.Errorf("gemini 不应重复报 stray：%q", got)
	}
}

// 缺席静默：无任何引用时四族都不报 stray。
func TestR71NoCitationsSilent(t *testing.T) {
	bare := &ir.Request{Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "x"}}}}}
	for _, name := range codec.OutboundNames() {
		if got := r71lossy(t, name, bare); strings.Contains(got, "non-assistant message") {
			t.Errorf("%s 无引用误报 stray：%q", name, got)
		}
	}
}
