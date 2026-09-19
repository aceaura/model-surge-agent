package codec_test

import (
	"reflect"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/codec/schemadialect"
)

// structuralCaps 是各出站协议的结构层能力位的期望值。
//
// 写成显式表而非「非零即已填」：三家协议的正确取值本来就是零值，
// 零值无法区分「填过且为零」与「忘了填」。有了表，新增协议不进表即失败，
// 改了取值不同步表也失败——这正是我们要的那种失败。
var structuralCaps = map[string]struct {
	dialect           schemadialect.Dialect
	cacheBreakpoints  int
	maxStopSequences  int
	thinkingExcl      bool
	minThinkingBudget int
	systemAsText      bool
	serverTools       bool
}{
	codec.ProtocolAnthropic: {
		cacheBreakpoints:  4,
		thinkingExcl:      true,
		minThinkingBudget: 1024,
		serverTools:       true,
	},
	codec.ProtocolChatCompletions: {
		maxStopSequences: 4,
	},
	codec.ProtocolResponses: {
		systemAsText: true,
	},
	codec.ProtocolGemini: {
		dialect: schemadialect.Dialect{
			Drop: []string{
				"$schema", "$id", "additionalProperties", "patternProperties",
				"minLength", "maxLength", "minItems", "maxItems",
				"exclusiveMinimum", "exclusiveMaximum", "deprecated", "title",
			},
			UppercaseType:       true,
			CollapseUnionType:   true,
			OmitEmptyProperties: true,
		},
		systemAsText: true,
	},
}

func TestEveryOutboundDeclaresStructuralCaps(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			want, ok := structuralCaps[name]
			if !ok {
				t.Fatalf("出站协议 %q 未在 structuralCaps 表中声明结构层能力位", name)
			}
			oc, ok := codec.Outbound(name)
			if !ok {
				t.Fatalf("outbound %q not registered", name)
			}
			got := oc.Caps()
			if !reflect.DeepEqual(got.SchemaDialect, want.dialect) {
				t.Errorf("SchemaDialect 不符\n want %+v\n got  %+v", want.dialect, got.SchemaDialect)
			}
			if got.CacheBreakpoints != want.cacheBreakpoints {
				t.Errorf("CacheBreakpoints = %d，想要 %d", got.CacheBreakpoints, want.cacheBreakpoints)
			}
			if got.MaxStopSequences != want.maxStopSequences {
				t.Errorf("MaxStopSequences = %d，想要 %d", got.MaxStopSequences, want.maxStopSequences)
			}
			if got.ThinkingExcludesSampling != want.thinkingExcl {
				t.Errorf("ThinkingExcludesSampling = %v，想要 %v", got.ThinkingExcludesSampling, want.thinkingExcl)
			}
			if got.MinThinkingBudget != want.minThinkingBudget {
				t.Errorf("MinThinkingBudget = %d，想要 %d", got.MinThinkingBudget, want.minThinkingBudget)
			}
			if got.SystemAsText != want.systemAsText {
				t.Errorf("SystemAsText = %v，想要 %v", got.SystemAsText, want.systemAsText)
			}
			if got.ServerTools != want.serverTools {
				t.Errorf("ServerTools = %v，想要 %v", got.ServerTools, want.serverTools)
			}
		})
	}
}

// TestCacheBreakpointsAgreeWithCacheControl 断言两个缓存能力位不矛盾：
// 声明支持断点却给出 0 上限（或反之）会让 budgetCache 与 DescribeLossy
// 对同一请求给出相反的判断。
func TestCacheBreakpointsAgreeWithCacheControl(t *testing.T) {
	for _, name := range outboundNames() {
		oc, _ := codec.Outbound(name)
		caps := oc.Caps()
		if caps.CacheControl && caps.CacheBreakpoints == 0 {
			t.Errorf("%s: 声明支持 cache_control 却给出 0 断点上限", name)
		}
		if !caps.CacheControl && caps.CacheBreakpoints != 0 {
			t.Errorf("%s: 不支持 cache_control 却给出断点上限 %d", name, caps.CacheBreakpoints)
		}
	}
}
