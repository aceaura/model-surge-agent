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

// 轮次72：系统提示（req.System）块上的引用被三外族静默丢弃——responses 把 system
// 收敛成字符串 instructions、chat 的 system 消息只有 text 槽、gemini 的
// systemInstruction part 只装 Text，系统提示在任何协议都没有 annotations 槽位。
// 三个 Messages 级引用计数器（CountCitations / CountNonPortableCitations /
// CountStrayPortableCitations）都只遍历 req.Messages、看不见独立的 req.System，
// 故此前系统提示引用一律无注记（漏报）。DescribeLossy 必须按 name != anthropic
// 统一报，与同块既有的 serverTools/containerUploads/docConfig（都遍历 req.System）
// 同列。anthropic 同族经 encodeBlocks 写回 Citations、无损往返，不报。

func r72portable(url string) ir.Citation {
	return ir.Citation{URL: url, CitedText: "x", Start: 0, End: 1}
}

func r72nonPortable() ir.Citation {
	return ir.Citation{CitedText: "x", Start: 0, End: 1}
}

// r72req 构造一个系统提示带引用、Messages 为空的请求。
func r72req(sysCites ...ir.Citation) *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 10,
		System: []ir.Block{{Type: ir.BlockText, Text: "s", Citations: sysCites}}}
}

func r72lossy(t *testing.T, name string, req *ir.Request) string {
	t.Helper()
	oc, ok := codec.Outbound(name)
	if !ok {
		t.Fatalf("outbound %q not registered", name)
	}
	return strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
}

// 三外族都报系统提示引用丢失（措辞含 "system prompt"）。
func TestR72ForeignFamiliesNoteSystemCitation(t *testing.T) {
	for _, name := range []string{
		codec.ProtocolChatCompletions,
		codec.ProtocolResponses,
		codec.ProtocolGemini,
	} {
		got := r72lossy(t, name, r72req(r72portable("https://wx.test/1")))
		if !strings.Contains(got, "system prompt") {
			t.Errorf("%s 应报系统提示引用丢失：%q", name, got)
		}
	}
}

// responses：系统提示上的可移植引用也丢（instructions=joinText 只取 Text），这与
// Messages 侧「可移植引用一律保全」相反——System 无槽是整体丢弃，必须报；且不得
// 误用 Messages 侧 chat-only 的 stray 措辞。
func TestR72ResponsesDropsPortableSystemCitation(t *testing.T) {
	got := r72lossy(t, codec.ProtocolResponses, r72req(r72portable("https://wx.test/1")))
	if !strings.Contains(got, "system prompt") {
		t.Errorf("responses 系统提示可移植引用应丢弃并报注记：%q", got)
	}
	if strings.Contains(got, "non-assistant message") {
		t.Errorf("responses 系统提示引用不应走 stray 措辞：%q", got)
	}
}

// anthropic：同族经 encodeBlocks 写回 Citations、无损往返，不报（报了即谎报）。
func TestR72AnthropicSystemCitationSilent(t *testing.T) {
	got := r72lossy(t, codec.ProtocolAnthropic, r72req(r72portable("https://wx.test/1")))
	if strings.Contains(got, "system prompt") {
		t.Errorf("anthropic 同族保全系统提示引用，不应报：%q", got)
	}
}

// 计数正确：系统提示跨块多条引用累加，可移植与非可移植都计入（System 整体无槽）。
func TestR72SystemCitationCount(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, System: []ir.Block{
		{Type: ir.BlockText, Text: "a", Citations: []ir.Citation{r72portable("https://wx.test/1"), r72nonPortable()}},
		{Type: ir.BlockText, Text: "b", Citations: []ir.Citation{r72portable("https://wx.test/2")}},
	}}
	got := r72lossy(t, codec.ProtocolChatCompletions, req)
	if !strings.Contains(got, "3 source annotation(s) on the system prompt") {
		t.Errorf("应数到 3 条系统提示引用（可移植+非可移植都算）：%q", got)
	}
}

// System 与 Messages 分账：系统提示引用 + 消息引用并存时各报各的、互不重复计数。
func TestR72SystemAndMessagesDisjoint(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10,
		System: []ir.Block{{Type: ir.BlockText, Text: "s",
			Citations: []ir.Citation{r72portable("https://wx.test/sys")}}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "u",
			Citations: []ir.Citation{r72portable("https://wx.test/msg")}}}}},
	}
	// chat：系统提示引用走 system 注记，user 消息可移植引用走 stray 注记，两条并存。
	got := r72lossy(t, codec.ProtocolChatCompletions, req)
	if !strings.Contains(got, "1 source annotation(s) on the system prompt") {
		t.Errorf("chat 应报 1 条系统提示引用：%q", got)
	}
	if !strings.Contains(got, "1 source annotation(s) on non-assistant") {
		t.Errorf("chat 应同时报 1 条 user 消息 stray 引用（与 System 分账）：%q", got)
	}
}

// 缺席静默：系统提示无引用时四族都不报 system 注记。
func TestR72NoSystemCitationSilent(t *testing.T) {
	bare := &ir.Request{Model: "m", MaxTokens: 10,
		System:   []ir.Block{{Type: ir.BlockText, Text: "s"}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "x"}}}}}
	for _, name := range codec.OutboundNames() {
		if got := r72lossy(t, name, bare); strings.Contains(got, "system prompt") {
			t.Errorf("%s 系统提示无引用误报：%q", name, got)
		}
	}
}

// gemini 是唯一会同时产生两条引用注记的族：无标注槽位（!caps.Citations），故
// Messages 引用走「no slot」注记（CountCitations 只数 Messages），System 引用走
// 「system prompt」注记（CountSystemCitations 只数 System）。二者 map 键不同
// （"citations" vs "system citations"）、计数不相交，必须各自独立报出、互不抑制。
// 钉住这条以防未来误以为「no slot」已涵盖 System 而删掉系统提示注记（或反之），
// 也证明 R72 未把 gemini 的 Messages 无槽注记挤掉。
func TestR72GeminiSystemAndMessagesBothNoted(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10,
		System: []ir.Block{{Type: ir.BlockText, Text: "s",
			Citations: []ir.Citation{r72portable("https://wx.test/sys")}}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "u",
			Citations: []ir.Citation{r72portable("https://wx.test/m1"), r72portable("https://wx.test/m2")}}}}},
	}
	got := r72lossy(t, codec.ProtocolGemini, req)
	if !strings.Contains(got, "1 source annotation(s) on the system prompt") {
		t.Errorf("gemini 应报 1 条系统提示引用：%q", got)
	}
	if !strings.Contains(got, "dropped 2 citation(s): upstream protocol has no slot") {
		t.Errorf("gemini 应同时报 2 条消息引用无槽位丢弃（与 System 分账、互不抑制）：%q", got)
	}
}
