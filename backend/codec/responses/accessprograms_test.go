package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// 轮次39：responses 独有的 access_programs（域专属访问计划，{cyber: standard|
// daybreak_blue|daybreak_red}）同族原文透传。此前 wireRequest 不建模该键，
// json.Unmarshal 静默吞掉，responses→responses 同族往返一次即蒸发，客户端显式选定
// 的 cyber 访问档位被悄悄降级成上游默认（standard），无注记。官方对照：openai-python
// response_create_params.py:46 `access_programs: AccessPrograms`（AccessPrograms.cyber
// 枚举 standard|daybreak_blue|daybreak_red）。这组测试钉住解码落 IR、编码原样回写、
// 缺席与显式 null 归一为空、同族往返不漂移。跨族无槽位由 DescribeLossy 报出
// （见 codec/accessprogramsloss_test.go）。

const apReqPrefix = `{"model":"gpt-x","input":[{"role":"user","content":"hi"}],`

func TestAccessProgramsDecodePassthrough(t *testing.T) {
	body := apReqPrefix + `"access_programs":{"cyber":"daybreak_red"}}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.AccessPrograms) == 0 {
		t.Fatal("access_programs 未落进 IR")
	}
	if !strings.Contains(string(r.AccessPrograms), `"cyber":"daybreak_red"`) {
		t.Errorf("access_programs 原文漂移：%s", r.AccessPrograms)
	}
	// 必须是合法 JSON（原文透传不能破坏结构）。
	var probe any
	if err := json.Unmarshal(r.AccessPrograms, &probe); err != nil {
		t.Errorf("access_programs 非合法 JSON：%v", err)
	}
}

func TestAccessProgramsDecodeAbsentAndNull(t *testing.T) {
	for _, body := range []string{
		// 完全缺席。
		`{"model":"gpt-x","input":[{"role":"user","content":"hi"}]}`,
		// 显式 null（归一为没给，避免 4 字节字面量被当成配置回写）。
		apReqPrefix + `"access_programs":null}`,
	} {
		r, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeRequest: %v", err)
		}
		if len(r.AccessPrograms) != 0 {
			t.Errorf("缺席/null 时 access_programs 应为空：%s", r.AccessPrograms)
		}
	}
}

func TestAccessProgramsEncodePassthrough(t *testing.T) {
	in := apReqPrefix + `"access_programs":{"cyber":"daybreak_blue"}}`
	r, err := DecodeRequest([]byte(in))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"access_programs"`) ||
		!strings.Contains(string(out), `"cyber":"daybreak_blue"`) {
		t.Errorf("access_programs 未原样回写：%s", out)
	}
}

func TestAccessProgramsEncodeAbsentNoKey(t *testing.T) {
	r, err := DecodeRequest([]byte(`{"model":"gpt-x","input":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), "access_programs") {
		t.Errorf("缺席不应出键：%s", out)
	}
}

// 同族往返：decode→encode→decode 原文不漂移。Clone 走 out:=*r 浅拷贝，
// RawMessage 字节按不可变惯例随值共享，往返不得丢失。
func TestAccessProgramsRequestRoundTrip(t *testing.T) {
	in := apReqPrefix + `"access_programs":{"cyber":"daybreak_red"}}`
	r, err := DecodeRequest([]byte(in))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	back, err := DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	if !strings.Contains(string(back.AccessPrograms), `"cyber":"daybreak_red"`) {
		t.Errorf("往返 access_programs 漂移：%s", back.AccessPrograms)
	}
}
