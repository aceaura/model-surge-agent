package gemini

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守「gemini 请求编码器保全空正文但带同族签名的思考块」。
//
// gemini 的 thoughtSignature 是扩展思考的续话凭据。gemini 自己的解码器
// （decode_stream.go:727-734，case p.Thought 不带 text!="" 门控）会从无正文的
// thought part 产出 ir.Thinking{Text:"", Signature:sig, SignatureFrom:gemini}，
// 多轮历史回放时这一形状合法且常见。此前请求编码器（encode_request.go:189-199）
// 只判 Text=="" 就整块 continue，把同族签名一并静默丢弃（违规则 a），且与保全同
// 形状的 anthropic（encode_request.go:416 仅 text 与 sig 皆空才跳过）/responses
// 不对称（违规则 c）。修法=对齐 anthropic：仅当正文与可写回签名皆空才跳过。

func encodeThinking(t *testing.T, th *ir.Thinking) ([]byte, []string) {
	t.Helper()
	req := &ir.Request{Model: "gemini-3-pro", Messages: []ir.Message{{
		Role:    ir.RoleAssistant,
		Content: []ir.Block{{Type: ir.BlockThinking, Thinking: th}},
	}}}
	body, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	return body, notes
}

// thoughtPart 取出请求体里带 thought 标记的 part；没有则 ok=false。
func thoughtPart(t *testing.T, body []byte) (wirePart, bool) {
	t.Helper()
	var w struct {
		Contents []struct {
			Parts []wirePart `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	for _, c := range w.Contents {
		for _, p := range c.Parts {
			if p.Thought {
				return p, true
			}
		}
	}
	return wirePart{}, false
}

// 核心：空正文 + 同族签名 → 必须保全签名（发出 thought part），且不出说明。
//
// 变异锚点：把 encode_request.go 的跳过条件改回 `b.Thinking.Text == ""` 即令本用例转红。
func TestEncodePreservesEmptyTextOwnThinkingSignature(t *testing.T) {
	body, notes := encodeThinking(t, &ir.Thinking{
		Text: "", Signature: "sig-cont", SignatureFrom: Name,
	})
	part, ok := thoughtPart(t, body)
	if !ok {
		t.Fatalf("空正文同族签名的思考块被整块丢弃，请求体里没有 thought part：%s", body)
	}
	if part.ThoughtSignature != "sig-cont" {
		t.Errorf("thoughtSignature = %q，want sig-cont——丢了续话凭据下一轮上游不认这次思考",
			part.ThoughtSignature)
	}
	if hasNoteAbout(notes, "thinking") {
		t.Errorf("保全了同族签名却报了丢弃说明（假阳性）：%#v", notes)
	}
}

// 真空壳（正文与签名皆空）仍整块跳过、不出说明：守保全没退化成无脑保留空 part。
func TestEncodeStillSkipsEmptyShellWithoutSignature(t *testing.T) {
	body, notes := encodeThinking(t, &ir.Thinking{Text: "", Signature: "", SignatureFrom: Name})
	if _, ok := thoughtPart(t, body); ok {
		t.Errorf("正文与签名皆空的空壳被发了出去：%s", body)
	}
	if hasNoteAbout(notes, "thinking") {
		t.Errorf("空壳无内容可丢，不该出说明：%#v", notes)
	}
}

// 空正文 + 异族签名：签名不可写回（发给上游会拒整轮），整块跳过，但异族签名的
// 丢弃必须由请求侧报出一条 thinking signature 说明（不是静默）。这条钉住保全改动
// 没有顺手把异族签名也「保全」进 wire（那会把会被拒的密文送出去）。
func TestEncodeStripsForeignSignatureOnEmptyTextWithNote(t *testing.T) {
	body, notes := encodeThinking(t, &ir.Thinking{
		Text: "", Signature: "sig-from-elsewhere", SignatureFrom: codec.ProtocolAnthropic,
	})
	if part, ok := thoughtPart(t, body); ok {
		t.Errorf("异族签名的空正文思考块本应整块跳过，却发出了 part：%+v", part)
	}
	if !hasNoteAbout(notes, "thinking signature") {
		t.Errorf("剥离了异族签名却一声不响：%#v", notes)
	}
}

// 回归护栏：正文非空 + 同族签名仍照常保全两者（保全逻辑没改坏正常路径）。
func TestEncodePreservesTextAndOwnSignature(t *testing.T) {
	body, _ := encodeThinking(t, &ir.Thinking{
		Text: "pondering", Signature: "sig-cont", SignatureFrom: Name,
	})
	part, ok := thoughtPart(t, body)
	if !ok {
		t.Fatalf("请求体里没有 thought part：%s", body)
	}
	if part.Text != "pondering" || part.ThoughtSignature != "sig-cont" {
		t.Errorf("thought part = {text:%q sig:%q}，want {pondering sig-cont}",
			part.Text, part.ThoughtSignature)
	}
}
