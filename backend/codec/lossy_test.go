package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// fullCaps 是「什么都表达得了」的能力位，各用例只关掉自己要验的那一位，
// 以免顺带触发别的判定把断言污染成偶然通过。
func fullCaps() Capabilities {
	return Capabilities{
		Thinking:      true,
		ThinkingSig:   true,
		Tools:         true,
		Images:        true,
		CacheControl:  true,
		TopK:          true,
		StopSequences: true,
		MediaTypes:    []string{"image/png", "application/pdf"},
	}
}

func msg(blocks ...ir.Block) []ir.Message {
	return []ir.Message{{Role: ir.RoleUser, Content: blocks}}
}

func TestDescribeLossyJudgments(t *testing.T) {
	topK := 3
	cases := []struct {
		name string
		req  ir.Request
		caps func(Capabilities) Capabilities
		want []string // 每项是应出现在某条说明里的子串
	}{
		{
			name: "tools dropped when protocol has no tool calling",
			req:  ir.Request{Tools: []ir.Tool{{Name: "f"}}},
			caps: func(c Capabilities) Capabilities { c.Tools = false; return c },
			want: []string{"dropped tools"},
		},
		{
			name: "top_k dropped",
			req:  ir.Request{TopK: &topK},
			caps: func(c Capabilities) Capabilities { c.TopK = false; return c },
			want: []string{"dropped top_k"},
		},
		{
			name: "stop_sequences dropped",
			req:  ir.Request{StopSequences: []string{"END"}},
			caps: func(c Capabilities) Capabilities { c.StopSequences = false; return c },
			want: []string{"dropped stop_sequences"},
		},
		{
			name: "thinking config dropped",
			req:  ir.Request{Thinking: &ir.ThinkingConfig{Enabled: true}},
			caps: func(c Capabilities) Capabilities { c.Thinking = false; return c },
			want: []string{"dropped thinking"},
		},
		{
			name: "cache_control dropped",
			req:  ir.Request{Messages: msg(ir.Block{Type: ir.BlockText, Text: "hi", CacheCtl: "ephemeral"})},
			caps: func(c Capabilities) Capabilities { c.CacheControl = false; return c },
			want: []string{"dropped cache_control"},
		},
		{
			name: "thinking blocks dropped when no reasoning",
			req: ir.Request{Messages: msg(ir.Block{
				Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "why"}})},
			caps: func(c Capabilities) Capabilities { c.Thinking = false; return c },
			want: []string{"dropped thinking blocks"},
		},
		{
			name: "signature dropped when protocol cannot sign",
			req: ir.Request{Messages: msg(ir.Block{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Text: "why", Signature: "sig", SignatureFrom: "self"}})},
			caps: func(c Capabilities) Capabilities { c.ThinkingSig = false; return c },
			want: []string{"dropped thinking signature", "no signed reasoning"},
		},
		{
			name: "signature dropped across families",
			req: ir.Request{Messages: msg(ir.Block{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Text: "why", Signature: "sig", SignatureFrom: "other"}})},
			caps: func(c Capabilities) Capabilities { return c },
			want: []string{"dropped thinking signature", "own protocol family"},
		},
		{
			name: "foreign ciphertext dropped despite same-family claim",
			req: ir.Request{Messages: msg(ir.Block{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Text: "why", Signature: "gAAAAABxyz", SignatureFrom: "self"}})},
			caps: func(c Capabilities) Capabilities { return c },
			want: []string{"another vendor's ciphertext"},
		},
		{
			name: "redacted thinking reported even when protocol supports reasoning",
			req: ir.Request{Messages: msg(ir.Block{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Redacted: true, SignatureFrom: "self"}})},
			caps: func(c Capabilities) Capabilities { return c },
			want: []string{"dropped redacted_thinking", "cannot be re-encoded"},
		},
		{
			name: "media blocks dropped when no media input",
			req: ir.Request{Messages: msg(ir.Block{Type: ir.BlockImage,
				Media: &ir.Media{MediaType: "image/png", Data: "x"}})},
			caps: func(c Capabilities) Capabilities { c.Images = false; return c },
			want: []string{"dropped image blocks", "no media input"},
		},
		{
			name: "media downgraded when type not whitelisted",
			req: ir.Request{Messages: msg(ir.Block{Type: ir.BlockAudio,
				Media: &ir.Media{MediaType: "audio/wav", Data: "x"}})},
			caps: func(c Capabilities) Capabilities { return c },
			want: []string{"dropped audio blocks", "downgraded to text"},
		},
		{
			name: "tool blocks dropped when no tool calling",
			req: ir.Request{Messages: msg(ir.Block{Type: ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "t1", Name: "f", Input: "{}"}})},
			caps: func(c Capabilities) Capabilities { c.Tools = false; return c },
			want: []string{"dropped tool blocks"},
		},
		{
			name: "nested tool_result content is inspected",
			req: ir.Request{Messages: msg(ir.Block{Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{{
					Type: ir.BlockImage, Media: &ir.Media{MediaType: "audio/wav", Data: "x"}}}}})},
			caps: func(c Capabilities) Capabilities { return c },
			want: []string{"downgraded to text"},
		},
		{
			name: "system blocks are inspected too",
			req:  ir.Request{System: []ir.Block{{Type: ir.BlockText, Text: "sys", CacheCtl: "ephemeral"}}},
			caps: func(c Capabilities) Capabilities { c.CacheControl = false; return c },
			want: []string{"dropped cache_control"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DescribeLossy(&tc.req, "self", tc.caps(fullCaps()))
			if len(got) == 0 {
				t.Fatalf("expected lossy notes, got none")
			}
			joined := strings.Join(got, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in:\n%s", want, joined)
				}
			}
		})
	}
}

func TestDescribeLossyReturnsNilWhenNothingDropped(t *testing.T) {
	topK := 3
	req := ir.Request{
		Model:         "m",
		Tools:         []ir.Tool{{Name: "f"}},
		TopK:          &topK,
		StopSequences: []string{"END"},
		Thinking:      &ir.ThinkingConfig{Enabled: true},
		System:        []ir.Block{{Type: ir.BlockText, Text: "sys", CacheCtl: "ephemeral"}},
		Messages: msg(
			ir.Block{Type: ir.BlockText, Text: "hi", CacheCtl: "ephemeral"},
			ir.Block{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "x"}},
			ir.Block{Type: ir.BlockDocument, Media: &ir.Media{MediaType: "application/pdf", Data: "x"}},
			ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "why", Signature: "sig", SignatureFrom: "self"}},
			ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f", Input: "{}"}},
		),
	}
	if got := DescribeLossy(&req, "self", fullCaps()); got != nil {
		t.Fatalf("expected nil for a fully expressible request, got %v", got)
	}
}

func TestDescribeLossyDedupesPerField(t *testing.T) {
	// 同一字段在三条消息上被丢弃只报一条，否则长会话会刷出成百条重复说明。
	block := ir.Block{Type: ir.BlockText, Text: "hi", CacheCtl: "ephemeral"}
	req := ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{block, block}},
		{Role: ir.RoleAssistant, Content: []ir.Block{block}},
	}}
	caps := fullCaps()
	caps.CacheControl = false

	got := DescribeLossy(&req, "self", caps)
	if len(got) != 1 {
		t.Fatalf("expected one deduped note, got %v", got)
	}
}

func TestDescribeLossyNilRequest(t *testing.T) {
	if got := DescribeLossy(nil, "self", fullCaps()); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestForeignSignature(t *testing.T) {
	cases := []struct {
		name string
		th   *ir.Thinking
		want bool
	}{
		{"nil is not foreign", nil, false},
		{"empty signature is not foreign", &ir.Thinking{SignatureFrom: "other"}, false},
		{"same family passes", &ir.Thinking{Signature: "sig", SignatureFrom: "self"}, false},
		{"other family is foreign", &ir.Thinking{Signature: "sig", SignatureFrom: "other"}, true},
		{"gAAAA overrides same-family claim",
			&ir.Thinking{Signature: "gAAAAABxyz", SignatureFrom: "self"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForeignSignature(tc.th, "self"); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
