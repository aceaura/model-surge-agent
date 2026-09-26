package chatcompletions

import (
	"strings"
	"testing"
)

// 废弃的 functions 声明数组：畸形条目与无名条目此前被静默吞掉（json.Unmarshal
// 失败或 name 为空即跳过），症状是「模型声称没有这个工具」，而客户端从响应里
// 看不出是自己声明被丢还是模型不愿调。现代 tools 路径与 responses 兄弟都会留
// 一条 DecodeNote，废弃形态不该是悄悄吞掉声明的理由。

func TestDecodeRequestNotesMalformedLegacyFunction(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"functions":[123]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if !hasDecodeNoteSubstr(req.DecodeNotes, "malformed legacy function declaration") {
		t.Errorf("畸形废弃 function 声明没有说明: %v", req.DecodeNotes)
	}
}

func TestDecodeRequestNotesNamelessLegacyFunction(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"functions":[{"description":"d"}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if !hasDecodeNoteSubstr(req.DecodeNotes, "legacy function declaration with no name") {
		t.Errorf("无名废弃 function 声明没有说明: %v", req.DecodeNotes)
	}
}

func TestDecodeRequestKeepsValidLegacyFunction(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"functions":[{"name":"f","description":"d","parameters":{"type":"object"}}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "f" {
		t.Fatalf("合法废弃 function 声明蒸发: %+v", req.Tools)
	}
	if hasDecodeNoteSubstr(req.DecodeNotes, "legacy function declaration") {
		t.Errorf("合法废弃 function 声明被误报: %v", req.DecodeNotes)
	}
}

func hasDecodeNoteSubstr(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
