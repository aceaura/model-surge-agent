package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 官方帧三键：sequence_number 全事件必填、从 0 严格递增；annotation.added
// 带 per-part 的 annotation_index；delta/done/part 帧在条目真有原号时带
// item_id——本仓不合成假号（#77 口径），外族来源的文本条目该键缺席。
func TestFrameSequenceItemAnnotationKeys(t *testing.T) {
	enc := newStreamEncoder()
	var wire strings.Builder
	feed := func(ev ir.Event) {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		for _, f := range frames {
			wire.Write(f)
		}
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gpt"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "正文"})
	feed(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{
		{URL: "https://a.example", Title: "甲", CitedText: "正文"},
		{URL: "https://b.example", Title: "乙", CitedText: "正文"},
	}})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{
		Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{
			ID: "c1", Name: "grep", ItemID: "fc_real",
		},
	}})
	feed(ir.Event{Type: ir.EvToolInput, Index: 1, Text: `{"q":1}`})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 1})
	feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse})
	feed(ir.Event{Type: ir.EvMessageStop})
	for _, f := range enc.Finish() {
		wire.Write(f)
	}

	var frames []map[string]json.RawMessage
	for _, line := range strings.Split(wire.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
			t.Fatalf("帧不是合法 JSON：%v\n%s", err, line)
		}
		frames = append(frames, m)
	}
	if len(frames) == 0 {
		t.Fatal("一帧都没编出来")
	}
	// sequence_number：每帧必有，从 0 严格递增。
	for i, f := range frames {
		raw, ok := f["sequence_number"]
		if !ok {
			t.Fatalf("帧 %d 缺 sequence_number：%v", i, f)
		}
		var seq int64
		if err := json.Unmarshal(raw, &seq); err != nil {
			t.Fatalf("帧 %d sequence_number 不是数字：%s", i, raw)
		}
		if seq != int64(i) {
			t.Fatalf("帧 %d 的 sequence_number = %d，不从 0 严格递增", i, seq)
		}
	}
	// annotation_index：两条标注依次 0、1。
	var annIdx []int
	for _, f := range frames {
		typ, _ := unquote(f["type"])
		if typ != evOutputTextAnnotationAdded {
			continue
		}
		raw, ok := f["annotation_index"]
		if !ok {
			t.Fatalf("annotation.added 缺 annotation_index：%v", f)
		}
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("annotation_index 不是数字：%s", raw)
		}
		annIdx = append(annIdx, v)
	}
	if len(annIdx) != 2 || annIdx[0] != 0 || annIdx[1] != 1 {
		t.Fatalf("annotation_index = %v, want [0 1]", annIdx)
	}
	// item_id：同族给过原号的工具条目，增量帧原样带；文本条目没有原号，
	// 键缺席而不是合成假号。
	sawToolDelta := false
	for _, f := range frames {
		typ, _ := unquote(f["type"])
		id, hasID := f["item_id"]
		if typ == evFunctionArgsDelta {
			sawToolDelta = true
			if !hasID {
				t.Fatalf("function_call_arguments.delta 缺 item_id：%v", f)
			}
			if v, _ := unquote(id); v != "fc_real" {
				t.Fatalf("item_id = %q, want fc_real", v)
			}
		}
		if typ == evOutputTextDelta && hasID {
			t.Fatalf("无原号的文本增量帧合成了 item_id：%v", f)
		}
	}
	if !sawToolDelta {
		t.Fatal("没编出 function_call_arguments.delta 帧")
	}
}

func unquote(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// usage.input_tokens_details.cache_write_tokens 双向：解码进 IR 的
// CacheWriteTokens，同族编码原样写回。缓存写入单价与新鲜输入不同，
// 丢掉这一维成本对账就做不了。
func TestCacheWriteTokensRoundTrip(t *testing.T) {
	body := `{"id":"resp_1","model":"gpt-x","status":"completed",` +
		`"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,` +
		`"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":25}}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.CacheWriteTokens != 25 {
		t.Fatalf("CacheWriteTokens = %d, want 25", resp.Usage.CacheWriteTokens)
	}
	if resp.Usage.CacheReadTokens != 30 || resp.Usage.InputTokens != 70 {
		t.Fatalf("cache_read/input = %d/%d, want 30/70",
			resp.Usage.CacheReadTokens, resp.Usage.InputTokens)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"cache_write_tokens":25`) {
		t.Fatalf("cache_write_tokens 没写回：%s", s)
	}
	if !strings.Contains(s, `"cached_tokens":30`) {
		t.Fatalf("cached_tokens 没写回：%s", s)
	}
	// 没有写入量时不造键。
	plain := &ir.Response{ID: "resp_2", Model: "gpt-x",
		Usage: ir.Usage{InputTokens: 10, OutputTokens: 5}}
	out2, err := EncodeResponse(plain)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if strings.Contains(string(out2), "input_tokens_details") {
		t.Fatalf("没给被伪造：%s", out2)
	}
}

// context_window 档出站投影：本族没有专属 reason 取值，归
// incomplete/max_output_tokens——status 至少让客户端知道输出不完整。
func TestContextWindowRendersIncomplete(t *testing.T) {
	status, incomplete := renderStatus(ir.StopContextWindow)
	if status != "incomplete" || incomplete == nil || incomplete.Reason != "max_output_tokens" {
		t.Fatalf("renderStatus = %q, %+v", status, incomplete)
	}
}
