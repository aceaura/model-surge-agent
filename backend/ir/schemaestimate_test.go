package ir

import (
	"strings"
	"testing"
)

// 结构化输出的 schema 与工具的 schema 一样要进提示词，而它常有数千 token。
// 漏掉它让带结构化输出的请求被系统性低估，而据此写调度策略的人看不出
// 拿到的数是偏小的。
func TestResponseFormatSchemaCountsTowardTheEstimate(t *testing.T) {
	base := &Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "hi"}}}},
	}
	without := EstimateRequest(base)

	big := strings.Repeat(`{"type":"object","properties":{"a":{"type":"string"}}},`, 200)
	withSchema := base.Clone()
	withSchema.ResponseFormat = &ResponseFormat{
		Kind:   ResponseFormatSchema,
		Name:   "big_report",
		Schema: big,
	}
	got := EstimateRequest(withSchema)

	if got <= without {
		t.Fatalf("带 schema 的估算 = %d，没带的 = %d；schema 没有计入", got, without)
	}
	// 增量至少要是 schema 本身的量级，否则等于只象征性加了几个 token。
	wantDelta := EstimateTokens(big)
	if got-without < wantDelta {
		t.Errorf("增量 = %d，至少该有 schema 自身的 %d", got-without, wantDelta)
	}
}

// Name 也要计入：它和 Schema 一样出现在出站请求体里，
// 只算 Schema 会让「同一份 schema 换个长名字」的两个请求估出一样的数。
func TestResponseFormatNameCountsTowardTheEstimate(t *testing.T) {
	mk := func(name string) int64 {
		return EstimateRequest(&Request{
			Model:          "m",
			ResponseFormat: &ResponseFormat{Kind: ResponseFormatSchema, Name: name, Schema: `{"type":"object"}`},
		})
	}
	short, long := mk("r"), mk(strings.Repeat("very_long_schema_name_", 20))
	if long <= short {
		t.Errorf("长名字估算 = %d，短名字 = %d；Name 没有计入", long, short)
	}
}

// ToolChoice 只计名字。Mode 是 auto/any/none/tool 的枚举，出站编成一个
// 结构化字段而不是提示词文本；把枚举名也算进去等于凭空虚增。
func TestToolChoiceContributesOnlyItsName(t *testing.T) {
	bare := EstimateRequest(&Request{Model: "m"})

	modeOnly := EstimateRequest(&Request{
		Model:      "m",
		ToolChoice: &ToolChoice{Mode: ToolChoiceAuto},
	})
	if modeOnly != bare {
		t.Errorf("只有 Mode 时估算 = %d，想要与裸请求相同的 %d：枚举不进提示词", modeOnly, bare)
	}

	named := EstimateRequest(&Request{
		Model:      "m",
		ToolChoice: &ToolChoice{Mode: ToolChoiceTool, Name: "search_the_whole_index"},
	})
	want := bare + EstimateTokens("search_the_whole_index")
	if named != want {
		t.Errorf("指定工具名时估算 = %d，想要 %d（裸请求 + 工具名）", named, want)
	}
}

// 两个字段都为 nil 时估算不得变化——这是把新增项加进求和时最容易碰坏的地方：
// 对 nil 取字段会 panic，而给它们一个零值默认又会让所有请求凭空多出几 token。
func TestNilOptionalsLeaveTheEstimateUnchanged(t *testing.T) {
	r := &Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "hello world"}}}},
	}
	// 只有正文：模型名不进估算（它不是提示词的一部分）。
	want := EstimateTokens("hello world")
	if got := EstimateRequest(r); got != want {
		t.Errorf("估算 = %d，想要 %d（只有正文，无任何可选项贡献）", got, want)
	}
}
