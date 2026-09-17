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
