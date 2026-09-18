package gemini

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// Gemini 的 functionCall 常常不带 id。两轮的同名调用必须拿到不同的合成 id，
// 否则客户端把两轮都回传进历史后，配对会错位到另一轮的调用上。
func TestNonStreamSynthIDsDifferAcrossTurns(t *testing.T) {
	first := nonStreamToolID(t, "r1")
	second := nonStreamToolID(t, "r2")
	if first == second {
		t.Fatalf("两轮的无 id 调用撞成同一个合成 id %q", first)
	}
}

// 流式路径同理：scope 取自帧里的 responseId。
func TestStreamSynthIDsDifferAcrossTurns(t *testing.T) {
	first := streamToolID(t, "r1")
	second := streamToolID(t, "r2")
	if first == second {
		t.Fatalf("两轮流式的无 id 调用撞成同一个合成 id %q", first)
	}
}

func nonStreamToolID(t *testing.T, respID string) string {
	t.Helper()
	body := `{"responseId":"` + respID + `","modelVersion":"m","candidates":[{"index":0,` +
		`"content":{"role":"model","parts":[{"functionCall":{"name":"grep","args":{}}}]}}]}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return soleToolID(t, resp.Content)
}

func streamToolID(t *testing.T, respID string) string {
	t.Helper()
	dec := newStreamDecoder()
	frame := `{"responseId":"` + respID + `","modelVersion":"m","candidates":[{"index":0,` +
		`"content":{"role":"model","parts":[{"functionCall":{"name":"grep","args":{}}}]}}]}`
	var agg ir.Aggregator
	events, err := dec.Feed("", frame)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	for _, ev := range events {
		agg.Add(ev)
	}
	for _, ev := range dec.Finish() {
		agg.Add(ev)
	}
	return soleToolID(t, agg.Response().Content)
}

func soleToolID(t *testing.T, blocks []ir.Block) string {
	t.Helper()
	var ids []string
	for _, b := range blocks {
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			ids = append(ids, b.ToolUse.ID)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("tool blocks = %v, want exactly 1", ids)
	}
	return ids[0]
}
