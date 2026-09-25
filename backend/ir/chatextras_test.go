package ir

import "testing"

// chat 专属四维进 Clone 的两维（Modalities 切片、AudioOut 指针）必须换头
// 深拷贝：换目标重试的两份编码各自独立，改副本不得穿透到原件。
// Prediction/WebSearchOptions 字节按 RawMessage 不可变惯例随值共享。
func TestCloneDeepCopiesChatExtras(t *testing.T) {
	src := &Request{
		Model: "m", MaxTokens: 8,
		Modalities: []string{"text", "audio"},
		AudioOut:   &AudioOut{Format: "wav", Voice: "alloy"},
		Prediction: []byte(`{"type":"content"}`),
	}
	dst := src.Clone()
	if &dst.Modalities[0] == &src.Modalities[0] {
		t.Error("Clone 共享了 Modalities 底层数组")
	}
	if dst.AudioOut == src.AudioOut {
		t.Error("Clone 共享了 AudioOut 指针")
	}
	dst.Modalities[0] = "mutated"
	dst.Modalities = append(dst.Modalities, "extra")
	dst.AudioOut.Voice = "mutated"
	if src.Modalities[0] != "text" || len(src.Modalities) != 2 || src.AudioOut.Voice != "alloy" {
		t.Errorf("Clone 穿透改到了原件：%v %+v", src.Modalities, src.AudioOut)
	}
	if string(dst.Prediction) != `{"type":"content"}` {
		t.Errorf("Prediction 丢了：%s", dst.Prediction)
	}
}

// 没给的维度 Clone 后保持缺省：nil 指针不得克隆成空结构，nil 切片保持 nil。
func TestCloneKeepsChatExtrasAbsent(t *testing.T) {
	src := &Request{Model: "m", MaxTokens: 8}
	dst := src.Clone()
	if dst.AudioOut != nil || dst.Modalities != nil || dst.Prediction != nil || dst.WebSearchOptions != nil {
		t.Errorf("缺席维度被克隆出了值：%+v", dst)
	}
}
