package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次60：chat 流式 CacheCreationDetailsDropNote 在客户端 opt-out usage 帧时误报。
//
// 客户端显式 stream_options.include_usage:false 时，finish() 不发那一帧 usage
// （encode_stream.go 的 !suppressUsageFrame 守卫），usage 细分维度注记也早已按
// 同一门控静默（Notes() 里「usage 细分维度与 TTL 明细同口径门控」）。但缓存写入
// TTL 明细注记（droppedCacheDetails）此前在门控之外无条件报出，且其措辞宣称
// 「aggregate input token totals remain preserved」——那一帧根本没发、合计也没
// 交付，属误报（违反规则 a：注记当且仅当真实丢弃；照客户端要求执行不是丢它要的
// 东西，见 suppressUsageFrame 字段注释与 TestSuppressionIsNotLossy）。
//
// 可达性：chat 客户端 → anthropic 上游（CacheWriteDetailsKnown 来自 anthropic 的
// cache_creation 5m/1h TTL 明细），客户端带 include_usage:false。

const cacheTTLDropSub = "cache-creation TTL details"

// cacheNotes 用给定请求体驱动一条最短流，usage 帧带 CacheWriteDetailsKnown，
// 收回 Notes()。
func cacheNotes(t *testing.T, body string) []string {
	t.Helper()
	enc := inboundCodec{}.NewStreamEncoder(decode(t, body))
	usage := ir.Usage{InputTokens: 10, OutputTokens: 3, CacheWriteDetailsKnown: true}
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &usage},
		{Type: ir.EvMessageStop},
	} {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
	}
	enc.Finish()
	n, ok := enc.(codec.StreamNotes)
	if !ok {
		t.Fatal("编码器没实现 StreamNotes")
	}
	return n.Notes()
}

func hasCacheNote(notes []string) bool {
	for _, x := range notes {
		if strings.Contains(x, cacheTTLDropSub) {
			return true
		}
	}
	return false
}

// opt-out usage 帧 + 缓存 TTL 明细 → 不报（误报已修）。
func TestR60CacheDetailsSuppressedWithUsageFrame(t *testing.T) {
	notes := cacheNotes(t,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false},
		  "messages":[{"role":"user","content":"hi"}]}`)
	if hasCacheNote(notes) {
		t.Errorf("客户端 opt-out usage 帧仍报缓存 TTL 明细丢弃（误报）：%v", notes)
	}
}

// 未 opt-out（usage 帧照发）+ 缓存 TTL 明细 → 照报（回归守卫：门控没把该报的也吞掉）。
func TestR60CacheDetailsNotedWhenUsageDelivered(t *testing.T) {
	notes := cacheNotes(t,
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !hasCacheNote(notes) {
		t.Errorf("usage 帧照发时缓存 TTL 明细丢弃没报出：%v", notes)
	}
}

// 显式 include_usage:true（usage 帧照发）+ 缓存 TTL 明细 → 照报。
func TestR60CacheDetailsNotedWhenUsageExplicitlyRequested(t *testing.T) {
	notes := cacheNotes(t,
		`{"model":"m","stream":true,"stream_options":{"include_usage":true},
		  "messages":[{"role":"user","content":"hi"}]}`)
	if !hasCacheNote(notes) {
		t.Errorf("include_usage:true 时缓存 TTL 明细丢弃没报出：%v", notes)
	}
}
