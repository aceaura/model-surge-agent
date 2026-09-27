package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次20：response.metadata 同族往返保真。官方 responses 的 response 对象会回显
// 请求里的 metadata（客户端自定义关联键值），是「客户端发出去、期望原样收回」的
// 往返契约——与 created_at / completed_at / service_tier 同类。此前 wireResponse
// 未建模该字段，解码即被 json.Unmarshal 静默吞掉，同族 responses→responses 也丢，
// 破坏客户端按 metadata 做异步关联/幂等。修法镜像 ResponsesModeration 管道。

func TestResponseMetadataRoundTrip(t *testing.T) {
	body := []byte(`{"id":"r1","object":"response","model":"m","status":"completed",` +
		`"metadata":{"trace":"abc","shard":"7"},` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.ClientMetadata["trace"] != "abc" || resp.ClientMetadata["shard"] != "7" {
		t.Fatalf("上游 metadata 没落进 IR：%v", resp.ClientMetadata)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"trace":"abc"`) || !strings.Contains(s, `"shard":"7"`) {
		t.Errorf("出站 metadata 未原样回显：%s", s)
	}
}

// 上游没给 metadata（空）时 omitempty 不写：绝不凭空造一个空对象。
func TestResponseMetadataAbsentNotWritten(t *testing.T) {
	resp, err := DecodeResponse([]byte(`{"id":"r1","model":"m","status":"completed","output":[]}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ClientMetadata) != 0 {
		t.Fatalf("无 metadata 却落进 IR：%v", resp.ClientMetadata)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if strings.Contains(string(out), `"metadata"`) {
		t.Errorf("空 metadata 不该写出：%s", out)
	}
}

// 流式同族往返：metadata 随终止帧 response.completed 的 response 对象抵达，
// 经 EvMessageDelta 进聚合器，编码侧写回收尾帧的 response 对象。
func TestStreamMetadataRoundTrip(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed","metadata":{"trace":"abc"}}}`,
	)
	// 聚合后 IR 应带上 metadata。
	var agg ir.Aggregator
	for _, ev := range evs {
		agg.Add(ev)
	}
	if got := agg.Response().ClientMetadata["trace"]; got != "abc" {
		t.Fatalf("metadata 没随终止帧进 IR：%v", agg.Response().ClientMetadata)
	}
	// 编码侧原值回写。
	if s := encodeAll(t, evs...); !strings.Contains(s, `"trace":"abc"`) {
		t.Errorf("流式出站 metadata 丢失：\n%s", s)
	}
}

// 整份响应投影路径（上游忽略 stream:true）：ResponseEvents 把 ClientMetadata
// 随首帧投影，responses 流式编码器在 EvMessageStart 收下并写回收尾帧。
func TestReplayMetadataRoundTrip(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", StopReason: ir.StopEndTurn,
		ClientMetadata: map[string]string{"trace": "abc"}}
	evs := ir.ResponseEvents(resp)
	if s := encodeAll(t, evs...); !strings.Contains(s, `"trace":"abc"`) {
		t.Errorf("投影路径 metadata 丢失：\n%s", s)
	}
}
