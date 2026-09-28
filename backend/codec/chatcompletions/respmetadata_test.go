package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次46：官方非流式 ChatCompletion 对象带 metadata（客户端关联键值的响应回显）与
// moderation（moderated completions 的审核结果回执）两键，此前 wireResponse 全未建模，
// json.Unmarshal 静默吞掉——同族往返丢回声，responses 上游投影来的 ClientMetadata /
// ResponsesModeration 编码进 chat 时也整维蒸发。这组测试钉住非流式解码入 IR、同族
// 编码逐字回吐、跨族（responses→chat）编码保全、显式 null 与缺席不发明。

const chatRespBody = `{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"metadata":{"trace":"abc","shard":"7"},` +
	`"moderation":{"input":{"flagged":true},"output":{"flagged":false}}}`

func TestChatResponseMetadataModerationDecodedIntoIR(t *testing.T) {
	resp, err := DecodeResponse([]byte(chatRespBody))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.ClientMetadata["trace"] != "abc" || resp.ClientMetadata["shard"] != "7" {
		t.Fatalf("上游 metadata 没落进 IR：%v", resp.ClientMetadata)
	}
	if !strings.Contains(string(resp.ResponsesModeration), `"flagged":true`) {
		t.Fatalf("上游 moderation 没落进 IR：%s", resp.ResponsesModeration)
	}
}

func TestChatResponseMetadataModerationRoundTripsVerbatim(t *testing.T) {
	resp, err := DecodeResponse([]byte(chatRespBody))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"metadata":{"shard":"7","trace":"abc"}`) &&
		!strings.Contains(string(out), `"metadata":{"trace":"abc","shard":"7"}`) {
		t.Fatalf("metadata 回写丢失或变形：%s", out)
	}
	if !strings.Contains(string(out), `"moderation":`) || !strings.Contains(string(out), `"flagged":true`) {
		t.Fatalf("moderation 回写丢失：%s", out)
	}
}

// 跨族保全：responses 上游把回执落进 IR 的 ClientMetadata / ResponsesModeration 后，
// 编码进 chat 非流式响应必须原样带出（两族同为 OpenAI 形状），不再静默蒸发。
func TestEncodeChatResponseWritesMetadataModerationFromIR(t *testing.T) {
	resp := &ir.Response{
		ID: "c1", Model: "m", Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		ClientMetadata:      map[string]string{"trace": "abc"},
		ResponsesModeration: json.RawMessage(`{"input":{"flagged":true}}`),
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"metadata":{"trace":"abc"}`) {
		t.Fatalf("IR ClientMetadata 没写进 chat 响应：%s", out)
	}
	if !strings.Contains(string(out), `"moderation":{"input":{"flagged":true}}`) {
		t.Fatalf("IR ResponsesModeration 没写进 chat 响应：%s", out)
	}
}

// 显式 null 等同没给：不把 4 字节字面量当成审核回执（判据与 responses decode 同源）。
func TestChatResponseModerationExplicitNullIgnored(t *testing.T) {
	body := `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"moderation":null,"metadata":null}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesModeration) != 0 {
		t.Fatalf("null 被当成了审核回执：%s", resp.ResponsesModeration)
	}
	if len(resp.ClientMetadata) != 0 {
		t.Fatalf("null metadata 被发明：%v", resp.ClientMetadata)
	}
}

// 缺席不发明：没有 metadata / moderation 的普通响应，解码不落 IR、编码不写键。
func TestNoChatResponseMetadataModerationWhenAbsent(t *testing.T) {
	body := `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.ClientMetadata != nil || len(resp.ResponsesModeration) != 0 {
		t.Fatalf("缺席被发明：metadata=%v moderation=%s", resp.ClientMetadata, resp.ResponsesModeration)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if strings.Contains(string(out), "metadata") || strings.Contains(string(out), "moderation") {
		t.Fatalf("缺席却写了键：%s", out)
	}
}
