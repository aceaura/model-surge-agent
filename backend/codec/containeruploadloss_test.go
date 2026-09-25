package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// R34：container_upload 块的跨族损耗注记与跳过安全性。该块是 anthropic 专属
// （容器文件引用），外族没有 file_id 槽位：整块跳过不降级（file_id 拼进正文
// 会污染回答），损耗经流式 Notes()、非流式 EncodeResponseLossy 与请求侧
// DescribeLossy 三条通道报出。file_id 属会话内容，既不进线上帧也不进注记。
//
// 对应旧仓 #34（d14cf51）。

var rUpload = ir.Block{Type: ir.BlockContainerUpload,
	ContainerUpload: &ir.ContainerUploadRef{FileID: "file_secret1"}}

// uploadStreamEvents 正文 + container_upload 块投影成的事件流。走
// ir.ResponseEvents 而非仅 start/stop：正文要经 EvTextDelta 才会真正下发，
// container_upload 则随 EvBlockStart 的骨架整块到达（该块无增量）。
func uploadStreamEvents() []ir.Event {
	return ir.ResponseEvents(&ir.Response{
		ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "chart ready"}, rUpload},
	})
}

// uploadOnlyEvents 只有两个 container_upload 块（计数聚合用）。
func uploadOnlyEvents() []ir.Event {
	return ir.ResponseEvents(&ir.Response{
		ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{rUpload, rUpload},
	})
}

// assertNoUploadID 注记里不得出现 file_id：那是会话内容，注记会进响应头与日志。
func assertNoUploadID(t *testing.T, notes []string) {
	t.Helper()
	for _, n := range notes {
		if strings.Contains(n, "file_secret1") {
			t.Errorf("注记带出了 file_id：%s", n)
		}
	}
}

// 外族流式：块不出现在线上（file_id 不泄漏），正文不受影响，Notes 报丢失。
func TestContainerUploadStreamForeignSkip(t *testing.T) {
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			out, notes := renderStreamNotes(t, name, uploadStreamEvents())
			if strings.Contains(out, "file_secret1") || strings.Contains(out, "container_upload") {
				t.Errorf("file_id 泄漏进流：\n%s", out)
			}
			if !strings.Contains(out, "chart ready") {
				t.Errorf("正文被误删：\n%s", out)
			}
			if !strings.Contains(strings.Join(notes, "; "), "dropped 1 container upload block(s)") {
				t.Errorf("应报 1 块丢失：%q", notes)
			}
			assertNoUploadID(t, notes)
		})
	}
}

// 多块计数聚合为一条；anthropic 自家 encoder 不报。
func TestContainerUploadStreamCountsAndAnthropicSilent(t *testing.T) {
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			evs := uploadOnlyEvents()
			_, notes := renderStreamNotes(t, name, evs)
			if !strings.Contains(strings.Join(notes, "; "), "dropped 2 container upload block(s)") {
				t.Errorf("多块应计数为 2：%q", notes)
			}
		})
	}
	ic, _ := codec.Inbound(codec.ProtocolAnthropic)
	e := ic.NewStreamEncoder(nil)
	for _, ev := range uploadStreamEvents() {
		if _, err := e.Encode(ev); err != nil {
			t.Fatalf("anthropic Encode(%s): %v", ev.Type, err)
		}
	}
	e.Finish()
	if sn, ok := e.(codec.StreamNotes); ok {
		if notes := sn.Notes(); len(notes) != 0 {
			t.Errorf("anthropic 误报：%v", notes)
		}
	}
}

// responses 的 response.completed 带全量 output：被跳过的块不得从这里漏出，
// 也不得留下空壳 message item。
func TestContainerUploadAbsentFromResponsesFullOutput(t *testing.T) {
	out, _ := renderStreamNotes(t, codec.ProtocolResponses, uploadStreamEvents())
	idx := strings.LastIndex(out, `"type":"response.completed"`)
	if idx < 0 {
		t.Fatalf("没有 response.completed：\n%s", out)
	}
	if final := out[idx:]; strings.Contains(final, "file_secret1") {
		t.Errorf("全量 output 泄漏 file_id：\n%s", final)
	}
	if n := strings.Count(out, `"type":"response.output_item.added"`); n != 1 {
		t.Errorf("output_item.added 应只有正文一条，实得 %d：\n%s", n, out)
	}
}

// 非流式响应侧：EncodeResponseLossy 报出，anthropic 自家静默，无块全静默。
func TestContainerUploadResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "chart ready"}, rUpload}}
	for _, name := range foreignInbound {
		notes := responseLossyNotes(t, name, resp)
		if !strings.Contains(strings.Join(notes, "; "), "dropped 1 container upload block(s)") {
			t.Errorf("%s 应报块丢失：%q", name, notes)
		}
		assertNoUploadID(t, notes)
	}
	if notes := responseLossyNotes(t, codec.ProtocolAnthropic, resp); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
	// 无块全静默。
	resp.Content = resp.Content[:1]
	for _, name := range append([]string{codec.ProtocolAnthropic}, foreignInbound...) {
		if notes := responseLossyNotes(t, name, resp); len(notes) != 0 {
			t.Errorf("%s 无块误报：%v", name, notes)
		}
	}
}

// 非流式编码：外族线上不出现 file_id，正文保留。
func TestContainerUploadNotLeakedIntoResponse(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "chart ready"}, rUpload}}
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			ic, _ := codec.Inbound(name)
			body, err := ic.EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if strings.Contains(string(body), "file_secret1") || strings.Contains(string(body), "container_upload") {
				t.Errorf("非流式响应泄漏 file_id：%s", body)
			}
			if !strings.Contains(string(body), "chart ready") {
				t.Errorf("正文被误删：%s", body)
			}
		})
	}
}

// 请求侧诊断：历史里带 container_upload 时，外族出站报丢失，anthropic 静默，
// 缺席静默。file_id 不进注记。
func TestDiagnoseContainerUploadDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "run"}, rUpload}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		notes := codec.DescribeLossy(req, name, oc.Caps())
		got := strings.Join(notes, "; ")
		if !strings.Contains(got, "dropped 1 container upload block(s)") {
			t.Errorf("%s 应报 container_upload 丢失：%q", name, got)
		}
		assertNoUploadID(t, notes)
	}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if notes := codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
	// 缺席静默。
	req.Messages[0].Content = req.Messages[0].Content[:1]
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(req, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 无 container_upload 误报：%v", name, notes)
		}
	}
}

// 投给外族上游时整块跳过：既不硬报错也不泄漏 file_id，正文保留。
func TestContainerUploadSkippedInForeignRequest(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "run this"},
			rUpload,
		}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		t.Run(name, func(t *testing.T) {
			c, ok := codec.Outbound(name)
			if !ok {
				t.Fatalf("outbound %q not registered", name)
			}
			body, err := c.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if strings.Contains(string(body), "file_secret1") || strings.Contains(string(body), "container_upload") {
				t.Errorf("请求体泄漏 container_upload：%s", body)
			}
			if !strings.Contains(string(body), "run this") {
				t.Errorf("正文被误删：%s", body)
			}
		})
	}
}
