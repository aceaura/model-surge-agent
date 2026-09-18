package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件是错误路径的矩阵，与请求侧的 crossmatrix_test.go、响应侧的
// responsematrix_test.go 并列。按入站协议展开：错误渲染是入站职责。
//
// 前五轮的矩阵都假定这轮最终成功，这一份守的是失败本身被表达对了。

// errorKinds 是要跨协议对齐的错误类别。
// 取全集而非抽样：漏掉一类就等于允许它在某个协议下渲染成别的语义。
func errorKinds() []ir.ErrorKind {
	return []ir.ErrorKind{
		ir.ErrInvalidRequest,
		ir.ErrAuth,
		ir.ErrNotFound,
		ir.ErrRateLimit,
		ir.ErrContextExceeded,
		ir.ErrUpstream,
		ir.ErrTimeout,
		ir.ErrInternal,
		ir.ErrCanceled,
	}
}

// TestEveryInboundRendersEveryKind 每个入站协议都要能渲染每一类错误，
// 且给出 4xx/5xx 区间内的状态码与非空消息。
//
// 维度取自注册表而非写死协议名：新增入站协议即自动进这套判定。
func TestEveryInboundRendersEveryKind(t *testing.T) {
	for _, name := range codec.InboundNames() {
		in, ok := codec.Inbound(name)
		if !ok {
			t.Fatalf("注册表里有 %s 但取不出来", name)
		}
		for _, kind := range errorKinds() {
			t.Run(name+"/"+string(kind), func(t *testing.T) {
				status, body := in.RenderError(ir.NewError(kind, 0, "", "boom"))
				if status < 400 || status > 599 {
					t.Errorf("status = %d，错误响应的状态码必须落在 4xx/5xx", status)
				}
				if !strings.Contains(string(body), "boom") {
					t.Errorf("body = %s，必须带上消息原文", body)
				}
			})
		}
	}
}

// TestKindStatusIsConsistentAcrossInbound 同一类错误在各入站协议下的状态码
// 必须一致。
//
// 不一致意味着客户端换个端点就会看到不同的 HTTP 语义，而错误类别是本服务
// 归一出来的、与入站协议无关的东西——它不该因渲染端不同而变。
func TestKindStatusIsConsistentAcrossInbound(t *testing.T) {
	for _, kind := range errorKinds() {
		t.Run(string(kind), func(t *testing.T) {
			var want int
			var from string
			for _, name := range codec.InboundNames() {
				in, _ := codec.Inbound(name)
				status, _ := in.RenderError(ir.NewError(kind, 0, "", "boom"))
				if want == 0 {
					want, from = status, name
					continue
				}
				if status != want {
					t.Errorf("%s 给 %d 而 %s 给 %d，同一类错误的状态码必须一致",
						name, status, from, want)
				}
			}
		})
	}
}

// TestUpstreamStatusWinsOverKind 上游给了状态码就以它为准。
// 按类别反查会把上游的 529（过载）改写成 500，丢掉「等一等再来」这层语义。
func TestUpstreamStatusWinsOverKind(t *testing.T) {
	for _, name := range codec.InboundNames() {
		in, _ := codec.Inbound(name)
		t.Run(name, func(t *testing.T) {
			status, _ := in.RenderError(ir.NewError(ir.ErrUpstream, 529, "", "overloaded"))
			if status != 529 {
				t.Errorf("status = %d, want 529（上游原值）", status)
			}
		})
	}
}

// TestStreamErrorCarriesNoNormalTerminator 流内错误收尾不得含正常终止帧。
//
// 这是本轮最容易回退的一条：补闭合帧的改动一旦写过头，就会把失败说成成功，
// 客户端会把残缺内容当完整回答存进历史，下一轮重放时整个请求被上游拒收。
func TestStreamErrorCarriesNoNormalTerminator(t *testing.T) {
	// 各协议表示「这轮正常结束」的帧，出现即为回退。
	normalTerminators := map[string][]string{
		codec.ProtocolAnthropic:       {"message_stop", "message_delta"},
		codec.ProtocolChatCompletions: {"finish_reason"},
		codec.ProtocolResponses:       {"response.completed", "response.incomplete"},
	}
	for _, name := range codec.InboundNames() {
		want, ok := normalTerminators[name]
		if !ok {
			t.Fatalf("新入站协议 %s 未登记它的正常终止帧，矩阵会漏测", name)
		}
		t.Run(name, func(t *testing.T) {
			joined := errorStreamOf(t, name)
			for _, frame := range want {
				if strings.Contains(joined, frame) {
					t.Errorf("错误收尾含正常终止帧 %s：%s", frame, joined)
				}
			}
		})
	}
}

// TestStreamErrorClosesOpenBlocks 已开启的块要在错误帧之前收到闭合帧。
// 缺了它客户端 SDK 的块状态机会悬在半开状态。
func TestStreamErrorClosesOpenBlocks(t *testing.T) {
	for _, name := range codec.InboundNames() {
		closer, ok := blockCloseFrame[name]
		if !ok {
			// 该协议没有块概念，登记在 protocolLimitations 里。
			continue
		}
		t.Run(name, func(t *testing.T) {
			joined := errorStreamOf(t, name)
			at := strings.Index(joined, closer)
			boom := strings.Index(joined, "boom")
			if at < 0 {
				t.Fatalf("缺块闭合帧 %s：%s", closer, joined)
			}
			if at > boom {
				t.Errorf("闭合帧必须排在错误帧之前：%s", joined)
			}
		})
	}
}

// blockCloseFrame 是各入站协议的块闭合帧名。
// chat_completions 缺席：它的流式形态是扁平的 choices[].delta，没有块生命周期。
var blockCloseFrame = map[string]string{
	codec.ProtocolAnthropic: "content_block_stop",
	codec.ProtocolResponses: "output_item.done",
}

// TestStreamErrorHasATerminalEvent 流内错误必须给出本协议的终态信号，
// 否则客户端会一直等一个不会来的事件，挂到自己的超时。
func TestStreamErrorHasATerminalEvent(t *testing.T) {
	// 各协议的失败终态信号。anthropic 的 error 帧自身即终态。
	terminal := map[string]string{
		codec.ProtocolAnthropic:       "event: error",
		codec.ProtocolChatCompletions: "[DONE]",
		codec.ProtocolResponses:       "response.failed",
	}
	for _, name := range codec.InboundNames() {
		want, ok := terminal[name]
		if !ok {
			t.Fatalf("新入站协议 %s 未登记它的失败终态信号", name)
		}
		t.Run(name, func(t *testing.T) {
			if joined := errorStreamOf(t, name); !strings.Contains(joined, want) {
				t.Errorf("缺失败终态 %s：%s", want, joined)
			}
		})
	}
}

// TestErrorParamMatrix param 要么出现在信封里，要么该协议登记了限制。
func TestErrorParamMatrix(t *testing.T) {
	for _, name := range codec.InboundNames() {
		t.Run(name, func(t *testing.T) {
			e := ir.NewError(ir.ErrInvalidRequest, 400, "", "bad value")
			e.Param = "max_tokens"
			in, _ := codec.Inbound(name)
			_, body := in.RenderError(e)
			has := strings.Contains(string(body), "max_tokens")
			if has == inboundDropsParam(name) {
				if has {
					t.Errorf("%s 登记了丢弃 param 却渲染出来了，登记已过期", name)
				} else {
					t.Errorf("%s 丢了 param 却没登记协议限制：%s", name, body)
				}
			}
		})
	}
}

// inboundDropsParam 报告该入站协议是否表达不了 param。
// 从接口实现读而非写死协议名：真的丢维度的协议才实现 LossyErrorRenderer。
func inboundDropsParam(name string) bool {
	in, ok := codec.Inbound(name)
	if !ok {
		return false
	}
	lr, ok := in.(codec.LossyErrorRenderer)
	if !ok {
		return false
	}
	e := ir.NewError(ir.ErrInvalidRequest, 400, "", "bad value")
	e.Param = "probe"
	_, _, notes := lr.RenderErrorLossy(e)
	return len(notes) > 0
}

// TestLossyErrorRendererKeepsBytesStable 诊断开关不得改变客户端看到的字节。
// 两条路径若有出入，排查动作本身就成了故障变量。
func TestLossyErrorRendererKeepsBytesStable(t *testing.T) {
	for _, name := range codec.InboundNames() {
		in, _ := codec.Inbound(name)
		lr, ok := in.(codec.LossyErrorRenderer)
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			for _, kind := range errorKinds() {
				e := ir.NewError(kind, 0, "", "boom")
				e.Param = "max_tokens"
				wantStatus, wantBody := in.RenderError(e)
				gotStatus, gotBody, _ := lr.RenderErrorLossy(e)
				if wantStatus != gotStatus || string(wantBody) != string(gotBody) {
					t.Errorf("%s 两条路径不一致：%d/%s vs %d/%s",
						kind, wantStatus, wantBody, gotStatus, gotBody)
				}
			}
		})
	}
}

// errorStreamOf 走一遍「开块、发增量、报错、收尾」，把所有帧拼成一段文本。
//
// 必须调 Finish()：错误收尾的正确性一半在于它此后什么都不补，
// 不调就测不到「不含正常终止帧」这条。
func errorStreamOf(t *testing.T, inbound string) string {
	t.Helper()
	in, ok := codec.Inbound(inbound)
	if !ok {
		t.Fatalf("取不出入站 codec %s", inbound)
	}
	enc := in.NewStreamEncoder()
	var joined string
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m_1", Model: "native"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "half a sentence"},
		{Type: ir.EvError, Err: ir.NewError(ir.ErrUpstream, 500, "", "boom")},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %s: %v", ev.Type, err)
		}
		for _, f := range frames {
			joined += string(f)
		}
	}
	for _, f := range enc.Finish() {
		joined += string(f)
	}
	return joined
}
