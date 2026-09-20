package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这个文件守两件事，共性是「整形阶段必须按协议的**有效**取值判断，
// 而不是客户端给的那个数」——协议默认值是在整形之后由编码器补的，
// 只看客户端给的数会让整条路径逃过约束，把不可重试的 400 留给上游。

// TestBudgetClampedAgainstProtocolDefaultMaxTokens 是 G3 的核心：客户端不给
// max_tokens 时，anthropic 随后会补 DefaultMaxTokens，预算必须按它夹紧。
func TestBudgetClampedAgainstProtocolDefaultMaxTokens(t *testing.T) {
	checked := 0
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if !caps.Thinking || !caps.RequiresMaxTokens || caps.DefaultMaxTokens <= 0 {
			continue
		}
		t.Run(out, func(t *testing.T) {
			checked++
			req := shapeRequest()
			req.MaxTokens = 0 // 客户端没给
			req.Thinking = &ir.ThinkingConfig{
				Enabled:      ir.ThinkingOn(),
				BudgetTokens: caps.DefaultMaxTokens * 10,
			}
			notes := codec.ShapeRequest(req, out, caps)
			if req.Thinking == nil {
				t.Fatalf("预算远高于默认上限不该整个关掉推理，notes=%v", notes)
			}
			if req.Thinking.BudgetTokens >= caps.DefaultMaxTokens {
				t.Errorf("budget = %d，必须低于协议默认上限 %d，否则上游回 400 "+
					"budget_tokens must be less than max_tokens",
					req.Thinking.BudgetTokens, caps.DefaultMaxTokens)
			}
			if !hasNoteWith(notes, "thinking.budget_tokens") {
				t.Errorf("按默认上限夹紧也要出说明：%v", notes)
			}
		})
	}
	if checked == 0 {
		t.Skip("没有协议同时具备推理、必填 max_tokens 与默认值")
	}
}

// TestBudgetClampReflectedInOutboundBody 端到端钉一遍：整形与编码两步合起来
// 不能产出 budget >= max_tokens 的请求体。单看整形结果会漏掉「编码器补的默认
// 值与整形用的数不是同一个」这类错位。
func TestBudgetClampedInOutboundBody(t *testing.T) {
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if !caps.Thinking || !caps.RequiresMaxTokens || caps.DefaultMaxTokens <= 0 {
			continue
		}
		t.Run(out, func(t *testing.T) {
			req := shapeRequest()
			req.MaxTokens = 0
			req.Thinking = &ir.ThinkingConfig{
				Enabled:      ir.ThinkingOn(),
				BudgetTokens: caps.DefaultMaxTokens * 10,
			}
			body, _ := lossyOf(t, out, req)
			var obj map[string]any
			if err := json.Unmarshal(body, &obj); err != nil {
				t.Fatalf("请求体不是合法 JSON：%v", err)
			}
			maxTok, ok := obj["max_tokens"].(float64)
			if !ok {
				t.Fatalf("必填 max_tokens 未写出：%s", body)
			}
			th, ok := obj["thinking"].(map[string]any)
			if !ok {
				t.Fatalf("thinking 未写出：%s", body)
			}
			budget, ok := th["budget_tokens"].(float64)
			if !ok {
				t.Fatalf("budget_tokens 未写出：%s", body)
			}
			if budget >= maxTok {
				t.Errorf("出站请求体 budget_tokens=%g >= max_tokens=%g，"+
					"这是不可重试的 400 且归因会指向上游", budget, maxTok)
			}
		})
	}
}

// TestClientMaxTokensStillWins 钉住客户端给了数字时仍以它为准：
// 按默认值判断会在「客户端要短回答」时用一个更大的上限，漏掉真实冲突。
func TestClientMaxTokensStillWins(t *testing.T) {
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if !caps.Thinking || caps.MinThinkingBudget <= 0 {
			continue
		}
		t.Run(out, func(t *testing.T) {
			req := shapeRequest()
			req.MaxTokens = caps.MinThinkingBudget * 4
			req.Thinking = &ir.ThinkingConfig{
				Enabled:      ir.ThinkingOn(),
				BudgetTokens: req.MaxTokens + 1000,
			}
			notes := codec.ShapeRequest(req, out, caps)
			if req.Thinking == nil {
				t.Fatalf("不该关掉推理：%v", notes)
			}
			if want := caps.MinThinkingBudget*4 - 1; req.Thinking.BudgetTokens != want {
				t.Errorf("budget = %d，想要按客户端的 max_tokens 夹到 %d",
					req.Thinking.BudgetTokens, want)
			}
		})
	}
}

// --- G4：stop 序列上限 ---

// TestStopSequencesTruncatedPerProtocol 钉住每个声明了上限的协议都截断。
// gemini 此前只声明布尔支持，超限直落上游 INVALID_ARGUMENT——同一请求
// 换上游即成功，用户看到「只有 Gemini 坏了」。
func TestStopSequencesTruncatedPerProtocol(t *testing.T) {
	declared := 0
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if !caps.StopSequences {
			continue
		}
		t.Run(out, func(t *testing.T) {
			if caps.MaxStopSequences <= 0 {
				// 留零值必须是有理由的：四协议里只有 anthropic 没有公开上限。
				if out != codec.ProtocolAnthropic {
					t.Errorf("%s 支持 stop 序列却未声明上限，超限会直落上游", out)
				}
				return
			}
			declared++
			req := shapeRequest()
			for i := 0; i < caps.MaxStopSequences+3; i++ {
				req.StopSequences = append(req.StopSequences, string(rune('A'+i)))
			}
			notes := codec.ShapeRequest(req, out, caps)
			if len(req.StopSequences) != caps.MaxStopSequences {
				t.Errorf("stop 序列 = %d 条，想要截到 %d 条",
					len(req.StopSequences), caps.MaxStopSequences)
			}
			if !hasNoteWith(notes, "stop_sequences") {
				t.Errorf("截断必须出说明：%v", notes)
			}
			// 截的是尾部：前面的序列是客户端最先声明的，保留它们更可能
			// 命中它真正关心的那几个。
			if req.StopSequences[0] != "A" {
				t.Errorf("截断改了顺序：%v", req.StopSequences)
			}
		})
	}
	if declared == 0 {
		t.Fatal("没有协议声明 stop 序列上限，截断逻辑未被覆盖")
	}
}

// TestStopSequencesAtLimitNotTruncated 钉住恰好等于上限时不动手也不报说明。
func TestStopSequencesAtLimitNotTruncated(t *testing.T) {
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if caps.MaxStopSequences <= 0 {
			continue
		}
		req := shapeRequest()
		for i := 0; i < caps.MaxStopSequences; i++ {
			req.StopSequences = append(req.StopSequences, string(rune('A'+i)))
		}
		notes := codec.ShapeRequest(req, out, caps)
		if len(req.StopSequences) != caps.MaxStopSequences {
			t.Errorf("%s 恰好等于上限却被改：%v", out, req.StopSequences)
		}
		if strings.Contains(strings.Join(notes, "|"), "stop_sequences") {
			t.Errorf("%s 恰好等于上限却报了说明：%v", out, notes)
		}
	}
}
