package codec

import "fmt"

// PayloadBudgetNote 在出站请求体超出协议预算时给出说明，未超限返回空串。
//
// 只测量不改写。裁历史是有损且不可逆的语义改动：静默丢掉几轮对话会让模型
// 失忆，而客户端从 200 响应里看不出任何异常——正是本轮要避免的那类故障。
// 先让体积问题在诊断里可见，裁不裁由调用方按业务决定。
//
// 也不在本地拒收：上限是按实测推出的估计值，上游可能就接受了。
// 本地拒收会把这份余地一并剥掉。
func PayloadBudgetNote(body []byte, name string, caps Capabilities) string {
	if caps.MaxPayloadBytes <= 0 {
		return ""
	}
	if len(body) <= caps.MaxPayloadBytes {
		return ""
	}
	// 措辞里点明「误导性」：实测中这类超限回的是一个 reason 为空的 400
	// Improperly formed request，排查者不会想到是体积问题。
	return fmt.Sprintf(
		"request body is %d bytes, over the %d-byte budget for %s "+
			"(the upstream may reject it with a misleading 400)",
		len(body), caps.MaxPayloadBytes, name)
}
