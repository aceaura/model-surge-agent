package agentv1

import (
	"encoding/json"
	"strings"
	"testing"
)

// 管理面 DTO 的 JSON 键名是前端契约。钉死字面量：改 tag 在仓内是自洽的，
// 而线上效果是前端某一列永远为空。
func TestRequestSummaryWireKeys(t *testing.T) {
	assertKeys(t, RequestSummary{DispatchMS: 1, UpstreamMS: 2, LatencyMS: 3},
		map[string]float64{"dispatch_ms": 1, "upstream_ms": 2, "latency_ms": 3})
}

func TestLiveEntryWireKeys(t *testing.T) {
	assertKeys(t, LiveEntry{DispatchMS: 1, UpstreamMS: 2, LatencyMS: 3},
		map[string]float64{"dispatch_ms": 1, "upstream_ms": 2, "latency_ms": 3})
}

func TestPoolStatsWireKeys(t *testing.T) {
	assertKeys(t, PoolStats{Acquired: 1, Idle: 2, Total: 3, Max: 4, AcquireWaiting: 5},
		map[string]float64{
			"acquired": 1, "idle": 2, "total": 3, "max": 4, "acquire_waiting": 5,
		})
}

func assertKeys(t *testing.T, v any, want map[string]float64) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, w := range want {
		got, ok := m[key]
		if !ok {
			t.Errorf("缺少键 %q：%s", key, raw)
			continue
		}
		if got != w {
			t.Errorf("键 %q = %v，要 %v", key, got, w)
		}
	}
}

// 键名是前端与运维脚本读的契约，改一个字就断。断言必须落在字面量上，
// 否则测试自己也走同一套 tag，改名两边一起变、完全测不出来。
func TestCaptureWireKeys(t *testing.T) {
	raw, err := json.Marshal(CaptureList{
		Mode: "errors",
		Items: []CaptureSummary{{
			RequestID: "r1",
			Sizes:     map[string]int{"client_request": 7},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"mode"`, `"items"`, `"request_id"`, `"at"`, `"sizes"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("CaptureList 缺键 %s：%s", key, raw)
		}
	}

	raw, err = json.Marshal(CaptureDetail{RequestID: "r1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"request_id"`, `"at"`,
		`"client_request"`, `"upstream_request"`, `"upstream_response"`, `"client_response"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("CaptureDetail 缺键 %s：%s", key, raw)
		}
	}

	raw, err = json.Marshal(CaptureBody{Body: "x", Truncated: true, Dropped: 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"body"`, `"truncated"`, `"dropped"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("CaptureBody 缺键 %s：%s", key, raw)
		}
	}
	// body 必须是明文字符串而不是 base64：这个端点唯一的用途是人眼看
	// 哪一步坏了，base64 之后要先解一层才能看。
	if !strings.Contains(string(raw), `"body":"x"`) {
		t.Errorf("body 不是明文：%s", raw)
	}
}
