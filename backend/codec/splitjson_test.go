package codec

import (
	"errors"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 探针实证的缺口：兼容层把两帧粘进一个 data 行，整行解码失败，
// 整条流就断在这里。
func TestSplitJSONDocumentsSplitsTwoDocs(t *testing.T) {
	docs, ok := SplitJSONDocuments(`{"a":1}{"b":2}`)
	if !ok {
		t.Fatal("two concatenated documents should split")
	}
	want := []string{`{"a":1}`, `{"b":2}`}
	if len(docs) != len(want) {
		t.Fatalf("docs = %#v, want %#v", docs, want)
	}
	for i := range want {
		if docs[i] != want[i] {
			t.Errorf("docs[%d] = %s, want %s", i, docs[i], want[i])
		}
	}
}

// 单文档行不算多文档：报成拆分成功会把真正的解码错误藏起来。
func TestSplitJSONDocumentsRejectsSingleDoc(t *testing.T) {
	if _, ok := SplitJSONDocuments(`{"a":1}`); ok {
		t.Error("a single document should not report as split")
	}
}

func TestSplitJSONDocumentsRejectsMalformed(t *testing.T) {
	for _, data := range []string{`{"a":1}{`, `{"a":1}garbage`, ``, `not json`} {
		if _, ok := SplitJSONDocuments(data); ok {
			t.Errorf("%q should not report as split", data)
		}
	}
}

func TestSplitJSONDocumentsRejectsTooManyDocs(t *testing.T) {
	over := strings.Repeat(`{}`, maxJSONDocsPerLine+1)
	if _, ok := SplitJSONDocuments(over); ok {
		t.Errorf("%d documents should exceed the cap", maxJSONDocsPerLine+1)
	}
	atCap := strings.Repeat(`{}`, maxJSONDocsPerLine)
	if _, ok := SplitJSONDocuments(atCap); !ok {
		t.Errorf("%d documents should still split", maxJSONDocsPerLine)
	}
}

// 单文档路径不该经过拆分：正常帧每一帧都走这里。
func TestFeedWithSplitLeavesGoodFramesAlone(t *testing.T) {
	var seen []string
	out, split, err := FeedWithSplit("", `{"a":1}`, func(_, data string) ([]ir.Event, error) {
		seen = append(seen, data)
		return []ir.Event{{Type: ir.EvTextDelta, Text: data}}, nil
	})
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if split {
		t.Error("a decodable frame should not report a split")
	}
	if len(seen) != 1 || seen[0] != `{"a":1}` {
		t.Errorf("feed calls = %#v, want the frame verbatim once", seen)
	}
	if len(out) != 1 {
		t.Errorf("events = %#v, want one", out)
	}
}

func TestFeedWithSplitFeedsEachDocument(t *testing.T) {
	var seen []string
	out, split, err := FeedWithSplit("", `{"a":1}{"b":2}`, func(_, data string) ([]ir.Event, error) {
		if strings.Contains(data, "}{") {
			return nil, errors.New("undecodable")
		}
		seen = append(seen, data)
		return []ir.Event{{Type: ir.EvTextDelta, Text: data}}, nil
	})
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if !split {
		t.Error("a multi-document line should report a split")
	}
	if len(seen) != 2 {
		t.Fatalf("feed calls = %#v, want two", seen)
	}
	if len(out) != 2 {
		t.Errorf("events = %#v, want two", out)
	}
}

// 拆不出来时必须把原始错误原样交回，不能换成拆分失败的说法：
// 排查的人要看到解码器对这一行的真实判断。
func TestFeedWithSplitKeepsOriginalError(t *testing.T) {
	want := errors.New("undecodable stream frame")
	_, split, err := FeedWithSplit("", `not json`, func(_, _ string) ([]ir.Event, error) {
		return nil, want
	})
	if split {
		t.Error("a malformed line should not report a split")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want the decoder's own error", err)
	}
}

// 拆出的某一份解不动仍算整行失败：部分解码会让客户端收到半截内容，
// 却看不出后面丢了东西。
func TestFeedWithSplitFailsWhenOneDocumentFails(t *testing.T) {
	boom := errors.New("bad document")
	_, _, err := FeedWithSplit("", `{"a":1}{"b":2}`, func(_, data string) ([]ir.Event, error) {
		if data == `{"b":2}` {
			return nil, boom
		}
		if strings.Contains(data, "}{") {
			return nil, errors.New("undecodable")
		}
		return nil, nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the failing document's error", err)
	}
}
