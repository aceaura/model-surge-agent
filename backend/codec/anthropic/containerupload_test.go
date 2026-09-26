package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// R34：anthropic container_upload 内容块全链路贯通。块形
// {type:"container_upload", file_id}：请求侧把已上传文件送进代码执行容器的
// 输入目录，响应侧是模型运行代码后产出的文件引用。
//
// 独立块型而非并入 BlockMedia：它只有 file_id 没有内容本体，语义是「进容器
// 输入目录」，当成普通附件投递会被外族按内容解码而 400。同族往返必须逐字节
// 保真，外族整块跳过并计损耗（见 codec/containeruploadloss_test.go）。

// 请求侧解码：user 消息里的 container_upload（客户端把文件送进容器）。
func TestContainerUploadDecodeBlock(t *testing.T) {
	b, ok, err := decodeBlock(wireBlock{Type: blockContainerUpload, FileID: "file_abc123"}, nil)
	if err != nil || !ok {
		t.Fatalf("decodeBlock: ok=%v err=%v", ok, err)
	}
	if b.Type != ir.BlockContainerUpload || b.ContainerUpload == nil ||
		b.ContainerUpload.FileID != "file_abc123" {
		t.Fatalf("container_upload 块 = %#v", b)
	}
}

// 同族编码回吐：只有 file_id 一个载荷，其余槽位全空。
func TestContainerUploadEncodeBlock(t *testing.T) {
	out, ok, err := encodeBlock(ir.Block{
		Type:            ir.BlockContainerUpload,
		ContainerUpload: &ir.ContainerUploadRef{FileID: "file_abc123"},
	})
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	if out.Type != blockContainerUpload || out.FileID != "file_abc123" {
		t.Fatalf("wire = %#v", out)
	}
	raw, _ := json.Marshal(out)
	if string(raw) != `{"type":"container_upload","file_id":"file_abc123"}` {
		t.Errorf("wire json = %s", raw)
	}

	// ContainerUpload 为 nil 时 FileID 留空，不伪造引用。
	out, _, err = encodeBlock(ir.Block{Type: ir.BlockContainerUpload})
	if err != nil {
		t.Fatalf("encodeBlock(nil): %v", err)
	}
	if out.Type != blockContainerUpload || out.FileID != "" {
		t.Errorf("nil 载荷 wire = %#v", out)
	}
}

// 请求往返：user 消息里的 container_upload 同族不漂移。
func TestContainerUploadRequestRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[
		{"type":"text","text":"run this"},
		{"type":"container_upload","file_id":"file_abc123"}]}]}`)
	r, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := r.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d", len(blocks))
	}
	if b := blocks[1]; b.Type != ir.BlockContainerUpload ||
		b.ContainerUpload == nil || b.ContainerUpload.FileID != "file_abc123" {
		t.Fatalf("container_upload 块 = %#v", b)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `{"type":"container_upload","file_id":"file_abc123"}`) {
		t.Errorf("container_upload 块回写错：%s", out)
	}
	back, err := DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	if b := back.Messages[0].Content[1]; b.Type != ir.BlockContainerUpload ||
		b.ContainerUpload.FileID != "file_abc123" {
		t.Errorf("往返漂移：%#v", b)
	}
}

// 多轮历史：assistant 消息里的 container_upload（模型产出的文件引用）也要保真。
func TestContainerUploadAssistantHistory(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"make a chart"},
		{"role":"assistant","content":[
			{"type":"text","text":"done"},
			{"type":"container_upload","file_id":"file_out1"}]},
		{"role":"user","content":"thanks"}]}`)
	r, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if b := r.Messages[1].Content[1]; b.Type != ir.BlockContainerUpload ||
		b.ContainerUpload.FileID != "file_out1" {
		t.Fatalf("assistant 历史块 = %#v", b)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"file_id":"file_out1"`) {
		t.Errorf("历史块回写丢失：%s", out)
	}
}

// ---- 响应侧 ----

func TestContainerUploadResponseRoundTrip(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"chart ready"},
		{"type":"container_upload","file_id":"file_chart"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if b := resp.Content[1]; b.Type != ir.BlockContainerUpload ||
		b.ContainerUpload == nil || b.ContainerUpload.FileID != "file_chart" {
		t.Fatalf("响应块 = %#v", b)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `{"type":"container_upload","file_id":"file_chart"}`) {
		t.Errorf("响应回写错：%s", out)
	}
}

// 流式：content_block_start 的 content_block 直接带全形（该块无增量），
// 聚合器原样收下。
func TestContainerUploadStreamDecode(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`); err != nil {
		t.Fatalf("Feed start: %v", err)
	}
	evs, err := d.Feed("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"container_upload","file_id":"file_s1"}}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("Feed block_start: %v %d", err, len(evs))
	}
	ev := evs[0]
	if ev.Type != ir.EvBlockStart || ev.Block == nil ||
		ev.Block.Type != ir.BlockContainerUpload || ev.Block.ContainerUpload.FileID != "file_s1" {
		t.Fatalf("block_start 事件 = %#v", ev)
	}
	if _, err := d.Feed("content_block_stop", `{"type":"content_block_stop","index":0}`); err != nil {
		t.Fatalf("Feed block_stop: %v", err)
	}
	// 聚合收得到块。
	var a ir.Aggregator
	a.Add(ir.Event{Type: ir.EvMessageStart, MessageID: "m"})
	a.Add(ev)
	a.Add(ir.Event{Type: ir.EvBlockStop, Index: 0})
	a.Add(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got := a.Response()
	if len(got.Content) != 1 || got.Content[0].Type != ir.BlockContainerUpload ||
		got.Content[0].ContainerUpload.FileID != "file_s1" {
		t.Errorf("聚合丢失：%#v", got.Content)
	}
}

func TestContainerUploadStreamEncode(t *testing.T) {
	e := newStreamEncoder()
	if _, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"}); err != nil {
		t.Fatalf("Encode start: %v", err)
	}
	frames, err := e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockContainerUpload, ContainerUpload: &ir.ContainerUploadRef{FileID: "file_s1"},
	}})
	if err != nil || len(frames) != 1 {
		t.Fatalf("Encode block_start: %v %d", err, len(frames))
	}
	if !strings.Contains(string(frames[0]), `"content_block":{"type":"container_upload","file_id":"file_s1"}`) {
		t.Errorf("block_start 帧：%s", frames[0])
	}
	frames, err = e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
	if err != nil || len(frames) != 1 {
		t.Fatalf("Encode block_stop: %v %d", err, len(frames))
	}
	if !strings.Contains(string(frames[0]), `"type":"content_block_stop"`) {
		t.Errorf("block_stop 帧：%s", frames[0])
	}
	// anthropic 自家编码器不为同族块报损耗。
	if notes := e.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
}
