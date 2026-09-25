package ir

import "testing"

// 音频维度的 IR 语义（移植旧仓 #35）：投影携带、聚合保真、规范化保留、
// 克隆复制。

// 非流式完整响应投影成事件流时，Audio 随 EvMessageStart 携带；
// 聚合器把它并回 Response。
func TestResponseEventsCarriesAudioOutput(t *testing.T) {
	audio := &AudioOutput{ID: "audio_1", Data: "AA==", ExpiresAt: 42, Transcript: "hi"}
	resp := &Response{ID: "m", Model: "m",
		Content: []Block{{Type: BlockText, Text: "caption"}},
		Audio:   audio}

	evs := ResponseEvents(resp)
	if len(evs) == 0 || evs[0].Type != EvMessageStart {
		t.Fatalf("首帧 = %+v", evs[0])
	}
	if evs[0].Audio != audio {
		t.Fatalf("EvMessageStart.Audio = %+v, want %+v", evs[0].Audio, audio)
	}

	a := &Aggregator{}
	for _, ev := range evs {
		a.Add(ev)
	}
	got := a.Response()
	if got.Audio == nil || *got.Audio != *audio {
		t.Fatalf("聚合丢音频: %+v", got.Audio)
	}
}

// 无音频时事件不带、聚合不出。
func TestResponseEventsAudioAbsent(t *testing.T) {
	evs := ResponseEvents(&Response{ID: "m", Model: "m"})
	if evs[0].Audio != nil {
		t.Fatalf("无音频却携带: %+v", evs[0].Audio)
	}
	a := &Aggregator{}
	for _, ev := range evs {
		a.Add(ev)
	}
	if a.Response().Audio != nil {
		t.Fatal("聚合凭空出音频")
	}
}

// 规范化：音频引用就是内容——pruneEmpty 不得清掉 AudioID-only 的 assistant
// 历史（官方 chat 允许该形态）；mergeAdjacentRoles 不得把带引用的消息并入
// 相邻同角色消息（合并只拼 Content，引用会无声丢失）。
func TestSanitizeKeepsAudioReferenceMessages(t *testing.T) {
	r := &Request{Model: "m", Messages: []Message{
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "hi"}}},
		{Role: RoleAssistant, AudioID: "audio_2"},
		{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "next"}}},
	}}
	Sanitize(r)
	if len(r.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(r.Messages), r.Messages)
	}
	if r.Messages[1].AudioID != "audio_2" {
		t.Fatalf("AudioID 丢失: %+v", r.Messages[1])
	}
	if len(r.Messages[1].Content) != 0 {
		t.Fatalf("引用消息被伪造内容: %+v", r.Messages[1])
	}
}

// 克隆复制 AudioID。
func TestCloneMessagesCopiesAudioID(t *testing.T) {
	r := &Request{Model: "m", Messages: []Message{
		{Role: RoleAssistant, AudioID: "audio_9"},
	}}
	if got := r.Clone().Messages[0].AudioID; got != "audio_9" {
		t.Fatalf("Clone AudioID = %q", got)
	}
}
