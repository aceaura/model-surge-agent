package pipeline

import (
	"net/http/httptest"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// silentEncoder 实现 StreamHeartbeat 但明确表示不发保活。
type silentEncoder struct{}

func (silentEncoder) Encode(ir.Event) ([][]byte, error) { return nil, nil }
func (silentEncoder) Finish() [][]byte                  { return nil }
func (silentEncoder) HeartbeatFrame() []byte            { return nil }

// plainEncoder 不实现 StreamHeartbeat，用来验证回落。
type plainEncoder struct{}

func (plainEncoder) Encode(ir.Event) ([][]byte, error) { return nil, nil }
func (plainEncoder) Finish() [][]byte                  { return nil }

// 编码器明确返回 nil 时一个字节都不能写。
//
// 不能回落到注释帧：那会绕过编码器的判断。这个形态在端到端跑不出来——
// 四个入站协议要么实现了这个出口并返回真帧，要么压根没实现，
// 「实现了但说不发」只能在这一层测。
func TestNilHeartbeatFrameWritesNothing(t *testing.T) {
	w := httptest.NewRecorder()
	if err := writeHeartbeat(w, silentEncoder{}, nil); err != nil {
		t.Fatalf("writeHeartbeat: %v", err)
	}
	if got := w.Body.String(); got != "" {
		t.Errorf("编码器说不发保活，却写出了字节：%q", got)
	}
}

// 没实现该出口的编码器回落到 SSE 注释帧。
func TestHeartbeatFallsBackToComment(t *testing.T) {
	w := httptest.NewRecorder()
	if err := writeHeartbeat(w, plainEncoder{}, nil); err != nil {
		t.Fatalf("writeHeartbeat: %v", err)
	}
	if got := w.Body.String(); got != string(codec.HeartbeatComment) {
		t.Errorf("回落帧 = %q，想要 %q", got, codec.HeartbeatComment)
	}
}

// 回落帧也要进捕获，与协议自有帧同口径。
func TestFallbackHeartbeatEntersCapture(t *testing.T) {
	st := capture.New(capture.Options{Mode: capture.ModeAll})
	sess := st.Begin("req-hb")
	w := httptest.NewRecorder()
	if err := writeHeartbeat(w, plainEncoder{}, sess); err != nil {
		t.Fatalf("writeHeartbeat: %v", err)
	}
	sess.Finish(false)
	snap, ok := st.Get("req-hb")
	if !ok {
		t.Fatal("捕获不存在")
	}
	if got := string(snap.Bodies[capture.ClientResponse].Bytes); got != string(codec.HeartbeatComment) {
		t.Errorf("回落帧没进捕获：%q", got)
	}
}
