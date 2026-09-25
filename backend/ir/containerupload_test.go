package ir

import "testing"

// R34：container_upload 块在 IR 内部路径上的保真。该块只有 file_id 一个载荷，
// 既不进正文也不进增量通道——Clone 要深拷贝指针（改副本不穿透原件），
// ResponseEvents 要把它原样放进 block_start 骨架（投影成事件后仍可还原）。

func TestCloneBlocksDeepCopiesContainerUpload(t *testing.T) {
	src := []Block{{
		Type:            BlockContainerUpload,
		ContainerUpload: &ContainerUploadRef{FileID: "file_a"},
	}}
	dst := cloneBlocks(src)
	if dst[0].ContainerUpload == src[0].ContainerUpload {
		t.Fatal("clone 复用了同一指针，不是深拷贝")
	}
	dst[0].ContainerUpload.FileID = "mutated"
	if src[0].ContainerUpload.FileID != "file_a" {
		t.Errorf("clone 穿透改到了原件: %#v", src[0].ContainerUpload)
	}
}

// 投影成事件流后，container_upload 随 block_start 骨架整块到达，
// 再聚合回来 file_id 不丢、不增块。
func TestResponseEventsCarriesContainerUpload(t *testing.T) {
	resp := &Response{ID: "m", Model: "m", StopReason: StopEndTurn,
		Content: []Block{
			{Type: BlockText, Text: "chart ready"},
			{Type: BlockContainerUpload, ContainerUpload: &ContainerUploadRef{FileID: "file_out"}},
		}}
	var a Aggregator
	for _, ev := range ResponseEvents(resp) {
		a.Add(ev)
	}
	got := a.Response()
	if len(got.Content) != 2 {
		t.Fatalf("块数 = %d，want 2：%#v", len(got.Content), got.Content)
	}
	if got.Content[0].Text != "chart ready" {
		t.Errorf("正文 = %q", got.Content[0].Text)
	}
	b := got.Content[1]
	if b.Type != BlockContainerUpload || b.ContainerUpload == nil ||
		b.ContainerUpload.FileID != "file_out" {
		t.Errorf("container_upload 块投影丢失：%#v", b)
	}
}
