package gemini

import (
	"testing"
)

// gemini 的 finishReason 折进 IR 只有 content_filter 一档能承载「回答没能正常
// 产出」。内容策略族（SAFETY/RECITATION/…）折进 content_filter 是忠实的；但
// MALFORMED_FUNCTION_CALL 根本不是拦截——是模型生成的工具调用不合法被丢弃，补救
// 动作是重试整个回合而非改措辞，未识别的新枚举同理无从判断。这两类被折进
// content_filter 会把成因与补救方向一起带偏，此前静默无注记（FinishDetailNote 只
// 带 finishMessage 原文串、gemini 罕发）。以下钉住：失真折叠把原枚举回带报出
// （FinishReasonMisfoldNote），忠实折叠与不折进 content_filter 的不报。

const misfoldSub = "not a content-policy block"

// 分类器：只有 MALFORMED_FUNCTION_CALL 与未识别枚举算失真。
func TestFinishReasonMisfoldClassification(t *testing.T) {
	faithful := []string{
		"STOP", "MAX_TOKENS", "FINISH_REASON_UNSPECIFIED", "",
		"SAFETY", "RECITATION", "LANGUAGE", "OTHER", "BLOCKLIST",
		"PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY",
		"IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION",
	}
	for _, s := range faithful {
		if finishReasonMisfold(s) {
			t.Errorf("finishReasonMisfold(%q) = true，应忠实不报", s)
		}
	}
	for _, s := range []string{"MALFORMED_FUNCTION_CALL", "SOME_FUTURE_REASON", "WEIRD"} {
		if !finishReasonMisfold(s) {
			t.Errorf("finishReasonMisfold(%q) = false，应报失真", s)
		}
	}
}

// 非流式：MALFORMED_FUNCTION_CALL 折进 content_filter → 原枚举回带报出。
func TestMalformedFunctionCallNotedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"finishReason":"MALFORMED_FUNCTION_CALL"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.StopReason != "content_filter" {
		t.Errorf("停因应仍折成 content_filter，实得 %q", resp.StopReason)
	}
	if !anyNoteContains(notes, misfoldSub) || !anyNoteContains(notes, "MALFORMED_FUNCTION_CALL") {
		t.Errorf("MALFORMED_FUNCTION_CALL 失真折叠没被报出：%v", notes)
	}
}

// 流式：同损同措辞（规则 b）。
func TestMalformedFunctionCallNotedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, err := dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"candidatesTokenCount":1}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, misfoldSub) || !anyNoteContains(notes, "MALFORMED_FUNCTION_CALL") {
		t.Errorf("流式 MALFORMED_FUNCTION_CALL 失真折叠没被报出：%v", notes)
	}
}

// 非流式：未识别枚举折进 content_filter → 原枚举回带报出。
func TestUnknownFinishReasonNotedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"finishReason":"SOME_FUTURE_REASON"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, misfoldSub) || !anyNoteContains(notes, "SOME_FUTURE_REASON") {
		t.Errorf("未识别枚举失真折叠没被报出：%v", notes)
	}
}

// 非流式：忠实的内容策略拦截（SAFETY）不报失真注记（避免噪音/假阳性）。
func TestSafetyFinishReasonNoMisfoldNote(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"finishReason":"SAFETY"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.StopReason != "content_filter" {
		t.Errorf("SAFETY 应折成 content_filter，实得 %q", resp.StopReason)
	}
	if anyNoteContains(notes, misfoldSub) {
		t.Errorf("忠实的 SAFETY 折叠被误报失真：%v", notes)
	}
}

// 非流式：IMAGE_PROHIBITED_CONTENT 走 convertFinishReason 的 default 分支折进
// content_filter，但它是货真价实的内容拦截，不该报失真（分类器须认得它）。
func TestImageProhibitedContentNoMisfoldNote(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"finishReason":"IMAGE_PROHIBITED_CONTENT"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, misfoldSub) {
		t.Errorf("IMAGE_PROHIBITED_CONTENT 忠实折叠被误报失真：%v", notes)
	}
}

// 非流式：正常结束（STOP）不报失真注记。
func TestStopFinishReasonNoMisfoldNote(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, misfoldSub) {
		t.Errorf("STOP 被误报失真：%v", notes)
	}
}
