package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #24：safety_identifier 与 user_id 同一维度（滥用检测标识），本协议只有
// metadata.user_id 一个槽位：槽空着时映进去，被 user_id 占了则 user_id
// 优先（丢弃由 codec 层诊断专测）。

func fixtureReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 16,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
}

func TestSafetyIdentifierMappedIntoMetadataUserID(t *testing.T) {
	req := fixtureReq()
	req.SafetyIdentifier = "si-1"
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"metadata":{"user_id":"si-1"}`) {
		t.Errorf("safety_identifier 没映进 metadata.user_id：%s", out)
	}
}

func TestUserIDWinsOverSafetyIdentifier(t *testing.T) {
	req := fixtureReq()
	req.Metadata = map[string]string{"user_id": "u1"}
	req.SafetyIdentifier = "si-1"
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"metadata":{"user_id":"u1"}`) {
		t.Errorf("user_id 没优先：%s", out)
	}
	if strings.Contains(string(out), "si-1") {
		t.Errorf("被挤掉的 safety_identifier 不应出现在请求体：%s", out)
	}
}

func TestSafetyIdentifierAbsentNoMetadata(t *testing.T) {
	out, err := EncodeRequest(fixtureReq())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "metadata") {
		t.Errorf("两维都缺省却造出 metadata：%s", out)
	}
}
