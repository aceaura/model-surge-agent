package ir

import "testing"

func TestEstimateTokensBounds(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
	// 短于一个 token 的文本仍占位，否则大量短消息会被整体估成 0。
	if got := EstimateTokens("a"); got != 1 {
		t.Errorf("single char = %d, want at least 1", got)
	}
	if got := EstimateTokens("12345678"); got != 2 {
		t.Errorf("8 chars = %d, want 2", got)
	}
}

// CJK 按字符单独加权，不按「每 4 字符 1 token」。
//
// 三家实测的 CJK 系数是 0.68–1.21（new-api service/token_estimator.go:36-46），
// 每 4 字符 1 token 的 0.25 低了近一个量级。
func TestEstimateWeighsCJKPerCharacter(t *testing.T) {
	const runes = 8
	got := EstimateTokens("中文中文中文中文")
	if got <= runes/4 {
		t.Errorf("%d 个 CJK 字符按调度方向 = %d，还是每 4 字符 1 token 的量级",
			runes, got)
	}
	if got > runes {
		t.Errorf("调度方向 = %d > 字符数 %d，这个方向应当低估", got, runes)
	}
}

func TestEstimateRequestCoversAllTextSources(t *testing.T) {
	r := &Request{
		System: []Block{{Type: BlockText, Text: "0123"}},
		Messages: []Message{{
			Role: RoleUser,
			Content: []Block{
				{Type: BlockText, Text: "0123"},
				{Type: BlockThinking, Thinking: &Thinking{Text: "0123"}},
				{Type: BlockToolUse, ToolUse: &ToolUse{Name: "read", Input: "0123"}},
				{Type: BlockToolResult, ToolResult: &ToolResult{
					Content: []Block{{Type: BlockText, Text: "0123"}},
				}},
			},
		}},
		Tools: []Tool{{Name: "read", Description: "0123", Schema: "0123"}},
	}
	// system 1 + text 1 + thinking 1 + tool name 1 + tool input 1 +
	// nested result 1 + tool decl (name 1 + desc 1 + schema 1) = 9
	if got := EstimateRequest(r); got != 9 {
		t.Errorf("estimate = %d, want 9", got)
	}
}

// 媒体块按块数计固定下限，不按 base64 长度，也不算 0。
//
// base64 长度与 token 消耗无关（不能按它算），但算 0 会让带图请求的估算
// 偏低一个量级——一张图在任何厂商都至少几百 token。
func TestEstimateCountsMediaAsFixedFloor(t *testing.T) {
	one := &Request{Messages: []Message{{
		Role:    RoleUser,
		Content: []Block{{Type: BlockImage, Media: &Media{Data: string(make([]byte, 4096))}}},
	}}}
	got := EstimateRequest(one)
	if got == 0 {
		t.Fatal("只含一张图的请求估算为 0，带图请求会被严重低估")
	}

	// 同一张图换成 8 倍长的 base64：结果必须不变，否则算的是编码长度。
	long := &Request{Messages: []Message{{
		Role:    RoleUser,
		Content: []Block{{Type: BlockImage, Media: &Media{Data: string(make([]byte, 32768))}}},
	}}}
	if bigger := EstimateRequest(long); bigger != got {
		t.Errorf("base64 变长后估算从 %d 变成 %d，说明算的是编码长度而非块数",
			got, bigger)
	}

	// 两张图是一张的两倍。
	two := &Request{Messages: []Message{{
		Role: RoleUser,
		Content: []Block{
			{Type: BlockImage, Media: &Media{Data: "a"}},
			{Type: BlockImage, Media: &Media{Data: "b"}},
		},
	}}}
	if want := got * 2; EstimateRequest(two) != want {
		t.Errorf("两块媒体 = %d, want %d", EstimateRequest(two), want)
	}
}

func TestEstimateNilInputs(t *testing.T) {
	if got := EstimateRequest(nil); got != 0 {
		t.Errorf("nil request = %d", got)
	}
	if got := EstimateResponse(nil); got != 0 {
		t.Errorf("nil response = %d", got)
	}
}

func TestEstimateResponse(t *testing.T) {
	got := EstimateResponse(&Response{Content: []Block{{Type: BlockText, Text: "01234567"}}})
	if got != 2 {
		t.Errorf("estimate = %d, want 2", got)
	}
}

// 两个方向的定义性关系：公开方向不得低于调度方向。
//
// 这条是本轮的核心不变量。两个方向服务相反的用途——调度筛候选与配额累计
// 宁可低估，客户端裁上下文宁可高估——写成同一份权重就等于放弃其中一个。
func TestPublicModeNeverBelowDispatchMode(t *testing.T) {
	for _, s := range []string{
		"hello world",
		"你好世界",
		"混合 mixed 内容 content 123",
		"日本語のテキスト",
		"한국어 텍스트",
		"a",
		"中",
	} {
		pub := EstimateTokensMode(s, ModePublic)
		dis := EstimateTokensMode(s, ModeDispatch)
		if pub < dis {
			t.Errorf("%q: 公开 %d < 调度 %d，客户端据公开方向裁上下文会踩上限",
				s, pub, dis)
		}
	}
}

// 公开方向对 CJK 必须达到「每字符约 1 token」的量级。
//
// 中文真实比值约 1.7 字符/token 偏高估一侧的说法来自各家实测：三家系数
// 0.68/0.85/1.21，取上界才能保证「宁多勿少」。
func TestPublicModeCoversCJKAtOneTokenPerRune(t *testing.T) {
	const text = "你好世界你好世界" // 8 runes
	got := EstimateTokensMode(text, ModePublic)
	if got < 8 {
		t.Errorf("8 个 CJK 字符按公开方向 = %d，低于每字符 1 token，"+
			"中文客户端据此裁上下文仍会被上游以超长拒掉", got)
	}
}

// ASCII 英文两个方向都该在「每 4 字符约 1 token」附近，不因 CJK 上调而虚高。
func TestASCIIStaysAtFourCharsPerToken(t *testing.T) {
	const text = "0123456789abcdef" // 16 ASCII runes
	for _, mode := range []EstimateMode{ModeDispatch, ModePublic} {
		got := EstimateTokensMode(text, mode)
		if got < 3 || got > 6 {
			t.Errorf("mode=%d: 16 个 ASCII 字符 = %d，偏离每 4 字符 1 token 的量级",
				mode, got)
		}
	}
}

// 空串两个方向都是 0；非空至少 1。
func TestEmptyAndFloorInBothModes(t *testing.T) {
	for _, mode := range []EstimateMode{ModeDispatch, ModePublic} {
		if got := EstimateTokensMode("", mode); got != 0 {
			t.Errorf("mode=%d: 空串 = %d, want 0", mode, got)
		}
		if got := EstimateTokensMode("a", mode); got < 1 {
			t.Errorf("mode=%d: 单字符 = %d，非空必须至少 1", mode, got)
		}
	}
}

// 两个方向算的是同一批块，只在字符权重上不同。
//
// 「算哪些块」若也分方向，公开方向漏算一类块就会破掉上界保证，
// 而那种漏算在两处代码里各写一遍时很难看出来。
func TestBothModesCountTheSameBlocks(t *testing.T) {
	r := &Request{
		System: []Block{{Type: BlockText, Text: "system"}},
		Messages: []Message{{
			Role: RoleUser,
			Content: []Block{
				{Type: BlockText, Text: "text"},
				{Type: BlockImage, Media: &Media{Data: "img"}},
				{Type: BlockThinking, Thinking: &Thinking{Text: "think"}},
				{Type: BlockToolUse, ToolUse: &ToolUse{Name: "read", Input: "{}"}},
				{Type: BlockToolResult, ToolResult: &ToolResult{
					Content: []Block{{Type: BlockText, Text: "result"}},
				}},
			},
		}},
		Tools: []Tool{{Name: "read", Description: "desc", Schema: "{}"}},
	}
	// 媒体块是两个方向都计入的，所以两者都必须超过媒体下限。
	dis := EstimateRequestMode(r, ModeDispatch)
	pub := EstimateRequestMode(r, ModePublic)
	if dis <= mediaTokens || pub <= mediaTokens {
		t.Errorf("调度 %d / 公开 %d，至少有一个方向没把文本块算进去（媒体下限 %d）",
			dis, pub, mediaTokens)
	}
	if pub < dis {
		t.Errorf("公开 %d < 调度 %d", pub, dis)
	}
}

// EstimateRequest / EstimateTokens 的无参版本保持调度方向。
//
// est_tokens 与用量兜底都走这两个函数，它们改成公开方向会让配额累计凭空虚高。
func TestBareHelpersStayOnDispatchMode(t *testing.T) {
	const text = "你好世界"
	if EstimateTokens(text) != EstimateTokensMode(text, ModeDispatch) {
		t.Error("EstimateTokens 不再等于调度方向")
	}
	r := &Request{Messages: []Message{{Role: RoleUser, Content: []Block{
		{Type: BlockText, Text: text},
	}}}}
	if EstimateRequest(r) != EstimateRequestMode(r, ModeDispatch) {
		t.Error("EstimateRequest 不再等于调度方向")
	}
}
