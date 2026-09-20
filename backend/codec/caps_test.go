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
	dialect            schemadialect.Dialect
	cacheBreakpoints   int
	maxStopSequences   int
	maxTemperature     float64
	thinkingExcl       bool
	minThinkingBudget  int
	systemAsText       bool
	serverTools        bool
	toolResultError    bool
	toolResultTextOnly bool
	imageDetail        bool
}{
	codec.ProtocolAnthropic: {
		cacheBreakpoints: 4,
		// 上游 400 原文：temperature: range: 0..1。
		maxTemperature:    1.0,
		thinkingExcl:      true,
		minThinkingBudget: 1024,
		serverTools:       true,
		toolResultError:   true,
	},
	codec.ProtocolChatCompletions: {
		maxStopSequences: 4,
		// role:tool 消息不接受媒体 part。
		toolResultTextOnly: true,
		imageDetail:        true,
	},
	codec.ProtocolResponses: {
		systemAsText: true,
		// function_call_output.output 是单个字符串。
		toolResultTextOnly: true,
		imageDetail:        true,
	},
	codec.ProtocolGemini: {
		// 白名单而非黑名单：结构性关键字（$ref / $defs / oneOf / allOf /
		// prefixItems）漏一个就原样发出去拿一个 400，且 DroppedKeys 为空
		// 意味着连有损说明都报不出来。表内刻意不含 title 与四个长度/数量
		// 约束——它们原来就被剔除且上线未见问题，参考实现放行不构成改它
		// 的证据。
		dialect: schemadialect.Dialect{
			Allow: []string{
				"anyOf", "default", "description", "enum", "example", "format",
				"items", "maxProperties", "maximum", "minProperties", "minimum",
				"nullable", "pattern", "properties", "propertyOrdering",
				"required", "type",
			},
			StringEnumOnly:      true,
			UppercaseType:       true,
			CollapseUnionType:   true,
			OmitEmptyProperties: true,
		},
		systemAsText:       true,
		toolResultError:    true,
		toolResultTextOnly: true,
		// 官方限定至多 5 个 stopSequences，超出即 INVALID_ARGUMENT。
		maxStopSequences: 5,
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
			// 取值范围维度同受这张表管辖：没有实测或官方确证的上限必须留
			// 零值（零值=跳过检查），猜出来的上限会把本来能过的请求改坏。
			if got.MaxTemperature != want.maxTemperature {
				t.Errorf("MaxTemperature = %g，想要 %g", got.MaxTemperature, want.maxTemperature)
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
			if got.ToolResultError != want.toolResultError {
				t.Errorf("ToolResultError = %v，想要 %v", got.ToolResultError, want.toolResultError)
			}
			// 工具结果的媒体承载力与 Images 是两件事：三家协议都能在普通
			// 消息里带图，只有工具结果这一处装不下。
			if got.ToolResultTextOnly != want.toolResultTextOnly {
				t.Errorf("ToolResultTextOnly = %v，想要 %v", got.ToolResultTextOnly, want.toolResultTextOnly)
			}
			if got.ImageDetail != want.imageDetail {
				t.Errorf("ImageDetail = %v，想要 %v", got.ImageDetail, want.imageDetail)
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
