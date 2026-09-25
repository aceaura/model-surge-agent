package chatcompletions

import (
	"strings"
	"testing"
)

// chat 专属四维（modalities/audio/prediction/web_search_options）此前
// wireRequest 连槽位都没有：同协议往返也静默蒸发——客户端要的音频输出、
// 预测加速、搜索调优全丢，且响应上无从察觉。四维均为 chat 一族独有，
// 收进 IR 只为同族回写与跨族诊断，不作映射尝试（跨族注记在 codec 层测）。

func TestChatExtrasRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}],` +
		`"modalities":["text","audio"],` +
		`"audio":{"format":"wav","voice":"alloy"},` +
		`"prediction":{"type":"content","content":"hello"},` +
		`"web_search_options":{"search_context_size":"high"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Modalities) != 2 || req.Modalities[1] != "audio" {
		t.Errorf("modalities 没收下：%v", req.Modalities)
	}
	if req.AudioOut == nil || req.AudioOut.Format != "wav" || req.AudioOut.Voice != "alloy" {
		t.Errorf("audio 没收下：%+v", req.AudioOut)
	}
	if !strings.Contains(string(req.Prediction), `"type":"content"`) {
		t.Errorf("prediction 没收下：%s", req.Prediction)
	}
	if !strings.Contains(string(req.WebSearchOptions), `"search_context_size":"high"`) {
		t.Errorf("web_search_options 没收下：%s", req.WebSearchOptions)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"modalities":["text","audio"]`,
		`"audio":{"format":"wav","voice":"alloy"}`,
		`"prediction":{"type":"content","content":"hello"}`,
		`"web_search_options":{"search_context_size":"high"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("回写丢了 %s：%s", want, s)
		}
	}
}

// voice 官方两形态：内置名 string 或自定义 {id} 对象。对象形态解码归一成
// string（语义等价），回写恒取 string 简形。
func TestAudioVoiceObjectFormNormalized(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}],` +
		`"audio":{"format":"mp3","voice":{"id":"v_custom"}}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.AudioOut == nil || req.AudioOut.Voice != "v_custom" {
		t.Fatalf("voice 对象形态没归一：%+v", req.AudioOut)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"voice":"v_custom"`) {
		t.Errorf("回写没取 string 简形：%s", out)
	}
	if strings.Contains(string(out), `"id"`) {
		t.Errorf("对象形态残留：%s", out)
	}
}

// 没给 voice 不造空串：空 voice 会被上游当成非法音色名拒掉，
// 缺省保持缺省。
func TestAudioVoiceNotInventedWhenEmpty(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}],` +
		`"audio":{"format":"wav"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"audio":{"format":"wav"}`) {
		t.Errorf("audio 回写形状错：%s", out)
	}
	if strings.Contains(string(out), "voice") {
		t.Errorf("没给的 voice 被凭空造出：%s", out)
	}
}

// 显式 null 等同没给（stop/tool_choice 同款先例）：留下字面量 null 会让
// 回写多出一个上游解不动的 null 键，跨族诊断也会误报。
func TestChatExtrasExplicitNullNormalized(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}],` +
		`"prediction":null,"web_search_options":null}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Prediction != nil || req.WebSearchOptions != nil {
		t.Fatalf("显式 null 没归一：%s %s", req.Prediction, req.WebSearchOptions)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "prediction") || strings.Contains(string(out), "web_search_options") {
		t.Errorf("null 被回写成键：%s", out)
	}
}

// 四维缺席时出站一个键也不造。
func TestChatExtrasAbsentStaysAbsent(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"modalities", `"audio"`, "prediction", "web_search_options"} {
		if strings.Contains(string(out), unwanted) {
			t.Errorf("没给的 %s 被凭空造出：%s", unwanted, out)
		}
	}
}
