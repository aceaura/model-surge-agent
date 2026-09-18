package ir

import "testing"

func sampleRequest() *Request {
	temp := 0.6
	topK := 40
	return &Request{
		Model:  "k3",
		System: []Block{{Type: BlockText, Text: "be brief"}},
		Messages: []Message{{
			Role: RoleUser,
			Content: []Block{
				{Type: BlockText, Text: "hi"},
				{Type: BlockImage, Media: &Media{MediaType: "image/png", Data: "AAAA"}},
				{Type: BlockAudio, Media: &Media{MediaType: "audio/wav", Data: "BBBB"}},
				{Type: BlockDocument, Media: &Media{MediaType: "application/pdf", Data: "CCCC", Name: "spec.pdf"}},
				{Type: BlockFile, Media: &Media{MediaType: "text/csv", URL: "https://example.com/a.csv"}},
				{Type: BlockToolResult, ToolResult: &ToolResult{
					ToolUseID: "tu_1",
					Content:   []Block{{Type: BlockText, Text: "result"}},
				}},
			},
		}},
		Tools:         []Tool{{Name: "read", Schema: `{"type":"object"}`}},
		ToolChoice:    &ToolChoice{Mode: ToolChoiceTool, Name: "read"},
		Temperature:   &temp,
		TopK:          &topK,
		StopSequences: []string{"END"},
		Thinking:      &ThinkingConfig{Enabled: ThinkingOn(), BudgetTokens: 8192},
		Metadata:      map[string]string{"user_id": "u1"},
	}
}

// 换目标重试要重编码同一份请求，克隆共享底层内存会让上一次尝试的
// native model 或参数改动串到下一次。
func TestCloneIsDeep(t *testing.T) {
	src := sampleRequest()
	got := src.Clone()

	got.Model = "other"
	got.Messages[0].Content[0].Text = "mutated"
	for _, i := range []int{1, 2, 3, 4} {
		got.Messages[0].Content[i].Media.MediaType = "mutated"
		got.Messages[0].Content[i].Media.Data = "mutated"
		got.Messages[0].Content[i].Media.URL = "mutated"
		got.Messages[0].Content[i].Media.Name = "mutated"
	}
	got.Messages[0].Content[5].ToolResult.Content[0].Text = "mutated"
	got.System[0].Text = "mutated"
	got.Tools[0].Name = "mutated"
	got.StopSequences[0] = "mutated"
	*got.Temperature = 1.0
	*got.TopK = 1
	got.ToolChoice.Name = "mutated"
	got.Thinking.BudgetTokens = 1
	got.Metadata["user_id"] = "mutated"

	if src.Model != "k3" {
		t.Error("model leaked")
	}
	if src.Messages[0].Content[0].Text != "hi" {
		t.Error("message block leaked")
	}
	if src.Messages[0].Content[5].ToolResult.Content[0].Text != "result" {
		t.Error("nested tool result content leaked")
	}
	wantMedia := []Media{
		{MediaType: "image/png", Data: "AAAA"},
		{MediaType: "audio/wav", Data: "BBBB"},
		{MediaType: "application/pdf", Data: "CCCC", Name: "spec.pdf"},
		{MediaType: "text/csv", URL: "https://example.com/a.csv"},
	}
	for i, want := range wantMedia {
		if got := *src.Messages[0].Content[i+1].Media; got != want {
			t.Errorf("media block[%d] leaked: got %+v, want %+v", i+1, got, want)
		}
	}
	if src.System[0].Text != "be brief" {
		t.Error("system block leaked")
	}
	if src.Tools[0].Name != "read" {
		t.Error("tools leaked")
	}
	if src.StopSequences[0] != "END" {
		t.Error("stop sequences leaked")
	}
	if *src.Temperature != 0.6 || *src.TopK != 40 {
		t.Error("numeric pointers leaked")
	}
	if src.ToolChoice.Name != "read" {
		t.Error("tool choice leaked")
	}
	if src.Thinking.BudgetTokens != 8192 {
		t.Error("thinking config leaked")
	}
	if src.Metadata["user_id"] != "u1" {
		t.Error("metadata leaked")
	}
}

func TestCloneNilIsNil(t *testing.T) {
	var r *Request
	if r.Clone() != nil {
		t.Error("cloning nil should stay nil")
	}
}

func TestCloneKeepsNilSlicesNil(t *testing.T) {
	got := (&Request{Model: "k3"}).Clone()
	if got.Messages != nil || got.System != nil || got.Tools != nil || got.StopSequences != nil {
		t.Errorf("clone invented slices: %+v", got)
	}
	if got.ToolChoice != nil || got.Temperature != nil || got.Thinking != nil || got.Metadata != nil {
		t.Errorf("clone invented pointers: %+v", got)
	}
}

// TestThinkingTriStateIsExhaustive 把三态逐格钉住，包括「结构在但 Enabled
// 未表态」这一格——它不会由入站解码产生，但 IR 是导出类型，别处构造得出来，
// 而 On()/Off() 任何一侧把 nil 当成表态都会让出站写出客户端没要求的标记。
func TestThinkingTriStateIsExhaustive(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *ThinkingConfig
		wantOn  bool
		wantOff bool
	}{
		{"nil config", nil, false, false},
		{"enabled unset", &ThinkingConfig{BudgetTokens: 1024}, false, false},
		{"explicit on", &ThinkingConfig{Enabled: ThinkingOn()}, true, false},
		{"explicit off", &ThinkingConfig{Enabled: ThinkingOff()}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.On(); got != c.wantOn {
				t.Errorf("On() = %v, want %v", got, c.wantOn)
			}
			if got := c.cfg.Off(); got != c.wantOff {
				t.Errorf("Off() = %v, want %v", got, c.wantOff)
			}
		})
	}
}
