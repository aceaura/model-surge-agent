package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件覆盖第二十四轮：目标协议承载得了这一维，而本服务没写出去。
// 与前几轮的「没有承载位置」相反，这里的位置一直都在，只是没往里写——
// 有的是能力位声明错了，有的是线上结构缺字段，有的是两个字段间的换算没做。

// --- 需求2：Gemini 的 seed 与两个惩罚项 ---

func TestGeminiWritesSeedAndPenalties(t *testing.T) {
	seed := 4242
	pp, fp := 0.5, -0.25
	req := &ir.Request{
		Model:            "native",
		MaxTokens:        64,
		Messages:         []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Seed:             &seed,
		PresencePenalty:  &pp,
		FrequencyPenalty: &fp,
	}
	body, notes := lossyOf(t, codec.ProtocolGemini, req)
	var probe struct {
		GenerationConfig struct {
			Seed             *int     `json:"seed"`
			PresencePenalty  *float64 `json:"presencePenalty"`
			FrequencyPenalty *float64 `json:"frequencyPenalty"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	cfg := probe.GenerationConfig
	if cfg.Seed == nil || *cfg.Seed != seed {
		t.Errorf("seed 未写出：%s", body)
	}
	if cfg.PresencePenalty == nil || *cfg.PresencePenalty != pp {
		t.Errorf("presencePenalty 未写出：%s", body)
	}
	if cfg.FrequencyPenalty == nil || *cfg.FrequencyPenalty != fp {
		t.Errorf("frequencyPenalty 未写出：%s", body)
	}
	// 能力位置真后不该再报这三维的有损——报一条事实错误的说明比不报更坏，
	// 排查的人会照着它去找一个不存在的原因。
	for _, field := range []string{"seed", "presence_penalty", "frequency_penalty"} {
		if hasNoteWith(notes, field) {
			t.Errorf("gemini 承载得了 %s，不该报有损：%v", field, notes)
		}
	}
}

// TestGeminiOmitsAbsentSeedAndPenalties 未触发路径：客户端没给时三个键缺席。
func TestGeminiOmitsAbsentSeedAndPenalties(t *testing.T) {
	req := &ir.Request{
		Model:     "native",
		MaxTokens: 64,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
	body, _ := lossyOf(t, codec.ProtocolGemini, req)
	for _, key := range []string{"seed", "presencePenalty", "frequencyPenalty"} {
		if strings.Contains(string(body), key) {
			t.Errorf("客户端未给 %s 却写出了：%s", key, body)
		}
	}
}

// TestGeminiWireKeysAreCamelCase 钉住键名字面量：本协议用驼峰，
// 照抄 OpenAI 的蛇形会让上游整个忽略这三维（不报错，只是不生效）。
func TestGeminiWireKeysAreCamelCase(t *testing.T) {
	seed := 1
	pp := 1.0
	req := &ir.Request{
		Model:           "native",
		MaxTokens:       64,
		Messages:        []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Seed:            &seed,
		PresencePenalty: &pp,
	}
	body, _ := lossyOf(t, codec.ProtocolGemini, req)
	raw := string(body)
	if !strings.Contains(raw, `"presencePenalty":`) {
		t.Errorf("惩罚项键名应为驼峰 presencePenalty：%s", raw)
	}
	if strings.Contains(raw, "presence_penalty") {
		t.Errorf("蛇形键名会被上游忽略：%s", raw)
	}
}

// --- 需求3：responses 出站索要推理签名 ---

// includeOf 取出站请求体里的 include 数组。
func includeOf(t *testing.T, body []byte) []string {
	t.Helper()
	var probe struct {
		Include []string `json:"include"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return probe.Include
}

func thinkingReq(on bool) *ir.Request {
	req := &ir.Request{
		Model:     "native",
		MaxTokens: 2048,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
	if on {
		req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: "medium"}
	}
	return req
}

// TestResponsesAsksForReasoningSignature store 恒为假时上游只在被点名时
// 才回 encrypted_content，不要就等于永远拿不到签名。
func TestResponsesAsksForReasoningSignature(t *testing.T) {
	body, notes := lossyOf(t, codec.ProtocolResponses, thinkingReq(true))
	if !hasString(includeOf(t, body), "reasoning.encrypted_content") {
		t.Errorf("请求推理却没索要签名：%s", body)
	}
	// 这是为达成客户端意图补的，不是损失。
	if hasNoteWith(notes, "include") {
		t.Errorf("追加 include 不该报有损：%v", notes)
	}
}

// TestResponsesDoesNotAskSignatureWithoutThinking 未触发路径：不请求推理
// 时不追加——为一个不存在的过程索要签名是自相矛盾的请求。
func TestResponsesDoesNotAskSignatureWithoutThinking(t *testing.T) {
	body, _ := lossyOf(t, codec.ProtocolResponses, thinkingReq(false))
	if strings.Contains(string(body), "reasoning.encrypted_content") {
		t.Errorf("未请求推理却索要了签名：%s", body)
	}
}

// TestResponsesIncludeAppendsNotReplaces 客户端给了别的 include 项时保留，
// 覆盖掉是另一种丢维度。
func TestResponsesIncludeAppendsNotReplaces(t *testing.T) {
	req := thinkingReq(true)
	req.Include = []string{"message.output_text.logprobs"}
	body, _ := lossyOf(t, codec.ProtocolResponses, req)
	got := includeOf(t, body)
	if !hasString(got, "message.output_text.logprobs") {
		t.Errorf("客户端的 include 项被覆盖了：%v", got)
	}
	if !hasString(got, "reasoning.encrypted_content") {
		t.Errorf("未追加签名项：%v", got)
	}
}

// TestResponsesIncludeNotDuplicated 客户端已经给了同一项时不重复添加：
// 重复项本身可能被上游拒收。
func TestResponsesIncludeNotDuplicated(t *testing.T) {
	req := thinkingReq(true)
	req.Include = []string{"reasoning.encrypted_content"}
	body, _ := lossyOf(t, codec.ProtocolResponses, req)
	got := includeOf(t, body)
	var n int
	for _, v := range got {
		if v == "reasoning.encrypted_content" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("签名项出现 %d 次，应恰好一次：%v", n, got)
	}
}

// --- 需求4：top_logprobs 回填 ---

func TestResponsesBackfillsTopLogProbs(t *testing.T) {
	yes, no := true, false
	five := 5
	cases := []struct {
		name     string
		logProbs *bool
		topN     *int
		wantTop  *int
	}{
		{"only-switch", &yes, nil, intPtr(1)},
		{"switch-off", &no, nil, nil},
		{"both-given", &yes, &five, &five},
		{"only-top", nil, &five, &five},
		{"neither", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &ir.Request{
				Model:     "native",
				MaxTokens: 64,
				Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
				LogProbs:  tc.logProbs,
			}
			req.TopLogProbs = tc.topN
			body, _ := lossyOf(t, codec.ProtocolResponses, req)
			var probe struct {
				TopLogProbs *int `json:"top_logprobs"`
			}
			if err := json.Unmarshal(body, &probe); err != nil {
				t.Fatalf("unmarshal %s: %v", body, err)
			}
			switch {
			case tc.wantTop == nil && probe.TopLogProbs != nil:
				t.Errorf("不该补 top_logprobs，got %d：%s", *probe.TopLogProbs, body)
			case tc.wantTop != nil && probe.TopLogProbs == nil:
				t.Errorf("应有 top_logprobs=%d，实际缺席：%s", *tc.wantTop, body)
			case tc.wantTop != nil && *probe.TopLogProbs != *tc.wantTop:
				t.Errorf("top_logprobs = %d，想要 %d", *probe.TopLogProbs, *tc.wantTop)
			}
		})
	}
}

// TestLogProbsViaTopNDeclaredOnce 能力位只有 responses 为真：它是唯一
// 把开关与档位合成一个字段的协议。
func TestLogProbsViaTopNDeclaredOnce(t *testing.T) {
	for _, name := range outboundNames() {
		want := name == codec.ProtocolResponses
		if got := capsOf(t, name).LogProbsViaTopN; got != want {
			t.Errorf("%s 的 LogProbsViaTopN = %v，想要 %v", name, got, want)
		}
	}
}

// --- 需求5：图片 detail 透传 ---

func imageReq(detail string) *ir.Request {
	return &ir.Request{
		Model:     "native",
		MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "what is this"},
			{Type: ir.BlockImage, Media: &ir.Media{
				MediaType: "image/png", Data: "AA==", Detail: detail}},
		}}},
	}
}

// TestImageDetailRoundTrips 两个有这一维的协议双向保真。
func TestImageDetailRoundTrips(t *testing.T) {
	cases := []struct {
		proto string
		body  string
	}{
		{codec.ProtocolChatCompletions, `{"model":"m","messages":[{"role":"user","content":[` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"low"}}]}]}`},
		{codec.ProtocolResponses, `{"model":"m","input":[{"type":"message","role":"user","content":[` +
			`{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"low"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.proto, func(t *testing.T) {
			req := decodeReq(t, tc.proto, tc.body)
			block := findImage(req)
			if block == nil {
				t.Fatal("没有解出图片块")
			}
			if block.Detail != "low" {
				t.Fatalf("入站没有解出 detail：%+v", block)
			}
			req.Model = "native"
			body, notes := lossyOf(t, tc.proto, req)
			if !strings.Contains(string(body), `"detail":"low"`) {
				t.Errorf("出站没有写回 detail：%s", body)
			}
			if hasNoteWith(notes, "detail") {
				t.Errorf("%s 承载得了 detail，不该报有损：%v", tc.proto, notes)
			}
		})
	}
}

// TestImageDetailDroppedWhereUnsupported anthropic 与 gemini 没有这一维：
// 客户端给过的东西悄悄没了会让账单对不上，必须报。
func TestImageDetailDroppedWhereUnsupported(t *testing.T) {
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		t.Run(name, func(t *testing.T) {
			if capsOf(t, name).ImageDetail {
				t.Fatalf("%s 声明了 ImageDetail，本用例的前提不再成立", name)
			}
			body, notes := lossyOf(t, name, imageReq("low"))
			if !hasNoteWith(notes, "image_url.detail") {
				t.Errorf("%s 丢弃 detail 却不报说明：%v", name, notes)
			}
			// 说明要点出计费后果，笼统的 dropped 读不出这一点。
			if !hasNoteWith(notes, "billed") {
				t.Errorf("%s 的说明未点出计费后果：%v", name, notes)
			}
			if strings.Contains(string(body), `"detail"`) {
				t.Errorf("%s 不该写出 detail：%s", name, body)
			}
		})
	}
}

// TestImageDetailAbsentNotSynthesized 未触发路径：客户端没给时不合成、
// 不报说明。合成会把「按上游默认」变成「按我们猜的」，两者计费可能不同。
func TestImageDetailAbsentNotSynthesized(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			body, notes := lossyOf(t, name, imageReq(""))
			if strings.Contains(string(body), `"detail"`) {
				t.Errorf("%s 在客户端未给时合成了 detail：%s", name, body)
			}
			if hasNoteWith(notes, "image_url.detail") {
				t.Errorf("%s 在客户端未给时报了 detail 说明：%v", name, notes)
			}
		})
	}
}

// TestImageDetailWireKey 钉住键名字面量与零值缺席。
func TestImageDetailWireKey(t *testing.T) {
	raw, err := json.Marshal(ir.Media{MediaType: "image/png", Detail: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"detail":"high"`) {
		t.Errorf("ir.Media 的键名应为 detail，got %s", raw)
	}
	plain, err := json.Marshal(ir.Media{MediaType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "detail") {
		t.Errorf("零值不该出现 detail，got %s", plain)
	}
}

// --- 共用辅助 ---

func intPtr(n int) *int { return &n }

func hasString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func findImage(req *ir.Request) *ir.Media {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockImage && b.Media != nil {
				return b.Media
			}
		}
	}
	return nil
}
