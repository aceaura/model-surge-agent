package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次42：gemini 上游 usageMetadata 的按模态 token 明细（promptTokensDetails /
// candidatesTokensDetails / responseTokensDetails，元素 {modality,tokenCount}）
// 此前 wireUsage 全未建模、convertUsage 不读，多模态归因被 json.Unmarshal 静默
// 吞掉——与轮次41 保全的 chat prompt_tokens_details.image_tokens/text_tokens 同维。
// 这组测试钉住：能归一进 IR 既有模态槽位的保全，IR 无槽位的（VIDEO/输出侧 IMAGE/
// 缓存与工具用量的模态细分）经注记报出。

// 输入侧 TEXT/IMAGE/AUDIO 归一进 IR 的 prompt 模态槽位。
func TestModalityUsageDecodePrompt(t *testing.T) {
	got, dropped := convertUsage(wireUsage{
		PromptTokenCount: 100,
		PromptTokensDetails: []wireModalityTokenCount{
			{Modality: "TEXT", TokenCount: 60},
			{Modality: "IMAGE", TokenCount: 30},
			{Modality: "AUDIO", TokenCount: 10},
		},
	})
	if got.PromptTextTokens != 60 || got.PromptImageTokens != 30 || got.PromptAudioTokens != 10 {
		t.Fatalf("输入模态明细没归一进 IR：%+v", got)
	}
	if dropped != 0 {
		t.Errorf("全可归一时不该有丢弃计数，得到 %d", dropped)
	}
}

// 输出侧 TEXT/AUDIO 归一进 completion 槽位；candidatesTokensDetails 与
// responseTokensDetails 两种官方命名都读。
func TestModalityUsageDecodeOutputBothNames(t *testing.T) {
	got, dropped := convertUsage(wireUsage{
		CandidatesTokensDetails: []wireModalityTokenCount{
			{Modality: "TEXT", TokenCount: 40},
			{Modality: "AUDIO", TokenCount: 5},
		},
	})
	if got.CompletionTextTokens != 40 || got.CompletionAudioTokens != 5 {
		t.Fatalf("candidatesTokensDetails 没归一：%+v", got)
	}
	if dropped != 0 {
		t.Errorf("不该有丢弃计数，得到 %d", dropped)
	}
	got2, _ := convertUsage(wireUsage{
		ResponseTokensDetails: []wireModalityTokenCount{{Modality: "TEXT", TokenCount: 7}},
	})
	if got2.CompletionTextTokens != 7 {
		t.Fatalf("responseTokensDetails 没归一：%+v", got2)
	}
}

// IR 无槽位的模态（输入 VIDEO、输出 IMAGE）计入丢弃。
func TestModalityUsageCountsUnmappable(t *testing.T) {
	_, dropped := convertUsage(wireUsage{
		PromptTokensDetails: []wireModalityTokenCount{
			{Modality: "TEXT", TokenCount: 1},
			{Modality: "VIDEO", TokenCount: 20}, // 无输入视频槽位
		},
		CandidatesTokensDetails: []wireModalityTokenCount{
			{Modality: "IMAGE", TokenCount: 8}, // 无输出图片槽位
		},
	})
	if dropped != 2 {
		t.Fatalf("VIDEO 与输出 IMAGE 两条该计入丢弃，得到 %d", dropped)
	}
}

// 缓存与工具用量的模态细分整组计入丢弃（IR 只有总量槽位）。
func TestModalityUsageCountsCacheAndToolSplits(t *testing.T) {
	_, dropped := convertUsage(wireUsage{
		CacheTokensDetails: []wireModalityTokenCount{
			{Modality: "TEXT", TokenCount: 3}, {Modality: "IMAGE", TokenCount: 4},
		},
		ToolUsePromptTokensDetails: []wireModalityTokenCount{
			{Modality: "TEXT", TokenCount: 2},
		},
	})
	if dropped != 3 {
		t.Fatalf("缓存2条+工具1条该计入丢弃，得到 %d", dropped)
	}
}

// 非流式端到端：可归一的进 IR，不可归一的经注记报出。
func TestModalityUsageNonStreamingNote(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]}}],` +
		`"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,` +
		`"promptTokensDetails":[{"modality":"TEXT","tokenCount":60},{"modality":"IMAGE","tokenCount":40}],` +
		`"candidatesTokensDetails":[{"modality":"VIDEO","tokenCount":50}]}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.Usage.PromptTextTokens != 60 || resp.Usage.PromptImageTokens != 40 {
		t.Errorf("输入模态明细没进 IR：%+v", resp.Usage)
	}
	if !hasModalityNote(notes) {
		t.Errorf("输出 VIDEO 无槽位，应有注记，得到 %v", notes)
	}
}

// 非流式：全部可归一时不报注记（避免噪音）。
func TestModalityUsageNonStreamingNoNoteWhenAllMapped(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]}}],` +
		`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,` +
		`"promptTokensDetails":[{"modality":"TEXT","tokenCount":10}],` +
		`"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":5}]}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.Usage.CompletionTextTokens != 5 {
		t.Errorf("输出 TEXT 没进 IR：%+v", resp.Usage)
	}
	if hasModalityNote(notes) {
		t.Errorf("全可归一时不该报注记，得到 %v", notes)
	}
}

// 流式：模态明细经终止 EvMessageDelta 带回并聚合进 IR，不可归一的经 Notes() 报出。
func TestModalityUsageStreaming(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],`+
		`"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,`+
		`"promptTokensDetails":[{"modality":"IMAGE","tokenCount":40}],`+
		`"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":9},{"modality":"VIDEO","tokenCount":41}]}}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	var agg ir.Aggregator
	for _, ev := range dec.Finish() {
		agg.Add(ev)
	}
	u := agg.Response().Usage
	if u.PromptImageTokens != 40 || u.CompletionAudioTokens != 9 {
		t.Errorf("流式模态明细没聚合进 IR：%+v", u)
	}
	notes := dec.Notes()
	if !hasModalityNote(notes) {
		t.Errorf("输出 VIDEO 无槽位，流式应有注记，得到 %v", notes)
	}
}

// 上游没给模态明细：槽位留零、无注记。
func TestModalityUsageAbsentStaysZero(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]}}],` +
		`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.Usage.PromptTextTokens != 0 || resp.Usage.PromptImageTokens != 0 ||
		resp.Usage.PromptAudioTokens != 0 || resp.Usage.CompletionTextTokens != 0 ||
		resp.Usage.CompletionAudioTokens != 0 {
		t.Errorf("缺席时模态槽位应为零：%+v", resp.Usage)
	}
	if hasModalityNote(notes) {
		t.Errorf("缺席时不该报注记，得到 %v", notes)
	}
}

func hasModalityNote(notes []string) bool {
	for _, n := range notes {
		if strings.Contains(n, "per-modality token breakdown") {
			return true
		}
	}
	return false
}
