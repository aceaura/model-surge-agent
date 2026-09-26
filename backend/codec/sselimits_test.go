package codec

import (
	"io"
	"strings"
	"testing"
)

// maxFrameBytes 此前只对「已切成行的 data 累积」生效，readLine 本身用
// ReadString 无行长上限：上游发一条不带换行的巨行时内存先无界增长。
// readLine 补上限后，本组测试把三条防线（单行上限、整帧上限、拆分上限）
// 都钉住——它们全是异常路径，正常流量永远碰不到，没有测试就等于没有。

func TestScanRunawayLineHitsShortBuffer(t *testing.T) {
	// 一条比行长上限还长、始终不出现换行的行：必须在攒到上限时报
	// io.ErrShortBuffer，而不是把整条行读进内存。
	input := strings.Repeat("a", maxLineBytes+1)
	s := NewFrameScanner(strings.NewReader(input))
	if s.Scan() {
		t.Fatal("巨行不该产出帧")
	}
	if s.Err() != io.ErrShortBuffer {
		t.Fatalf("Err() = %v，想要 io.ErrShortBuffer", s.Err())
	}
}

func TestScanLineExactlyAtLimitStillWorks(t *testing.T) {
	// 上限判据是「超过才拒」：贴着上限的合法大帧（responses completed
	// 带全量响应）不能被误杀。行长含 data: 前缀与换行，共 maxFrameBytes+1
	// 字节，恰在 maxLineBytes 之内；data 内容不超帧上限。
	payload := strings.Repeat("a", maxFrameBytes-len("data:"))
	input := "data:" + payload + "\n\n"
	s := NewFrameScanner(strings.NewReader(input))
	if !s.Scan() {
		t.Fatalf("边界行被拒：%v", s.Err())
	}
	if got := s.Frame().Data; len(got) != len(payload) {
		t.Fatalf("data 长度 = %d，想要 %d", len(got), len(payload))
	}
}

func TestScanFrameAccumulatedOverLimit(t *testing.T) {
	// 多条 data 行累积超过整帧上限：原有的帧级防线，补上直接测试。
	half := strings.Repeat("b", maxFrameBytes/2+64)
	input := "data:" + half + "\ndata:" + half + "\n\n"
	s := NewFrameScanner(strings.NewReader(input))
	if s.Scan() {
		t.Fatal("超限帧不该产出")
	}
	if s.Err() != io.ErrShortBuffer {
		t.Fatalf("Err() = %v，想要 io.ErrShortBuffer", s.Err())
	}
}

func TestSplitJSONDocumentsCountLimit(t *testing.T) {
	one := `{"a":1}`
	// 恰好 maxJSONDocsPerLine 份：允许。
	atLimit, ok := SplitJSONDocuments(strings.Repeat(one, maxJSONDocsPerLine))
	if !ok || len(atLimit) != maxJSONDocsPerLine {
		t.Fatalf("上限内拆分 = %d 份, ok=%v，想要 %d 份", len(atLimit), ok, maxJSONDocsPerLine)
	}
	// 多一份：畸形输入在这里止住，不往解码器送 17 个事件。
	if _, ok := SplitJSONDocuments(strings.Repeat(one, maxJSONDocsPerLine+1)); ok {
		t.Fatal("超过份数上限仍拆分成功")
	}
}

func TestSplitJSONDocumentsLineBytesLimit(t *testing.T) {
	// 参与拆分的行本身超限：直接拒绝，不进入逐文档解析。
	big := `{"a":"` + strings.Repeat("c", maxJSONLineBytes) + `"}`
	if _, ok := SplitJSONDocuments(big); ok {
		t.Fatal("超限行仍拆分成功")
	}
}
