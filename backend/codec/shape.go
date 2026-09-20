package codec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec/schemadialect"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// emptyObjectSchema 是工具 schema 的兜底形态。
//
// 补齐而非省略：anthropic 的 input_schema 必填，gemini 又要求 type 存在，
// 省略字段会拿到不可重试的 400。而「无参数可调用的工具」至少还能被调用。
const emptyObjectSchema = `{"type":"object","properties":{}}`

// ShapeRequest 按目标协议的结构约束调整请求，返回有损说明。
//
// 本函数原地改写 req，调用方须传入自己拥有的副本。
//
// 四个阶段的顺序不可交换：tool_choice 的校验依赖 shapeTools 定下的最终
// 工具集合（被丢弃的工具不能再被指向），采样参数的取舍依赖 thinking
// 是否已被关闭，断点预算依赖 system 折叠后的最终块布局。
//
// 不返回 error：每个分支都有确定的降级路径。返回 error 会逼调用方在
// 「整轮失败」与「忽略错误」之间二选一，而两者都比降级差。
func ShapeRequest(req *ir.Request, name string, caps Capabilities) []string {
	if req == nil {
		return nil
	}
	c := &noteCollector{name: name}
	shapeTools(req, caps, c)
	shapeParams(req, caps, c)
	shapeSystem(req, caps, c)
	budgetCache(req, caps, c)
	// 排在最后：shapeTools 会改工具名、也会丢掉整个工具声明，
	// 而改名会同步改历史里的调用。放在它之前就是对着一批即将变形或
	// 消失的调用算 id 长度。
	shapeToolIDs(req, caps, c)
	return c.notes()
}

// noteCollector 按字段名去重：同一字段在多个工具或多条消息上被改写只报一条。
type noteCollector struct {
	name string
	seen map[string]string
}

func (c *noteCollector) drop(field, why string) {
	c.put(field, fmt.Sprintf("dropped %s (%s cannot express it: %s)", field, c.name, why))
}

func (c *noteCollector) rewrite(field, why string) {
	c.put(field, fmt.Sprintf("rewrote %s (%s cannot express it: %s)", field, c.name, why))
}

func (c *noteCollector) put(field, text string) {
	if c.seen == nil {
		c.seen = map[string]string{}
	}
	// 先到先得：同一字段上第一条原因通常是更具体的那条
	// （方言剔除先于兜底补齐）。
	if _, ok := c.seen[field]; !ok {
		c.seen[field] = text
	}
}

func (c *noteCollector) notes() []string {
	if len(c.seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.seen))
	for _, v := range c.seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// MergeNotes 合并多组有损说明，去重后排序。
func MergeNotes(groups ...[]string) []string {
	seen := map[string]bool{}
	for _, g := range groups {
		for _, n := range g {
			seen[n] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// shapeTools 归一工具 schema，并把 tool_choice 校正到与最终工具集合相容。
func shapeTools(req *ir.Request, caps Capabilities, c *noteCollector) {
	dropServerTools(req, caps, c)
	for i := range req.Tools {
		if req.Tools[i].ServerType != "" {
			// 服务端工具不带我方能理解的参数形状，schema 归一对它无意义，
			// 走一遍只会给它塞上一个空对象 schema 发出去。
			continue
		}
		shapeToolSchema(&req.Tools[i], caps, c)
	}
	shapeToolChoice(req, c)
}

// dropServerTools 在目标协议表达不了服务端工具时把它们整条剔除。
//
// 剔除而不是降级成函数工具：降级后上游会把它当成等客户端回结果的函数，
// 而本服务永远不会回那个结果，对话就停在那里且不报错。剔除的后果是模型
// 少一件工具可用，可见且有说明。
//
// 排在 shapeToolChoice 之前：tool_choice 若正指向被剔除的那一件，
// 剔除后它就指向一个未声明的工具，须由后者降级成 auto。
func dropServerTools(req *ir.Request, caps Capabilities, c *noteCollector) {
	if caps.ServerTools {
		return
	}
	kept := req.Tools[:0]
	for _, t := range req.Tools {
		if t.ServerType == "" {
			kept = append(kept, t)
			continue
		}
		// 字段名带上工具名：说明按字段去重，都写 "tools" 会让声明了两件
		// 服务端工具的请求只报出第一件。
		c.drop(fmt.Sprintf("tools[%s]", t.Name),
			fmt.Sprintf("server-side tool of type %q has no equivalent here", t.ServerType))
	}
	req.Tools = kept
}

func shapeToolSchema(t *ir.Tool, caps Capabilities, c *noteCollector) {
	raw := strings.TrimSpace(t.Schema)
	// 合法性判定不走 Normalize：方言为空时它直接原样返回，坏字节会一路
	// 送到编码器里炸成不可重试的错。schema 坏了不等于这轮对话没救，
	// 把工具降级成「无参数可调用」比整轮 400 对用户更有用。
	if raw != "" && !json.Valid([]byte(raw)) {
		c.rewrite("tool schema", "schema is not valid JSON, replaced with an empty object schema")
		t.Schema = emptyObjectSchema
		return
	}
	if raw == "" {
		t.Schema = emptyObjectSchema
		raw = emptyObjectSchema
	} else if filled, ok := fillObjectType(raw); ok {
		// 缺 type 的 schema 在 anthropic 与 gemini 都会被拒。只补 type 而不是
		// 整体换成空对象：原有的 properties 是模型填参的唯一依据，换掉等于
		// 把工具变成无参可调。
		c.rewrite("tool schema", "schema declares no top-level type, filled in as an object")
		t.Schema = filled
		raw = filled
	}

	res, err := schemadialect.Normalize([]byte(raw), caps.SchemaDialect)
	if err != nil {
		c.rewrite("tool schema", "schema is not valid JSON, replaced with an empty object schema")
		t.Schema = emptyObjectSchema
		return
	}
	if res.Omit {
		// 空 properties 的 parameters 会被本协议拒收，整体省略。
		// 不报有损：没有参数的工具省掉 parameters 没丢任何约束。
		t.Schema = ""
		return
	}
	if res.Changed {
		t.Schema = string(res.Out)
	}
	if res.Truncated {
		c.drop("tool schema", "schema nesting exceeds the normalization depth cap, deeper subtrees passed through as-is")
	}
	// 只有被剔除的关键字才是真丢了约束。type 大写化与联合折叠是把同一
	// 约束换个写法，报进有损会让 gemini 的每个带工具请求都带一条说明，
	// 那个字段就再也指不出哪条路由真的削弱了请求。
	if len(res.DroppedKeys) > 0 {
		c.drop("tool schema", "schema keywords not in this protocol's dialect: "+strings.Join(res.DroppedKeys, ", "))
	}
}

// fillObjectType 在 schema 顶层缺 type 时补上 object，返回补齐后的字节。
// 已有 type、或根不是 JSON 对象时返回 ok=false，调用方照原样使用。
//
// 只看顶层：子 schema 缺 type 上游多能容忍，顶层缺则一定被拒。
func fillObjectType(raw string) (string, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return "", false
	}
	if _, ok := obj["type"]; ok {
		return "", false
	}
	obj["type"] = "object"
	out, err := json.Marshal(obj)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func shapeToolChoice(req *ir.Request, c *noteCollector) {
	if req.ToolChoice == nil {
		return
	}
	if len(req.Tools) == 0 {
		// 上游一律回「tool_choice is only allowed when tools are specified」，
		// 且这是不可重试的 400。
		c.drop("tool_choice", "no tools remain in the request")
		req.ToolChoice = nil
		return
	}
	if req.ToolChoice.Mode != ir.ToolChoiceTool {
		return
	}
	for _, t := range req.Tools {
		if t.Name == req.ToolChoice.Name {
			return
		}
	}
	// 指向未声明的工具同样是不可重试的 400。降级 auto 而非丢弃：
	// 客户端的意图是「要用工具」，auto 比完全不给保留得更多。
	c.rewrite("tool_choice", "the named tool is not declared in this request, downgraded to auto")
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
}

// forcedToolChoice 判断 tool_choice 是否强制模型调工具。
//
// 只有 any 与具名算强制：auto 是「模型自己决定」，none 是「不要调」，
// 两者都不与推理冲突，把它们算进来会无端关掉推理。
func forcedToolChoice(tc *ir.ToolChoice) bool {
	if tc == nil {
		return false
	}
	return tc.Mode == ir.ToolChoiceAny || tc.Mode == ir.ToolChoiceTool
}

// shapeParams 解开参数互斥并套上数量上限。
func shapeParams(req *ir.Request, caps Capabilities, c *noteCollector) {
	thinkingOn := req.Thinking.On() && caps.Thinking

	// 预算必须低于 max_tokens：推理预算是从输出上限里划出来的，两者相等
	// 意味着留给回答本身的 token 为零。客户端同时给出两个数字时它们可能
	// 冲突（合法的入站形状），原样出站会拿到不可重试的 400——换目标也无用。
	//
	// 夹紧预算而不是抬 max_tokens：max_tokens 是客户端对成本与响应长度的
	// 约束，抬它是替客户端花钱，还会让「我只要 4096 个 token」回出更长的
	// 内容。预算只是「想多久」，调小它只降质量。
	//
	// 排在关掉 thinking 之前：夹后的值可能低于协议下限，那时应当落到关掉
	// 那一支。反过来先关后夹会漏掉这条路径。
	// 取协议的有效上限而非客户端给的那个数：客户端不给 max_tokens 时
	// 编码器随后会补上 DefaultMaxTokens（anthropic 是 4096），而那一步在
	// 整形之后。只看 req.MaxTokens 会让「不给 max_tokens + 大 budget」这条
	// 路径整个逃过夹紧，出站成 max_tokens=4096 / budget=60000，
	// 拿到上游 400 budget_tokens must be less than max_tokens——
	// 参数错误不可重试，且归因指向上游而不是我们。
	effMax, hasMax, err := MaxTokensFor(req.MaxTokens, caps)
	if err != nil {
		// 协议要求 max_tokens 却没配默认值，这是配置缺陷而非请求问题。
		// 整形阶段不报错（本函数只返回说明），留给编码器那一处报同一个错。
		effMax, hasMax = 0, false
	}

	if thinkingOn && hasMax && req.Thinking.BudgetTokens >= effMax {
		was := req.Thinking.BudgetTokens
		req.Thinking.BudgetTokens = effMax - 1
		c.rewrite("thinking.budget_tokens", fmt.Sprintf(
			"%d is not below max_tokens %d, clamped to %d",
			was, effMax, req.Thinking.BudgetTokens))
	}

	if thinkingOn && caps.MinThinkingBudget > 0 && hasMax &&
		effMax-1 < caps.MinThinkingBudget {
		// 预算必须同时低于 max_tokens 且不低于协议下限，两个约束在
		// max_tokens 过小时无解。关掉 thinking 保住这一轮，
		// 而不是发一个注定被拒的请求。
		c.drop("thinking", "max_tokens leaves no room for this protocol's minimum reasoning budget")
		req.Thinking = nil
		thinkingOn = false
	}

	if thinkingOn && caps.ThinkingExcludesForcedTools && forcedToolChoice(req.ToolChoice) {
		// 关推理而不是降级工具约束：降级约束的故障不可见，上游会正常回一段
		// 文本，调用方以为模型自己决定不调工具。
		//
		// 这一步必须排在采样参数互斥之前：关了推理，temperature / top_p 就不
		// 再需要剥离。顺序反了会先白丢采样参数，再把推理也关掉——两样都没了，
		// 而实际上只该丢一样。
		c.drop("thinking", "reasoning cannot be combined with a forced tool choice")
		req.Thinking = nil
		thinkingOn = false
	}

	if thinkingOn && caps.ThinkingExcludesSampling {
		if req.Temperature != nil || req.TopP != nil {
			c.drop("temperature/top_p", "sampling parameters must be absent while reasoning is enabled")
			req.Temperature = nil
			req.TopP = nil
		}
	}

	// 取值范围夹紧排在采样参数互斥之后：那一支会把 temperature 整个剥掉，
	// 剥掉之后没有值可夹。顺序反了会先夹一个马上要被丢弃的值，白报一条说明。
	//
	// 夹紧而不是拒请求：目标协议是调度层选的，客户端按 OpenAI 习惯发
	// temperature 1.5 是合法入站，它无从预知这一跳会落到 Anthropic。
	// 拒掉等于把调度的内部选择变成客户端的错误。夹紧会改变输出的随机性，
	// 所以报一条有损说明让调用方看得见。
	if caps.MaxTemperature > 0 && req.Temperature != nil {
		// 下界 0 与上界同出一处证据：上游 400 原文是 range: 0..1，
		// 两端都在这句话里，所以不另设一个能力位。
		switch {
		case *req.Temperature > caps.MaxTemperature:
			was := *req.Temperature
			v := caps.MaxTemperature
			req.Temperature = &v
			c.rewrite("temperature", fmt.Sprintf(
				"%g exceeds this protocol's maximum %g, clamped", was, caps.MaxTemperature))
		case *req.Temperature < 0:
			was := *req.Temperature
			v := 0.0
			req.Temperature = &v
			c.rewrite("temperature", fmt.Sprintf(
				"%g is below this protocol's minimum 0, clamped", was))
		}
	}

	if caps.MaxStopSequences > 0 && len(req.StopSequences) > caps.MaxStopSequences {
		c.drop("stop_sequences", fmt.Sprintf("at most %d stop sequences", caps.MaxStopSequences))
		req.StopSequences = req.StopSequences[:caps.MaxStopSequences]
	}
}

// shapeSystem 把系统提示折成本协议能承载的形态。
//
// 清空块对所有协议生效。折成单一字符串只对 SystemAsText 的协议生效：
// 那些协议的编码器只会取文本块，非文本块会被静默吃掉——客户端放在
// system 里的截图就这样消失了。
func shapeSystem(req *ir.Request, caps Capabilities, c *noteCollector) {
	// 空文本块先清掉，与协议无关：写出 system:[{"type":"text"}] 或空字符串
	// 会被部分上游拒收，而空块本身不承载任何信息，清掉不算有损。
	kept := req.System[:0]
	for _, b := range req.System {
		if b.Type == ir.BlockText && b.Text == "" {
			continue
		}
		kept = append(kept, b)
	}
	req.System = kept
	if len(req.System) == 0 {
		req.System = nil
		return
	}
	if !caps.SystemAsText {
		return
	}
	parts := make([]string, 0, len(req.System))
	cacheCtl := ""
	for _, b := range req.System {
		switch {
		case b.Type == ir.BlockText:
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case b.Type.IsMedia():
			c.rewrite("system media", "system prompts carry text only, attachment described in words instead")
			parts = append(parts, DowngradeMedia(b).Text)
		default:
			c.drop(string(b.Type)+" blocks in system", "system prompts carry text only")
		}
		if b.CacheCtl != "" {
			cacheCtl = b.CacheCtl
		}
	}
	if len(parts) == 0 {
		// 空 system 不写字段：写出空字符串或空 parts 数组会被部分上游拒收。
		req.System = nil
		return
	}
	// 折成一块并以空行分隔：编码器的 joinText 无分隔拼接，
	// 分隔符必须在这里就写进文本，否则相邻两段会粘成一句。
	req.System = []ir.Block{{
		Type:     ir.BlockText,
		Text:     strings.Join(parts, "\n\n"),
		CacheCtl: cacheCtl,
	}}
}

// budgetCache 把缓存断点裁到协议上限内。
//
// 只裁不加：主动注入断点需要知道模型的缓存最小 token 数与计费策略，
// 那属于上游配置层的知识，不在 codec 的职责范围。
func budgetCache(req *ir.Request, caps Capabilities, c *noteCollector) {
	if caps.CacheBreakpoints <= 0 {
		// 不支持断点的协议由 DescribeLossy 统一报，避免同一件事报两条。
		return
	}
	marked := collectCacheMarks(req)
	excess := len(marked) - caps.CacheBreakpoints
	if excess <= 0 {
		return
	}
	// 从最靠前的开始丢：靠后的断点覆盖更长的前缀，命中时省得更多。
	for i := 0; i < excess; i++ {
		*marked[i] = ""
	}
	c.drop("cache_control", fmt.Sprintf("at most %d cache breakpoints, earliest ones dropped", caps.CacheBreakpoints))
}

// collectCacheMarks 按出现顺序收集所有带断点的块，返回可写指针。
func collectCacheMarks(req *ir.Request) []*string {
	var out []*string
	walk := func(blocks []ir.Block) {
		for i := range blocks {
			if blocks[i].CacheCtl != "" {
				out = append(out, &blocks[i].CacheCtl)
			}
			if blocks[i].ToolResult != nil {
				for j := range blocks[i].ToolResult.Content {
					nested := &blocks[i].ToolResult.Content[j]
					if nested.CacheCtl != "" {
						out = append(out, &nested.CacheCtl)
					}
				}
			}
		}
	}
	walk(req.System)
	for i := range req.Messages {
		walk(req.Messages[i].Content)
	}
	return out
}
