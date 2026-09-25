package responses

import (
	"strings"
	"testing"
)

// #70 的托管输出项注记：web_search_call / file_search_call 这类条目本变换
// 没有映射，整项丢弃。丢弃必须报出——静默丢掉会让客户端对不上
// 「付了托管执行的钱却看不到产出」。计数点在 done 帧：每个条目恒有一次
// output_item.done，而 added 帧在部分网关上缺席。

func TestStreamHostedItemsAreCountedInNotes(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"file_search_call","id":"fs_1","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if !anyNoteHas(notes, "2 hosted output item") {
		t.Errorf("两条托管项没按数报出：%v", notes)
	}
}

// 普通条目不触发注记：message 有映射，丢了才该报；没丢就不许制造噪音。
func TestStreamMessageItemsProduceNoHostedNote(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if anyNoteHas(notes, "hosted output item") {
		t.Errorf("message 条目被误报成托管项：%v", notes)
	}
}

// Notes() 是抽干式的：取过一次之后不许重复报同一条，否则同一批丢弃
// 会在流式转发里被逐段重复注记。
func TestHostedNoteDrainedOnce(t *testing.T) {
	d := newStreamDecoder()
	frames := []string{
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	}
	for _, f := range frames {
		if _, err := d.Feed("", f); err != nil {
			t.Fatalf("Feed(%s): %v", f, err)
		}
	}
	first := d.Notes()
	if !anyNoteHas(first, "1 hosted output item") {
		t.Fatalf("首取没报出托管项：%v", first)
	}
	if second := d.Notes(); anyNoteHas(second, "hosted output item") {
		t.Errorf("同一批丢弃被重复报出：%v", second)
	}
}

// 非流式整份解码：output 里的托管项同样计数报出，message 块照常保留。
// 这条路径喂给 pipeline 的 adoptWholeResponse（LossyResponseDecoder），
// 不实现就只剩流式有注记——非流式客户端永远看不到丢弃说明。
func TestWholeDecodeNotesHostedItems(t *testing.T) {
	body := []byte(`{"id":"r1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},` +
		`{"type":"web_search_call","id":"ws_1","status":"completed"}]}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, "1 hosted output item") {
		t.Errorf("整份解码没报出托管项：%v", notes)
	}
	if len(resp.Content) != 1 {
		t.Errorf("content = %d 块，want 1（message 该保留、托管项该丢弃）：%+v",
			len(resp.Content), resp.Content)
	}

	// 无托管项时不许凭空注记。
	body = []byte(`{"id":"r2","model":"m","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
	_, notes, err = DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy(clean): %v", err)
	}
	if anyNoteHas(notes, "hosted output item") {
		t.Errorf("干净的响应被误注记：%v", notes)
	}
}

// 注记措辞不许断言「协议没有槽位」：IR 有服务端工具块，只是本变换不映射。
// 口径与 ServerToolDropNote 一致，说「本变换丢弃」而不是「协议装不下」。
func TestHostedNoteWordingBlamesConversion(t *testing.T) {
	note := ""
	for _, n := range mustHostedNote(t) {
		if strings.Contains(n, "hosted output item") {
			note = n
		}
	}
	if note == "" {
		t.Fatalf("没拿到托管项注记")
	}
	if !strings.Contains(note, "conversion") {
		t.Errorf("注记没把责任落在变换上：%s", note)
	}
}

func mustHostedNote(t *testing.T) []string {
	t.Helper()
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	return notes
}
