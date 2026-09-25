package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// metadata 是官方文档维度（16 对键值，随响应回显），此前 wireRequest 连
// 槽位都没有：客户端的关联数据入站即蒸发，同族往返也回不来，且没有任何
// 注记可循。现在全键解进 ClientMetadata、同族回写原样发。

func TestMetadataDecodeAllKeys(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}],` +
		`"metadata":{"trace":"abc","user_id":"meta-u"},"user":"top-u"}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.ClientMetadata["trace"] != "abc" || req.ClientMetadata["user_id"] != "meta-u" {
		t.Errorf("metadata 没全键收下：%v", req.ClientMetadata)
	}
	// user 与 metadata.user_id 同维度，同给时顶层 user 胜出：Metadata 只
	// 承载被翻译成各协议用户标识字段的那一份。
	if req.Metadata["user_id"] != "top-u" {
		t.Errorf("Metadata = %v，想要 user_id=top-u", req.Metadata)
	}
}

func TestMetadataAbsentStaysAbsent(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.ClientMetadata != nil || req.Metadata != nil {
		t.Errorf("缺席被发明：ClientMetadata=%v Metadata=%v", req.ClientMetadata, req.Metadata)
	}
}

func TestMetadataEchoEncode(t *testing.T) {
	req := &ir.Request{
		Model: "gpt",
		Messages: []ir.Message{{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		ClientMetadata: map[string]string{"trace": "abc"},
		Metadata:       map[string]string{"user_id": "u1"},
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"metadata":{"trace":"abc"}`) {
		t.Errorf("回写丢了 metadata：%s", s)
	}
	if !strings.Contains(s, `"user":"u1"`) {
		t.Errorf("回写丢了 user：%s", s)
	}
}

// modalities 的官方值集只有 text/audio：客户端递来别的值写出去是上游
// 必 400 的形状，编码器滤掉，丢弃由 DescribeLossy 报出（codec 层测）。
func TestModalitiesFilteredToChatValueSet(t *testing.T) {
	req := &ir.Request{
		Model:      "gpt",
		Modalities: []string{"text", "image", "audio"},
		Messages: []ir.Message{{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"modalities":["text","audio"]`) {
		t.Errorf("值集没滤：%s", out)
	}
}
