package codec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次52：DescribeResponseClientMetaLoss 的「提示缓存诊断回执」维度门控。
//
// prompt_cache_diagnostics 由客户端在请求侧用 prompt_cache_options.
// comparison_response_id 主动索要（官方：「supplying this field requests prompt
// cache diagnostics」），只有 responses 一族响应有槽位承载——官方 ChatCompletion
// 响应也无此字段，anthropic 更没有。故 chat_completions / anthropic / gemini 三族
// 拿到 responses 上游返回的诊断回执时都无处承载、真非空即照实报出；responses
// 承载族原样带回不报。
//
// 与 moderation / metadata 分账：那两族 chat_completions 也是承载族，只
// anthropic / gemini 丢；诊断回执 chat 同样装不下，承载族只有 responses 一家。
// 判据同源、措辞一致，字段空 / 显式 null / nil 响应一律静默，绝不误报。

const cacheDiagFingerprint = "prompt cache diagnostics receipt"

func cacheDiagResp() *ir.Response {
	return &ir.Response{
		ID: "r1", Model: "m",
		ResponsesPromptCacheDiagnostics: json.RawMessage(`{"type":"cache_hit","cached_tokens":128}`),
	}
}

// responses 承载族：诊断回执原样带回，不得报丢弃。
func TestCacheDiagResponsesCarrierSilent(t *testing.T) {
	if notes := DescribeResponseClientMetaLoss(cacheDiagResp(), ProtocolResponses); len(notes) != 0 {
		t.Errorf("承载族 responses 被误报诊断丢弃：%v", notes)
	}
}

// chat / anthropic / gemini 非承载族：真非空即报出，且此 resp 只设了诊断一维，
// 故只报一条。
func TestCacheDiagNonCarriersNoted(t *testing.T) {
	for _, name := range []string{ProtocolChatCompletions, ProtocolAnthropic, ProtocolGemini} {
		notes := DescribeResponseClientMetaLoss(cacheDiagResp(), name)
		if len(notes) != 1 || !strings.Contains(notes[0], cacheDiagFingerprint) {
			t.Errorf("非承载族 %s 未正确报出诊断丢弃（want 1 条）：%v", name, notes)
		}
	}
}

// chat 是 moderation/metadata 承载族、却是诊断非承载族：三维都非空时，chat 只报
// 诊断这一维，绝不误报它承载得了的 moderation/metadata。
func TestCacheDiagChatOnlyReportsDiagnostics(t *testing.T) {
	resp := cacheDiagResp()
	resp.ResponsesModeration = json.RawMessage(`{"input":{"flagged":true}}`)
	resp.ClientMetadata = map[string]string{"trace": "abc"}
	notes := DescribeResponseClientMetaLoss(resp, ProtocolChatCompletions)
	if len(notes) != 1 || !strings.Contains(notes[0], cacheDiagFingerprint) {
		t.Fatalf("chat 应只报诊断一维，实得：%v", notes)
	}
	for _, n := range notes {
		if strings.Contains(n, "moderation receipt") || strings.Contains(n, "echoed client metadata") {
			t.Errorf("chat 误报了它承载的维：%v", notes)
		}
	}
}

// anthropic 三维全非承载：诊断 + moderation + metadata 三维都报。
func TestCacheDiagAnthropicReportsAllThree(t *testing.T) {
	resp := cacheDiagResp()
	resp.ResponsesModeration = json.RawMessage(`{"input":{"flagged":true}}`)
	resp.ClientMetadata = map[string]string{"trace": "abc"}
	notes := DescribeResponseClientMetaLoss(resp, ProtocolAnthropic)
	var diag, mod, meta bool
	for _, n := range notes {
		switch {
		case strings.Contains(n, cacheDiagFingerprint):
			diag = true
		case strings.Contains(n, "moderation receipt"):
			mod = true
		case strings.Contains(n, "echoed client metadata"):
			meta = true
		}
	}
	if !diag || !mod || !meta {
		t.Errorf("anthropic 未报全三维（diag=%v mod=%v meta=%v）：%v", diag, mod, meta, notes)
	}
}

// 空 / 显式 null / nil 响应：一律静默，绝不误报（对称防御的下界）。
func TestCacheDiagEmptyAndNullSilent(t *testing.T) {
	if notes := DescribeResponseClientMetaLoss(nil, ProtocolChatCompletions); len(notes) != 0 {
		t.Errorf("nil 响应被误报：%v", notes)
	}
	empty := &ir.Response{ID: "r1", Model: "m"}
	nullDiag := &ir.Response{ID: "r1", Model: "m",
		ResponsesPromptCacheDiagnostics: json.RawMessage(`null`)}
	for _, name := range []string{ProtocolChatCompletions, ProtocolAnthropic, ProtocolGemini} {
		if notes := DescribeResponseClientMetaLoss(empty, name); len(notes) != 0 {
			t.Errorf("空字段被误报（%s）：%v", name, notes)
		}
		if notes := DescribeResponseClientMetaLoss(nullDiag, name); len(notes) != 0 {
			t.Errorf("显式 null 诊断被误报（%s）：%v", name, notes)
		}
	}
}
