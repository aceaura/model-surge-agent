package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次38：responses assistant message 条目的 phase（官方 response_output_message.phase，
// 枚举 commentary|final_answer，docstring 明确「后续请求 preserve 并在所有 assistant
// 消息上 resend」）此前 wireItem 不建模、decode_request 不读，responses→responses 请求
// 历史往返一次即蒸发，无注记。这组测试钉住请求侧同族往返、缺席不造键、以及 phase 仅
// 挂 assistant（user 消息带 phase 不是官方形状，不承载）。

func TestRespPhaseRequestRoundTrip(t *testing.T) {
	// 用交替角色避免两条 assistant 消息被 appendBlocks 合并（本协议把连续同角色
	// 条目并进一条消息，消息级元数据 last-wins——phase 与 ItemID 同款处置）。
	body := `{"model":"gpt-x","input":[` +
		`{"type":"message","id":"msg_u","role":"user","content":[{"type":"input_text","text":"q"}]},` +
		`{"type":"message","id":"msg_1","role":"assistant","phase":"commentary",` +
		`"content":[{"type":"output_text","text":"thinking aloud"}]},` +
		`{"type":"message","id":"msg_u2","role":"user","content":[{"type":"input_text","text":"q2"}]},` +
		`{"type":"message","id":"msg_2","role":"assistant","phase":"final_answer",` +
		`"content":[{"type":"output_text","text":"the answer"}]}` +
		`]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var phases []string
	for _, m := range req.Messages {
		if m.Role == ir.RoleAssistant {
			phases = append(phases, m.ResponsesPhase)
		}
	}
	if len(phases) != 2 || phases[0] != "commentary" || phases[1] != "final_answer" {
		t.Fatalf("phase 解码即丢: %v", phases)
	}

	out, err := EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	for _, want := range []string{`"phase":"commentary"`, `"phase":"final_answer"`} {
		if !strings.Contains(s, want) {
			t.Errorf("重编码丢了 %s: %s", want, s)
		}
	}
}

// 缺席 phase 的消息不得凭空造 phase 键。
func TestRespPhaseAbsentNotInvented(t *testing.T) {
	req, err := DecodeRequest([]byte(
		`{"model":"gpt-x","input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), `"phase"`) {
		t.Errorf("缺席的 phase 被发明: %s", out)
	}
}

// phase 只挂 assistant：user 消息带 phase 不是官方形状，不承载（也不该串到别的消息）。
func TestRespPhaseNotCarriedOnUserMessage(t *testing.T) {
	req, err := DecodeRequest([]byte(
		`{"model":"gpt-x","input":[{"type":"message","role":"user","phase":"final_answer","content":[{"type":"input_text","text":"q"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	for _, m := range req.Messages {
		if m.ResponsesPhase != "" {
			t.Errorf("user 消息不该承载 phase: role=%s phase=%q", m.Role, m.ResponsesPhase)
		}
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), `"phase"`) {
		t.Errorf("user 消息 phase 被回写: %s", out)
	}
}
