package gemini

import (
	"strings"
	"testing"
)

// 未建模的 part 种类（executableCode 等）此前落到 switch 外被静默跳过。
// 流式与非流式都应计数并报出种类名，客户端才分得出「没产」与「产了被丢」。
func TestUnknownPartKindNotedStream(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"candidates":[{"content":{"parts":[{"executableCode":{"code":"print(1)"}}]}}]}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	notes := dec.Notes()
	if !hasNoteSubstr(notes, "unmodeled") || !hasNoteSubstr(notes, "executableCode") {
		t.Errorf("未建模 part 应报种类名注记：%v", notes)
	}
}

func TestUnknownPartKindNotedNonStream(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"codeExecutionResult":{"outcome":"SUCCESS"}}]}}]}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !hasNoteSubstr(notes, "unmodeled") || !hasNoteSubstr(notes, "codeExecutionResult") {
		t.Errorf("非流式未建模 part 应报种类名注记：%v", notes)
	}
}

// 带已知内容的 part 附带未知键时不算「未知 part」：它已被正常解码，
// 不该误报整块丢弃。
func TestKnownPartWithExtraKeyNotFlagged(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"candidates":[{"content":{"parts":[{"text":"hi","someFutureFlag":true}]}}]}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	if hasNoteSubstr(dec.Notes(), "unmodeled") {
		t.Errorf("带正文的 part 不该被判为未知 part 丢弃：%v", dec.Notes())
	}
}

// 整轮安全阻断：停因压成 content_filter 后，具体阻断原因串仍要带出，
// 否则客户端分不出是哪条策略命中。流式与非流式同处置。
func TestBlockReasonNotedStream(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"promptFeedback":{"blockReason":"SAFETY"}}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	notes := dec.Notes()
	if !hasNoteSubstr(notes, "block reason") || !hasNoteSubstr(notes, "SAFETY") {
		t.Errorf("整轮阻断应报出原因串：%v", notes)
	}
}

func TestBlockReasonNotedNonStream(t *testing.T) {
	body := []byte(`{"promptFeedback":{"blockReason":"OTHER"},"candidates":[]}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if string(resp.StopReason) == "" {
		t.Errorf("应置 content_filter 停因")
	}
	if !hasNoteSubstr(notes, "block reason") || !strings.Contains(strings.Join(notes, "|"), "OTHER") {
		t.Errorf("非流式整轮阻断应报出原因串：%v", notes)
	}
}
