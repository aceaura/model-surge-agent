package responses

import (
	"strings"
	"testing"
)

// 轮次84：reasoning.generate_summary 是官方废弃别名（openai-python
// shared/reasoning.py 明标「Deprecated: use summary instead」，与 summary 同为
// Literal["auto","concise","detailed"]）。此前 wireReasoning 只认 summary，客户
// 端只发 generate_summary 会让整份摘要配置静默消失（连 default 分支的
// t.Summary != "" 都不成立，reasoning 整个不写）。折进现代槽位后：解码收下、
// 编码只吐 summary，功能等价无需注记——同 chat 的 functions/function_call
//（r109_test 钉住）。

// 只发废弃别名：折进 Summary，编码归一到现代键 summary，绝不回吐 generate_summary。
func TestGenerateSummaryFoldsIntoSummary(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","reasoning":{"generate_summary":"detailed"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking == nil {
		t.Fatal("只给 generate_summary 却没建 Thinking——废弃别名被整个丢了")
	}
	if req.Thinking.Summary != "detailed" {
		t.Errorf("Summary = %q, want detailed（generate_summary 没折进来）", req.Thinking.Summary)
	}
	// 没表态开关：generate_summary 不是「要不要思考」，Enabled 保持三态的没提。
	if req.Thinking.Enabled != nil {
		t.Errorf("只给摘要别名不该点亮开关：Enabled = %v", *req.Thinking.Enabled)
	}

	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"summary":"detailed"`) {
		t.Errorf("编码没把折入的摘要写成现代键 summary：%s", s)
	}
	if strings.Contains(s, "generate_summary") {
		t.Errorf("编码回吐了废弃键 generate_summary（应归一到 summary）：%s", s)
	}
}

// 废弃别名 + effort 同给：折进 Summary 且 effort 点亮开关，编码走 On() 分支
// 原样送 detailed（不被 auto 兜底覆盖）。
func TestGenerateSummaryWithEffort(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","reasoning":{"effort":"high","generate_summary":"detailed"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Thinking.On() || req.Thinking.Effort != "high" {
		t.Fatalf("effort 没点亮开关：On=%v Effort=%q", req.Thinking.On(), req.Thinking.Effort)
	}
	if req.Thinking.Summary != "detailed" {
		t.Errorf("Summary = %q, want detailed", req.Thinking.Summary)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"effort":"high"`) || !strings.Contains(s, `"summary":"detailed"`) {
		t.Errorf("编码丢了 effort 或折入的 summary：%s", s)
	}
	if strings.Contains(s, `"summary":"auto"`) {
		t.Errorf("客户端已点 detailed，被 auto 兜底降级了：%s", s)
	}
}

// 现代键与废弃别名同给：现代键 summary 胜出（官方「use summary instead」）。
func TestSummaryWinsOverGenerateSummary(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","reasoning":{"summary":"concise","generate_summary":"detailed"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking.Summary != "concise" {
		t.Errorf("Summary = %q, want concise（现代键该胜出，不被废弃别名盖掉）", req.Thinking.Summary)
	}
}

// 只给现代键：既有行为不变（回归护栏）。
func TestModernSummaryUnchanged(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","reasoning":{"summary":"detailed"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking.Summary != "detailed" {
		t.Errorf("Summary = %q, want detailed", req.Thinking.Summary)
	}
}

// 两个摘要键都不给：Summary 留空，编码不凭空造 summary（除非 On() 分支补 auto）。
func TestNeitherSummaryKeyStaysEmpty(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","reasoning":{"effort":"high"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking.Summary != "" {
		t.Errorf("没给摘要键却凭空出现 Summary = %q", req.Thinking.Summary)
	}
}
