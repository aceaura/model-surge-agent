package ir

import "testing"

// 后到的输入输出覆盖先到的：同一次流可能从两个位置给出 usage，
// 后到的那份口径更完整（如减去缓存量后的修正值）。
func TestMergeUsageLetsLaterInputWin(t *testing.T) {
	got := Usage{InputTokens: 120, OutputTokens: 45}
	MergeUsage(&got, Usage{InputTokens: 90, OutputTokens: 40})
	if got.InputTokens != 90 || got.OutputTokens != 40 {
		t.Fatalf("usage = %+v, the later frame must win", got)
	}
}

// 零值不覆盖：多数帧只带部分字段，覆盖会把已知值抹成 0。
func TestMergeUsageIgnoresZeroes(t *testing.T) {
	got := Usage{InputTokens: 90, OutputTokens: 45}
	MergeUsage(&got, Usage{})
	if got.InputTokens != 90 || got.OutputTokens != 45 {
		t.Fatalf("usage = %+v, a frame without numbers must change nothing", got)
	}
}

// 推理维度与输出同口径：后到的非零值覆盖，零值不覆盖。
func TestMergeUsageTracksReasoningTokens(t *testing.T) {
	got := Usage{OutputTokens: 45, ReasoningTokens: 12}
	MergeUsage(&got, Usage{OutputTokens: 60, ReasoningTokens: 30})
	if got.ReasoningTokens != 30 {
		t.Errorf("reasoning = %d, the later frame must win", got.ReasoningTokens)
	}
	MergeUsage(&got, Usage{OutputTokens: 60})
	if got.ReasoningTokens != 30 {
		t.Errorf("reasoning = %d, a frame without the field must not erase it", got.ReasoningTokens)
	}
}

// 推理大于输出时保留上游原值：部分上游把两者作为独立计量而非包含关系，
// 钳制会把上游的真实数字改掉。
func TestMergeUsageDoesNotClampReasoningToOutput(t *testing.T) {
	got := Usage{}
	MergeUsage(&got, Usage{OutputTokens: 10, ReasoningTokens: 99})
	if got.ReasoningTokens != 99 {
		t.Errorf("reasoning = %d, want the upstream value untouched", got.ReasoningTokens)
	}
}

// 缓存字段取较大值：它在流中通常只出现一次，取 max 能容忍缺帧。
func TestMergeUsageKeepsLargestCacheCounts(t *testing.T) {
	got := Usage{CacheReadTokens: 30, CacheWriteTokens: 12}
	MergeUsage(&got, Usage{CacheReadTokens: 0, CacheWriteTokens: 20})
	if got.CacheReadTokens != 30 {
		t.Errorf("cache_read = %d, a later zero must not erase it", got.CacheReadTokens)
	}
	if got.CacheWriteTokens != 20 {
		t.Errorf("cache_write = %d, want the larger value", got.CacheWriteTokens)
	}
}

// TTL 明细整组随「已知」标记走：带明细的帧到达时两位一起覆盖（含清零），
// 不带明细的帧既不清零已知明细，也不把标记伪造成已知。
//
// 覆盖而非取 max：明细两档是同一个对象的两半，各取 max 会把两帧的
// 明细拼成一个上游从没说过的组合。
func TestMergeUsageCacheWriteDetailsMoveAsAGroup(t *testing.T) {
	got := Usage{CacheWriteTokens: 30, CacheWrite5mTokens: 20,
		CacheWrite1hTokens: 10, CacheWriteDetailsKnown: true}
	MergeUsage(&got, Usage{OutputTokens: 5})
	if got.CacheWrite5mTokens != 20 || got.CacheWrite1hTokens != 10 || !got.CacheWriteDetailsKnown {
		t.Errorf("a frame without details changed the known group: %+v", got)
	}
	MergeUsage(&got, Usage{CacheWriteTokens: 25, CacheWrite5mTokens: 25, CacheWriteDetailsKnown: true})
	if got.CacheWrite5mTokens != 25 || got.CacheWrite1hTokens != 0 {
		t.Errorf("the later known frame must replace both halves, zero included: %+v", got)
	}
	var fresh Usage
	MergeUsage(&fresh, Usage{CacheWriteTokens: 7})
	if fresh.CacheWriteDetailsKnown || fresh.CacheWrite5mTokens != 0 || fresh.CacheWrite1hTokens != 0 {
		t.Errorf("a frame without details must not fake the known flag: %+v", fresh)
	}
}
