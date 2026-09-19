package agentv1

import (
	"encoding/json"
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
