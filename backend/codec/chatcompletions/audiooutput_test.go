package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// chat 音频输出与多轮音频引用（移植旧仓 #35）。
//
// 请求侧：assistant 历史里的 audio 只接受 {id} 引用形态，进 ir.Message.AudioID，
// 同族出站原样写回；响应侧：非流式 message.audio 是全网唯一能承载完整音频
// （id/data/expires_at/transcript 四键）的槽位，必须四键齐发。

// 解码 assistant 音频引用 → Clone 存活 → 同族编码只回 {id}。
func TestAssistantAudioReferenceRequestRoundTrip(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":"sing"},` +
		`{"role":"assistant","content":"spoken","audio":{"id":"audio_123"}}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	if got := req.Messages[1].AudioID; got != "audio_123" {
		t.Fatalf("AudioID = %q, want audio_123", got)
	}

	cloned := req.Clone()
	if got := cloned.Messages[1].AudioID; got != "audio_123" {
		t.Fatalf("Clone 丢 AudioID: %q", got)
	}

	out, err := EncodeRequest(cloned)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if n := strings.Count(string(out), `"audio"`); n != 1 {
		t.Fatalf(`"audio" 出现 %d 次，want 1: %s`, n, out)
	}
	if !strings.Contains(string(out), `"audio":{"id":"audio_123"}`) {
		t.Fatalf("引用未原样写回: %s", out)
	}
}

// 音频引用消息必须活过规范化：官方 chat 允许 assistant 历史只带 {audio:{id}}
// 不带 content；相邻 assistant 合并只拼 Content，带引用的消息不得被并入。
func TestAssistantAudioReferenceSurvivesNormalization(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, AudioID: "audio_2"},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "next"}}},
	}}
	ir.Sanitize(req)
	if len(req.Messages) != 3 {
		t.Fatalf("规范化后 messages = %d, want 3: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].AudioID != "audio_2" {
		t.Fatalf("规范化丢 AudioID: %+v", req.Messages[1])
	}

	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var w struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &w); err != nil {
		t.Fatalf("wire unmarshal: %v", err)
	}
	if len(w.Messages) != 3 {
		t.Fatalf("wire messages = %d, want 3: %s", len(w.Messages), out)
	}
	if !strings.Contains(string(w.Messages[1]), `"audio":{"id":"audio_2"}`) {
		t.Fatalf("wire[1] 缺音频引用: %s", w.Messages[1])
	}
	// 不得给引用消息伪造 content。
	if strings.Contains(string(w.Messages[1]), `"content"`) {
		t.Fatalf("wire[1] 被伪造 content: %s", w.Messages[1])
	}
}

// audio 缺席与显式 null 都不产生 AudioID，出站不写 audio 键。
func TestAssistantAudioReferenceAbsentAndNull(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"assistant","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":"x","audio":null}]}`,
	} {
		req, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeRequest(%s): %v", body, err)
		}
		if req.Messages[0].AudioID != "" {
			t.Fatalf("%s: AudioID = %q, want empty", body, req.Messages[0].AudioID)
		}
		out, err := EncodeRequest(req)
		if err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		if strings.Contains(string(out), `"audio"`) {
			t.Fatalf("%s: 出站多出 audio 键: %s", body, out)
		}
	}
}

// 非流式响应四键齐发：解上游 → 编客户端 → 再解无漂移。
func TestAudioOutputResponseRoundTrip(t *testing.T) {
	upstream := `{"id":"c1","model":"m","choices":[{"index":0,"message":` +
		`{"role":"assistant","content":"caption","audio":{"id":"audio_123",` +
		`"data":"UklGRg==","expires_at":1893456000,"transcript":"hello"}},` +
		`"finish_reason":"stop"}]}`
	resp, _, err := DecodeResponseLossy([]byte(upstream))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.Audio == nil {
		t.Fatal("resp.Audio = nil")
	}
	if resp.Audio.ID != "audio_123" || resp.Audio.Data != "UklGRg==" ||
		resp.Audio.ExpiresAt != 1893456000 || resp.Audio.Transcript != "hello" {
		t.Fatalf("resp.Audio = %+v", resp.Audio)
	}

	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	// chat 自家接得住音频输出，不得有注记。
	if len(notes) != 0 {
		t.Fatalf("chat 误报: %v", notes)
	}
	for _, want := range []string{`"id":"audio_123"`, `"data":"UklGRg=="`,
		`"expires_at":1893456000`, `"transcript":"hello"`, `"content":"caption"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("客户端响应缺 %s: %s", want, body)
		}
	}

	// 客户端拿到的响应再进解码器（多轮回传的上游视角），四键无漂移。
	again, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if again.Audio == nil || *again.Audio != *resp.Audio {
		t.Fatalf("再解码漂移: %+v vs %+v", again.Audio, resp.Audio)
	}
}

// audio 缺席 / null → 不解出音频；nil 音频 → 不写键。
func TestAudioOutputResponseAbsent(t *testing.T) {
	for _, upstream := range []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x","audio":null},"finish_reason":"stop"}]}`,
	} {
		resp, _, err := DecodeResponseLossy([]byte(upstream))
		if err != nil {
			t.Fatalf("DecodeResponseLossy: %v", err)
		}
		if resp.Audio != nil {
			t.Fatalf("%s: Audio = %+v, want nil", upstream, resp.Audio)
		}
	}

	body, _, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{ID: "c1", Model: "m"})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if strings.Contains(string(body), `"audio"`) {
		t.Fatalf("nil 音频写出了 audio 键: %s", body)
	}
}
