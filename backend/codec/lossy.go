package codec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/textsafe"
)

// foreignSigPrefixes 是别家协议的推理密文特征前缀。
//
// 命中即剥离，即使 SignatureFrom 声称同族：声明可能来自伪造，也可能来自
// 本服务早期版本写下的历史数据。把别家的密文发给上游会拿到不可重试的 400，
// 而不可重试意味着换目标也救不回来。
var foreignSigPrefixes = map[string]string{
	// OpenAI/Codex 系的加密推理载荷，归属 responses 协议。
	"gAAAA": ProtocolResponses,
}

// prefixSaysForeign 按密文特征前缀判断签名是否属于别家。
//
// 归属协议必须参与判定：早先这里只看前缀不看协议名，于是 responses 自己的
// gAAAA 密文发回 responses 上游时也被当异族剥掉，该协议上的多轮推理
// 永远拿不到签名，而上游又要求带签名才接受带思考的续写。
func prefixSaysForeign(signature, name string) bool {
	for p, owner := range foreignSigPrefixes {
		if strings.HasPrefix(signature, p) {
			return owner != name
		}
	}
	return false
}

// DescribeLossy 按出站能力位推导本次编码会丢弃哪些 IR 字段。
//
// 只看 IR 与 Caps，不看编码结果：编码器丢字段是无声的，事后从请求体反推
// 会漏掉「本来就没编出去」的情形。返回值已去重并排序，令同一字段在多条
// 消息上被丢弃只报一条，且顺序稳定便于落库比对。
//
// jsonContainerEmpty 报告一段原文 JSON 是否为「空容器/空值」——null、空数组 []、
// 空对象 {}（含任意内部空白，如 [ ] / { }）都算空。用于请求侧探测式注记：
// mcp_servers / context_management / access_programs 这类 RawMessage 透传字段，
// 客户端显式给了空数组/空对象时（部分 SDK 会把「空列表」序列化成 [] 而非省略），
// 语义等于「什么都没声明」，跨族丢弃它不损失任何信息，不该报「声明的 X 被丢弃」
// 的假阳性注记——与轮次53 responses output_text.logprobs:[] 被误计为丢弃同款
// 「规范空值被当成真载荷」缺口，违反「注记当且仅当真实丢弃」这条不变量（误报与
// 漏报同为缺口）。用 json.Compact 归一空白而非裸字符串比较，兼容 [ ]/{ }。非法
// JSON 或标量按「非空」保守处理：真给了东西宁可报、不可漏。
func jsonContainerEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return false
	}
	switch buf.String() {
	case "null", "[]", "{}":
		return true
	}
	return false
}

// 无丢弃时返回 nil。
func DescribeLossy(req *ir.Request, name string, caps Capabilities) []string {
	if req == nil {
		return nil
	}
	notes := map[string]string{}
	note := func(field, why string) {
		notes[field] = fmt.Sprintf("dropped %s (%s cannot express it: %s)", field, name, why)
	}
	// filled 与 note 分开：兜底不是丢弃，套上 "dropped" 的措辞会让读者
	// 以为客户端给的值被扔了，而真相是它根本没给、这个值是本服务加的。
	filled := func(field, why string) {
		notes[field] = fmt.Sprintf("filled in %s (%s requires it: %s)", field, name, why)
	}
	// unreturned 是第三种形态：字段照原样发给了上游、上游也会照办，
	// 只是结果回不到客户端手里。既不是 dropped（没丢，发出去了）
	// 也不是 filled（没兜底）。措辞混用会让读者以为换个目标就有。
	unreturned := func(field, why string) {
		notes[field] = fmt.Sprintf("forwarded %s but the result is not returned (%s)", field, why)
	}
	// rewrote 是第四种形态：这一维承载不了，但没丢——本服务把它改写成了
	// 目标协议里能表达的另一种形状。措辞与 dropped 分开，因为读者对这两者
	// 的下一步动作不同：dropped 要考虑换目标，rewrote 要考虑上游会怎么读。
	rewrote := func(field, why string) {
		notes[field] = fmt.Sprintf("rewrote %s (%s cannot express it: %s)", field, name, why)
	}

	if len(req.Tools) > 0 && !caps.Tools {
		note("tools", "no tool calling")
	}
	if !caps.ToolStrict {
		// 报数不报值：客户端关心的是「几个工具的保证没了」，逐个列名
		// 会把说明拖成工具清单；显式 false 也计数——它同样是客户端的
		// 表态（明确不要严格校验），到了没有这一维的协议一样无从表达。
		n := 0
		for _, t := range req.Tools {
			if t.Strict != nil {
				n++
			}
		}
		if n > 0 {
			notes["strict flag"] = fmt.Sprintf(
				"dropped strict flag on %d tool(s): the target protocol has no schema-strictness switch, tool call arguments are not guaranteed to validate against the schema", n)
		}
	}
	if name != ProtocolAnthropic {
		// anthropic 工具定义的 2026 修饰四维（defer_loading /
		// eager_input_streaming / input_examples / allowed_callers）其余协议
		// 一个都没有。四维任一出现即计数该工具；报数不报值。
		n := 0
		for _, t := range req.Tools {
			if t.DeferLoading || t.EagerInputStreaming != nil ||
				len(t.InputExamples) > 0 || len(t.AllowedCallers) > 0 {
				n++
			}
		}
		if n > 0 {
			notes["tool modifiers"] = fmt.Sprintf(
				"dropped tool modifiers on %d tool(s): the target protocol has no defer-loading, eager-streaming, input-example or caller-restriction fields, tools behave with the upstream defaults", n)
		}
	}
	if !caps.CacheControl {
		// tools[].cache_control 也是缓存断点：块级断点由
		// describeBlocksLossy 报，这里数工具定义上的，报数不报值。
		// 断点蒸发后缓存命中率与计费都变，客户端却看不到任何迹象。
		n := 0
		for _, t := range req.Tools {
			if t.CacheCtl != "" {
				n++
			}
		}
		if n > 0 {
			notes["tool cache breakpoints"] = fmt.Sprintf(
				"dropped cache breakpoint on %d tool definition(s): the target protocol has no prompt-caching breakpoint parameter, cached prefixes may be reprocessed and billed", n)
		}
	}
	if !caps.ToolResultError && hasFailedToolResult(req) {
		rewrote("tool_result.is_error",
			fmt.Sprintf("prefixed the content with %q", toolErrorPrefix))
	}
	// 畸形工具参数与协议无关：任何出站都会发生处置（对象槽位挪进
	// RawArgsKey，字符串槽位原样透出但工具侧多半也解析不了），
	// 一律报告，只是措辞按槽位形态分开——两种后果不同，读者要改的
	// 地方也不同。
	if bad := CountMalformedToolArgs(req); bad > 0 {
		if caps.ToolInputObject {
			notes["tool arguments"] = fmt.Sprintf(
				"rewrapped %d tool call argument(s): malformed or non-object JSON moved to %s, the tool will not receive its parameters",
				bad, ir.RawArgsKey)
		} else {
			notes["tool arguments"] = fmt.Sprintf(
				"passed through %d malformed tool call argument(s) verbatim: the tool will fail to parse them", bad)
		}
	}
	if req.TopK != nil && !caps.TopK {
		note("top_k", "no top_k parameter")
	}
	if len(req.StopSequences) > 0 && !caps.StopSequences {
		note("stop_sequences", "no stop sequence parameter")
	}
	// 只有明确开启才算丢失：明确关闭在不支持推理的协议上本就是要的结果，
	// 没提则什么都没被丢。
	if req.Thinking.On() && !caps.Thinking {
		note("thinking", "no reasoning mode")
	}
	describeThinkingModernLossy(req, name, caps, note, filled, rewrote)
	describeParamsLossy(req, name, caps, note, filled, unreturned)

	describeBlocksLossy(req.System, name, caps, note)
	for _, m := range req.Messages {
		describeBlocksLossy(m.Content, name, caps, note)
	}

	// 历史里的拒绝正文在目标协议没有 refusal 槽位时被并进普通文本发出。
	// 措辞用 merged 而不是 dropped：内容没丢，丢的是「这是拒绝」的标记——
	// 上游模型看不出自己上一轮拒绝过，可能被同样的追问绕过。
	if !caps.Refusal {
		if n := countRequestRefusals(req); n > 0 {
			notes["refusal blocks"] = fmt.Sprintf(
				"merged %d refusal(s) into plain text: the target protocol has no refusal field, the model cannot tell it previously refused", n)
		}
	}

	// 工具结果内容里的非文本子块（拒绝、不透明块）：responses 与 gemini 的出站把
	// 整段工具结果用 joinText 折成一个纯文本串，只留 text 子块，折掉的子块此前无人
	// 报。与顶层不透明块的处置不同——顶层跨族会硬报错（见各编码器 BlockOpaque 分支），
	// 嵌在工具结果里却被 joinText 静默吞掉，故这里补一条说明（rule a：丢弃必须有注记）。
	// 只数 responses/gemini 两族：anthropic（encodeBlocks）与 chat（encodeContent）
	// 递归编码工具结果子块，跨族不透明块硬报错、拒绝另有处置，都不经 joinText 折平；
	// 媒体子块由 shape.go 的 moveToolResultMedia 抽出并单独报，也不在此列，故只数
	// refusal/opaque 两种确会出现且确被折掉的类型，避免与那两条重叠。
	if name == ProtocolResponses || name == ProtocolGemini {
		if n := countToolResultFlattenedContent(req); n > 0 {
			notes["tool_result non-text content"] = fmt.Sprintf(
				"dropped %d non-text part(s) nested in tool_result content (refusal/opaque): this protocol flattens tool output to a plain string, so only the text survives", n)
		}
	}

	// 顶层 cache_control 便捷糖官方语义=自动一个缓存断点，与块级断点同维度，
	// 共用同一条说明（notes 按字段去重，两者并存也只报一条）。
	if req.TopCacheCtl != "" && !caps.CacheControl {
		note("cache_control", "no prompt-caching breakpoint parameter")
	}
	// 推理地理偏好同为 anthropic 专属：外族没有任何对应参数，丢了请求会
	// 落到上游默认区域，合规敏感的客户端必须知道。值不回显（客户端自选值
	// 不入诊断）。
	if req.InferenceGeo != "" && name != ProtocolAnthropic {
		note("inference_geo", "no geographic-region preference, inference runs wherever the upstream's default region is")
	}
	// 代码执行容器复用标识与技能声明：外族没有容器概念，丢了上游只能开
	// 新容器、技能不加载，客户端期待的状态全丢。anthropic 同族原样往返，
	// 报了就是谎报。
	if req.Container != nil && name != ProtocolAnthropic {
		note("container", "no code-execution container reuse or skill declaration, the upstream starts with a fresh container and no skills loaded")
	}
	// MCP 服务器声明（mcp_servers，anthropic beta）：外族没有 MCP 连接器槽位，
	// 声明的外部 MCP 服务器整条丢弃，上游无从发现或调用其工具。anthropic 同族
	// 原样往返（RawMessage 透传），报了就是谎报。
	if !jsonContainerEmpty(req.McpServers) && name != ProtocolAnthropic {
		note("mcp_servers", "no MCP connector slot: the declared external MCP servers are dropped, the upstream cannot discover or call their tools")
	}
	// 上下文管理（context_management，anthropic beta）：外族没有上下文编辑槽位，
	// 客户端请求的上下文裁剪（如清除旧工具调用）整条丢弃，上游发送未裁剪的完整
	// 上下文，可能撞上客户端本以为会被裁掉的窗口上限。anthropic 同族原样往返
	// （RawMessage 透传），报了就是谎报。
	if !jsonContainerEmpty(req.ContextManagement) && name != ProtocolAnthropic {
		note("context_management", "no context-editing slot: the requested context management (e.g. clearing old tool uses) is dropped, the upstream sends the full unpruned context and may hit the window limit the client expected to be pruned")
	}
	// 请求级诊断（diagnostics，anthropic：{previous_message_id}）：外族没有承载
	// 「带上一轮 msg_id 以换取 prompt-cache 失配归因」的槽位，整条丢弃后客户端拿不到
	// 响应里的 diagnostics.cache_miss_reason（responses 的 prompt_cache_options.
	// comparison_response_id 是类似机制但线格式不同、不互映）。anthropic 同族原样
	// 往返（RawMessage 透传），报了就是谎报。
	if !jsonContainerEmpty(req.Diagnostics) && name != ProtocolAnthropic {
		note("diagnostics", "no request-level diagnostics slot: the previous_message_id used to opt into prompt-cache divergence reporting is dropped, so the response cannot carry diagnostics.cache_miss_reason explaining why the prompt-cache prefix was not reused")
	}
	// 域专属访问计划（access_programs，responses 独有，{cyber: standard|
	// daybreak_blue|daybreak_red}）：外族没有访问计划槽位，客户端显式选定的 cyber
	// 档位整条丢弃，上游按模型 tier 与组织/项目权限自行解析默认计划（通常是
	// standard），可能落到与客户端意图不同的档位。responses 同族原样往返
	// （RawMessage 透传），报了就是谎报。值是官方枚举非敏感串，但为与同槽位的
	// 其它访问/档位参数保持一致，不回显具体档位。
	if !jsonContainerEmpty(req.AccessPrograms) && name != ProtocolResponses {
		note("access_programs", "no domain-specific access-program slot: the client's explicit cyber access program (standard/daybreak_blue/daybreak_red) is dropped and the upstream resolves its own default program from the model tier and org/project access")
	}
	// 消息级发送者名（chat 的 message.name）：只有 chat_completions 解码器落进
	// IR、只有 chat_completions 编码器回写，其余族没有逐消息作者名字段，跨族整条
	// 丢失（rule a）。同族 chat→chat 原样保留，故报了就是谎报，排除之。
	if name != ProtocolChatCompletions {
		if n := countMessageNames(req); n > 0 {
			notes["message sender name"] = fmt.Sprintf(
				"dropped the sender name on %d message(s): the target protocol has no per-message author-name field, the model cannot tell which participant sent them", n)
		}
	}
	// responses 条目原号（item id）：只有 responses 解码器落进 IR、只有 responses
	// 编码器回写，其余族没有条目 id 槽位。跨族丢弃后，客户端稍后凭 store=true 发来的
	// item_reference 无处解析（ir.ToolUse.ItemID 注释已明确承诺此丢弃由有损诊断报出，
	// 此前却无人兑现）。同族 responses→responses 原样回写，排除之。
	if name != ProtocolResponses {
		if n := countItemIDs(req); n > 0 {
			notes["item id"] = fmt.Sprintf(
				"dropped the responses item id on %d item(s): the target protocol has no item-id slot, a stored-item reference the client sends later will not resolve", n)
		}
	}
	// responses assistant message 的 phase（commentary|final_answer）：只有
	// responses 解码器落进 IR、只有 responses 编码器回写，其余族没有对应槽位。
	// 官方要求后续请求 preserve-and-resend，跨族丢弃后上游无从区分旁白与最终
	// 答复。同族 responses→responses 原样回写，排除之（与 item id 同款门控）。
	if name != ProtocolResponses {
		if n := countMessagePhase(req); n > 0 {
			notes["message phase"] = fmt.Sprintf(
				"dropped the responses message phase (commentary/final_answer) on %d assistant message(s): the target protocol has no phase slot, the model cannot tell commentary from the final answer", n)
		}
	}
	// responses reasoning 条目的通道来源（content 通道 reasoning_text=模型内部推理
	// 原文 vs summary 通道 reasoning_summary_text=给用户看的摘要，见
	// ir.Thinking.ContentChannel）：只有 responses 解码器落进 IR、只有 responses
	// 编码器据此选回哪条通道，其余族的思考块只有一个正文槽，通道 provenance 无处
	// 安放。跨族时正文逐字保留（anthropic thinking / gemini thought part / chat
	// reasoning_content 都写回同一段文本），丢的只是「这段是原始推理还是摘要」这个
	// 标记——与 message phase 同款「内容不丢、语义标签丢」。同族 responses→responses
	// 按标记选回原通道，原样往返，报了就是谎报，排除之（与 item id / phase 同款门控）。
	// 判据与解码器同口径（ContentChannel 仅在 content 通道有正文时置真，故必带非空
	// 正文，跨族正文一定保留、不会被空壳跳过）。
	if name != ProtocolResponses {
		if n := countContentChannelReasoning(req); n > 0 {
			notes["reasoning channel"] = fmt.Sprintf(
				"dropped the content-channel marker on %d reasoning block(s): the reasoning text is preserved verbatim, but the target protocol has no reasoning-channel slot, so its model cannot tell the model's raw internal reasoning (reasoning_text) from a user-facing summary (reasoning_summary_text)", n)
		}
	}
	// responses 的 typed tool_choice（mcp/file_search/computer_use 等无 name
	// 变体，IR 的 Raw 不透明槽、Mode 留零值）：外族的 tool_choice 形状只有
	// auto/any/none/具名函数四档，托管工具指名变体整条编不出，出站缺省后
	// 模型自由选工具。responses 同族原样回写，报了就是谎报；带 Mode 的
	// 指名变体（function/custom）结构化字段照常编码，不在这里报。
	if req.ToolChoice != nil && req.ToolChoice.Mode == "" && len(req.ToolChoice.Raw) > 0 && name != ProtocolResponses {
		note("tool_choice", "no typed tool-choice variant (mcp/file_search and friends), the choice was dropped and the model picks tools freely")
	}

	// 历史里的 custom 工具调用与结果（responses 的 custom_tool_call /
	// custom_tool_call_output 条目）：自由文本入参的调用形态 responses 与
	// chat（type=custom 原生 tool call）两族都有，跨到其余族时调用降级成
	// 普通函数调用（自由文本包成 {"input":…} 投影落进 JSON 参数槽）。
	// 结果只有 responses 有原生条目形态：chat 的 tool 消息没有自由文本
	// 结果槽位，跨族时结果降级成普通函数结果。内容不丢但形态变了：
	// 上游模型看到的是一段包在对象里的文本。同族原样往返，不报。
	if name != ProtocolResponses {
		if calls, results := countRequestCustomTools(req); calls > 0 || results > 0 {
			if calls > 0 && name != ProtocolChatCompletions {
				notes["custom tool calls"] = CustomToolDowngradeNote(calls)
			}
			if results > 0 {
				notes["custom tool outputs"] = CustomToolOutputDowngradeNote(results)
			}
		}
	}

	// chat 一族专属四维（modalities/audio/prediction/web_search_options）：
	// responses 全系没有对应槽位，不作映射尝试，跨族丢了照实报。
	// audio 依附 modalities：模态丢了音频配置必然随之丢，合并成一则；
	// prediction 与 web_search_options 各自独立。chat 同族原样往返，不报。
	if name != ProtocolChatCompletions {
		if len(req.Modalities) > 0 || req.AudioOut != nil {
			note("modalities/audio", "no audio-output request, the response will be text-only")
		}
		if len(req.Prediction) > 0 {
			note("prediction", "no predicted-output parameter, the regeneration speedup the client asked for will not happen")
		}
		if len(req.WebSearchOptions) > 0 {
			note("web_search_options", "no web-search tuning parameter, search behavior follows the upstream default")
		}
	} else {
		// chat 目标的 modalities 值集只有 text/audio：其余值（如 image）被
		// 编码器滤掉——写出去是上游必 400 的形状，这里报出来。
		var unsupported []string
		for _, m := range req.Modalities {
			if m != "text" && m != "audio" {
				unsupported = append(unsupported, m)
			}
		}
		if len(unsupported) > 0 {
			note(fmt.Sprintf("modalities %s", strings.Join(unsupported, "/")),
				"chat completions accepts only text/audio output modalities, the response will not include that output")
		}
	}

	// 历史里的托管工具块（server_tool_use / web_search_tool_result）：三个
	// 外族出站编码器都整块跳过。两种块型成对出现、一起丢反而不撕毁
	// tool_use/tool_result 配平，上游不会拒——但模型看不到自己上一轮让
	// 网关搜了什么、搜回了哪些页面，只能重新搜一遍。
	// anthropic 一律不报：同族原样往返，报了就是谎报。
	if name != ProtocolAnthropic {
		if calls, results := countRequestServerTools(req); calls > 0 || results > 0 {
			notes["server tool blocks"] = ServerToolDropNote(calls, results)
		}
		// 历史里的容器文件引用块（container_upload）：外族没有 file_id 槽位，
		// 三个外族出站编码器都整块跳过。模型看不到客户端上一轮送进容器的文件
		// 或模型自己产出的文件引用，只能当作文件不存在继续。file_id 不回显。
		// anthropic 一律不报：同族原样往返，报了就是谎报。
		if n := countRequestContainerUploads(req); n > 0 {
			notes["container upload blocks"] = ContainerUploadDropNote(n)
		}
		// 历史里 document 块的两项配置（context 用途旁注与 citations.enabled
		// 引用开关）：外族附件槽位只装文件本身，两项配置跨族必丢。
		// anthropic 一律不报：同族原样往返，报了就是谎报。
		if ctx, cites := countRequestDocConfig(req); ctx > 0 || cites > 0 {
			notes["document config"] = DocumentConfigDropNote(ctx, cites)
		}
		// 系统提示（req.System）块上的来源标注：三外族都把 system 收敛成纯文本
		// （responses 的字符串 instructions / chat 的 system 消息 text 槽 / gemini 的
		// systemInstruction part），系统提示在任何协议都没有 annotations 槽位，引用
		// 整组丢弃。与上面 Messages 侧的 citations/stray citations 分账——那三个
		// 计数器（CountCitations/CountNonPortableCitations/CountStrayPortableCitations）
		// 只遍历 req.Messages、看不见独立的 req.System，故系统提示引用此前一律静默。
		// anthropic 一律不报：同族经 encodeBlocks 写回 Citations、原样往返。
		if n := ir.CountSystemCitations(req); n > 0 {
			notes["system citations"] = SystemCitationDropNote(n)
		}
	}

	// assistant 历史里的音频引用（chat 多轮音频上下文的 {audio:{id}}）：
	// 外族协议没有 replay-id 槽位，模型看得到文字却取不回前几轮生成的音频。
	// id 是上游的服务端引用，属会话内容，值不进注记。
	// chat 同族原样回传，不报。
	if name != ProtocolChatCompletions {
		if n := countRequestAudioRefs(req); n > 0 {
			notes["assistant audio references"] = AudioRefDropNote(n)
		}
	}

	if !caps.Citations {
		if n := ir.CountCitations(req); n > 0 {
			// 历史消息里的来源标注会被抹掉。读者能做的是别指望模型在后续轮次里
			// 复述出处——它看到的正文已经不带任何来源了。
			notes["citations"] = fmt.Sprintf(
				"dropped %d citation(s): upstream protocol has no slot for source annotations", n)
		}
	} else if name != ProtocolAnthropic {
		// 有槽位，但槽位以 URL 为来源身份：Anthropic 的文档类引用只有
		// document_index 与页/块/字符下标，装不进去。caps.Citations 是个
		// 整族布尔量，看不见这种「逐条装不下」的损耗，必须单独数。
		// anthropic 不报：五种形态它都装得下（同族往返走 Raw 原样带回）。
		if n := ir.CountNonPortableCitations(req); n > 0 {
			notes["citations"] = CitationDropNote(n)
		}
		// chat 独有的一类：可移植引用（有 URL）挂在非 assistant 消息上时，
		// 编码器只在 assistant 消息写 annotations（encode_request.go 的 role
		// 门控），user/system/tool 消息的引用被静默丢弃。这类引用形态没问题、
		// CountNonPortableCitations 数不到，必须按角色单独数。
		// responses 不分角色一律写 annotations（input_text 也带）、anthropic
		// 同族原样往返、gemini 已由上面 !caps.Citations 分支整体报过，都不重复报。
		if name == ProtocolChatCompletions {
			if n := ir.CountStrayPortableCitations(req); n > 0 {
				notes["stray citations"] = StrayCitationDropNote(n)
			}
		}
	}

	if len(notes) == 0 {
		return nil
	}
	out := make([]string, 0, len(notes))
	for _, v := range notes {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func describeBlocksLossy(blocks []ir.Block, name string, caps Capabilities, note func(field, why string)) {
	for _, b := range blocks {
		if b.CacheCtl != "" && !caps.CacheControl {
			note("cache_control", "no per-block cache markers")
		}
		switch {
		case b.Type == ir.BlockThinking && b.Thinking != nil:
			if b.Thinking.Redacted {
				// 同族（anthropic↔anthropic）逐字往返：redacted_thinking 的密文原样
				// 回吐，不算损耗。跨族没有密文槽位，整块丢弃并报一条——塞进别家的
				// encrypted_content / thoughtSignature 等于伪造凭据，客户端下一轮回传必被拒。
				if name != ProtocolAnthropic {
					note("redacted_thinking", "encrypted reasoning payload cannot be re-encoded")
				} else if b.Thinking.RedactedData == "" {
					// 同族但密文为空：编码器无从伪造（空 data 会被上游拒收），整块
					// 跳过（见 anthropic/encode_request.go 的 BlockThinking 分支，其
					// 注释声称「由有损诊断报出」——这一条就是那处诊断）。判据与编码器
					// 同口径（RedactedData==""），跳过才不是静默的：畸形历史里一个没有
					// data 的 redacted_thinking 此前会凭空消失，既无说明也不 400，与
					// 下面非涂抹空壳思考块（text 与签名皆空）的处置不对称。
					note("redacted_thinking", "a redacted reasoning block with no ciphertext would encode to a data-less shell that the upstream rejects")
				}
				continue
			}
			if !caps.Thinking {
				note("thinking blocks", "no reasoning content")
				continue
			}
			if name == ProtocolAnthropic && b.Thinking.Text == "" &&
				(b.Thinking.Signature == "" || ForeignSignature(b.Thinking, name)) {
				// 空壳判据与 anthropic 编码器同口径（见 anthropic/encode_request.go
				// 的 BlockThinking 分支）：Anthropic 拒收缺 thinking 字段的块，
				// 编码器会整块跳过——这里先报出来，跳过才不是静默的。
				note("empty thinking blocks", "a reasoning block with no text and no writable signature would encode to a payload-less shell that the upstream rejects")
				continue
			}
			if b.Thinking.Signature == "" {
				continue
			}
			if why, drop := sigDropReason(b.Thinking, name, caps); drop {
				note("thinking signature", why)
			}

		case b.Type.IsMedia():
			if !caps.Images {
				note(string(b.Type)+" blocks", "no media input")
				continue
			}
			if b.Type == ir.BlockImage {
				describeImageLossy(b.Media, caps, note)
				if name != ProtocolAnthropic && b.Media != nil && b.Media.OversizedImage != "" &&
					imageDelivered(b.Media, caps) {
					// transformations.oversized_image 是 anthropic 图片块独有的渲染
					// 指令（图片超大时 downsize|error），纯指令性 provenance。同族
					// 逐字往返无损；投给外族（chat/responses/gemini 的图片形状都没有
					// 对应字段）整维丢弃——与 tool_use 的 caller/toolset_name 同一
					// 处置口径，报一条而不是静默蒸发。
					//
					// imageDelivered 门控（轮次74）：图片整块没送达（空载荷 / 降级为
					// 文本 / file_id 无从投递）时不报——超大图处置指令对一张根本没送到
					// 的图无意义，且整块丢弃已由 describeImageLossy 报出，叠报属过报。
					note("image transformations", oversizedImageWhy)
				}
			} else if b.Media != nil && !b.Media.HasPayload() {
				// 非图片媒体（document/file/audio）base64 与 URL 两载体全空。
				// 若还带 file_id 引用，则只有原生收文件引用的目标能逐字投递，不算损耗：
				// chat_completions 的 file part、responses 的 input_file（见 caps.NativeFileRef，
				// document/file/audio 皆可），以及 anthropic 的 document file 源
				//（FileDocumentSourceParam，仅 document/PDF——它没有通用文件 part，音频与
				// 未知类型通用文件仍无从投递，故 NativeFileRef 为假、这里按块型单列）。
				// gemini 降级为文本，表达不了纯引用，仍报「无可投递载荷」。file_id 也空时
				// 所有目标都无从编起（照编是缺必填键/data:"" 的形状，上游 400 拒整轮），一律报。
				// 判据与各出站编码器 encodeMediaPart 的 FileID 分支同源，避免漂移。
				// 图片的三维细则由 describeImageLossy 报（含 anthropic 的 image file 源，
				// 走 caps.ImageFileRef）。跳过/降级优先于「类型不支持」的降级说明，故 continue。
				fileRefDeliverable := caps.NativeFileRef ||
					(name == ProtocolAnthropic && b.Type == ir.BlockDocument)
				if b.Media.FileID == "" || !fileRefDeliverable {
					note(string(b.Type)+" blocks", emptyMediaWhy)
				}
				continue
			}
			// 「降级为文本」只在确有载荷可渲染成文本时才成立。空壳图片是被整块
			// 跳过（describeImageLossy 已报「无可投递载荷」），不能再叠一条说它
			// 被降级——否则同一处丢弃报出两条互相矛盾的处置。b.Media==nil 沿用
			// 旧行为：chat 编码器会把无载荷图片降级成文本 part，那条注记是准的。
			if (b.Media == nil || b.Media.HasPayload()) && !caps.AcceptsMedia(SniffMediaType(b.Media)) {
				note(string(b.Type)+" blocks", "unsupported media type, downgraded to text")
			}
			// 类型在白名单里（上面那条因此不报），但只带远程 URL、没有内联字节，
			// 而目标又不能凭 URL 投递非图片媒体（chat 的 file part / responses 的
			// input_file 只认内联 data 或 file_id 引用）：编码器把它降级成「附件已
			// 省略」文本占位，URL 没送到模型面前，模型读不到那个文件。这是与「类型
			// 不支持」不同的第二种降级，此前无人报。判据与各 encodeMediaPart 的
			// 「非图片需内联 Data」分支同源。图片另有 URL 槽位（四家都能按 URL 投
			// 图片），不在此列；纯 file_id 引用已在上面的空载荷分支按 NativeFileRef
			// 判过，走不到这里。
			if b.Type != ir.BlockImage && b.Media != nil && b.Media.Data == "" && b.Media.URL != "" &&
				caps.AcceptsMedia(SniffMediaType(b.Media)) && !caps.NonImageMediaURL {
				note(string(b.Type)+" blocks", "the part is only a remote URL and this target cannot deliver non-image media by reference, downgraded to a text placeholder")
			}

		case b.Type == ir.BlockToolUse, b.Type == ir.BlockToolResult:
			if !caps.Tools {
				note("tool blocks", "no tool calling")
			}
			if why, drop := toolSigDropReason(b.ToolUse, name, caps); drop {
				note("tool call signature", why)
			}
			if b.ToolUse != nil && name != ProtocolAnthropic &&
				(len(b.ToolUse.Caller) > 0 || b.ToolUse.ToolsetName != "") {
				// caller / toolset_name 是 anthropic tool_use 块独有的发起方标记与
				// beta toolsets 归属名，纯 provenance。同族逐字往返无损；投给外族
				// 没有对应槽位，整维丢弃——与 ToolUse.ItemID 同一处置口径，报一条
				// 而不是静默蒸发。
				note("tool call caller/toolset_name", toolCallerWhy)
			}
			if b.ToolUse != nil && name != ProtocolResponses &&
				(len(b.ToolUse.ResponsesCaller) > 0 || b.ToolUse.ResponsesNamespace != "" ||
					b.ToolUse.ResponsesAsync != nil) {
				// caller / namespace / async 是 responses function_call/custom_tool_call
				// 条目独有的发起方/命名空间/异步标记，纯 provenance。同族逐字往返
				// 无损；投给外族没有对应槽位，整维丢弃——与 anthropic caller、
				// ToolUse.ItemID 同一处置口径，报一条而不是静默蒸发。
				note("tool call caller/namespace/async", respCallerWhy)
			}
			if b.ToolResult != nil {
				if name != ProtocolAnthropic && b.ToolResult.ToolsetName != "" {
					// toolset_name 是 anthropic tool_result 块独有的 beta toolsets
					// 归属名（配对 tool_use 所属的 toolset 家族），纯 provenance。
					// 同族逐字往返无损；投给外族（chat/responses/gemini 的工具结果
					// 形状都没有 toolset 槽位）整维丢弃——与 tool_use 的
					// caller/toolset_name 同一处置口径，报一条而不是静默蒸发。
					note("tool result toolset_name", toolResultToolsetWhy)
				}
				describeBlocksLossy(b.ToolResult.Content, name, caps, note)
			}
			// server_tool_use / web_search_tool_result 的 caller 发起方标记不在此单列：
			// 两种块型跨族都被外族编码器整块跳过，已由 DescribeLossy 的 ServerToolDropNote
			// 统一报出（整块都没了，caller 随之消失）。再叠一条 caller 专项注记会与整块
			// 注记重复计报同一次丢弃（轮次74 去过报），也与 ir.WebSearchToolResult.Caller
			// 早就采用的「随整块统一报出、不单设 caller 说明」处置对称。
		}
	}
}

// emptyMediaWhy 是「媒体部件三载体全空（无 base64、无 URL、无可用文件引用）」
// 的统一措辞。处置随目标协议而异——anthropic 整块跳过、chat/gemini 降级为文本
// 占位——但共性是「不会作为媒体发出去」，故措辞与具体处置无关。图片走
// describeImageLossy、其余媒体走 describeBlocksLossy，两处共用此串以免一处改了
// 另一处漂移：此前图片那条写「空图片部件会被上游拒收」，对把空媒体降级成文本
// 的 chat/gemini 并不准确（它压根没被当媒体发出去，谈不上拒收）。
const emptyMediaWhy = "the part carries no payload the target protocol can express (no base64, no URL, no usable file reference); it is not sent as media"

// toolCallerWhy 是「anthropic 工具调用块的 caller / toolset_name 投给外族被丢」
// 的统一措辞。caller 标识调用由模型直接发起还是 code_execution/advisor 编排
// 发起，toolset_name 是 beta toolsets 归属名——都是纯 provenance/可观测信息，
// 外族没有对应槽位。tool_use 与 server_tool_use 共用此串以免两处漂移。
const toolCallerWhy = "the tool call carries an anthropic-only caller/toolset_name provenance marker that the target protocol has no field for; it is dropped"

// toolResultToolsetWhy 是「anthropic tool_result 块的 toolset_name 投给外族被丢」
// 的措辞。与 toolCallerWhy 分立：tool_result 官方只有 toolset_name（无 caller），
// 且它是工具「结果」而非工具「调用」，措辞各表其形以免误导。toolset_name 是配对
// tool_use 所属的 beta toolsets 家族名，纯 provenance，外族工具结果形状没有槽位。
const toolResultToolsetWhy = "the tool result carries an anthropic-only toolset_name provenance marker (the toolset family of the paired tool_use) that the target protocol has no field for; it is dropped"

// oversizedImageWhy 是「anthropic 图片块的 transformations.oversized_image 指令投
// 给外族被丢」的措辞。官方 image_block_param.transformations.oversized_image 取值
// "downsize"|"error"，规定图片超大时上游是缩小还是报错——是纯渲染指令性 provenance，
// 只有 anthropic 图片块有槽位（chat/responses/gemini 的图片形状都没有）。同族逐字
// 往返无损；跨族整维丢弃报一条而不是静默蒸发，与 toolCallerWhy 同一处置口径。
const oversizedImageWhy = "the image carries an anthropic-only transformations.oversized_image directive (downsize/error for oversized images) that the target protocol has no field for; it is dropped"

// respCallerWhy 是「responses function_call/custom_tool_call 条目的 caller /
// namespace / async 投给外族被丢」的统一措辞。caller 是发起方标记（union
// direct{caller_id}|program）、namespace 是命名空间、async 是异步标记——都是
// responses 一族独有的纯 provenance/可观测信息，外族没有对应槽位。与
// toolCallerWhy 分立：anthropic 的 caller 形状不同（DirectCaller|ServerToolCaller），
// 两族标记互不通用，措辞各表其族以免误导。
const respCallerWhy = "the tool call carries a responses-only caller/namespace/async provenance marker that the target protocol has no field for; it is dropped"

// imageFilenameWhy 是「带载荷图片块的文件名投给无图片文件名槽位的目标被丢」的
// 措辞。文件名不影响图片字节的投递，只是少了「这本来叫什么文件」的旁注，故措辞
// 与「整张图没了」区分开。gemini 的 displayName 接得住，不报此条。
const imageFilenameWhy = "the image part carries a filename but this target's image slot has no filename field (only the bytes/URL are sent); the name is dropped"

// imageDelivered 判定一张图片是否会以「图片」形态真正投递到目标，而不是被整块
// 跳过或降级成文本。判据与 describeImageLossy 的提前 return 逐一对齐（同一出处，
// 避免漂移）：空载荷（无字节、无 URL、无 file_id）、有载荷但类型不被接受（降级为
// 文本）、以及只带 file_id 但目标不认图片文件引用（字节无从恢复）三种都算未投递。
//
// detail 档位与 transformations.oversized_image 都是「图片确实送达、上游据以切图」
// 才有意义的渲染维度。图片整块没了却仍报这两维，措辞会与实际处置对不上（规则 a：
// 「上游会按默认档切图、计费可能不同」对一张根本没送达的图是假话），且与整块丢弃
// 注记重复计报同一件事。轮次73 已就空载荷图片去过 detail 的重复注记，轮次74 用本
// 判据把「降级为文本」与「file_id 无从投递」两种未投递情形一并覆盖。
func imageDelivered(m *ir.Media, caps Capabilities) bool {
	if m == nil {
		return false
	}
	if !m.HasPayload() && m.FileID == "" {
		return false
	}
	if !caps.AcceptsMedia(SniffMediaType(m)) && m.FileID == "" {
		return false
	}
	if m.HasPayload() {
		return true
	}
	return caps.ImageFileRef
}

// describeImageLossy 报一张图片相对目标协议丢掉的三维：detail 档位、file_id
// 引用、以及整个部件没有任何可投递载荷。与能力位判定同一出处，编码器的跳过
// 判据（HasPayload）与这里的「确实丢了」口径一致。
//
// 判序很关键：先看「有没有载荷」。三载体全空（既无内联字节、又无 file_id）的
// 图片是被编码器整块跳过的，只报「无可投递载荷」这一条——detail/file 细则对
// 一个根本发不出去的部件没有意义，叠上去只会盖掉真因，还会让 describeBlocks
// Lossy 误以为它被「降级成文本」。只有「有载荷但类型不被接受」才走降级路径，
// 那条「降级为文本」由调用方统一报，这里保持沉默。
func describeImageLossy(m *ir.Media, caps Capabilities, note func(field, why string)) {
	if m == nil {
		return
	}
	if !m.HasPayload() && m.FileID == "" {
		// 三个载体全空：照编上去是缺必填键的形状（anthropic 的 base64 source
		// 缺 media_type/data，OpenAI 两系写出 url:"" 或连 image_url 键都没有），
		// 上游 400 拒整轮，编码器于是整块跳过。这一维与媒体类型无关——裸图连
		// 类型都嗅不出，此前会因 AcceptsMedia("")==false 提前 return，最终被
		// describeBlocksLossy 误报成「类型不支持，降级为文本」。
		note("image payload", emptyMediaWhy)
		return
	}
	if !caps.AcceptsMedia(SniffMediaType(m)) && m.FileID == "" {
		// 有载荷但类型不被接受：整张图会降级成文本，detail/file 细则不必叠报，
		// 「降级为文本」由 describeBlocksLossy 统一报出。
		return
	}
	if m.Detail != "" && !caps.ImageDetail && imageDelivered(m, caps) {
		// 档位决定上游怎么切图、进而决定输入 token 计费。丢掉之后上游一律
		// 按自己的默认档处理，账单上看得出、请求里看不出。这是 detail 丢弃的
		// 唯一注记点：describeBlocksLossy 逐块经此报出，覆盖 Messages/System/
		// tool_result 内嵌图片。勿再在 describeParamsLossy 加请求级 hasImageDetail
		// 门控——那只遍历 Messages 顶层、对空载荷图片仍会与此处叠报（轮次73 去过报）。
		//
		// imageDelivered 门控（轮次74）：图片整块没送达（空载荷 / 降级为文本 /
		// file_id 无从投递）时不报——此时「上游会按默认档切图、计费可能不同」是假话，
		// 且整块丢弃已由本函数下方或 describeBlocksLossy 报出，叠一条 detail 属过报。
		note("image detail", "no detail slot, the upstream will tile it at its own default level and may be billed differently")
	}
	if m.Name != "" && m.HasPayload() && !caps.ImageFilename {
		// 带载荷的图片走各家的图片槽位（chat image_url / responses input_image /
		// anthropic image source），这些槽位只装 URL/字节/detail，没有文件名字段，
		// Media.Name 静默蒸发。gemini 的 Blob.displayName 接得住（caps.ImageFilename
		// 为真），排除之。只在 HasPayload 时报：file_id-only 的图片不经图片槽位，
		// chat/responses 用 file/input_file part 把文件名保住了，报了就是谎报。
		note("image filename", imageFilenameWhy)
	}
	if m.HasPayload() {
		return
	}
	// 走到这里：无内联载荷但带 file_id 引用。
	if caps.ImageFileRef {
		// 只凭文件引用即可投递，本目标装得下。
		return
	}
	// 图片字节从未内联进请求体，本服务也不代取上游文件服务，所以这一维
	// 装不下就是彻底没了——与 URL 那种「换成 base64 即可」不同。
	note("image file reference",
		"the image slot cannot point at a file-service id, and the bytes were never inlined, so they cannot be recovered here")
}

// sigDropReason 回答签名是否要剥离，以及为什么。
func sigDropReason(t *ir.Thinking, name string, caps Capabilities) (string, bool) {
	if !caps.ThinkingSig {
		return "no signed reasoning", true
	}
	if prefixSaysForeign(t.Signature, name) {
		return "signature carries another vendor's ciphertext", true
	}
	if t.SignatureFrom != name {
		return "signature is only valid within its own protocol family", true
	}
	return "", false
}

// ForeignEventSignature 判断一条流式签名增量是否要剥离，并给出说明。
//
// 与请求侧共用 foreignSigPrefixes 与同族判定：两侧结论不一致时，
// 同一份推理内容在流式与非流式两条路上会得到不同处置，客户端把流式
// 存下来的历史回放成非流式请求就会被上游拒收。
//
// 来源为空按异族处理：签名无从验证时放行的代价是客户端把一段它验不了的
// 密文存进历史，下一轮整个请求被拒；丢掉的代价只是这轮少一段签名。
func ForeignEventSignature(signature, from, name string) (string, bool) {
	if signature == "" {
		return "", false
	}
	if prefixSaysForeign(signature, name) {
		return responseSigNote(name, "signature carries another vendor's ciphertext"), true
	}
	if from != name {
		return responseSigNote(name, "signature is only valid within its own protocol family"), true
	}
	return "", false
}

func responseSigNote(name, why string) string {
	return fmt.Sprintf("dropped thinking signature from the response (%s cannot express it: %s)", name, why)
}

// toolSigDropReason 是请求侧的判据，与 sigDropReason 平行。
//
// 返回的 why 是裸理由（由 note 加前缀），而 ForeignToolSignature 返回的是
// 完整的响应侧说明——两侧格式不同但判据同一处，改一边不会漏另一边。
func toolSigDropReason(use *ir.ToolUse, name string, caps Capabilities) (string, bool) {
	if use == nil || use.Signature == "" {
		return "", false
	}
	if !caps.ToolCallSig {
		return "no signature field on function calls", true
	}
	if prefixSaysForeign(use.Signature, name) {
		return "signature carries another vendor's ciphertext", true
	}
	if use.SignatureFrom != name {
		return "signature is only valid within its own protocol family", true
	}
	return "", false
}

// ForeignToolSignature 判断一次工具调用附带的推理签名是否要剥离。
//
// 与 ForeignEventSignature 同一套判据（前缀说异族、或来源不是本协议），
// 但说明里点明丢的是工具调用那一处：一条响应里可以既有思考块的签名又有
// 若干次调用各自的签名，措辞不分开的话运维看不出丢的是哪个。
//
// sigSupported 为假表示本协议的函数调用根本没有签名字段（除 gemini 外
// 其余三家都是），此时与来源无关一律剥离。
func ForeignToolSignature(signature, from, name string, sigSupported bool) (string, bool) {
	if signature == "" {
		return "", false
	}
	if !sigSupported {
		return toolSigNote(name, "no signature field on function calls"), true
	}
	if prefixSaysForeign(signature, name) {
		return toolSigNote(name, "signature carries another vendor's ciphertext"), true
	}
	if from != name {
		return toolSigNote(name, "signature is only valid within its own protocol family"), true
	}
	return "", false
}

func toolSigNote(name, why string) string {
	return fmt.Sprintf("dropped tool call reasoning signature (%s cannot express it: %s)", name, why)
}

// ResponseSignatureUnsupported 给没有签名字段的协议用，措辞与其它响应侧说明一致。
func ResponseSignatureUnsupported(name string) string {
	return responseSigNote(name, "no signed reasoning")
}

// DescribeResponseSignatureLoss 推导非流式响应编码会丢弃哪些推理签名。
//
// sigSupported 为假表示本协议根本没有签名字段（如 chat_completions），
// 此时所有签名都表达不了，与来源无关。
//
// 与流式路径共用 responseSigNote：同一件事在两条路径上措辞不同，
// 按说明检索流水的人会以为是两种故障。
func DescribeResponseSignatureLoss(resp *ir.Response, name string, sigSupported bool) []string {
	if resp == nil {
		return nil
	}
	var notes []string
	for _, b := range resp.Content {
		if b.Type != ir.BlockThinking || b.Thinking == nil || b.Thinking.Signature == "" {
			continue
		}
		if !sigSupported {
			notes = append(notes, responseSigNote(name, "no signed reasoning"))
			continue
		}
		if note, drop := ForeignEventSignature(b.Thinking.Signature, b.Thinking.SignatureFrom, name); drop {
			notes = append(notes, note)
		}
	}
	return DedupeNotes(notes)
}

// DescribeResponseToolSignatureLoss 推导响应编码会丢弃哪些工具调用签名。
//
// 与 thinking 那条分开：gemini 上游的工具调用带签名，而三个入站协议的
// tool_use 都没有这个字段，于是这一位在回客户端时必然被剥离。此前它在
// 解码时就消失，有损列里查不到任何痕迹——运维无从知道上游给过推理凭据。
//
// toolSigSupported 为假表示本协议的函数调用没有签名字段（除 gemini 外全是）。
func DescribeResponseToolSignatureLoss(resp *ir.Response, name string, toolSigSupported bool) []string {
	if resp == nil {
		return nil
	}
	var notes []string
	for _, b := range resp.Content {
		if b.Type != ir.BlockToolUse {
			continue
		}
		if note, drop := ForeignToolSignature(signatureOf(b.ToolUse),
			signatureFromOf(b.ToolUse), name, toolSigSupported); drop {
			notes = append(notes, note)
		}
	}
	return DedupeNotes(notes)
}

// DescribeResponseToolArgsLoss 推导响应编码会对畸形工具参数做什么。
//
// 参数不是合法 JSON 对象（max_tokens 截断是最常见来源）时两条路径都不允许
// 静默清空成 {}——那会让工具不带参数执行，是一次真实副作用。对象槽位协议
// （anthropic 的 input）把原文挪进 ir.RawArgsKey 键位；字符串槽位协议
// （chat / responses 的 arguments）原样透传。两者都出说明，措辞不同：
// 读者要改的地方不同，一个看键位，一个看原文。
//
// 与各编码器的编码分支用同一判定（ir.NormalizeToolInput），扫描结果即实编结果。
func DescribeResponseToolArgsLoss(resp *ir.Response, objectSlot bool) []string {
	if resp == nil {
		return nil
	}
	bad := 0
	for _, b := range resp.Content {
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			if _, ok := ir.NormalizeToolInput([]byte(b.ToolUse.Input)); !ok {
				bad++
			}
		}
	}
	if bad == 0 {
		return nil
	}
	if objectSlot {
		return []string{ir.RewrapNote(bad)}
	}
	return []string{ir.RawArgsPassNote(bad)}
}

// CountMalformedToolArgs 统计请求消息里参数不是合法 JSON 对象的工具调用数。
// 判据与 ir.NormalizeToolInput 同源：非法 JSON（多为截断）与合法非对象都算。
func CountMalformedToolArgs(req *ir.Request) int {
	if req == nil {
		return 0
	}
	bad := 0
	count := func(blocks []ir.Block) {
		for _, b := range blocks {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				if _, ok := ir.NormalizeToolInput([]byte(b.ToolUse.Input)); !ok {
					bad++
				}
			}
		}
	}
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	return bad
}

func signatureOf(use *ir.ToolUse) string {
	if use == nil {
		return ""
	}
	return use.Signature
}

func signatureFromOf(use *ir.ToolUse) string {
	if use == nil {
		return ""
	}
	return use.SignatureFrom
}

// DedupeNotes 把累加的说明去重并排序，供响应侧编码器的 Notes 出口使用。
// 空输入返回 nil，保持「无丢弃则字段不出现」的语义。
func DedupeNotes(notes []string) []string {
	if len(notes) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(notes))
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// ForeignSignature 判断签名是否属于别家协议，出站 codec 编码前用它决定是否剥离。
//
// 与 DescribeLossy 共用一套判定：诊断说丢了而编码器实际发了出去，
// 比不诊断更糟——排查的人会照着诊断去找一个不存在的原因。
func ForeignSignature(t *ir.Thinking, name string) bool {
	if t == nil || t.Signature == "" {
		return false
	}
	if prefixSaysForeign(t.Signature, name) {
		return true
	}
	return t.SignatureFrom != name
}

// OpaqueVerbatimFor 报告不透明块能否在 name 的线上原样回吐：只有解码出它的
// 那一族能（From 记录来路族，见 ir.Opaque）。与 ForeignSignature 同为「同族
// 保真门控」，判据同源，供三族编码器共用，避免各自漂移。
//
// 跨族的处置与签名相反：签名跨族是「剥离后块仍可降级投递」，不透明块跨族是
// 「整块无法表达」——逐字发过去是目标上游不认识的块型/part 型，按块型校验
// 直接 400；降级成文本会把别家载荷拼进正文污染回答。所以编码器对本判定为假
// 的不透明块一律报错，不静默丢弃也不降级（理由见 ir.BlockOpaque）。
func OpaqueVerbatimFor(o *ir.Opaque, name string) bool {
	return o != nil && len(o.Body) > 0 && o.From == name
}

// describeParamsLossy 报这一批调参字段的丢弃。
//
// 判据统一：只有客户端**给了**的字段才报（指针非 nil / 字符串非空 /
// 集合非空）。没给的字段什么都没被丢，报了会让每个普通请求都拖着一串
// 无意义的说明，真正的丢弃反而看不见。
//
// 这一批一律只报不拒：拒绝会把一个能用的回答换成零回答，而目标协议是
// 调度层按策略选的、客户端无从预知，让它为一个自己控制不了的路由结果
// 吃 400，故障归因方向是错的。要强制可以用模型配置的 overrides。
func describeParamsLossy(req *ir.Request, name string, caps Capabilities, note, filled, unreturned func(field, why string)) {
	if !caps.Penalties {
		if req.PresencePenalty != nil {
			note("presence_penalty", "no presence penalty parameter")
		}
		if req.FrequencyPenalty != nil {
			note("frequency_penalty", "no frequency penalty parameter")
		}
	}
	if req.Seed != nil && !caps.Seed {
		note("seed", "no seed parameter, results are not reproducible")
	}
	if req.Candidates != nil && !caps.Candidates {
		// 措辞要说清后果：客户端按数组取第二个候选会越界，
		// 笼统的 dropped n 读不出这一点。
		//
		// 目标支持该参数时这里刻意不报：上游会真的回多路，实际丢了
		// 几路要到响应侧才知道，那个数字比「可能会丢」有用。两格互斥。
		note("n", "no multi-candidate parameter, only one candidate will be returned")
	}
	// logprobs:false 是客户端明确「不要按 token 的概率」（SDK 常把默认值
	// 序列化出来），不是「请求了又被丢」——两个分支都只在客户端真要概率时
	// 才报，否则对每个 logprobs:false 的请求都喷一条假阳性注记。top_logprobs
	// 一旦给了（非 nil）即表示要 top-N 概率，与 logprobs 开关同列。
	wantsProbs := (req.LogProbs != nil && *req.LogProbs) || req.TopLogProbs != nil
	if !caps.LogProbs {
		if req.LogProbs != nil && *req.LogProbs {
			note("logprobs", "no log probability parameter")
		}
		if req.TopLogProbs != nil {
			note("top_logprobs", "no log probability parameter")
		}
	} else if wantsProbs {
		// 目标支持、上游会算，但本服务的中立表示不承载按 token 的概率，
		// 解码时丢掉。与「目标不支持」是两件事，措辞必须不同：
		// 前者客户端换个目标就有，后者换谁都没有。
		unreturned("logprobs", "per-token probabilities are not carried across protocols")
	}
	if len(req.LogitBias) > 0 && !caps.LogitBias {
		// 不翻译：偏置的键是 token id，词表随模型而变，跨模型重映射
		// 没有正确答案。
		note("logit_bias", "no logit bias parameter")
	}
	if req.ServiceTier != "" {
		if !caps.ServiceTier {
			note("service_tier", "no service tier parameter, scheduling falls back to the upstream default")
		} else if _, ok := MapServiceTier(req.ServiceTier, name); !ok {
			// 有槽位不代表装得下：三家值集不同，provably 无等价的档位
			//（如 anthropic 收不了 priority、chat 收不了 ultrafast）照实
			// 报出。档位值是官方枚举不是敏感串，回显帮读者定位。
			note(fmt.Sprintf("service_tier %q", req.ServiceTier),
				"the target protocol's tier set has no equivalent, scheduling falls back to the upstream default")
		}
	}
	if req.PromptCacheKey != "" && !caps.PromptCacheKey {
		// 值是客户端自选串，不回显（与 user id 同款纪律）。
		note("prompt_cache_key", "no cache routing field, repeated prefixes may recompute instead of hitting the cache")
	}
	if req.SafetyIdentifier != "" {
		// safety_identifier 与 user_id 同一维度：无槽位的协议直接丢；
		// anthropic 映进 metadata.user_id，该槽被 user_id 占了才挤不进去。
		// 值是用户标识，不回显。
		switch {
		case !caps.SafetyIdentifier:
			note("safety_identifier", "no abuse-tracking identifier parameter, the upstream safety system will not see it")
		case name == ProtocolAnthropic && req.Metadata["user_id"] != "":
			note("safety_identifier", "the request already carries a user id and the target keeps a single abuse-tracking slot, only the user id reaches the upstream")
		}
	}
	// 端用户标识（Metadata["user_id"]，与 safety_identifier 不同维度）：anthropic
	// 映进 metadata.user_id、chat/responses 落顶层 user 字段，唯独 gemini 没有任何
	// 用户标识槽位，整条丢弃且此前无人报（rule a）。值是用户标识，不回显（与本函数
	// 其它用户标识同款纪律）。四家出站里只 gemini 缺槽，故按协议名判定。
	if req.Metadata["user_id"] != "" && name == ProtocolGemini {
		note("user id", "no end-user identifier slot, the upstream's abuse detection and per-user attribution will not see it")
	}
	if len(req.Moderation) > 0 && !caps.Moderation {
		note("moderation", "no request-level moderation parameter, moderation falls back to the upstream default")
	}
	if len(req.PromptCacheOptions) > 0 && !caps.PromptCacheOptions {
		note("prompt_cache_options", "no explicit cache breakpoint control, caching follows the upstream default policy")
	}
	// prompt_cache_retention（in_memory|24h）是 OpenAI 两系的缓存最大留存策略，
	// 与 prompt_cache_options.ttl（最小生命周期）独立。anthropic 走 cache_control、
	// gemini 没有，二者都无对应槽位。丢了上游按组织默认留存——对 ZDR（零数据留存）
	// 合规敏感：客户端选 in_memory 是要「别久留我的 prompt 缓存」，静默丢掉可能让
	// 上游按 24h 默认留存得比客户端意图更久。值是官方枚举非敏感串，但为与
	// prompt_cache_key/options 同款保持键稳定，不回显具体档位。
	if req.PromptCacheRetention != "" && !caps.PromptCacheRetention {
		note("prompt_cache_retention", "no cache retention policy field, the client's maximum-retention choice (in_memory vs 24h, e.g. for zero-data-retention compliance) is dropped and the upstream applies its own default retention")
	}
	if req.ParallelToolCalls != nil && !caps.ParallelToolCalls {
		note("parallel_tool_calls", "no parallel tool call switch")
	}
	if rf := req.ResponseFormat; rf != nil {
		// 两档语义分开：带 schema 与只要求合法 JSON，读者的补救动作不同
		//（前者要把 schema 写进提示，后者只需一句话要求输出 JSON）。
		// 这一条比别的更要紧：客户端会直接 JSON.parse 响应，拿到自由文本就是
		// 硬失败而非降级。
		if rf.Kind == ir.ResponseFormatSchema {
			switch {
			case caps.ResponseSchema:
				// schema 约束原样送达（含 anthropic 的 output_config.format）。
				// 例外：客户端选了 json_schema 却没带 schema 原文（OpenAI 两系会
				// 解成 Kind=Schema、Schema 空）。没有结构可约束时，能退回纯 JSON
				// 的目标（chat/responses 写 json_object、gemini 写 responseMimeType）
				// 保住了「至少是 JSON」这一最低要求，不算丢失；但只收 schema 约束
				// 形态、退回不了纯 JSON 的目标（anthropic：ResponseSchema 真而
				// ResponseFormat 假）空 schema 时什么都写不出，连「要 JSON」都落空，
				// 响应变自由文本——与纯 JSON 模式在它上面的处置（见下面 else 分支）
				// 一致，必须报。
				if rf.Schema == "" && !caps.ResponseFormat {
					note("response_format", "the request asked for schema-constrained output but carried no schema, and the target accepts only schema-constrained structured output, so the response will be free-form text")
				}
			case caps.ResponseFormat:
				// 支持 JSON 但不支持 schema：降级成「只要求是 JSON」，
				// 客户端的最低要求仍满足。
				note("response_format.schema", "structured output is supported but not schema constraints, downgraded to plain JSON")
			default:
				note("response_format", "no structured output parameter, the response may not be JSON")
			}
		} else {
			switch {
			case caps.ResponseFormat:
				// 纯 JSON 模式原样送达。
			case caps.ResponseSchema:
				// anthropic 的 output_config.format 只接 json_schema 一种 type：
				// 「只要求合法 JSON、不约束结构」这一档给不出。措辞说清是受限
				// 而非全无槽位——读者补一个 schema 就能用上。
				note("response_format", "upstream protocol accepts only schema-constrained structured output, the response will be free-form text")
			default:
				note("response_format", "no structured output parameter, the response may not be JSON")
			}
		}
		// json_schema.description 只有 OpenAI 两系有槽位：anthropic 的
		// output_config.format 与 gemini 的 responseSchema 都没有这一键，
		// 跨族时 schema 的自然语言说明到不了模型面前。
		if rf.Description != "" && name != ProtocolChatCompletions && name != ProtocolResponses {
			note("response_format.description", "the target protocol's structured-output slot has no description key, the schema's natural-language hint will not reach the model")
		}
		// json_schema.strict 与 .name 同样只有 OpenAI 两系有槽位。
		if name != ProtocolChatCompletions && name != ProtocolResponses {
			// strict：anthropic 的 output_config.format 与 gemini 的 responseSchema
			// 恒为严格语义、没有开关。客户端显式要 strict:false（放宽 schema、
			// 容忍额外字段或可选字段）时，这个放宽到不了目标，输出反被过度约束
			// ——与工具级 strict 一样报出。显式 true 与目标的恒严格同义，不算
			// 丢失，不报（否则每个严格结构化请求都白搭一条说明）。
			if rf.Strict != nil && !*rf.Strict {
				note("response_format.strict", "the target protocol's structured output is always schema-strict with no leniency switch, the requested non-strict mode is dropped and the response will be constrained to the schema")
			}
			// name：结构化输出的标识键（OpenAI 用于缓存/路由），目标没有这一键。
			if rf.Name != "" {
				note("response_format.name", "the target protocol's structured-output slot has no name key, the schema's identifier will not reach the model")
			}
		}
	}
	if req.Verbosity != "" && !caps.Verbosity {
		note("verbosity", "no verbosity parameter")
	}
	if len(req.Include) > 0 && !caps.Include {
		note("include", "no include parameter")
	}
	if req.Background != nil && *req.Background {
		// 显式 false 等同默认，不报。true 则无论目标是哪一族都必须报：
		// 本服务对上游一律流式请求且不留存（stream:true + store:false），
		// 与官方 background 的前置条件相反——responses 有槽位也兑现不了，
		// 出站不回写；其余三族连槽位都没有。客户端期待的异步任务在这里
		// 一律变成同步阻塞等待，静默兑现等于让它对着一个不存在的任务 id 等回调。
		if caps.Background {
			note("background", "background mode cannot be honored: this service relays synchronously (upstream requests are always streamed and never stored), the client expecting an async job gets a blocking response")
		} else {
			note("background", "no background mode, the request runs synchronously and the client expecting an async job gets a blocking response")
		}
	}
	if req.Store != nil && *req.Store {
		// 显式 false 等同默认（本服务对上游一律不留存），不报。true 则无论
		// 目标是哪一族都必须报：本服务自己记流水、对上游恒不留存（responses
		// 出站强制写 store:false，其余三族连槽位都没有），客户端期待「这次响应
		// 存在上游、稍后可按 id 取回或链式引用」在这里一律落空。与 background
		// 同款判据——不可兑现的留存意图静默吞掉，客户端会对着一个取不回的 id 等。
		note("store", "the response will not be stored server-side: this service relays without retaining upstream state (responses requests are always sent with store:false, the other protocols have no store parameter), a client expecting to fetch or reference this response by id later cannot")
	}
	if req.Truncation != "" && !caps.Truncation {
		note("truncation", "no upstream-side truncation parameter")
	}
	if req.MaxToolCalls != nil && !caps.MaxToolCalls {
		// 工具调用次数上限是 responses 一族专属：其他三族没有计数闸门，
		// 客户端要的安全上限在上游侧不再生效。
		note("max_tool_calls", "no tool-call budget, the model may make more tool calls than the client allowed")
	}
	if req.IncludeObfuscation != nil && !caps.StreamObfuscation {
		// 流式混淆开关是 OpenAI 两系专属：其余两族的流式帧没有混淆机制，
		// 客户端关保护的显式表态无从传达。
		note("stream_options.include_obfuscation", "no stream-obfuscation switch")
	}
	if len(req.ClientMetadata) > 0 && !caps.ClientMetadata {
		if name == ProtocolAnthropic {
			// anthropic 确有自己的 metadata 参数，但它只承载 user_id（滥用检测
			// 标识，取自独立的 Metadata/SafetyIdentifier 维度），与这里被丢的
			// ClientMetadata 无关——编码器从不读 ClientMetadata，客户端的自定义
			// 关联键值整体丢弃。措辞不能说成「从 client metadata 保留 end-user
			// id」：那个 id 另有来源，此处是「有 metadata 槽但装不下自定义键」。
			note("metadata", "the target protocol's metadata parameter carries only an end-user id from a separate field, it has no slot for the client's custom key-values")
		} else {
			note("metadata", "no client metadata parameter")
		}
	}
	if req.MaxTokens <= 0 && caps.RequiresMaxTokens && caps.DefaultMaxTokens > 0 {
		// 兜底不是丢弃，但同样是本服务改了客户端没给的东西，必须留痕：
		// 否则长回答在一个客户端从未设过的上限处被截断，无从查证。
		filled("max_tokens", fmt.Sprintf("the client gave none, defaulted to %d", caps.DefaultMaxTokens))
	}
}

// describeThinkingModernLossy 报思考配置现代化三维（adaptive / display /
// effort）的跨族与越集丢弃。
//
// adaptive（模型自主决定思考量）与 display（思考回显形态）都是 anthropic
// 专属维度：OpenAI 的 effort 是显式档位、reasoning.summary 是啰嗦程度而非
// 可见性，都不构成等价物，不映射只报出。只在目标支持思考时报——目标连
// 推理模式都没有时，上面已经报过一条 "thinking / no reasoning mode"，
// 再报这两条是同一件事说三遍。
//
// effort 值集诊断只管 anthropic 本族：它的 effort 是封闭五值
// （low/medium/high/xhigh/max），minimal 与未知值 provably 装不下；
// "none" 与未开思考同义，静默。其余协议 effort 直通或走自己的维度，不报。
//
// 预算↔档位折叠（轮次14）：思考强度有两种表达——anthropic/gemini 用 token
// 预算（BudgetTokens），chat/responses 用粗档位（Effort）。跨这两组时编码器
// 会互折，此前无人报：
//   - budget→effort（chat/responses，客户端给了预算）：精确 token 数被
//     effortForBudget 折成 low/medium/high，精确预算送不到上游——改写（rewrote）。
//   - 无强度→effort（chat/responses，客户端既没档位也没预算，轮次80 补）：
//     effortForBudget(0) 合成一个 medium 档，是替客户端发明强度——兜底（filled）。
//   - effort→budget（anthropic 非 adaptive / gemini）：目标用 token 数表达强度、
//     缺预算会被拒，编码器用 budgetForEffort 合成一个——是兜底（filled）。
//
// 各分支的触发条件与对应编码器严格同条件（含 BudgetTokens 的符号），避免假阳/漏报。
// 唯一需要额外门控的是 effort→budget 合成那条：编码器在 shape 之后才跑，shape 可能
// 已把 thinking 整块丢掉，故那条用 thinkingSurvivesShaping 与编码器的真实行为对齐
// （详见该分支注释）。
func describeThinkingModernLossy(req *ir.Request, name string, caps Capabilities, note, filled, rewrote func(field, why string)) {
	t := req.Thinking
	if t == nil {
		return
	}
	// 目标是 chat/responses（档位协议）时，编码器用 effortForBudget 决定出站
	// 档位，分两种客户端输入（与 chatcompletions/encode_request.go、responses/
	// encode_request.go 的 `if Effort == "" { effortForBudget(...) }` 折叠分支
	// 同条件）：
	//   - 给了精确预算：折成 low/medium/high，精确预算送不到上游——改写（rewrote）。
	//   - 连预算都没给（如 Anthropic 入站 thinking:{"type":"enabled"} 不带
	//     budget_tokens）：effortForBudget(0) 合成一个 medium 档。这是替客户端
	//     发明了一个它从没说过的强度值，与下面 effort→budget 合成预算同属「兜底
	//     改了客户端没给的东西」，必须留痕（对齐 max_tokens filled 纪律）——否则
	//     长回答在一个客户端从未设过的档位处被限制，无从查证。
	if t.On() && t.Effort == "" &&
		(name == ProtocolChatCompletions || name == ProtocolResponses) {
		if t.BudgetTokens > 0 {
			rewrote("thinking budget", "the target protocol takes only a coarse effort level, the exact token budget was folded into an effort tier and does not reach the upstream verbatim")
		} else {
			filled("thinking effort", "the client enabled thinking without an effort level or a token budget, so a default effort tier was synthesized for the target protocol")
		}
	}
	// effort→budget：目标用 token 预算表达强度、缺预算会被拒，编码器合成一个。
	// gemini 恒用预算；anthropic 仅非 adaptive 的 enabled 档需要预算（adaptive
	// 由模型自主决定思考量、不带预算）。与两编码器的 budgetForEffort 兜底同条件。
	//
	// 还要 thinkingSurvivesShaping 门控：编码器在 shape 之后才跑，而 shape 会在
	// 「max_tokens 容不下最小推理预算」或「推理与强制工具调用互斥」时把 thinking
	// 整块丢掉（anthropic 两位都开，故只有它可达）。诊断按原始请求推导，不加这道
	// 门就会在思考已被丢弃时仍报「合成了预算」——与 shapeNotes 的 "dropped
	// thinking" 自相矛盾的假阳性。门控后本条与编码器「真的合成了」严格同条件。
	if t.On() && t.BudgetTokens <= 0 &&
		(name == ProtocolGemini || (name == ProtocolAnthropic && !t.Adaptive)) &&
		thinkingSurvivesShaping(req, caps) {
		filled("thinking budget", "the target protocol expresses reasoning depth as a token budget, so one was synthesized because the request carried none")
	}
	// reasoning 子参数三维（summary / context / mode）只有 responses 族有
	// 线格：chat 的 reasoning_effort 是裸字符串，anthropic/gemini 的思考
	// 参数也装不下。这三个轴与开/关轴独立（客户端可以只给 summary 不谈
	// effort），所以不看 caps.Thinking——上面那条 "no reasoning mode"
	// 报的是开关轴，这里报的是子参数轴，各自成立。
	if name != ProtocolResponses {
		if t.Summary != "" {
			note(fmt.Sprintf("reasoning summary preference %q", t.Summary), "the target protocol has no summary-verbosity field, reasoning summaries come in the upstream default form")
		}
		if len(t.Context) > 0 {
			note("reasoning context scope", "the target protocol's reasoning parameter takes only an effort level, reasoning runs over the upstream default context")
		}
		if len(t.Mode) > 0 {
			note("reasoning mode", "the target protocol's reasoning parameter takes only an effort level, reasoning runs in the upstream default mode")
		}
	}
	if name != ProtocolAnthropic {
		if caps.Thinking {
			if t.Adaptive {
				note("adaptive thinking", "the target protocol only takes an explicit effort level, a fixed level will be used instead of the model choosing")
			}
			if t.Display != "" {
				note("thinking display preference", "the target protocol has no visibility control for reasoning content, thinking is echoed in the upstream default form")
			}
		}
		return
	}
	switch t.Effort {
	case "", "none", "low", "medium", "high", "xhigh", "max":
	case "minimal":
		note("minimal thinking effort", "the effort set starts at low, the upstream default level applies")
	default:
		note(fmt.Sprintf("thinking effort %q", t.Effort), "anthropic only accepts low, medium, high, xhigh or max, the upstream default level applies")
	}
}

// MaxTokensFor 决定出站要写的输出上限。
//
// 抽成公共函数而不是留在 anthropic 的编码器里：判定「必填协议缺兜底值就
// 失败」这条路在当前四个协议上不可达（唯一必填的那个填了值），留在编码器
// 里就没有任何测试能走到它，等于一段没人验证过的保护。放在这里可以直接
// 用构造出来的 Capabilities 测。
//
// 返回的 ok 为假表示本协议可以省略该字段。
func MaxTokensFor(want int, caps Capabilities) (n int, ok bool, err error) {
	if want > 0 {
		// 客户端给了就原样用，不设下限：它要一个极短回答是它的事，
		// 静默抬高是无痕改写客户端的意图。
		return want, true, nil
	}
	if !caps.RequiresMaxTokens {
		return 0, false, nil
	}
	if caps.DefaultMaxTokens <= 0 {
		return 0, false, fmt.Errorf("max_tokens is required by this protocol but no default is configured")
	}
	return caps.DefaultMaxTokens, true, nil
}

// DroppedCandidatesNote 是上游回了多路候选而中立表示只装得下一路的说明。
//
// 带上实际路数：客户端为全部候选付了 token 费用（上游的 usage 是按全部
// 候选算的），只说「丢了些」看不出多付了多少。
//
// 一个流恒报一条：调用方须累计最大候选索引后只在收尾时调用本函数，
// 逐帧生成会让每帧的数字不同、去重失效。
func DroppedCandidatesNote(extra int) string {
	return fmt.Sprintf("dropped %d extra response candidate(s): the neutral representation holds one", extra)
}

// DroppedUnknownPartsNote 是上游 part 种类未被建模而整块丢弃的说明（gemini 的
// executableCode / codeExecutionResult 之类）。kinds 是去重排序后的字段名，
// n 是丢弃的 part 总数。带上种类名：客户端据此分得出「模型没产这类内容」与
// 「产了被我们丢了」，也指明了要补建模的是哪一维。
func DroppedUnknownPartsNote(kinds []string, n int) string {
	return fmt.Sprintf("dropped %d response part(s) of unmodeled kind(s) [%s]: the neutral representation has no slot for them",
		n, strings.Join(kinds, ", "))
}

// DroppedCitationsNote 是响应带了来源标注（gemini 的 grounding/citation
// metadata、chat 的 message.annotations）却无处安放的说明：ir.Block.Citations
// 绑在文本块上，候选/选项没产出任何文本块时引用挂不上去，只能丢弃并报出。
// n 是丢弃的引用数。带 URI 的引用在候选有文本块时正常保全（见 gemini.candidateCitations
// 与 chat.attachCitations），这里只覆盖「有来源、无正文可挂」这一种真实丢弃。
// gemini 与 chat 两族的流式/非流式解码共用本措辞，保证同损同报（规则 b/c）。
func DroppedCitationsNote(n int) string {
	return fmt.Sprintf("dropped %d response citation(s): the candidate carried no text block to attach them to", n)
}

// CitationLicenseDropNote gemini 上游引用来源的 license 字段丢弃注记。官方
// CitationSource.license（Output only）是引用来源的版权/许可标识：wireCitationSource
// 已建模该键，但 candidateCitations 只把 URI 映进 ir.Citation，license 无处安放被静默
// 丢弃。ir.Citation 没有 license 槽位（URL/Title/CitedText/Start/End/EncryptedIndex/
// WireType/Raw 都不是它），且 gemini 是出站-only、跨族恒无对应维，故只探测计数报出
// （与 SafetyRatingsDropNote 同款「上游给了、IR 无槽位」处置）。与 startIndex/endIndex
// 的丢弃分账：那两个是字节偏移与 IR rune 口径不符、有意不携带（wire.go 注释在案），
// 而 license 此前既无注释也无注记，是纯静默丢弃。
func CitationLicenseDropNote(n int) string {
	return fmt.Sprintf(
		"dropped the license/attribution string from %d citation source(s): the internal citation model has no slot for a source license, so the client cannot see the license under which a cited source was provided", n)
}

// maxFinishDetail 是上游收尾原因原文的保留字节数。
// 说明会落库进流水，而原文长度不受本服务控制。
const maxFinishDetail = 200

// FinishDetailNote 是上游随 finish reason 附的人类可读原因被丢弃的说明。
//
// 原文必须带上：这条说明的全部价值就在原文里（比如具体触发了哪条安全
// 策略），砍掉只剩「有个细节丢了」等于没说。
func FinishDetailNote(detail string) string {
	if len(detail) > maxFinishDetail {
		detail = textsafe.Truncate(detail, maxFinishDetail) + "..."
	}
	return "dropped the upstream finish detail: " + detail
}

// FinishReasonMisfoldNote 报上游 finish reason 枚举被**失真地**折进 content_filter。
//
// 与 FinishDetailNote 分账：那条带的是上游另附的人类可读原文串（finishMessage），
// 这条带的是 finishReason **枚举本身**——IR 只有 content_filter 一档，装不下它。
// 内容策略类的枚举（SAFETY / RECITATION / BLOCKLIST / PROHIBITED_CONTENT / SPII /
// LANGUAGE / IMAGE_SAFETY / IMAGE_PROHIBITED_CONTENT / IMAGE_RECITATION / OTHER）
// 折进 content_filter 是忠实的，不报；但 MALFORMED_FUNCTION_CALL 根本不是拦截——
// 是模型生成的工具调用不合法被丢弃，补救动作是重试整个回合而非改措辞，未识别的
// 新枚举同理无从判断。这两类被折进 content_filter 会把成因和补救方向一起带偏，
// 故把原枚举回带报出。与 StopReasonFoldNote 也分账：那条是 IR 档跨族**出站**编码
// 时折叠，这条是上游枚举**解码进 IR** 时就失真，措辞落在解码侧、原枚举丢失。
func FinishReasonMisfoldNote(reason string) string {
	if len(reason) > maxFinishDetail {
		reason = textsafe.Truncate(reason, maxFinishDetail) + "..."
	}
	return "the upstream finish reason was " + reason +
		", which is not a content-policy block, but this gateway had no matching stop value and folded it into the generic content-filter reason; the specific cause — and the different remediation it implies (a malformed function call means retry the turn, not rephrase it) — is lost"
}

// BlockReasonNote 是上游整轮安全阻断原因（如 gemini 的
// promptFeedback.blockReason）被压成停因后、原文串本身的留存说明。
//
// 停因只告诉客户端「被内容过滤挡了」，分不出是哪条策略命中，而下一步动作
// （改提示词还是换安全档）取决于原文。与 FinishDetailNote 同一路数：原文
// 必须带上，砍掉只剩「被挡了」等于没说。
func BlockReasonNote(reason string) string {
	if len(reason) > maxFinishDetail {
		reason = textsafe.Truncate(reason, maxFinishDetail) + "..."
	}
	return "the upstream blocked the whole prompt, stop reason is content_filter; block reason: " + reason
}

// TierEchoDropNote 是上游回显的实际执行档位送不到客户端的说明。
// 流式编码器（越集、无槽位、到得太晚）与非流式响应损耗扫描共用同一
// 措辞。档位是枚举值非敏感，带值报出——客户端至少能对账「实际用的
// 是哪档容量」，这一维决定计费。
func TierEchoDropNote(tier string) string {
	return fmt.Sprintf(
		"dropped service tier echo %q: this protocol's response has no equivalent tier value, the client cannot see which capacity tier actually served the request", tier)
}

// DescribeResponseTierLoss 非流式响应侧的档位回显损耗扫描：目标协议的
// 回显值集装不下上游报的实际档位时，编码器会静默丢弃，这里照实报出。
// 装得下时编码器按 MapServiceTierEcho 翻译回写，无损耗。
func DescribeResponseTierLoss(resp *ir.Response, name string) []string {
	if resp == nil || resp.ServiceTier == "" {
		return nil
	}
	if _, ok := MapServiceTierEcho(resp.ServiceTier, name); !ok {
		return []string{TierEchoDropNote(resp.ServiceTier)}
	}
	return nil
}

// ContainerDropNote 是代码执行容器回显丢失的说明。容器回显是 anthropic
// 专属维度，外族响应没有 container 槽位：客户端拿不到容器 id 与过期时间，
// 下一轮无法复用同一容器续话，文件与已加载技能全部清零。非流式与流式
// 两条路径共用。
func ContainerDropNote() string {
	return "dropped container info: this protocol's response has no container field, the client cannot see or reuse the code-execution container that served the request"
}

// ResponseAnthropicDiagnosticsDropNote 是 anthropic 请求级诊断回执丢失的说明。
// 该回执（官方 Message.diagnostics={cache_miss_reason}）由客户端在请求侧用
// diagnostics.previous_message_id 主动索要（ir.Request.Diagnostics），上游据此
// 回填「prompt-cache 前缀为何未能复用」的归因（model_changed / system_changed /
// tools_changed / messages_changed / previous_message_not_found / unavailable）。
// 仅 anthropic 一族响应有 diagnostics 槽位，chat_completions / responses 都没有
// （responses 的 prompt_cache_diagnostics 是不同族、不同线格式的另一机制，不互映），
// 故跨族投影来的回执整体丢弃：索要过诊断的客户端看不到缓存失配成因，缓存调优失去
// 反馈。与 ContainerDropNote 同属「anthropic 专属回显被外族丢」一档，非流式与流式
// 两条路径共用。回执正文属会话内容，不进说明。同款对称防御：字段一旦非空即照实报，
// 空值绝不误报——实践中外族客户端无从索要（请求侧无对应槽位、丢弃另由
// describeRequestLossy 报出），故这条几乎不可达，只为字段非空时不静默。
func ResponseAnthropicDiagnosticsDropNote() string {
	return "dropped the upstream request-level diagnostics receipt: this protocol's response has no diagnostics field, so a client that requested prompt-cache divergence reporting via diagnostics.previous_message_id cannot see the cache_miss_reason explaining why the cache prefix was not reused"
}

// ResponseModerationDropNote 是上游审核回执丢失的说明。审核回执（官方 chat 的
// ChatCompletion.moderation、responses 的 response.moderation）是 chat/responses
// 两族专属的响应级槽位，由上游内容安全侧产出、不是客户端回声：开了 moderated
// completions 的客户端靠它门控输入/输出审核结果。anthropic 响应没有 moderation
// 字段（gemini 仅出站、不面向客户端），跨族投影来的一律丢弃，客户端拿不到审核
// 判定。与 ContainerDropNote 互为镜像（那是 anthropic 专属回显被外族丢，这是
// 外族专属回显被 anthropic 丢）。回执正文属会话内容，不进说明。
func ResponseModerationDropNote() string {
	return "dropped the upstream moderation receipt: this protocol's response has no moderation field, so a client gating on moderated completions cannot see the input/output moderation result the upstream produced"
}

// ResponseClientMetadataDropNote 是客户端自定义关联键值回声丢失的说明。响应级
// metadata（官方 chat 的 ChatCompletion.metadata、responses 的 response.metadata）
// 是 chat/responses 两族专属槽位，回显客户端请求里带的自定义键值，客户端按它做
// 异步关联 / 幂等对账。anthropic 响应没有 metadata 字段，跨族投影来的回声整体
// 丢弃。仅在真非空时报出：anthropic 客户端本就无从设置该键值（请求侧无对应槽位、
// 且请求侧丢弃另由 describeRequestLossy 报出），故回声对 anthropic 客户端通常
// 不可达，此注记是对称防御——字段一旦非空即照实报，绝不为空值误报。
func ResponseClientMetadataDropNote() string {
	return "dropped the echoed client metadata: this protocol's response has no metadata field, so a client relying on its custom key-values for async correlation or idempotency cannot read them back"
}

// ResponsePromptCacheDiagnosticsDropNote 是提示缓存诊断回执丢失的说明。诊断回执
// （官方 responses 的 response.prompt_cache_diagnostics：cache_miss / cache_hit /
// comparison_response_not_found / unavailable 四形态判别式联合）由客户端在请求侧
// 用 prompt_cache_options.comparison_response_id 主动索要（「supplying this field
// requests prompt cache diagnostics」）。仅 responses 一族响应有槽位承载它——官方
// ChatCompletion 响应无 prompt_cache_diagnostics 字段，anthropic 更没有。故
// chat_completions / anthropic 客户端经本网关路由到 responses 上游、上游返回诊断时，
// 出站协议无处安放，整体丢弃：客户端索要的缓存命中/未命中判定连同成因（model_changed
// / prompt_cache_key_changed / input_changed 等）一并看不到，缓存调优失去反馈。
// 与 moderation/metadata 分账——那两族 chat 也是承载族、只 anthropic 丢；诊断回执
// chat 同样装不下，承载族只有 responses 一家。回执正文属会话内容，不进说明。
// 同款对称防御：字段一旦非空即照实报，空值绝不误报。
func ResponsePromptCacheDiagnosticsDropNote() string {
	return "dropped the upstream prompt cache diagnostics receipt: this protocol's response has no prompt_cache_diagnostics field, so a client that requested cache-reuse diagnostics via prompt_cache_options.comparison_response_id cannot see the cache hit/miss verdict or its cause"
}

// DescribeResponseClientMetaLoss 非流式响应侧的审核回执 / 客户端关联键值回声 /
// 提示缓存诊断回执损耗扫描：承载族编码时原样带回、无损耗；非承载族没有对应槽位，
// 跨族投影来的一律静默丢弃，这里照实报出。请求侧同类丢弃已由 describeRequestLossy
// 报出，响应侧此前静默，判据与流式编码器 Notes() 同源。字段仅在真非空（且 RawMessage
// 非显式 null）时报出，回声不可达时不误报。
//
// 承载族按字段分账，不是单一门控：
//   - moderation / metadata：chat_completions 与 responses 两族都有响应级槽位，
//     故只 anthropic / gemini 丢；
//   - prompt_cache_diagnostics：只有 responses 一族有槽位（chat 也装不下），故
//     chat_completions / anthropic / gemini 都丢。
func DescribeResponseClientMetaLoss(resp *ir.Response, name string) []string {
	if resp == nil {
		return nil
	}
	var notes []string
	if name != ProtocolChatCompletions && name != ProtocolResponses {
		if len(resp.ResponsesModeration) > 0 && string(resp.ResponsesModeration) != "null" {
			notes = append(notes, ResponseModerationDropNote())
		}
		if len(resp.ClientMetadata) > 0 {
			notes = append(notes, ResponseClientMetadataDropNote())
		}
	}
	if name != ProtocolResponses {
		if len(resp.ResponsesPromptCacheDiagnostics) > 0 && string(resp.ResponsesPromptCacheDiagnostics) != "null" {
			notes = append(notes, ResponsePromptCacheDiagnosticsDropNote())
		}
	}
	return notes
}

// ContainerUploadDropNote 容器文件引用块（container_upload）丢失注记。该块是
// anthropic 专属（file_id 指向容器输入/产出文件），外族没有 file_id 槽位：
// 整块跳过而不降级（把 file_id 拼进正文会污染回答），损耗经此报出。
//
// 与 ServerToolDropNote 同一口径：措辞用「接收端」而非「客户端」，因为这条
// 注记同时用于响应侧（受众是客户端，模型产出的文件引用带不过去）与请求侧
// 诊断（受众是上游模型，客户端历史里的文件引用带不过去），两个方向都成立。
// file_id 属会话内容，不进注记。
//
// 三条路径共用：流式编码器 Notes()、非流式 EncodeResponseLossy、请求侧
// DescribeLossy。措辞只此一份，按说明检索流水的人不会把同一件事当成多种故障。
func ContainerUploadDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d container upload block(s): this protocol has no container file-reference slot, so the receiving side cannot see files uploaded to or produced by the code-execution container", n)
}

// RedactedDropNote 是涂抹思考块（redacted_thinking）丢失的说明：密文只有
// anthropic 同族槽位能逐字承载，跨族转换带不过去，客户端下一轮无从原样回传，
// Anthropic 的扩展思考续话校验会因此断链。密文属会话内容，不进说明。
func RedactedDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d redacted_thinking block(s): the encrypted reasoning payload has no slot in this protocol, so the receiving side cannot replay it verbatim and Anthropic extended-thinking continuity breaks across the conversion", n)
}

// ContentChannelStreamNote 是「content 通道推理正文在流式输出里被改标成
// summary 通道」的说明。responses 的 reasoning 条目有两条正文通道：content
// （reasoning_text，模型内部推理原文）与 summary（reasoning_summary_text，
// 给用户看的摘要）。非流式路径靠 ir.Thinking.ContentChannel 原样回吐，逐字
// 无损；流式的事件模型没有这个通道标记，一律渲染成 summary，于是 content
// 通道的推理塌缩进 summary：正文保留、通道语义丢失。
//
// 两条流式路径共用这一份措辞（规则 b：同损同措辞）——真流式在解码器侧计数
// （reasoning_text.delta 帧），整份响应投影（ir.ResponseEvents）在编码器侧计数
// （block_start 骨架上的 ContentChannel）。判据同源，措辞必须逐字一致。
func ContentChannelStreamNote(n int) string {
	return fmt.Sprintf(
		"re-labeled %d content-channel reasoning block(s) (reasoning_text, the model's internal reasoning) as summary-channel in streaming: the text is preserved, but the channel distinction the non-streaming path keeps via Thinking.ContentChannel has no slot in the streaming event model", n)
}

// ContentChannelCrossFamilyNote 报「content 通道推理标记在跨族响应里被丢弃」。
// responses 上游的 reasoning 条目可把模型内部推理原文放在 content 通道
// （reasoning_text，ir.Thinking.ContentChannel=true 标记），与给用户看的 summary
// 通道（reasoning_summary_text）并存、语义不同。anthropic / chat 响应的思考块只有
// 一个正文槽、没有通道维度：正文逐字保留（anthropic thinking / chat
// reasoning_content 写回同一段文本），丢的只是「这段是原始推理还是摘要」这个标记
// ——与请求侧 countContentChannelReasoning 是同一损类（规则 c：请求侧与响应侧对
// 同一损类都报），只是受众换成收响应的客户端。流式与非流式共用这一份措辞（规则 b）。
//
// 区别于 ContentChannelStreamNote：那条是 responses **同族**流式里 content 被改标成
// summary（流式事件模型无通道槽位）；本条是**跨族**——目标协议压根没有通道维度，
// 无论流式还是非流式都丢。同族 responses→responses 按标记选回原通道、原样往返，
// 报了就是谎报，故 DescribeResponseContentChannelLoss 门控排除 responses（与请求侧
// countContentChannelReasoning 的 name != ProtocolResponses 同款）。
func ContentChannelCrossFamilyNote(n int) string {
	return fmt.Sprintf(
		"dropped the content-channel marker on %d reasoning block(s): the reasoning text is preserved verbatim, but this response format has no reasoning-channel slot, so the client cannot tell the model's raw internal reasoning (reasoning_text) from a user-facing summary (reasoning_summary_text)", n)
}

// DescribeResponseContentChannelLoss 报非流式响应里 content 通道推理标记的跨族
// 丢弃。判据与流式编码器（anthropic / chat 的 contentChannel 计数）同源、措辞共用
// ContentChannelCrossFamilyNote。同族 responses 保全通道、报了就是谎报，排除之。
// gemini 仅出站、无客户端响应编码器，且 content 通道只由 responses 解码器产出，
// 故本损类只在 responses 上游 → anthropic / chat 客户端时可达。
func DescribeResponseContentChannelLoss(resp *ir.Response, name string) []string {
	if resp == nil || name == ProtocolResponses {
		return nil
	}
	n := 0
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil && b.Thinking.ContentChannel {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return []string{ContentChannelCrossFamilyNote(n)}
}

// ResponseRefusalMergeNote 是模型输出里的拒绝正文被并进普通文本的说明。
// 与请求侧的「merged N refusal(s) into plain text …the model cannot tell it
// previously refused」分账：那条讲的是**历史**里的拒绝（上游模型看不出自己
// 上一轮拒绝过），本条讲的是**本轮模型输出**的拒绝（客户端把拒答当成了普通
// 正文，分不出模型是在拒绝还是在正常作答）。正文逐字保留，丢的是「这是拒绝」
// 这一标记——目标协议没有独立 refusal 槽位（chat 的 message.refusal、responses
// 的 output refusal part 才有），只能降级成文本块。流式与非流式共用本措辞（规则 b）。
func ResponseRefusalMergeNote(n int) string {
	return fmt.Sprintf(
		"merged %d refusal(s) from the model output into plain text: the target protocol has no refusal field, so the client cannot distinguish the model's refusal from ordinary text", n)
}

// DescribeResponseRefusalLoss 报「上游响应里的拒绝正文块投给没有 refusal 槽位的
// 客户端协议时被并进普通文本」。chat_completions 与 responses 有独立 refusal 槽位
// （caps.Refusal=true），原样保全、不报；其余客户端协议（anthropic；gemini 无客户端
// 响应编码器）没有槽位，encodeBlock 把 BlockRefusal 渲染成 text 块，标记丢失。
// 判据与请求侧 countRequestRefusals 的 !caps.Refusal 门控同源（规则 c：请求/响应
// 同一损类都要报），与非流式 EncodeResponseLossy / 流式 Notes() 共用措辞（规则 b）。
func DescribeResponseRefusalLoss(resp *ir.Response, name string) []string {
	if resp == nil || name == ProtocolChatCompletions || name == ProtocolResponses {
		return nil
	}
	n := 0
	for _, b := range resp.Content {
		if b.Type == ir.BlockRefusal {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return []string{ResponseRefusalMergeNote(n)}
}

// ResponseStopDetailsDropNote 是上游拒绝档结构化分类（stop_details：策略分类
// category 与人类可读解释 explanation）跨族丢弃的说明。该对象是 anthropic 专属
// 响应槽位（官方 response.stop_details / message_delta.stop_details，type 恒
// "refusal"），只有 anthropic 解码器产出、只有 anthropic 出站编码器原样回写。
// chat_completions / responses 响应都没有 stop_details 字段：终止原因本身能投影
// （refusal 折成 chat 的 content_filter，由 renderFinishReason 承载）、拒绝正文块
// 也各有槽位保全，但「按哪条策略分类拒绝、官方给的解释是什么」这一层结构化归因
// 整条蒸发——客户端只看到「被拒」，看不到「为何被拒」。与 ContainerDropNote /
// ResponseModerationDropNote 同为「一族专属响应回执被外族丢」的镜像。category /
// explanation 属会话内容，不进注记。非流式 EncodeResponseLossy 与流式 Notes()
// 共用本措辞（规则 b）。
func ResponseStopDetailsDropNote() string {
	return "dropped the upstream refusal classification (stop_details): this protocol's response has no stop_details field, so the client sees that the turn was refused but not the policy category or the explanation the upstream attached to it"
}

// DescribeResponseStopDetailsLoss 报非流式响应里拒绝档结构化分类的跨族丢弃。
// ir.Response.StopDetails 只由 anthropic 解码器从合法上游 stop_details 产出
// （decodeStopDetails），故非 nil 即隐含 anthropic 上游；anthropic 出站原样回写
// （同族保全，门控排除），chat_completions / responses 出站无槽位、整条丢弃且此前
// 静默——而相邻的终止原因折叠（DescribeResponseStopReasonLoss）与拒绝正文块降级
// （DescribeResponseRefusalLoss）都各有注记，独漏这层结构化归因，属规则 c 缺口。
// category 与 explanation 官方均可显式 null（与缺省同归空串），二者皆空时没有额外
// 归因可丢——终止原因（refusal→content_filter）与拒绝正文块已各自处理，故仅在
// 至少一维非空时报出，杜绝空对象误报（规则 a）。判据与流式编码器 Notes() 的
// EvMessageDelta 捕获同源。
func DescribeResponseStopDetailsLoss(resp *ir.Response, name string) []string {
	if resp == nil || resp.StopDetails == nil || name == ProtocolAnthropic {
		return nil
	}
	if resp.StopDetails.Category == "" && resp.StopDetails.Explanation == "" {
		return nil
	}
	return []string{ResponseStopDetailsDropNote()}
}

// ToolProvenanceDropShape 判一个工具调用块上的两族发起方 provenance 标记投给
// 目标协议 name 时是否会被丢。返回两个布尔：anthropicShape（caller/toolset_name，
// anthropic tool_use 块独有）、responsesShape（caller/namespace/async，responses
// function_call/custom_tool_call 条目独有）。判据与请求侧 describeBlocksLossy 的
// 两条门控（lossy.go 的 name != ProtocolAnthropic / name != ProtocolResponses）逐字
// 同源——同族保全不报、跨族无槽位才报，且要求标记确实非空（避免零值假阳性）。
// 请求侧与响应侧、非流式与流式四条路径共用本函数，杜绝判据漂移。
func ToolProvenanceDropShape(u *ir.ToolUse, name string) (anthropicShape, responsesShape bool) {
	if u == nil {
		return false, false
	}
	if name != ProtocolAnthropic && (len(u.Caller) > 0 || u.ToolsetName != "") {
		anthropicShape = true
	}
	if name != ProtocolResponses &&
		(len(u.ResponsesCaller) > 0 || u.ResponsesNamespace != "" || u.ResponsesAsync != nil) {
		responsesShape = true
	}
	return anthropicShape, responsesShape
}

// ResponseToolCallerDropNote 是模型输出里的工具调用 anthropic 族发起方标记
// （caller/toolset_name）投给外族客户端被丢的说明。与请求侧 note("tool call
// caller/toolset_name", toolCallerWhy) 分账：那条讲**客户端请求历史**里工具调用的
// 发起方标记投给外族上游被丢，本条讲**上游响应里模型新产出**的工具调用发起方标记
// 投给外族客户端被丢——方向相反、读者不同（前者是发请求的人，后者是收结果的人），
// 故措辞各表其向。标记是纯 provenance（谁发起了这次调用：模型直接发起还是
// code_execution/advisor 编排发起、属于哪个 beta toolset），调用名与入参逐字保留，
// 丢的只是这层归属信息。流式与非流式共用本措辞（规则 b）。
func ResponseToolCallerDropNote(n int) string {
	return fmt.Sprintf(
		"dropped the caller/toolset_name provenance marker from %d tool call(s) in the model output: the target protocol has no field for this anthropic-only marker, so the client cannot tell who initiated the call or which toolset it belonged to", n)
}

// ResponseToolRespCallerDropNote 是模型输出里的工具调用 responses 族发起方标记
// （caller/namespace/async）投给外族客户端被丢的说明。与 ResponseToolCallerDropNote
// 分立：两族标记形状不同（anthropic 的 caller 是 DirectCaller|ServerToolCaller，
// responses 的是 union direct{caller_id}|program，外加 namespace/async 两维），
// 互不通用，措辞各表其族以免误导。与请求侧 note("tool call caller/namespace/async",
// respCallerWhy) 分账同理（方向相反）。流式与非流式共用本措辞（规则 b）。
func ResponseToolRespCallerDropNote(n int) string {
	return fmt.Sprintf(
		"dropped the caller/namespace/async provenance marker from %d tool call(s) in the model output: the target protocol has no field for this responses-only marker, so the client cannot tell who initiated the call or whether it was namespaced/async", n)
}

// DescribeResponseToolProvenanceLoss 报非流式响应里工具调用的两族发起方 provenance
// 标记跨族丢弃。可达两向：anthropic 上游响应（decode_stream 经 decodeRawBlock →
// decodeBlock 保全 caller/toolset_name）投给 chat/responses 客户端；responses 上游
// 响应（decode_stream 保全 caller/namespace/async）投给 anthropic/chat 客户端。同族
// 保全不报（门控排除）。请求侧同类丢弃由 describeBlocksLossy 的两条门控报出，响应
// 侧此前静默——这里补齐，判据与流式编码器 Notes() 的 EvBlockStart 计数同源、措辞
// 一致（规则 b/c）。
func DescribeResponseToolProvenanceLoss(resp *ir.Response, name string) []string {
	if resp == nil {
		return nil
	}
	var anthr, respShape int
	for _, b := range resp.Content {
		if b.Type != ir.BlockToolUse {
			continue
		}
		a, r := ToolProvenanceDropShape(b.ToolUse, name)
		if a {
			anthr++
		}
		if r {
			respShape++
		}
	}
	var notes []string
	if anthr > 0 {
		notes = append(notes, ResponseToolCallerDropNote(anthr))
	}
	if respShape > 0 {
		notes = append(notes, ResponseToolRespCallerDropNote(respShape))
	}
	return notes
}

// CumulativeTextNote 是累计式文本帧被改写的说明：部分上游每帧重发迄今
// 全部正文而非只发增量，逐字转发会让客户端文本按帧数重复膨胀（最轻句子
// 复读，最重 token 用量翻倍）。按前缀比对识别，只下发新增后缀。
//
// 判据的已知取舍：增量式上游若恰好发来一帧「以全部已下发文本为前缀」的
// 增量（模型逐字复读自己全部输出且分块边界对齐），会被误判为累计帧而少发
// 前缀部分——该形态出现的概率随文本变长指数下降，而漏判累计式上游的代价
// 是整段文本平方级重复，两害相权取其轻（参考实现同此取舍）。
func CumulativeTextNote(n int) string {
	return fmt.Sprintf(
		"rewrote %d cumulative text frame(s): each carried the whole text so far instead of an increment, so only the new suffix was forwarded to keep the client's text from duplicating", n)
}

// RewoundTextNote 是重复/回退文本帧被吞掉的说明：帧文本是已下发文本的
// 真前缀（重复重发或回退），增量流无法表达负增长，整帧不下发。
func RewoundTextNote(n int) string {
	return fmt.Sprintf(
		"swallowed %d duplicate or rewound text frame(s): the frame carried text already delivered, and a delta stream cannot express negative growth", n)
}

// NulTextStripNote 是文本里 NUL 字符被剥除的说明：Gemini 上游有在流式
// 文本里偶发夹杂 \x00 的已知行为，透传会污染下游终端、日志与 JSON 消费方
// （多数解析器把 NUL 当字符串终止或非法控制字符）。
func NulTextStripNote(n int) string {
	return fmt.Sprintf(
		"stripped NUL characters from %d text part(s): the upstream interleaved \\u0000 bytes into the text, which poison downstream terminals and log parsers", n)
}

// BadFrameSkipNote 是坏帧跳帧续流的说明：上游发来的某些 SSE 帧外层 JSON 都
// 解不开，取不出任何内容。SSE 以事件边界自同步，坏一帧不污染后续帧，于是跳过
// 续流而非终止整流——终止会让坏帧之后的全部正常正文一起陪葬。计数报出，数字
// 偏大即提示上游成帧可能已失步。帧体属会话内容，不进说明。
//
// 只覆盖结构损坏帧；认得出事件、载荷语义坏了的内容损坏帧仍 fail-fast 终止，
// 不在此列（跳过去会让客户端收到半截却看不出丢了东西的内容）。
func BadFrameSkipNote(n int) string {
	return fmt.Sprintf(
		"skipped %d malformed stream frame(s) and kept going: the wire carried bytes that were not a decodable payload, so no content could be extracted from them; a large count suggests the upstream framing desynchronized", n)
}

// MediaOutputDropNote 是模型产出附件丢失的说明：图片与非图片附件分开计数，
// 合成一个数字会让排障时分不清丢的是哪一类——两者在源协议里是不同块型，
// 处置路径也不同。
//
// 两类调用方共用：助手回合没有任何附件形态的编码器（chat_completions /
// responses 两族），流式与非流式两条路径。
//
// 措辞刻意不断言「目标协议没有附件槽位」：只陈述本服务的转换带不过去。
// 文件名与 base64 本体属会话内容，不进说明。
func MediaOutputDropNote(images, files int) string {
	var subject string
	switch {
	case images > 0 && files > 0:
		subject = fmt.Sprintf("%d image(s) and %d non-image attachment(s)", images, files)
	case images > 0:
		subject = fmt.Sprintf("%d image(s)", images)
	default:
		subject = fmt.Sprintf("%d non-image attachment(s)", files)
	}
	return "dropped " + subject +
		" from the model output: this protocol's conversion has no way to carry them in an assistant turn, so the receiving side sees only the text the model produced"
}

// EmptyMediaOutputDropNote 是模型产出里「有媒体块但无任何可投递载荷」被整块
// 跳过的说明。与 MediaOutputDropNote 分账：那条说的是「有载荷、但目标协议
// 装不下这个类型」（降级为文本占位），这条说的是「压根没有载荷」（base64 /
// URL / 文件引用三者全空），编码器只能整块跳过——照编是缺必填键的形状，
// 上游 400 拒整份响应。
//
// 请求侧同类空壳由 describeBlocksLossy / describeImageLossy 报「carries no
// payload」，响应侧此前静默：encodeBlock 的跳过判据（!HasPayload）没有任何
// 对应计数，而它自己的注释写着「损耗由有损诊断报出」。这条补齐响应侧与流式
// 侧，使那句注释在两条路径上都成立。
//
// 措辞刻意不含「from the model output」：那是 MediaOutputDropNote（降级）的
// 专属短语，两条注记必须能被分别断言，不能因为都提到媒体就互相误伤。
func EmptyMediaOutputDropNote(n int) string {
	return fmt.Sprintf("dropped %d media part(s) that carried no payload (no base64, no URL, no file-service reference): there was nothing to encode, so the part was skipped rather than sent as a payload-less shell the upstream would reject", n)
}

// DroppedStreamContentPartsNote 是流式解码时「content part 的种类本变换没有
// 映射」被丢弃的说明。中立的流式表示只携带文本与拒绝两类 part，上游若在
// delta/message 的 content 数组里发来图片、音频、文件之类，解码器只能跳过——
// 此前 chat_completions 一族静默跳过（responses 的 droppedMsgParts、gemini 的
// droppedUnknownParts 都计数报出，唯它没有），这里抽成共享措辞让三族对齐。
func DroppedStreamContentPartsNote(n int) string {
	return fmt.Sprintf("dropped %d message content part(s) of a kind this conversion does not map (e.g. audio output): the neutral stream representation carries only text and refusal parts, so the part was not forwarded", n)
}

// CountResponseMedia 数出响应里模型产出的附件块：图片单列，音频/文档/文件
// 合列——与 MediaOutputDropNote 的两类计数一一对应。
func CountResponseMedia(resp *ir.Response) (images, files int) {
	if resp == nil {
		return 0, 0
	}
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockImage:
			images++
		case ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			files++
		}
	}
	return images, files
}

// DocConfigOf 报告一个附件是否带 Anthropic 的文档配置，两个返回值各为 0 或 1，
// 便于调用方按块累加。请求侧诊断与响应扫描共用，避免两处各写一遍判空。
func DocConfigOf(m *ir.Media) (ctx, cites int) {
	if m == nil {
		return 0, 0
	}
	if m.Context != "" {
		ctx = 1
	}
	if m.CitationsEnabled != nil {
		cites = 1
	}
	return ctx, cites
}

// DocumentConfigDropNote 文档块配置丢失注记。Anthropic 的 document 块除附件本体
// 外还带两项配置：context（客户端给模型的用途旁注）与 citations.enabled（文档引用
// 开关）。外族的附件槽位只装文件本身，两项配置跨族必丢，且此前完全静默——
// 客户端明明开了文档引用，回来一条都没有，却看不到任何迹象。ctx / cites 分别是
// 带这两项配置的文档数，只渲染非零的那部分。配置内容属客户端提示词，不进注记。
//
// 与 ServerToolDropNote 同款纪律：措辞方向中立（请求侧受众是上游模型、响应侧
// 受众是客户端，故用「接收端」），且不断言「协议没有槽位」——说的是本仓的转换
// 没有对应字段，那才是可核实的事实。
func DocumentConfigDropNote(ctx, cites int) string {
	var subject, effect string
	switch {
	case ctx > 0 && cites > 0:
		subject = fmt.Sprintf("the usage context on %d document(s) and the citation switch on %d document(s)", ctx, cites)
		effect = "the guidance never reaches the receiving side and no document citation will come back"
	case ctx > 0:
		subject = fmt.Sprintf("the usage context on %d document(s)", ctx)
		effect = "the guidance never reaches the receiving side, which gets the file alone"
	default:
		subject = fmt.Sprintf("the citation switch on %d document(s)", cites)
		effect = "no document citation will come back"
	}
	return "dropped " + subject +
		": this protocol's conversion has no field for per-document configuration, so " + effect
}

// CountResponseDocConfig 数出响应里模型产出附件所带的文档配置（context /
// citations.enabled）文档数。与 CountResponseMedia 同一路数：跨族编码器把配置
// 抹掉之前先数出来，抹掉才不是静默的。
func CountResponseDocConfig(resp *ir.Response) (ctx, cites int) {
	if resp == nil {
		return 0, 0
	}
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			c, s := DocConfigOf(b.Media)
			ctx += c
			cites += s
		}
	}
	return ctx, cites
}

// CountResponseServerTools 数出响应里的托管工具块：调用与结果分开计数，
// 与 ServerToolDropNote 的两个入参一一对应。
func CountResponseServerTools(resp *ir.Response) (calls, results int) {
	if resp == nil {
		return 0, 0
	}
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockServerToolUse:
			calls++
		case ir.BlockWebSearchToolResult:
			results++
		}
	}
	return calls, results
}

// CountResponseContainerUploads 数出响应里的容器文件引用块（container_upload）
// 条数。与 CountResponseServerTools 同一路数：编码器整块跳过之前先数出来，
// 跳过才不是静默的。
func CountResponseContainerUploads(resp *ir.Response) int {
	if resp == nil {
		return 0
	}
	n := 0
	for _, b := range resp.Content {
		if b.Type == ir.BlockContainerUpload {
			n++
		}
	}
	return n
}

// CountResponseRedacted 数出响应里的涂抹思考块（redacted_thinking）。它的密文
// 只有 anthropic 同族槽位能逐字承载，跨族编码器整块跳过——跳过之前先数出来，
// 才不是静默的。与 CountResponseContainerUploads 同一路数。
func CountResponseRedacted(resp *ir.Response) int {
	if resp == nil {
		return 0
	}
	n := 0
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil && b.Thinking.Redacted {
			n++
		}
	}
	return n
}

// CountResponseEmptyThinking 数出响应里会编成空壳的 thinking 块：正文为空且
// 没有可写回的签名（无签名，或签名属别家会被剥离）。判据与 anthropic
// 编码器的跳过分支同口径——Anthropic 拒收缺 thinking 字段的块，编码器整块
// 跳过之前先数出来，才不是静默的。只服务 anthropic 的响应侧诊断：其余协议
// 的编码器不会因空壳硬失败（gemini 跳过空文本 part，chat 写空
// reasoning_content），没有「必须丢」的语义。
func CountResponseEmptyThinking(resp *ir.Response, name string) int {
	if resp == nil {
		return 0
	}
	n := 0
	for _, b := range resp.Content {
		if b.Type != ir.BlockThinking || b.Thinking == nil || b.Thinking.Redacted {
			continue
		}
		if b.Thinking.Text == "" &&
			(b.Thinking.Signature == "" || ForeignSignature(b.Thinking, name)) {
			n++
		}
	}
	return n
}

// EmptyThinkingDropNote 是空壳 thinking 块被跳过的说明。块本身没有任何正文，
// 丢的不是内容而是「上游给了一个空推理块」这个事实——客户端按块数对账时
// 需要它。
func EmptyThinkingDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d empty thinking block(s): a reasoning block with no text and no writable signature would encode to a payload-less shell that Anthropic rejects, so it was skipped whole", n)
}

// CountResponseNonPortableCitations 数出响应里外族标注槽位装不下的引用条数
// （用于非流式有损诊断）。与 CountResponseServerTools 同一路数：编码器
// 逐条跳过之前先数出来，跳过才不是静默的。
func CountResponseNonPortableCitations(resp *ir.Response) int {
	if resp == nil {
		return 0
	}
	n := 0
	for _, b := range resp.Content {
		n += CountNonPortableCitations(b.Citations)
	}
	return n
}

// CountNonPortableCitations 统计一批引用里带不出本族的条数。
// Portable 判据见 ir.Citation：外族槽位以 URL 为来源身份。
func CountNonPortableCitations(cs []ir.Citation) int {
	n := 0
	for _, c := range cs {
		if !c.Portable() {
			n++
		}
	}
	return n
}

// CitationDropNote 文档类引用丢失注记：非流式扫描、流式编码器与请求侧诊断
// 三条通道共用同一措辞，按说明检索流水的人不会把同一件事当成多种故障。
// 引用的正文与文档标题属会话内容，不进注记。
func CitationDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d document citation(s): this protocol identifies an annotation source by URL, and these citations point at a document index with page/block/character offsets instead, so the client cannot see which passage was cited", n)
}

// StrayCitationDropNote 「角色错位的可移植引用」丢失注记：引用本身带 URL、形态
// 完全可移植，但挂在了非 assistant 的历史消息上。chat 只在 assistant 消息上开
// annotations 槽位，出站编码器对 user/system/tool 消息的引用无处可写只能丢弃。
// 与 CitationDropNote 分账——那是「引用形态（文档下标）本协议渲染不下」，
// 这里是「形态没问题、只是挂错了角色，而本协议只有助手消息能标注」。
func StrayCitationDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d source annotation(s) on non-assistant message(s): this protocol only annotates assistant messages, so citations the client attached to user/system/tool history have no slot and cannot be re-encoded", n)
}

// SystemCitationDropNote 系统提示引用丢失注记：客户端把来源标注挂在 system 提示的
// 文本块上（Anthropic Citations API 的输入侧形态，经 decodeContent(w.System) 落进
// IR 的 req.System），但三外族都把系统提示收敛成纯文本——responses 写成字符串
// instructions、chat 的 system 消息只有 text/media 槽、gemini 的 systemInstruction
// part 只装 Text——系统提示在任何角色都没有 annotations 槽位，引用整组丢弃。
// 与 CitationDropNote（文档下标形态本协议渲染不下）、StrayCitationDropNote（可移植
// 引用挂错非 assistant 消息）分账：那两条是 req.Messages 上的逐条损耗，这里是
// req.System 这一独立 []Block 的整体无槽，三个 Messages 级计数器都遍历不到它。
// anthropic 同族经 encodeBlocks 写回 Citations、无损往返，一律不报。
func SystemCitationDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d source annotation(s) on the system prompt: this protocol encodes the system instruction as plain text with no annotation slot, so citations attached to system-prompt blocks cannot be re-encoded", n)
}

// NonURLCitationDropNote 解码侧「非 url_citation 标注」丢失注记：responses 上游
// 发来的 file_citation / container_file_citation / file_path 等标注，来源身份是
// 文件/容器下标而非 URL，IR.Citation 只有 URL/Title/偏移量，装不下这类形态，解码器
// 逐条跳过。与编码侧的 CitationDropNote 分账——那是「IR 已有引用、出站协议渲染不
// 下」，这里是「入站标注本就不是 URL 形态、进不了 IR」。三条解码通道（流式增量帧、
// 流式终态快照、请求回声）共用同一措辞。
func NonURLCitationDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d non-URL annotation(s) while decoding responses output (file_citation / container_file_citation / file_path): their source is a file or container index rather than a URL, and IR citations are keyed by URL, so the annotation was skipped instead of being mis-rendered as a URL citation", n)
}

// ResponsePhaseDropNote 解码侧「responses message phase」丢失注记：上游在响应的
// output message 条目上给出 phase（官方 response_output_message.phase，枚举
// commentary|final_answer），标记该助手消息是中间旁白还是最终答复。IR 的响应模型
// （ir.Response.Content 是扁平 []Block）没有消息级槽位，连 responses→responses
// 同族也无法把 phase 带到客户端，解码即丢。与请求侧 countMessagePhase 注记互为
// 镜像（那是客户端回传历史里的 phase 跨族丢失、gated name!=responses；这是上游
// 响应里的 phase 在解码边界丢失，IR 无槽位故不分族一律报）。官方 docstring 要求
// 客户端 preserve-and-resend phase，丢了它客户端无从遵循、后续请求会退化。
// 与 logprobs/非 URL 标注同款「上游给了、IR 无槽位」处置：只探测计数、不建模内容。
func ResponsePhaseDropNote(n int) string {
	return fmt.Sprintf(
		"dropped the responses message phase (commentary/final_answer) on %d upstream output message(s): the internal response model has no message-level slot, so a client cannot preserve-and-resend phase on follow-up requests as the official contract requires", n)
}

// ResponseItemStatusDropNote 解码侧「responses 条目级 status」丢失注记：上游在每个
// output 条目（官方 ResponseOutputMessage.status / FunctionToolCall.status，均为
// **必填**，枚举 in_progress|completed|incomplete）上标记该条目自身是生成中、已完成
// 还是被截断（如 max_output_tokens 在条目中途截停）。IR 的响应模型是扁平 []Block、
// 没有条目级状态槽位，且本族编码器给每个条目一律合成 "completed"（见 openItem.wire
// 与 EncodeResponse 的各处 Status:"completed"），于是上游标为 incomplete 的条目到
// 客户端被静默**改写**成 completed——既是丢弃也是改写。与响应级 status 互补而非重复：
// 响应级 status/incomplete_details（经 stopReasonFor 保全）给出「整个响应被截断」的
// 整体信号，这里丢的是「究竟哪个条目没写完」的条目粒度信息。
//
// 只在 status 表示未完成（非空且非 "completed"）时计数：completed 是终态响应里每个
// 条目的常态、且被如实改写回 completed（无丢失），若无条件计入会对每条正常条目误报，
// 违反「注记当且仅当真实丢弃」（假阳性与漏报同样是缺口）。与 phase 同款「上游给了、
// IR 无槽位」处置：只探测计数、不建模内容；流式只在 output_item.done 终态帧计一次。
func ResponseItemStatusDropNote(n int) string {
	return fmt.Sprintf(
		"dropped the non-completed status on %d upstream output item(s): the internal response model has no per-item status slot and same-family encoding rewrites every item to \"completed\", so a client cannot see which specific item the upstream marked in_progress or incomplete (e.g. a message truncated mid-item)", n)
}

// ResponseMergedSummaryNote 解码侧「多段 reasoning 摘要被折叠」的注记：官方
// responses 的 reasoning 条目把思考摘要建模为 summary 数组，每个 part 带独立的
// summary_index，多段之间是有边界、可按 index 寻址的。IR 的思考块只有一个 Text
// 字段，joinSummary 把各段文本首尾相接成一条，文本本身保全了，但「几段、各段边界、
// summary_index 寻址」丢失。流式路径对 summary_index>0 的帧计数并出注记；非流式的
// 整份响应路径做的是同一次折叠（joinSummary），按规则 b（流式/非流式同损同措辞）
// 必须出同一条注记——codec.go 里 DecodeResponseLossy 的注释也明写「与流式的 Notes()
// 对称」。两条路径共用本函数以保证措辞逐字节一致，避免各自 Sprintf 漂移。
//
// 计数口径与流式对齐：n 是被折叠进首段的「额外」摘要帧/段数（summary_index>0 的
// part 数），单段摘要 n=0 不出注记（无折叠、无丢失，误报即假阳性缺口）。
func ResponseMergedSummaryNote(n int) string {
	return fmt.Sprintf(
		"merged %d reasoning summary frame(s) with summary_index>0 into the first summary part: the text is preserved, but the part boundaries and summary_index addressing of a multi-part reasoning item are not", n)
}

// CitationResolveDropNote 反推失败的引用丢失注记：跨协议投影来的引用没有原文
// 可透传，编成 web_search_result_location 又必须带 cited_text，而它既没自带
// cited_text、也无法按范围从所在块正文切出来时，整条只能丢弃（带空 cited_text
// 发出去上游 400，丢一条引用好过整轮被拒）。与文档类引用的 CitationDropNote
// 分账——那是「形态装不下」，这是「形态装得下但正文里定位不到」。非流式扫描
// 与流式编码器共用同一措辞。
func CitationResolveDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d citation(s): a cross-protocol citation carried no raw form to pass through and its cited_text could not be resolved against the block text, and web_search_result_location requires cited_text, so the annotation was skipped rather than sent empty", n)
}

// ResponseSchemaDowngradeNote 响应 schema 整体退成「只要求是 JSON」的注记：
// 归一失败（schema 不是合法 JSON）或无可约束的 properties 时，本协议只发
// responseMimeType 而不发 responseSchema。客户端会直接 JSON.parse 响应，
// 少了 schema 约束就可能拿到不合结构的载荷，是它无从归因的硬失败。
func ResponseSchemaDowngradeNote(name, why string) string {
	return fmt.Sprintf(
		"dropped response_format.schema (%s cannot express it: %s), downgraded to plain JSON mode; the response will still be JSON but is no longer constrained to the requested schema", name, why)
}

// ResponseSchemaTruncatedNote 响应 schema 超深的注记：超过归一深度上限的子树
// 原样透传，可能被上游按方言拒收。与工具 schema 的同名诊断措辞对齐。
func ResponseSchemaTruncatedNote(name string) string {
	return fmt.Sprintf(
		"rewrote response_format.schema (%s cannot express it: schema nesting exceeds the normalization depth cap), deeper subtrees passed through as-is", name)
}

// ResponseSchemaDroppedKeysNote 响应 schema 被剔除关键字的注记：本协议的方言
// 白名单装不下的约束（minLength/maxLength/pattern/additionalProperties 等）
// 被削掉，上游可能返回违反客户端 schema 的 JSON。列出的键名是 schema 结构、
// 非会话内容，可进注记。
func ResponseSchemaDroppedKeysNote(name string, keys []string) string {
	return fmt.Sprintf(
		"dropped response_format.schema keywords (%s cannot express it: schema keywords not in this protocol's dialect: %s), the response may violate those constraints", name, strings.Join(keys, ", "))
}

// countRequestServerTools 数出请求历史里的托管工具块。计数刻意分开：
// 两个数字对称时「接反」这类错误在夹具上看不出来。
func countRequestServerTools(req *ir.Request) (calls, results int) {
	count := func(blocks []ir.Block) {
		for _, b := range blocks {
			switch b.Type {
			case ir.BlockServerToolUse:
				calls++
			case ir.BlockWebSearchToolResult:
				results++
			}
		}
	}
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	return calls, results
}

// countRequestContainerUploads 数出请求历史里的容器文件引用块（container_upload）
// 条数。与 countRequestServerTools 同一路数：外族编码器整块跳过之前先数出来。
func countRequestContainerUploads(req *ir.Request) int {
	n := 0
	count := func(blocks []ir.Block) {
		for _, b := range blocks {
			if b.Type == ir.BlockContainerUpload {
				n++
			}
		}
	}
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	return n
}

// countRequestDocConfig 数出请求历史里带 Anthropic 文档配置（context /
// citations.enabled）的附件文档数。顶层附件与 tool_result 内嵌附件都算——
// 漏掉内嵌那层会让「工具返回了带引用开关的 PDF」这类丢失完全不可见。
func countRequestDocConfig(req *ir.Request) (ctx, cites int) {
	var media func(b ir.Block)
	media = func(b ir.Block) {
		switch b.Type {
		case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			c, s := DocConfigOf(b.Media)
			ctx += c
			cites += s
		case ir.BlockToolResult:
			if b.ToolResult == nil {
				return
			}
			for _, c := range b.ToolResult.Content {
				media(c)
			}
		}
	}
	count := func(blocks []ir.Block) {
		for _, b := range blocks {
			media(b)
		}
	}
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	return ctx, cites
}

// countRequestCustomTools 数出请求历史里的 custom 工具调用与结果块。
// 计数分开：两种降级措辞不同，合成一个数字就分不清丢的是哪一半。
func countRequestCustomTools(req *ir.Request) (calls, results int) {
	count := func(blocks []ir.Block) {
		for _, b := range blocks {
			switch b.Type {
			case ir.BlockToolUse:
				if b.ToolUse != nil && b.ToolUse.Kind == ir.ToolCustom {
					calls++
				}
			case ir.BlockToolResult:
				if b.ToolResult != nil && b.ToolResult.Kind == ir.ToolCustom {
					results++
				}
			}
		}
	}
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	return calls, results
}

// CustomToolDowngradeNote custom 工具调用跨族降级注记。自由文本入参没有
// 丢失——它被包成 {"input":…} 投影落进目标协议的 JSON 参数槽，但原生形态
// （自由文本调用）目标协议表达不了。入参原文属会话内容，不进注记。
func CustomToolDowngradeNote(n int) string {
	return fmt.Sprintf(
		"downgraded %d custom tool call(s) to function calls: the target protocol has no free-form tool-input item, the input is wrapped as {\"input\":…} inside the JSON arguments", n)
}

// CustomToolOutputDowngradeNote custom 工具结果跨族降级注记。结果内容不丢，
// 丢的是「这是自定义工具的输出」这一形态：目标协议只有普通函数结果条目。
func CustomToolOutputDowngradeNote(n int) string {
	return fmt.Sprintf(
		"downgraded %d custom tool output(s) to ordinary function results: the target protocol has no custom_tool_call_output item", n)
}

// CustomToolStreamDowngradeNote 是「custom 工具调用在 Chat Completions 流式响应里
// 降级成 function 调用」的说明。官方明确：流式 chunk 的 choices.delta.tool_calls
// 只支持 type=function，custom 工具调用不进流式（这是 Chat Completions 的有意限制，
// 非 spec 缺漏）——而非流式 message.tool_calls 认 type=custom，故非流式编码器原样
// 保全、不报此损。于是流式路径只能把 custom 调用降级成 function：调用名保留，自由
// 文本入参逐片原样落进 function.arguments（不是 JSON、也不套 {"input":…} 投影，流式
// 增量无法逐片包裹），「这是自由文本 custom 调用」的形态标记丢失。入参原文属会话
// 内容，不进注记。与请求侧 CustomToolDowngradeNote 同属 custom→function 降级类，但
// 成因（流式无 custom 槽 vs 目标协议无自由文本条目）与入参处置（原样 vs 投影）不同，
// 故措辞分立；与非流式保全路径构成有意不对称（stream:true 降级、stream:false 保全）。
func CustomToolStreamDowngradeNote(n int) string {
	return fmt.Sprintf(
		"downgraded %d custom tool call(s) to function calls in streaming: the Chat Completions streaming chunk schema only supports type=function tool calls (custom tool calls are not streamed, an intentional API limitation; the non-streaming message does support type=custom and preserves it), so the free-form input is carried verbatim in function.arguments and the custom-call kind is lost", n)
}

// countRequestAudioRefs 数出请求历史里 assistant 消息携带的音频引用条数。
// 只数 assistant：官方只在 assistant 历史上接受 {audio:{id}}，其余角色
// 出现的引用是伪造形态，不计数也不外发。
func countRequestAudioRefs(req *ir.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == ir.RoleAssistant && m.AudioID != "" {
			n++
		}
	}
	return n
}

// countRequestRefusals 数历史消息里的拒绝正文块。
func countRequestRefusals(req *ir.Request) int {
	n := 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockRefusal {
				n++
			}
		}
	}
	return n
}

// countToolResultFlattenedContent 数出会被 joinText 折平丢掉的工具结果非文本
// 子块（拒绝、不透明块）条数。只数这两种：媒体子块由 shape.go 的
// moveToolResultMedia 抽出并单独报（见 describeImageLossy），text 子块本就
// 保留，都不在此列，避免与那两条重叠计数。
func countToolResultFlattenedContent(req *ir.Request) int {
	n := 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type != ir.BlockToolResult || b.ToolResult == nil {
				continue
			}
			for _, c := range b.ToolResult.Content {
				if c.Type == ir.BlockRefusal || c.Type == ir.BlockOpaque {
					n++
				}
			}
		}
	}
	return n
}

// countMessageNames 数携带发送者名（message.name）的消息条数。
// 该字段只由 chat_completions 解码器落进 IR、只由 chat_completions 编码器
// 回写，故跨到其余族时整条丢失。
func countMessageNames(req *ir.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.Name != "" {
			n++
		}
	}
	return n
}

// countItemIDs 数携带 responses 条目原号（item id）的项数：消息级 ItemID 与
// 块级 ToolUse/Thinking.ItemID 都算。该维度只由 responses 解码器落进 IR、
// 只由 responses 编码器回写，跨族丢弃后客户端稍后发来的 store=true 条目引用
// 无处解析（ir.ToolUse.ItemID 注释明确承诺此丢弃由有损诊断报出）。
func countItemIDs(req *ir.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.ItemID != "" {
			n++
		}
		for _, b := range m.Content {
			if b.ToolUse != nil && b.ToolUse.ItemID != "" {
				n++
			}
			if b.Thinking != nil && b.Thinking.ItemID != "" {
				n++
			}
		}
	}
	return n
}

// countMessagePhase 数携带 responses message phase（commentary|final_answer）
// 的助手消息条数。该维度只由 responses 解码器落进 IR、只由 responses 编码器
// 回写，跨族丢弃后上游无从区分旁白与最终答复（官方要求 preserve-and-resend）。
func countMessagePhase(req *ir.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.ResponsesPhase != "" {
			n++
		}
	}
	return n
}

// countContentChannelReasoning 数标记为 content 通道（reasoning_text 原文，
// 而非 summary 通道摘要）的思考块条数。该维度只由 responses 解码器落进 IR、
// 只由 responses 编码器据此选回哪条通道，跨族丢弃后目标族无从区分原始推理与
// 用户摘要（正文本身逐字保留）。判据与解码器同口径：ContentChannel 仅在
// content 通道有正文时置真，故每个被计数的块都带非空正文。
func countContentChannelReasoning(req *ir.Request) int {
	n := 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Thinking != nil && b.Thinking.ContentChannel {
				n++
			}
		}
	}
	return n
}

// AudioRefDropNote 请求侧 assistant 音频引用丢失注记。音频 id 是 chat
// 多轮上下文里的服务端引用，外族既没有引用槽位，也不能据此取回音频本体。
// id 值属会话内容，不进注记。
func AudioRefDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d assistant audio reference(s): the target protocol has no replay-id slot, the model cannot recover audio generated in earlier turns", n)
}

// AudioOutputDropNote 模型音频输出丢失注记。完整音频只存在于 chat 非流式
// 响应的 message.audio：chat 的 SSE delta 与 anthropic / responses 的响应
// 都没有等价槽位，音频本体、转写文本与回放 id 一起消失。
//
// 两条路径共用：流式编码器 Notes() 与非流式 EncodeResponseLossy。
// 措辞只此一份，按说明检索流水的人不会把同一件事当成多种故障。
func AudioOutputDropNote() string {
	return "dropped model audio output: this response format has no complete-audio slot, the client cannot play the generated audio or recover its transcript and replay id"
}

// CacheCreationDetailsDropNote 缓存写入 TTL 明细丢失注记。5m/1h 细分只有
// anthropic 响应有这个槽位；转到其他协议时细分消失，但写入总量
// （cache_write_tokens）仍完整保留，故措辞明说「合计不丢」。
//
// 两条路径共用：流式编码器 Notes() 与非流式 EncodeResponseLossy。
// 措辞只此一份，按说明检索流水的人不会把同一件事当成多种故障。
func CacheCreationDetailsDropNote() string {
	return "dropped Anthropic cache-creation TTL details: this response format has no 5-minute/1-hour cache-write usage fields; aggregate input token totals remain preserved"
}

// StreamUsageDetailFrameNote 报一类流式专有的用量明细损耗：明细只挂在协议
// 的「起始帧完整 usage」上，而它偏偏随「收尾帧」到达，于是没有可承载它的帧。
// 具体是 anthropic 的缓存写入 TTL 明细与 inference_geo——官方只允许它们出现在
// message_start 的完整 Usage 里，message_delta 的 MessageDeltaUsage 没有这两个
// 槽位（renderDeltaUsage 不编，编了就是写官方 schema 没有的键）。真流式里它们
// 随 message_start 到达、已下发；但当上游忽略 stream:true 回一整份 JSON 时，
// 投影（ir.ResponseEvents）把全部用量压在 EvMessageDelta、message_start 不带
// 用量，明细与地理就送不出去——与非流式（renderUsage 照写）分叉。dims 列出
// 送不出去的维度名。措辞与 CacheCreationDetailsDropNote / UsageDetailDropNote
// 区分开：那两条说「目标格式没有槽位」，本协议其实有，只是这一帧装不下。
func StreamUsageDetailFrameNote(dims []string) string {
	return "dropped usage detail(s) (" + strings.Join(dims, ", ") +
		"): they arrived on the stream's closing usage frame, but this protocol carries them only on the opening frame, which the whole-response replay left empty; the non-streaming path delivers them, and aggregate token totals remain preserved here"
}

// DescribeResponseCacheDetailsLoss 非流式 EncodeResponseLossy 报缓存写入
// TTL 明细损耗：5m/1h 细分只有 anthropic 的响应有槽位，其余协议收到
// 明细已知的用量只能丢掉细分（合计不受影响）。流式路径由各编码器在
// 合并用量的位置自行置位标记。
func DescribeResponseCacheDetailsLoss(resp *ir.Response, name string) []string {
	if resp == nil || name == ProtocolAnthropic || !resp.Usage.CacheWriteDetailsKnown {
		return nil
	}
	return []string{CacheCreationDetailsDropNote()}
}

// LogProbsDropNote 响应侧对数概率载荷丢弃注记：IR 响应模型没有逐 token
// 概率槽位（请求侧开关可贯通，算出来的内容带不走）。只探测计数、不建模
// 内容：概率数组体积与 token 数成正比，逐条留存会把流水撑爆，而它的用途
// 是客户端本地分析，代理侧只需要让「给了但没带过去」可见。
func LogProbsDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d logprobs payload(s): per-token log probabilities have no representation in the internal response model, only the generated content is preserved", n)
}

// ModalityUsageDropNote gemini 上游 usageMetadata 里无法归一的按模态 token 明细
// 丢弃注记。IR 有输入 TEXT/IMAGE/AUDIO 与输出 TEXT/AUDIO 的模态槽位（已保全），
// 但没有视频（两侧）、输出侧图片、以及缓存/工具用量的模态细分槽位——这些明细
// 是对已捕获总量的再细分（聚合 token 总量不丢），只损多模态成本归因的可观测性。
// 与 LogProbsDropNote 同款「上游给了、IR 无槽位」处置。
func ModalityUsageDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d per-modality token breakdown(s) from the upstream usage metadata: the internal model has no slot for video, output-image, or cache/tool modality splits; aggregate token totals remain preserved", n)
}

// SafetyRatingsDropNote gemini 上游候选的按类别内容安全评级丢弃注记。官方
// Candidate.safetyRatings（Output only）携带每个危害类别的命中概率档与是否因此
// 拦截，IR 响应模型没有结构化安全评级槽位：gemini 的评级形状与 chat/responses 的
// moderation 回执不同构，硬塞进那个 RawMessage 槽位会让客户端按 OpenAI moderation
// 误解析，故只探测计数报出（与 LogProbsDropNote / ModalityUsageDropNote 同款
// 「上游给了、IR 无槽位」处置）。finishReason=SAFETY 的自由文本已由 FinishDetailNote
// 报出，但结构化的类别/概率/拦截明细此前静默；正常完成时的信息性评级同样静默。
func SafetyRatingsDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d per-category safety rating(s) from the upstream candidate: the internal response model has no slot for structured content-safety assessments (harm category, probability, blocked), so the client cannot see which categories the upstream flagged or how strongly", n)
}

// PromptSafetyRatingsDropNote gemini 上游 **prompt 级** 按类别内容安全评级丢弃注记。
// 官方 PromptFeedback.safetyRatings（「Ratings for safety of the prompt」）评的是用户
// 输入本身，与 SafetyRatingsDropNote 评的模型输出候选（Candidate.safetyRatings）同维异源。
// 独立一条而不复用候选措辞：候选那条写死「from the upstream candidate」，套到 prompt 级会
// 把「评的是 prompt」误说成「评的是输出」，措辞与实际处置对不上（违反规则 a）。prompt 被安全
// 拦截时（candidates 为空）这段评级是客户端唯一能拿到的「命中哪些类别、多强、是否拦截」明细，
// 此前连同 blockReason 之外的一切被静默吞掉。流式 Notes() 与非流式 DecodeResponseLossy 共用。
func PromptSafetyRatingsDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d per-category safety rating(s) from the upstream prompt feedback: the internal response model has no slot for structured content-safety assessments (harm category, probability, blocked), so the client cannot see which categories the upstream flagged on the prompt itself or how strongly", n)
}

// UsageDetailDropNote usage 细分维度跨族丢弃注记（聚合 token 总量不丢）。
func UsageDetailDropNote(dims []string) string {
	return "dropped usage detail(s) (" + strings.Join(dims, ", ") +
		"): this protocol's usage has no field for them; aggregate token totals remain preserved"
}

// UsageDropDims 算出把 u 编码进 protoName 家族时会被丢掉的 usage 细分维度名。
// 流式编码器的 Notes() 与非流式 EncodeResponseLossy 共用同一判据，保证
// 同一响应按 stream=true/false 请求报出的损耗一致。
//
// 细分维度的原生槽位：服务端托管工具执行次数、推理区域与迭代用量细分只有
// anthropic 有；音频、预测加速与图片/文本模态 token 只有 chat 有。与
// CacheWriteDetailsKnown 的门控不同，这几位不需要「明细已知」标记：非零值本身
// 就是上游给过的证据。
func UsageDropDims(u *ir.Usage, protoName string) []string {
	if u == nil {
		return nil
	}
	var dims []string
	if protoName != ProtocolAnthropic {
		if u.WebSearchRequests > 0 {
			dims = append(dims, "web search request count")
		}
		if u.WebFetchRequests > 0 {
			dims = append(dims, "web fetch request count")
		}
		if u.InferenceGeo != "" {
			dims = append(dims, "inference geo")
		}
		if len(u.Iterations) > 0 {
			dims = append(dims, "usage iterations breakdown")
		}
	}
	if protoName != ProtocolChatCompletions {
		if u.PromptAudioTokens > 0 {
			dims = append(dims, "prompt audio tokens")
		}
		if u.CompletionAudioTokens > 0 {
			dims = append(dims, "completion audio tokens")
		}
		if u.AcceptedPredictionTokens > 0 || u.RejectedPredictionTokens > 0 {
			dims = append(dims, "prediction tokens")
		}
		if u.PromptImageTokens > 0 {
			dims = append(dims, "prompt image tokens")
		}
		if u.PromptTextTokens > 0 {
			dims = append(dims, "prompt text tokens")
		}
		if u.CompletionTextTokens > 0 {
			dims = append(dims, "completion text tokens")
		}
	}
	return dims
}

// DescribeResponseUsageDetailsLoss 非流式 EncodeResponseLossy 报 usage
// 细分维度损耗，判据与流式编码器 Notes() 同源（UsageDropDims）。
func DescribeResponseUsageDetailsLoss(resp *ir.Response, name string) []string {
	if resp == nil {
		return nil
	}
	if dims := UsageDropDims(&resp.Usage, name); len(dims) > 0 {
		return []string{UsageDetailDropNote(dims)}
	}
	return nil
}

// StopReasonFoldNote 跨族终止原因折叠注记。cause 是被折叠掉的具体成因描述。
// 措辞落在「本服务改写了终止原因、具体成因与它暗示的补救动作丢失」上——
// 目标协议确实没有对应取值，但客户端看到的通用「输出不完整」档会误导补救方向。
func StopReasonFoldNote(cause string) string {
	return "rewrote the stop reason: the upstream reported " + cause +
		`, but this protocol has no matching stop value and folded it into its generic "output incomplete" reason; the specific cause — and the different remediation it implies — is lost`
}

// DescribeResponseStopReasonLoss 报跨族终止原因折叠损耗。三档特殊原因
// （context_window_exceeded / max_messages / steered）IR 单列，因为客户端的补救
// 动作各不相同：context_window 要压缩输入、max_messages 与输出预算无关、steered
// 是用户已在安全边界转向（加大预算毫无意义）。但每档只有母协议能原样表达
// （context_window 属 anthropic，另两档属 responses），跨族出站只能塌进目标协议
// 的通用「输出不完整」档（chat 的 length / anthropic 的 max_tokens / responses 的
// max_output_tokens），成因丢失。同族往返或母协议出站返回 nil。
// 三条路径共用：流式编码器 Notes() 与非流式 EncodeResponseLossy。
func DescribeResponseStopReasonLoss(reason ir.StopReason, name string) []string {
	switch reason {
	case ir.StopContextWindow:
		if name != ProtocolAnthropic {
			return []string{StopReasonFoldNote(
				"context-window-exceeded (the input filled the model's context window and squeezed the output short, compress the input rather than raise the output budget)")}
		}
	case ir.StopMaxMessages:
		if name != ProtocolResponses {
			return []string{StopReasonFoldNote(
				"max-messages (a message-count cap, not an output-length cap, truncated the turn; raising the token budget will not help)")}
		}
	case ir.StopSteered:
		if name != ProtocolResponses {
			return []string{StopReasonFoldNote(
				"steered (the user redirected generation mid-stream at a safety boundary; raising the output budget is meaningless)")}
		}
	case ir.StopPauseTurn:
		if name != ProtocolAnthropic {
			return []string{StopReasonFoldNote(
				"pause-turn (a long-running server-side tool paused mid-turn and the turn is resumable; the client should re-submit the partial turn as-is to let the model continue, not raise the output budget)")}
		}
	}
	return nil
}

// StopSequenceEchoDropNote 报告「命中了哪条停止序列」这一回显值的跨族丢弃。
// 措辞与 StopReasonFoldNote 分账：终止原因本身（stop_sequence）折进目标的通用
// 「stop/completed」是良性改写、不误导补救方向；真正丢的是那条被命中的序列原文，
// 目标协议根本没有回显它的字段（chat 只有 finish_reason、responses 只有 status）。
func StopSequenceEchoDropNote() string {
	return "dropped the matched stop sequence: the upstream reported which stop sequence ended the turn, but this protocol's response has no field to echo it (only the generic finish/status survives); a client that supplied several stop sequences cannot tell which one matched"
}

// DescribeResponseStopSequenceLoss 报跨族「命中停止序列回显值」的丢弃。
// ir.Response.StopSequence 只在终止原因确为 stop_sequence 时携带那条序列原文
// （见 anthropic adoptStopSequence），而只有 anthropic 出站协议有 stop_sequence
// 字段能原样回吐；chat_completions / responses 都没有对应槽位，值整条蒸发且此前
// 静默。谓词与编码侧同源：原因非 stop_sequence、或上游没给序列原文时无值可丢，
// 不报（避免假阳性）；anthropic 出站原样保留，也不报。gemini 仅出站、无响应侧
// 编码点，不涉及。三条路径共用：非流式 EncodeResponseLossy 与流式 Notes()。
func DescribeResponseStopSequenceLoss(reason ir.StopReason, seq, name string) []string {
	if reason == ir.StopStopSequence && seq != "" && name != ProtocolAnthropic {
		return []string{StopSequenceEchoDropNote()}
	}
	return nil
}

// ServerToolDropNote 服务端托管工具块丢失注记。server_tool_use 与
// web_search_tool_result 是**有 IR 块型**的，三个外族编码器都没有为它们
// 输出任何对应形态：整块消失且不计数时，接收端既看不到网关代执行了哪次
// 托管搜索，也拿不到搜回来的页面。calls / results 分别是两种块型的条数，
// 只渲染非零的那部分。搜索结果的标题、URL 与摘要属会话内容，不进注记。
//
// 措辞刻意不断言「目标协议没有槽位」：Responses 官方确有 web_search_call
// 输出项，只是本服务的转换没有实现映射。说的是转换做了什么，不是协议
// 没有什么。也刻意不写「客户端」：这条注记同时用于响应侧（受众是客户端）
// 与请求侧诊断（受众是上游模型），用「接收端」才对两个方向都成立。
//
// 三条路径共用：流式编码器 Notes()、非流式 EncodeResponseLossy、
// 请求侧 DescribeLossy。措辞只此一份，按说明检索流水的人不会把同一件事
// 当成多种故障。
func ServerToolDropNote(calls, results int) string {
	var subject string
	switch {
	case calls > 0 && results > 0:
		subject = fmt.Sprintf("%d server-side tool call(s) and %d web search result block(s)", calls, results)
	case calls > 0:
		subject = fmt.Sprintf("%d server-side tool call(s)", calls)
	default:
		subject = fmt.Sprintf("%d web search result block(s)", results)
	}
	return "dropped " + subject +
		": this protocol's conversion emits no counterpart for Anthropic's hosted-tool blocks, so the receiving side sees neither which hosted search ran nor which pages it returned, and cannot replay either in a later turn"
}

// HostedOutputItemsNote 托管输出项丢弃注记（responses 解码侧）。
// web_search_call / file_search_call 之类是上游代执行的托管工具输出项，
// 本服务的 responses 转换没有为这些 item 类型实现映射：整项丢弃时接收端
// 既看不到网关代执行了哪次调用，也拿不到它产出的结果。
//
// 与 ServerToolDropNote 同一口径：措辞不断言「协议没有槽位」——中立表示
// 有服务端工具块型，只是这条转换没有实现映射。说的是转换做了什么。
// 查询串与搜索结果属会话内容，不进注记；执行次数若上游在 usage 里给了
// （server_tool_use / *_requests 维度），仍照常记账，故措辞只说内容不可见。
//
// 两条路径共用：responses 流式解码器 Notes() 与非流式 DecodeResponseLossy。
// 措辞只此一份，按说明检索流水的人不会把同一件事当成多种故障。
func HostedOutputItemsNote(n int) string {
	return fmt.Sprintf(
		"dropped %d hosted output item(s) (web_search_call and similar): this protocol's conversion maps no counterpart for their item types, so the receiving side sees neither which hosted calls ran nor what they returned", n)
}

// toolErrorPrefix 是工具结果失败态在无原生标记的协议上的表达。
//
// 方括号形态在工具输出里罕见，不易与工具自己打的内容混淆。
const toolErrorPrefix = "[tool error] "

// PrefixToolResultError 在目标协议没有失败标记时给工具结果前置一个标记块。
//
// caps 承载得了就原样返回内容：anthropic 与 gemini 有原生字段，加前缀等于
// 把同一件事说两遍，其中一遍还混在工具的真实输出里。
//
// 返回新切片、不改入参：调用方的 IR 要留着换目标重试。
//
// 出处只有这一个：两个调用点若各写一份措辞，改了一处忘另一处的症状是
// 「换个目标协议，同一个失败的前缀文本不一样」，而下游按文本匹配的
// 客户端会只认出其中一种。
func PrefixToolResultError(result *ir.ToolResult, caps Capabilities) []ir.Block {
	if result == nil {
		return nil
	}
	if !result.IsError || caps.ToolResultError {
		return result.Content
	}
	out := make([]ir.Block, 0, len(result.Content)+1)
	out = append(out, ir.Block{Type: ir.BlockText, Text: toolErrorPrefix})
	return append(out, result.Content...)
}

// ToolErrorPrefix 供需要拼字符串的调用点取用同一份措辞。
func ToolErrorPrefix() string { return toolErrorPrefix }

// AdoptToolResultError 在入站解码时把前缀还原成失败标记位。
//
// 没有原生失败字段的协议（chat_completions、responses）里，失败态是我们
// 自己上一轮用 PrefixToolResultError 写进正文的。客户端把整段历史回传后，
// 若不认这个前缀，标记位读作 false：再路由到 anthropic 或 gemini 时模型被
// 告知工具调用成功，而正文写着 [tool error] connection refused。多跳还会
// 把前缀叠成 [tool error] [tool error] …。
//
// 剥掉前缀而不是只置位：留着它等于把同一件事说两遍，且下一跳若又落到无
// 原生字段的协议，PrefixToolResultError 会再加一层。
//
// 只认块首那一处：正文中间出现同样的字面量是工具自己打的内容，
// 改写它会篡改工具输出。
func AdoptToolResultError(blocks []ir.Block) ([]ir.Block, bool) {
	if len(blocks) == 0 || blocks[0].Type != ir.BlockText {
		return blocks, false
	}
	if !strings.HasPrefix(blocks[0].Text, toolErrorPrefix) {
		return blocks, false
	}
	out := make([]ir.Block, len(blocks))
	copy(out, blocks)
	out[0].Text = strings.TrimPrefix(out[0].Text, toolErrorPrefix)
	// 前缀独占一块时（PrefixToolResultError 写出的正是这个形态）整块去掉，
	// 留一个空文本块会让下游协议多出一个无内容的 part。
	if out[0].Text == "" {
		out = out[1:]
	}
	return out, true
}

// hasFailedToolResult 判断请求里是否带着失败的工具结果。
func hasFailedToolResult(req *ir.Request) bool {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult && b.ToolResult != nil && b.ToolResult.IsError {
				return true
			}
		}
	}
	return false
}
