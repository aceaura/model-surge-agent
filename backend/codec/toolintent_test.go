package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这个文件守的是「客户端声明的工具意图不被整形层静默削弱」：
// 改名要同步到 tool_choice、推理预算与 max_tokens 冲突要就地解开、
// 服务端工具不得被伪装成函数工具、被丢掉的工具形态要出说明。
//
// 四条的共同点是故障不可见：上游都会正常回一段文本，客户端看不出
// 自己的声明被改过。

// --- 需求1：改名同步到 tool_choice ---

func TestToolRenameSyncsToolChoice(t *testing.T) {
	req := &ir.Request{
		Model:     "m",
		MaxTokens: 8192,
		Tools:     []ir.Tool{{Name: "my.tool!", Schema: `{"type":"object"}`}},
		// 客户端要求「必须调这件工具」，指的是它改名前的名字。
		ToolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "my.tool!"},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
	notes := ir.Sanitize(req)
	if len(req.Tools) != 1 {
		t.Fatalf("工具数 = %d，想要 1", len(req.Tools))
	}
	newName := req.Tools[0].Name
	if newName == "my.tool!" {
		t.Fatal("非法字符的名字应当被改写")
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ToolChoiceTool {
		t.Fatalf("tool_choice 不应降级，got %+v", req.ToolChoice)
	}
	if req.ToolChoice.Name != newName {
		t.Errorf("tool_choice.name = %q，想要跟随改名后的 %q", req.ToolChoice.Name, newName)
	}
	if !hasNoteWith(notes, "rewrote tool name") {
		t.Errorf("改名应当出说明，got %v", notes)
	}

	// 同步到位的判据不只是名字相等，还要求随后的出站整形不再降级它：
	// 只对齐名字而整形仍报「未声明的工具」说明同步发生在错误的时点。
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			_, lossy := lossyOf(t, out, req)
			if hasNoteWith(lossy, "downgraded to auto") {
				t.Errorf("%s: 改名已同步，不应再降级 tool_choice，got %v", out, lossy)
			}
		})
	}
}

// TestToolChoiceForGenuinelyUndeclaredStillDowngrades 守的是上一条的反面：
// 同步不能变成「凡是指不着就随便挑一个工具对上」。
func TestToolChoiceForGenuinelyUndeclaredStillDowngrades(t *testing.T) {
	req := &ir.Request{
		Model:      "m",
		MaxTokens:  8192,
		Tools:      []ir.Tool{{Name: "alpha", Schema: `{"type":"object"}`}},
		ToolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "never_declared"},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
	ir.Sanitize(req)
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			shaped := req.Clone()
			notes := codec.ShapeRequest(shaped, out, capsOf(t, out))
			if !hasNoteWith(notes, "downgraded to auto") {
				t.Errorf("%s: 指向未声明工具必须降级并出说明，got %v", out, notes)
			}
			if shaped.ToolChoice == nil || shaped.ToolChoice.Mode != ir.ToolChoiceAuto {
				t.Errorf("%s: tool_choice = %+v，想要 auto", out, shaped.ToolChoice)
			}
		})
	}
}

// TestToolRenameSyncsToolUseHistory 守的是改名的另一处同步点：
// 历史里的 tool_use 名字也必须跟着改，否则上游看到一个未声明的调用。
func TestToolRenameSyncsToolUseHistory(t *testing.T) {
	req := &ir.Request{
		Model:     "m",
		MaxTokens: 8192,
		Tools:     []ir.Tool{{Name: "my.tool!", Schema: `{"type":"object"}`}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{
				Type:    ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "t1", Name: "my.tool!", Input: `{}`},
			}}},
			{Role: ir.RoleUser, Content: []ir.Block{{
				Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{
					{Type: ir.BlockText, Text: "ok"},
				}},
			}}},
		},
	}
	ir.Sanitize(req)
	want := req.Tools[0].Name
	use := req.Messages[1].Content[0].ToolUse
	if use == nil {
		t.Fatal("tool_use 块被丢掉了")
	}
	got := use.Name
	if got != want {
		t.Errorf("历史里的 tool_use 名字 = %q，想要 %q", got, want)
	}
}

// --- 需求2：预算与 max_tokens 冲突 ---

func TestThinkingBudgetClampedBelowMaxTokens(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		budget    int
		// wantBudget 为 0 表示「不应改动，仍是原值」。
		wantBudget int
		wantClamp  bool
		wantOff    bool
	}{
		// 预算低于上限是合法形状，不能碰：碰了就是无端降低推理质量。
		{name: "budget-below-max", maxTokens: 8192, budget: 4096, wantBudget: 4096},
		// 相等意味着留给回答本身的 token 为零，上游回不可重试的 400。
		{name: "budget-equals-max", maxTokens: 8192, budget: 8192, wantBudget: 8191, wantClamp: true},
		{name: "budget-above-max", maxTokens: 8192, budget: 9000, wantBudget: 8191, wantClamp: true},
		// max_tokens 缺席时按协议的默认值比较：anthropic 的 max_tokens 必填，
		// 编码器随后会补 DefaultMaxTokens（4096），只看客户端给的那个数会让
		// 这条路径整个逃过夹紧，出站成 max_tokens=4096 / budget=9000，
		// 拿到上游 400 budget_tokens must be less than max_tokens。
		{name: "no-max-tokens", maxTokens: 0, budget: 9000, wantBudget: 4095, wantClamp: true},
		// 夹紧后低于协议下限时应落到关掉推理那一支，而不是发一个必被拒的预算。
		{name: "clamped-below-floor", maxTokens: 512, budget: 4096, wantOff: true, wantClamp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &ir.Request{
				Model:     "m",
				MaxTokens: tc.maxTokens,
				Thinking: &ir.ThinkingConfig{
					Enabled:      ir.ThinkingOn(),
					BudgetTokens: tc.budget,
				},
				Messages: []ir.Message{
					{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
				},
			}
			// 用 anthropic 的能力位：它是唯一同时有预算下限与必填 max_tokens
			// 的协议，也是这条约束真实来自的那一家。
			notes := codec.ShapeRequest(req, codec.ProtocolAnthropic,
				capsOf(t, codec.ProtocolAnthropic))
			if tc.wantOff {
				// 既有的无解分支是整条丢掉 Thinking 并出说明，
				// 这里只断言落到了那一支，不改它的处置方式。
				if req.Thinking != nil {
					t.Fatalf("预算夹到下限之下应落到关掉推理那一支，got %+v", req.Thinking)
				}
				if !hasNoteWith(notes, "minimum reasoning budget") {
					t.Errorf("关掉推理应出说明，got %v", notes)
				}
				return
			}
			if req.Thinking == nil {
				t.Fatal("Thinking 不应被清空")
			}
			if req.Thinking.BudgetTokens != tc.wantBudget {
				t.Errorf("budget = %d，想要 %d", req.Thinking.BudgetTokens, tc.wantBudget)
			}
			if tc.wantClamp && !hasNoteWith(notes, "thinking.budget_tokens") {
				t.Errorf("夹紧应当出说明，got %v", notes)
			}
			if !tc.wantClamp && hasNoteWith(notes, "thinking.budget_tokens") {
				t.Errorf("未冲突不应出说明，got %v", notes)
			}
		})
	}
}

// TestClampDoesNotRaiseMaxTokens 守的是夹紧方向：抬高 max_tokens 也能
// 解开冲突，但那是替客户端花钱，还会让「我只要这么多 token」回出更长的内容。
func TestClampDoesNotRaiseMaxTokens(t *testing.T) {
	req := &ir.Request{
		Model:     "m",
		MaxTokens: 4096,
		Thinking:  &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 9000},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
	codec.ShapeRequest(req, codec.ProtocolAnthropic, capsOf(t, codec.ProtocolAnthropic))
	if req.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d，客户端给的 4096 不得被抬高", req.MaxTokens)
	}
}

// --- 需求3：服务端工具 ---

// serverToolRequest 是一个声明了服务端工具与普通函数工具的请求。
// 服务端工具带着原文槽位（anthropic 入站的形态）：外族出站整条剔除时，
// 原文一并不得泄漏——矩阵里「承载不了的协议 body 不含 web_search」的断言
// 同时钉住这一点。
func serverToolRequest() *ir.Request {
	return &ir.Request{
		Model:     "m",
		MaxTokens: 8192,
		Tools: []ir.Tool{
			{Name: "web_search", ServerType: "web_search_20250305",
				ServerRaw: json.RawMessage(`{"type":"web_search_20250305","name":"web_search","max_uses":9}`)},
			{Name: "alpha", Schema: `{"type":"object"}`},
		},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
}

func TestServerToolsMatrix(t *testing.T) {
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			caps := capsOf(t, out)
			body, notes := lossyOf(t, out, serverToolRequest())
			if caps.ServerTools {
				// 承载得了就原样带上 type，且不得给它塞参数形状。
				if !strings.Contains(string(body), `"web_search_20250305"`) {
					t.Errorf("%s: 服务端工具的 type 应原样出站\n%s", out, body)
				}
				if hasNoteWith(notes, "server-side tool") {
					t.Errorf("%s: 承载得了不应报丢弃，got %v", out, notes)
				}
				return
			}
			if strings.Contains(string(body), "web_search") {
				t.Errorf("%s: 承载不了的服务端工具不得出站\n%s", out, body)
			}
			if !hasNoteWith(notes, "server-side tool") {
				t.Errorf("%s: 丢弃服务端工具必须出说明，got %v", out, notes)
			}
			// 同一请求里的函数工具不受牵连。
			if !strings.Contains(string(body), "alpha") {
				t.Errorf("%s: 函数工具应照常出站\n%s", out, body)
			}
		})
	}
}

// TestServerToolNotDisguisedAsFunction 守的是「丢弃而非降级」：
// 降级成函数工具后上游会等一个永不到来的结果，对话停住且不报错。
func TestServerToolNotDisguisedAsFunction(t *testing.T) {
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if caps.ServerTools {
			continue
		}
		t.Run(out, func(t *testing.T) {
			req := &ir.Request{
				Model:     "m",
				MaxTokens: 8192,
				Tools:     []ir.Tool{{Name: "web_search", ServerType: "web_search_20250305"}},
				Messages: []ir.Message{
					{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
				},
			}
			body, _ := lossyOf(t, out, req)
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(body, &probe); err != nil {
				t.Fatalf("%s: 请求体不是 JSON 对象: %v", out, err)
			}
			if _, ok := probe["tools"]; ok {
				t.Errorf("%s: 仅有服务端工具时不应写出 tools 字段\n%s", out, body)
			}
		})
	}
}

// TestServerToolDropTakesToolChoiceWithIt 守的是丢弃与 tool_choice 校正的
// 先后：指向被丢工具的 tool_choice 必须被降级，否则出站带一个指不着的名字。
func TestServerToolDropTakesToolChoiceWithIt(t *testing.T) {
	for _, out := range outboundNames() {
		if capsOf(t, out).ServerTools {
			continue
		}
		t.Run(out, func(t *testing.T) {
			req := serverToolRequest()
			req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "web_search"}
			shaped := req.Clone()
			notes := codec.ShapeRequest(shaped, out, capsOf(t, out))
			if shaped.ToolChoice == nil || shaped.ToolChoice.Mode != ir.ToolChoiceAuto {
				t.Errorf("%s: tool_choice = %+v，想要降级成 auto", out, shaped.ToolChoice)
			}
			if !hasNoteWith(notes, "downgraded to auto") {
				t.Errorf("%s: 降级必须出说明，got %v", out, notes)
			}
		})
	}
}

// TestServerToolSchemaNotNormalized 守的是整形层跳过服务端工具：
// 走一遍 schema 归一会给它补上一个空对象 schema 发出去，而参数形状
// 归上游那一版工具自己定义。
func TestServerToolSchemaNotNormalized(t *testing.T) {
	req := &ir.Request{
		Model:     "m",
		MaxTokens: 8192,
		Tools:     []ir.Tool{{Name: "web_search", ServerType: "web_search_20250305"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
	codec.ShapeRequest(req, codec.ProtocolAnthropic, capsOf(t, codec.ProtocolAnthropic))
	if len(req.Tools) != 1 {
		t.Fatalf("工具数 = %d，想要 1", len(req.Tools))
	}
	if req.Tools[0].Schema != "" {
		t.Errorf("服务端工具的 schema = %q，想要留空", req.Tools[0].Schema)
	}
}

// TestAnthropicRoundTripsServerToolType 守的是 IR 载得住服务端工具：
// 入站解出 ServerType、出站原样写回。
func TestAnthropicRoundTripsServerToolType(t *testing.T) {
	body := `{"model":"m","max_tokens":64,"tools":[` +
		`{"type":"web_search_20250305","name":"web_search"},` +
		`{"type":"custom","name":"alpha","input_schema":{"type":"object"}},` +
		`{"name":"beta","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	req := decodeReq(t, codec.ProtocolAnthropic, body)
	if len(req.Tools) != 3 {
		t.Fatalf("工具数 = %d，想要 3", len(req.Tools))
	}
	if req.Tools[0].ServerType != "web_search_20250305" {
		t.Errorf("ServerType = %q，想要 web_search_20250305", req.Tools[0].ServerType)
	}
	// custom 与省略都是函数工具的写法，不得被记成服务端工具。
	if req.Tools[1].ServerType != "" {
		t.Errorf("custom 被记成服务端工具 %q", req.Tools[1].ServerType)
	}
	if req.Tools[2].ServerType != "" {
		t.Errorf("省略 type 被记成服务端工具 %q", req.Tools[2].ServerType)
	}

	out, _ := lossyOf(t, codec.ProtocolAnthropic, req)
	var probe struct {
		Tools []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("出站体解析失败: %v", err)
	}
	if len(probe.Tools) != 3 {
		t.Fatalf("出站工具数 = %d，想要 3", len(probe.Tools))
	}
	if probe.Tools[0].Type != "web_search_20250305" {
		t.Errorf("出站 type = %q，想要原样写回", probe.Tools[0].Type)
	}
	if len(probe.Tools[0].InputSchema) != 0 {
		t.Errorf("服务端工具不应带 input_schema，got %s", probe.Tools[0].InputSchema)
	}
	// 函数工具那两件不得被带上 type。
	for _, i := range []int{1, 2} {
		if probe.Tools[i].Type != "" {
			t.Errorf("函数工具 %q 被写出 type %q", probe.Tools[i].Name, probe.Tools[i].Type)
		}
	}
}

// TestServerToolWireKeys 钉住线上键名字面量：改名会静默改变协议形状。
func TestServerToolWireKeys(t *testing.T) {
	raw, err := json.Marshal(ir.Tool{Name: "n", ServerType: "web_search_20250305"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"server_type":"web_search_20250305"`) {
		t.Errorf("ir.Tool 的服务端工具键名应为 server_type，got %s", raw)
	}
	// 零值必须缺席：函数工具占绝大多数，多一个空字段会让每个请求的
	// IR 快照都变长，也会让「未触发即字节不变」不再成立。
	plain, err := json.Marshal(ir.Tool{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "server_type") {
		t.Errorf("函数工具不应出现 server_type，got %s", plain)
	}
}

// --- 需求4：被跳过的工具形态出说明 ---

func TestSkippedToolProducesNote(t *testing.T) {
	cases := []struct {
		proto string
		body  string
		// wantNote 是应出现在说明里的片段。
		wantNote string
	}{
		{
			proto: codec.ProtocolResponses,
			body: `{"model":"m","input":"hi","tools":[` +
				`{"type":"web_search","name":"ws"},{"type":"function","name":"alpha"}]}`,
			wantNote: `skipped tool "ws": unsupported type "web_search"`,
		},
		{
			proto: codec.ProtocolChatCompletions,
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` +
				`{"type":"custom","function":{"name":"cs"}},{"type":"function","function":{"name":"alpha"}}]}`,
			wantNote: `skipped tool "cs": unsupported type "custom"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.proto, func(t *testing.T) {
			req := decodeReq(t, tc.proto, tc.body)
			notes := ir.Sanitize(req)
			if !hasNoteWith(notes, tc.wantNote) {
				t.Errorf("说明里缺 %q，got %v", tc.wantNote, notes)
			}
			// 说明走 sanitized 通道后字段应清空，避免同一条被上报两次。
			if len(req.DecodeNotes) != 0 {
				t.Errorf("DecodeNotes 应被取走，仍有 %v", req.DecodeNotes)
			}
			// 可承载的那件照常留下。
			if len(req.Tools) != 1 || req.Tools[0].Name != "alpha" {
				t.Errorf("工具集合 = %+v，想要仅剩 alpha", req.Tools)
			}
		})
	}
}

// TestSupportedToolTypesProduceNoNote 守的是说明不泛滥：
// function 与省略 type 都是正常形状，出说明会让这个字段再也指不出真问题。
func TestSupportedToolTypesProduceNoNote(t *testing.T) {
	cases := map[string]string{
		codec.ProtocolResponses: `{"model":"m","input":"hi","tools":[{"type":"function","name":"alpha"}]}`,
		codec.ProtocolChatCompletions: `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"function":{"name":"alpha"}},{"type":"function","function":{"name":"beta"}}]}`,
	}
	for proto, body := range cases {
		t.Run(proto, func(t *testing.T) {
			req := decodeReq(t, proto, body)
			if len(req.DecodeNotes) != 0 {
				t.Errorf("正常形状不应产生说明，got %v", req.DecodeNotes)
			}
			if notes := ir.Sanitize(req); len(notes) != 0 {
				t.Errorf("正常形状 Sanitize 应返回 nil，got %v", notes)
			}
		})
	}
}

// TestDecodeNotesSurviveEmptyMessages 守的是 Sanitize 的早返回：
// 「只声明了工具、还没说话」的请求同样要交代被跳过的声明。
func TestDecodeNotesSurviveEmptyMessages(t *testing.T) {
	req := &ir.Request{Model: "m", DecodeNotes: []string{`skipped tool "ws": unsupported type "web_search"`}}
	notes := ir.Sanitize(req)
	if !hasNoteWith(notes, "skipped tool") {
		t.Errorf("空消息列表也应交出解码说明，got %v", notes)
	}
	if len(req.DecodeNotes) != 0 {
		t.Errorf("DecodeNotes 应被取走，仍有 %v", req.DecodeNotes)
	}
}

// TestDecodeNotesClonedWithRequest 守的是 Clone 的完整性：
// 管线里多处按克隆传递请求，漏拷这个字段等于在某条路径上丢掉说明。
func TestDecodeNotesClonedWithRequest(t *testing.T) {
	req := &ir.Request{Model: "m", DecodeNotes: []string{"note-a"}}
	got := req.Clone().DecodeNotes
	if len(got) != 1 || got[0] != "note-a" {
		t.Errorf("Clone 后 DecodeNotes = %v，想要 [note-a]", got)
	}
}

// --- 未触发即字节不变 ---

// TestToolIntentUntriggeredPathsLeaveBytesUnchanged 守的是四条约束都不在
// 常态请求上生效：任一条误触发都会破坏上游的 prompt cache 前缀。
func TestToolIntentUntriggeredPathsLeaveBytesUnchanged(t *testing.T) {
	build := func() *ir.Request {
		return &ir.Request{
			Model:     "m",
			MaxTokens: 8192,
			Tools:     []ir.Tool{{Name: "alpha", Schema: `{"type":"object"}`}},
			// 名字合法、tool_choice 指得着、预算低于上限、无服务端工具：
			// 四条约束一条都不该动这个请求。
			ToolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "alpha"},
			Thinking:   &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 2048},
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			},
		}
	}
	req := build()
	if notes := ir.Sanitize(req); len(notes) != 0 {
		t.Errorf("常态请求不应产生 sanitized 说明，got %v", notes)
	}
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			before, _ := lossyOf(t, out, build())
			after, notes := lossyOf(t, out, req)
			if string(before) != string(after) {
				t.Errorf("%s: Sanitize 改动了常态请求\n before %s\n after  %s", out, before, after)
			}
			for _, n := range notes {
				if strings.Contains(n, "server-side tool") ||
					strings.Contains(n, "thinking.budget_tokens") ||
					strings.Contains(n, "downgraded to auto") ||
					strings.Contains(n, "skipped tool") {
					t.Errorf("%s: 常态请求触发了本轮约束：%s", out, n)
				}
			}
		})
	}
}

func hasNoteWith(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
