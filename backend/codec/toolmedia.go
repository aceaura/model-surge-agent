package codec

import (
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// SplitToolResultMedia 把工具结果里的媒体块抽出。
//
// 三个协议的工具结果载荷装不下媒体，但坏法各不相同：responses 的
// function_call_output.output 与 gemini 的 functionResponse.response 都是
// 单个字符串，编码器的 joinText 只取文本块，图片被静默碾掉——模型看不到
// 截图却被要求据此答题，答错且无人知道为什么。chat_completions 的 role:tool
// 消息编出的 image_url part 更糟：上游按格式错误拒收整个请求。
//
// 抽出而不是就地转成文本描述：一句「这里原本有一张图」对模型毫无用处，
// 而把媒体挪到紧随其后的一条用户消息里，模型仍能看到它，只是呈现位置
// 从「工具的输出」变成「用户随后给的材料」。这是有损的，调用方要能看见。
//
// 只看顶层块：工具结果里不会再嵌套工具结果（ir.Sanitize 保证），
// 而文本块里的内容与媒体承载力无关。
func SplitToolResultMedia(blocks []ir.Block) (kept, media []ir.Block) {
	hasMedia := false
	for _, b := range blocks {
		if b.Type.IsMedia() {
			hasMedia = true
			break
		}
	}
	if !hasMedia {
		// 原样返回同一个切片：没有媒体时不该产生一次拷贝，
		// 调用方据此也能省掉后续的追加判断。
		return blocks, nil
	}
	kept = make([]ir.Block, 0, len(blocks))
	for _, b := range blocks {
		if b.Type.IsMedia() {
			media = append(media, b)
			continue
		}
		kept = append(kept, b)
	}
	return kept, media
}

// ToolResultMediaMovedNote 是媒体被挪出工具结果的有损说明。
//
// 措辞落在一处：三个编码器各写一份会漂移，而这条说明是调用方判断
// 「模型看到的材料位置与我发的不同」的唯一线索。
func ToolResultMediaMovedNote(name string) string {
	return "rewrote tool_result content (" + name + " cannot express it: " +
		"its tool result payload carries text only, media moved into a following user turn)"
}
