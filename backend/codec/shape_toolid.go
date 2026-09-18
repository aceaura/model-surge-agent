package codec

import "github.com/aceaura/model-surge-agent/backend/ir"

// maxIDCollisionRetries 是收敛结果撞车后的重取上限。
// 放弃优于死循环：放弃时保留原 id，配对关系至少仍然成立。
const maxIDCollisionRetries = 16

// shapeToolIDs 把工具调用 id 收敛到本协议的长度上限以内。
//
// 上限为零（四协议当前都是）时整段跳过：没有实测证据的上限是猜的，
// 按猜的值改写 id 只会把本来能过的请求改坏。
//
// 合成 id 的省略不在这里做——那是 wire 层的事（见 gemini 的 outboundToolID）。
// 这里只管长度，因为长度约束是协议级的、而省略与否取决于 id 字段可选性。
func shapeToolIDs(req *ir.Request, caps Capabilities, c *noteCollector) {
	if caps.MaxToolIDLen <= 0 {
		return
	}

	// 先收集所有已占用的 id：收敛结果不能撞上任何一个既有 id，
	// 撞上就等于把两次调用并成一次，客户端会把两份入参串成非法 JSON。
	taken := map[string]bool{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				taken[b.ToolUse.ID] = true
			}
		}
	}

	renames := map[string]string{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type != ir.BlockToolUse || b.ToolUse == nil {
				continue
			}
			id := b.ToolUse.ID
			if len(id) <= caps.MaxToolIDLen || renames[id] != "" {
				continue
			}
			short := uniqueShortID(id, caps.MaxToolIDLen, taken)
			if short == id {
				// 重试用尽：保留原 id。发一个可能超限的 id 比发一个
				// 与别的调用撞号的 id 好——后者会让入参串味。
				continue
			}
			renames[id] = short
			taken[short] = true
			c.rewrite("tool call id", "the call id is longer than this protocol accepts")
		}
	}
	if len(renames) == 0 {
		return
	}
	applyToolIDRenames(req, renames)
}

// uniqueShortID 收敛 id 并避开已占用的结果。
func uniqueShortID(id string, max int, taken map[string]bool) string {
	for salt := 0; salt < maxIDCollisionRetries; salt++ {
		short := shortenToolID(id, max, salt)
		if short == id || !taken[short] {
			return short
		}
	}
	return id
}

// applyToolIDRenames 同步改写调用与结果两侧。
//
// 只改一侧会让配对断开，上游拿到一个指向不存在调用的结果，
// 整轮请求被拒——比 id 超限更糟。
func applyToolIDRenames(req *ir.Request, renames map[string]string) {
	for mi := range req.Messages {
		for bi := range req.Messages[mi].Content {
			b := &req.Messages[mi].Content[bi]
			switch {
			case b.Type == ir.BlockToolUse && b.ToolUse != nil:
				if to := renames[b.ToolUse.ID]; to != "" {
					b.ToolUse.ID = to
				}
			case b.Type == ir.BlockToolResult && b.ToolResult != nil:
				if to := renames[b.ToolResult.ToolUseID]; to != "" {
					b.ToolResult.ToolUseID = to
				}
			}
		}
	}
}
