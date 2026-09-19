package cache

import (
	"encoding/json"
	"testing"
)

// LiveEntry 的 JSON 键名是对外契约：前端与 /admin/live 的消费方按这些键读。
//
// 钉死字面量而不是对着结构体自身往返：往返测试里键名打错也是自洽的
// （写进去和读出来用的是同一个 tag），而线上效果是前端那一列永远为空。
// 探针实测把 dispatch_ms 改成 dispatch_msx 完全测不出来。
func TestLiveEntryWireKeys(t *testing.T) {
	raw, err := json.Marshal(LiveEntry{
		RequestID: "r", DispatchMS: 1, UpstreamMS: 2, LatencyMS: 3, FirstTokenMS: 4,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]float64{
		"dispatch_ms":    1,
		"upstream_ms":    2,
		"latency_ms":     3,
		"first_token_ms": 4,
	} {
		got, ok := m[key]
		if !ok {
			t.Errorf("缺少键 %q：%s", key, raw)
			continue
		}
		if got != want {
			t.Errorf("键 %q = %v，要 %v（键名对了但值串了）", key, got, want)
		}
	}
}
