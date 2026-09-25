package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// responses 的消息数上限档（max_messages）本协议没有对应值：出站取
// max_tokens 而非 end_turn——输出确实不完整，客户端不能把半截结果当终稿。
func TestMaxMessagesRendersMaxTokens(t *testing.T) {
	if got := renderStopReason(ir.StopMaxMessages); got != "max_tokens" {
		t.Fatalf("renderStopReason(StopMaxMessages) = %q, want max_tokens", got)
	}
}

// 流式 compaction_delta 独立计数：官方类型、语义清楚但 IR 无槽位，
// 与「连语义都不认识的型」分账，两种帧同流时两条注记各报各的。
func TestCompactionDeltaNoteSplitFromUnknown(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"compaction_delta","encrypted_content":"enc_abc"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"totally_new_delta","x":1}}`); err != nil {
		t.Fatal(err)
	}
	notes := dec.Notes()
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "dropped 1 compaction delta(s)") {
		t.Fatalf("compaction 注记缺失：%q", joined)
	}
	if !strings.Contains(joined, "ignored 1 stream event(s)") {
		t.Fatalf("未知型注记缺失：%q", joined)
	}
	// 排干后不再重复报。
	if again := dec.Notes(); len(again) != 0 {
		t.Fatalf("Notes() 未排干：%q", again)
	}
	// 没给 compaction_delta 时闭嘴。
	quiet := newStreamDecoder()
	if _, err := quiet.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`); err != nil {
		t.Fatal(err)
	}
	if n := quiet.Notes(); len(n) != 0 {
		t.Fatalf("正常流误报：%q", n)
	}
}

// 请求级 output_format（beta 旧槽位）：与 output_config.format 同判据收下；
// 两槽同给新槽胜出；编码恒写新槽，不产出废弃旧键。
func TestOutputFormatLegacySlot(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],` +
		`"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Kind != ir.ResponseFormatSchema ||
		!strings.Contains(req.ResponseFormat.Schema, `"properties"`) {
		t.Fatalf("output_format 没进 IR：%+v", req.ResponseFormat)
	}
	if req.ResponseFormat.Strict == nil || !*req.ResponseFormat.Strict {
		t.Fatalf("旧槽同为新槽判据，恒严格语义：%+v", req.ResponseFormat.Strict)
	}
	// 编码写新槽 output_config.format，不产出废弃旧键。
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"output_config"`) || strings.Contains(string(out), `"output_format"`) {
		t.Fatalf("应写新槽不写旧键：%s", out)
	}

	// 两槽同给：output_config.format 胜出。
	both, err := DecodeRequest([]byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],` +
		`"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"old":{"type":"string"}}}},` +
		`"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"new":{"type":"string"}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if both.ResponseFormat == nil || !strings.Contains(both.ResponseFormat.Schema, `"new"`) {
		t.Fatalf("新槽未胜出：%+v", both.ResponseFormat)
	}

	// 旧槽的不可用形态（非 json_schema / 空 schema / null）同样按没给处理。
	for name, body := range map[string]string{
		"unknown-type": `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"output_format":{"type":"json_object"}}`,
		"null-schema":  `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"output_format":{"type":"json_schema","schema":null}}`,
	} {
		got, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.ResponseFormat != nil {
			t.Errorf("%s 不该进 IR：%+v", name, got.ResponseFormat)
		}
	}
	// 编码侧永不写旧键：即便 IR 带约束，同族回吐也是新槽。
	var w map[string]json.RawMessage
	if err := json.Unmarshal(out, &w); err != nil {
		t.Fatal(err)
	}
	if _, ok := w["output_format"]; ok {
		t.Fatalf("编码吐出了废弃旧键：%s", out)
	}
}
