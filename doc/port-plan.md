# ModelSurge 误投提交移植评估与修改计划

> 范围：ModelSurge 仓（W:/github.com/aceaura/ModelSurge）`40ee69d..109ff61` 共 80 个 commit（R 系列保真强化），
> 逐个评估是否应移植到本仓（model-surge-agent）。评估基准为本仓当前 HEAD（016fd7c）的实际代码。
> 参考项目（已在本地，仅存疑时比对）：cc-switch、new-api、sub2api。

## 0. 判定口径与总体结论

| 判定 | 含义 | 数量 |
| --- | --- | --- |
| 【已有等价】 | 本仓已实现相同语义（机制可不同），附证据 | 24 |
| 【不适用】 | kiro 专属 / gemini 入站专属（本仓 gemini 纯出站）/ 缺陷条件在本仓不存在 / 本仓有意相反设计 | 7 |
| 【需要移植】 | 本仓完全缺失，应移植 | 23 |
| 【部分需要】 | 部分点已有等价，缺的部分应移植 | 26 |

**三个贯穿性事实**（影响大量判定）：

1. 本仓 IR 比旧仓瘦：无 `BlockOpaque` / 服务端工具响应块 / Citation / 音频输出 / HostedRaw 等。未知块、part、item 的策略是**入站 400 拒收**，而非旧仓的「归 opaque 同族透传」——静默丢失在本仓多为响亮拒绝，但同族往返保真也随之丧失（如 anthropic 客户端多轮历史带 server_tool_use 时本仓直接 400）。
2. gemini 在本仓**纯出站**：旧仓凡 gemini 入站解码、gemini 客户端方向流编码器的改动均不适用。
3. 本仓已有自己的一整套保真强化（`.kiro/specs/` 40+ 个 spec），约三成提交已有等价，判定以代码证据为准。

## 1. 【协议-需批准】清单（上下游对接协议语义变化，须经同意后才可实施）

全部 80 个 commit **无一**改动 relayclient 契约字段、env 变量、DB schema、Redis 键或 admin API 契约。仅以下 3 项会改变**上报给 relay 的 outcome 归因语义**（控制面行为变化，枚举值不变但归因口径变）：

| # | commit | 变化内容 | Before → After | 涉及文件 |
| --- | --- | --- | --- | --- |
| P1 | 0720efc | 402 归入 rate_limit 族 | 欠费上游：outcome=abnormal、不换目标、白烧当前账号 → outcome=retrying、换号重试 | `codec/errkind.go` KindForStatus 加 `402 → ir.ErrRateLimit` |
| P2 | 63ce626 | 408/425 归入 timeout 族 | 落 default=invalid_request：abnormal、不重试 → retrying、换目标 | `codec/errkind.go` KindForStatus 加 `408/425 → ir.ErrTimeout` |
| P3 | 4b6afce | 流内风控错误（cyber_policy/content_filter 类）特判不可重试 | 流内风控拦截归 ErrUpstream：retrying、反复换号硬撞 → 不重试，直接向客户端报错 | `codec/errkind.go` codeKinds 加不可重试码表 + `responses/wire.go`+`decode_stream.go` 嵌套 error 形态回落 |

注意：P1/P2 与 P3 方向相反（前者放大重试、后者收敛重试），但共用同一张 `codeKinds`/`KindFor` 判据表，建议作为一个批次一起评审。

## 2. 【设计冲突-需决策】清单（不属协议，但移植会推翻本仓既定设计，须先拍板）

| # | commit | 冲突点 |
| --- | --- | --- |
| D1 | 75aab75 / 3364ffa | 未知块/part 归 opaque 同族透传 **vs** 本仓「未知一律 400」——本仓更防御，但牺牲同族往返保真（chat→chat 的未知 part 也 400）。是否引入 opaque 机制需决策 |
| D2 | 0842392 | redacted_thinking 同族密文往返 **vs** 本仓钉死测试 `TestSameProtocolStillNormalizes`（"must be dropped, not passed through"）。移植需推翻该测试与安全续话考量 |
| D3 | 42ce081 | 坏帧跳帧续流+注记 **vs** 本仓坏帧即终止整流报错（fail-fast）。两种容错哲学二选一 |
| 已决不回退 | ad3e34e / 344c99c | 旧仓透传 previous_response_id/conversation/context_management/prompt，本仓**有意 400 拒收**（multi-turn-state-fidelity 已决：代理不托管会话状态）。维持现状，不移植 |

## 3. 逐 commit 评估表（80 行，按提交顺序）

| # | commit | 判定 | 标记 | 要点（证据 / 移植建议） |
| --- | --- | --- | --- | --- |
| 1 | a803964 有损诊断补假阴性+kiro | 已有等价 | — | ParallelToolCalls 三态贯通（ir/types.go:224、paramfidelity_test.go）；kiro 部分不适用 |
| 2 | 828e645 gemini includeThoughts 思考抑制 | 不适用 | — | 唯一触发源是 gemini 入站，本仓无 gemini 入站 |
| 3 | 7025216 responses 跳过服务端工具块 | 需要移植 | — | 本仓无服务端工具响应块型，anthropic 上游真用 web_search 会整流报 unknown block type（decode_stream.go:77-81）。IR 增块型+anthropic 解码放行+三外族编码 skip 不烧 output_index |
| 4 | 62f3150 零状态码按类型反推 | 已有等价 | — | codec.StatusForKind（errkind.go:159）+三族编码器统一回落 |
| 5 | 004c82b 工具失败标志 | 已有等价 | — | Capabilities.ToolResultError + PrefixToolResultError 往返（lossy.go:500/528） |
| 6 | 0ec89e5 ReasoningTokens 贯通 | 已有等价 | — | ir.Usage 五维，已进 store/cache/agentv1 契约（usagecolumns_test.go） |
| 7 | b889c44 结构化输出贯通 IR | 已有等价 | — | ir.ResponseFormat + 能力位 + gemini applyResponseFormat |
| 8 | b9c4916 停止原因保真 | 已有等价 | — | StopStopSequence/incomplete_details 矩阵（crossmatrix_test.go:2016） |
| 9 | 937727a 多模态块进 IR | 已有等价 | — | Media 四类块 + DowngradeMedia 占位 + moveToolResultMedia |
| 10 | de788ca normalize 原地写入修复 | 已有等价 | — | 本仓同类展开均新分配（shape.go:87-110），缺陷形态不存在 |
| 11 | 1c98192 拒绝正文保真 | 部分需要 | — | responses 方向已有；**chatcompletions 全仓无 refusal**，message.refusal 正文被丢弃。补 chat wire Refusal+解码 |
| 12 | b2737e7 Citation 维度四协议贯通 | 需要移植 | — | 本仓零承载，citation_delta 被静默跳过（stream_test.go:222 钉住）。IR+四协议+ir/citation.go 互推逻辑 |
| 13 | 48a9d30 断流不伪装正常完成 | 已有等价 | — | pipeline 层 teardownCause 归上游错误（budget.go:33-53） |
| 14 | cce486c 思考签名来源门控 | 已有等价 | — | Event.SignatureFrom + ForeignSignature 门控 + crossmatrix 同族/外族矩阵 |
| 15 | d556ff2 采样参数五维 | 已有等价 | — | 五维指针三态 + 能力位 + paramfidelity 全矩阵 |
| 16 | f7a422e 工具参数完整性 | 部分需要 | — | 截断识别/同 index 拆块已有；**病态入参被静默补 {} 无诊断**（anthropic/chat encode_request）。移植 NormalizeToolInput 思路+lossy 注记 |
| 17 | 1b0d65a gemini 流式引用 partIndex | 不适用 | — | gemini 客户端编码器不存在；且依赖 #12，未来加 gemini 入站再议 |
| 18 | e233dde user 标识三维贯通 | 已有等价 | — | Metadata["user_id"] 约定三协议双向（paramfidelity_test.go:113） |
| 19 | ad3e34e 会话链保真 | 已有等价 | — | 本仓有意 400 拒收 statefulFields（设计已决，见 §2） |
| 20 | 34f9e5b 响应侧损耗报出 | 已有等价 | — | StreamNotes/LossyResponseEncoder 四出口 + rec.addResponseLossy |
| 21 | 4dace58 responses 专属字段 | 部分需要 | — | include 已有；conversation/prompt 有意拒收不回退；**background 静默蒸发**，补 wire+IR+同族回写+跨族注记 |
| 22 | bcc67c8 anthropic output_config.format | 需要移植 | — | 全仓无 output_config；改 anthropic wire/decode/encode，caps 拆 ResponseSchema，lossy 分档骨架已具备 |
| 23 | b5c2d9d service_tier+prompt_cache_key | 部分需要 | — | chat/responses 原值贯通已有；缺 anthropic 槽位、MapServiceTier 值集映射（standard_only 原写会 400）、**prompt_cache_key 零命中** |
| 24 | f3bb14b OpenAI 2026 字段 | 部分需要 | — | responses verbosity 已有；缺 chat 顶层 verbosity、safety_identifier、moderation、prompt_cache_options |
| 25 | b2c66d6 cache_control ttl+工具断点 | 部分需要 | — | 块级断点往返+4 断点整形已有；缺 CacheTTL 维与 Tool.CacheCtl |
| 26 | ca016e7 工具 strict 三族贯通 | 需要移植 | — | ir.Tool 无 Strict；改 ir+三族 wire/decode/encode |
| 27 | a9d0095 anthropic 工具修饰四维 | 需要移植 | — | defer_loading/eager_input_streaming/input_examples/allowed_callers 零命中；ir.Tool+anthropic+lossy 计数 |
| 28 | f2be160 思考现代化 adaptive/display/effort | 需要移植 | — | 本仓 budgetForEffort 折预算即旧仓改造前行为；移植后保留其作跨族兜底 |
| 29 | 9164308 响应侧 service_tier 回显 | 部分需要 | — | chat/responses 完整等价（aggregate.go:268 晚到覆盖）；缺 anthropic 响应侧槽位与 MapServiceTierEcho |
| 30 | 41a1688 顶层 cache_control 糖+inference_geo | 需要移植 | — | 零命中；ir.Request+anthropic+lossy |
| 31 | 2d9fc72 anthropic container 链路 | 需要移植 | — | ir（Request/Event/Response+聚合器）+anthropic 四文件+外族丢容器注记；与 #34 一起移 |
| 32 | 344c99c responses context_management | 不适用 | — | 本仓有意 400 拒收（multi-turn-state-fidelity 已决） |
| 33 | cb28f56 chat 专属四维 | 需要移植 | — | modalities/audio/prediction/web_search_options 零命中；ir.Request+chatcompletions+lossy |
| 34 | d14cf51 container_upload 块 | 需要移植 | — | 依赖 #31；ir 块型+anthropic 编解码+外族跳过计数 |
| 35 | bf41a0b chat 音频输出与多轮引用 | 需要移植 | — | 本仓只有输入音频；Response.Audio/Message.AudioID+sanitize 相邻不合并 |
| 36 | e182c11 畸形工具参数保真 | 部分需要 | — | kiro 部分不适用；**三族编码器静默换 {} 且无注记**（chat encode_stream.go:259 等），补保留原文/至少必报注 |
| 37 | 1f67a84 gemini 流式引用稳定 part | 不适用 | — | gemini 客户端编码器不存在 |
| 38 | 2ae8f77 done 帧参数前缀补齐 | 部分需要 | — | item.added 已处理；**evFunctionArgsDone 被忽略、done-only 上游参数整条丢**（decode_stream.go:135-139） |
| 39 | 1b0ea01 responses 自定义工具 | 部分需要 | — | 声明侧跳过+注记已有（有意不降级）；缺响应侧 custom_tool_call 自由文本输入（IR ToolUse 无 InputText） |
| 40 | 4c841d6 chat 多候选报裁剪 | 已有等价 | — | DroppedCandidatesNote（lossy.go:454）；可选补：无 index 0 时回退最小序号 |
| 41 | 7300b6b 缓存 TTL 明细 | 部分需要 | — | 总量贯通+分帧合并已有；缺 5m/1h TTL 三维+anthropic cache_creation 子对象+外族注记 |
| 42 | a5c9348 responses 块寻址稠密 | 已有等价 | — | partKey 两级键+allocate 稠密发号+backfillIndexFields |
| 43 | 5e856cf done 终态回补 | 部分需要 | — | 编码侧已有一半；**解码侧 done 帧整帧丢弃，只发终态的上游丢正文**（decode_stream.go:135-138）。文本/拒绝/思考摘要三通道回补 |
| 44 | 4b6afce responses 流式错误帧 | 部分需要 | P3 | 终止性已有；缺嵌套 `{"error":{...}}` 双层回落 + 风控码不可重试特判（见 P3） |
| 45 | c1c7046 流内错误帧+共用判据表 | 已有等价 | — | wireErrorEnvelope 识别 + Kind 单点推导 Retryable |
| 46 | 60e3c39 kiro 远程错误保真 | 不适用 | — | kiro 不存在 |
| 47 | 4dc1015 错误帧终止性对齐 | 已有等价 | — | 三族 errored 终止守卫（streamerrorteardown_test.go） |
| 48 | 08018fd 可重试性按类型判 | 已有等价 | — | Retryable 由 Kind 单点推导，设计上不可能抹平 |
| 49 | 0720efc 402/413 规范类型 | 部分需要 | P1 | 413 已有（归 ErrContextExceeded 更优）；402 缺（见 P1） |
| 50 | 63913bf 思考档位与 reasoning 子参数 | 部分需要 | — | none/minimal 三态已有；缺 summary/context/mode 子参数（encode 硬编码 Summary="auto"） |
| 51 | 63ce626 错误体+Retry-After+408/425 | 部分需要 | P2 | 错误体解析/Retry-After 已有且更强（errbody.go+ratelimit 包）；缺 408/425（见 P2） |
| 52 | b1f6764 chat 流式 usage 帧 | 已有等价 | — | include_usage 三态 9 测试锁定（includeusage_test.go） |
| 53 | 0f82244 响应头回传 | 部分需要 | — | 限流两族白名单+X-Accel-Buffering 已有；缺 x-request-id 回传 |
| 54 | 3e72a1c anthropic-beta 头转交 | 已有等价 | — | ParseBetas+DeclarationEncoder 协议门控（declarations_test.go） |
| 55 | 0842392 redacted_thinking 往返 | 部分需要 | D2 | 外族丢弃+诊断已有；同族往返与本仓钉死测试冲突（见 D2） |
| 56 | 75aab75 未知块归 opaque | 需要移植 | D1 | 无 BlockOpaque；未知块整请求 400。需先决策 D1 |
| 57 | d278429 错误体类型不匹配容忍 | 部分需要 | — | 主形态已有（ExtractMessage any 树）；残余：数字 message 丢 code。低优先 |
| 58 | 09ac4d0 移除全部 Kiro 逻辑 | 不适用 | — | 本仓从初始起无 kiro |
| 59 | c08951e 引用五类型 | 需要移植 | — | 依赖 #12 底座一起移植；anthropic citation.go 五形态+三通道损耗注记 |
| 60 | 8a096ba Clone 空 Raw 变 null 修复 | 不适用 | — | 本仓无 Citation.Raw 且无 JSON 往返 Clone；移植引用时须遵守「RawMessage 必带 omitempty」纪律 |
| 61 | e830da2 托管工具块跨族损耗可见 | 需要移植 | — | 依赖 #3 块型；三外族流式/非流式/请求侧统一 ServerToolDropNote |
| 62 | 4689a8a anthropic document 块保真 | 需要移植 | — | source.type=content 现产空壳 Media 正文静默消失；wire Source 加 content 形态+块级 context/citations+跨族注记 |
| 63 | 4877bf7 responses output_index 稠密 | 已有等价 | — | openBlock 无 skip 路径，稠密不变量结构上成立 |
| 64 | ecf908f 模型产出附件跨族保真 | 需要移植 | — | **bug 原样存在**：responses 媒体块伪造成空 message 烧 output_index（encode_stream.go:82-87）；chat 流式媒体零注记蒸发。跳过+计数+MediaOutputDropNote |
| 65 | 3364ffa 未知 content part 归 opaque | 需要移植 | D1 | 与 #56 同一决策；若否决 opaque 则两项同判不适用 |
| 66 | 5f2d8e2 Clone 不伪造 null | 已有等价 | — | 本仓 IR 无 RawMessage 字段，Clone 手写深拷贝，机理不存在 |
| 67 | a85bb34 图片部件保真 | 部分需要 | — | detail 已有；**空壳图片仍编非法形状**（空 base64）、image_url 单形态（另一形态 400）、file_id 缺建模（responses 给 file_id 直接 400、chat 把 file_id 当 URL 透传） |
| 68 | 749c224 R100 tool_choice 等 | 部分需要 | — | 零工具丢 tool_choice/指名未声明降级已有；缺 AllowedTools 收窄（现 400）、chat metadata 全键（静默丢无注记）、chat modalities 输出请求注记 |
| 69 | e701cc5 R101 托管声明参数+保活帧 | 部分需要 | — | 保活帧/原生名往返已有；**HostedParams 五字段同族往返也静默丢**（wireTool 无字段） |
| 70 | d572f80 R102 托管输出项+usage 细分+回执 | 需要移植 | — | responses 托管输出项 default 静默忽略连注记都没有（decode_stream.go:205-208）；缺 StopDetails/SystemFingerprint/Container 回执与 usage 扩维 |
| 71 | 42ce081 R103 结构边界 | 部分需要 | D3 | 缺：跳帧续流（D3 决策项）、function_call_output.output 双形态（数组形态 400）、created 保真（恒本地钟）、零增量 tool_use 补 "{}" delta |
| 72 | b727827 connection/upstream_error 拆分 | 已有等价 | — | ErrTransport/ErrUpstream/ErrInvalidRequest 分类已达成同等结论 |
| 73 | 2cfd0d0 R104 错误终态与帧保真 | 需要移植 | — | 多点缺失：**非流式 failed 伪装 completed、chat 200+error 体假成功、function_call_output 空 output 键消失成非法形状**、零增量 "{}"、hosted 修饰回写、流式 created |
| 74 | 4c90c37 R105 HostedRaw 原文回吐 | 需要移植 | — | anthropic wireTool 只解析四字段，display_width_px 等静默丢；Tool 加原文槽位同族回吐+responses 对称 |
| 75 | e69032b R105 请求侧剩余声明字段 | 部分需要 | — | truncation/gemini effort 已有；缺 max_tool_calls、include_obfuscation、json_schema description、gemini executableCode 类 part 静默蒸发 |
| 76 | c4974be R106 跨族明细十点 | 部分需要 | — | additionalProperties/service_tier 注记已有；缺未知帧计数注记（低成本高收益）、chat messages[].name、logprobs/usage 注记等 |
| 77 | daf5e9f R107 同族往返 | 部分需要 | — | 缺 responses item id 三槽位（编码从不填 id）、reasoning content 数组通道、未知帧计数、chat custom/deprecated function_call 形态 |
| 78 | 2e16572 R108 2026 新字段与帧结构 | 需要移植 | — | 多点缺失：帧三键（sequence_number/item_id/annotation_index）、cache_write 双向（wireInputDetails 只 CachedTokens）、typed tool_choice、max_completion_tokens 键名带回、context_window 独立停止档（现误归 StopContentFilter）等 |
| 79 | 2d025d0 R109 契约缺口 16 点 | 需要移植 | — | 多点缺失：Created 透传、incomplete reason 细分（max_messages 等一律 StopMaxTokens）、compaction_delta/进度帧/mergedSummary 注记、anthropic output_format 旧槽、废弃 functions 折现代槽位 |
| 80 | 109ff61 R110 响应侧回执与停止档 | 需要移植 | — | 多点缺失：steered 独立档、usage.iterations、completed_at/缓存诊断/审核回执透传、reasoning 双通道分离（现并入同一 Thinking 块）、gemini toolCall/toolResponse part 静默蒸发 |

## 4. 移植计划（分批，含依赖关系与建议顺序）

### B0 · 真 bug 修复（产非法形状 / 伪装成功，最高优先）

不引入新设计、不动协议，全部是对现有 codec 的纠正：

| 项 | 内容 | 主要文件 |
| --- | --- | --- |
| B0-1（#73） | responses 非流式 failed/cancelled 不再伪装 completed；chat 200+error 体不再假成功；function_call_output 空 output 恒写键（现 omitempty 致非法形状）；零增量 tool_use 补 "{}" delta | `responses/decode_stream.go`、`chatcompletions/decode_stream.go`、`responses/encode_request.go`、三族 `encode_stream.go` |
| B0-2（#64） | responses 媒体块不再伪造空 message 烧 output_index；chat 流式/两族非流式媒体输出丢弃计数+MediaOutputDropNote | `responses/encode_stream.go`、`chatcompletions/encode_stream.go`、`codec/lossy.go` |
| B0-3（#67） | 空壳图片跳过判据不编非法形状；image_url 双形态兼容；file_id 建模（同族槽位+跨族注记） | `ir/types.go` Media、`chatcompletions`、`responses`、`anthropic` 编解码 |
| B0-4（#43+#38） | responses done 终态回补：output_text/refusal/思考摘要三通道按前缀判据补缺；function_call_arguments.done 完整参数回补 | `responses/decode_stream.go`、`responses/encode_stream.go` |
| B0-5（#71 部分） | function_call_output.output 双形态（字符串/数组）；非流式 created 保真（不恒本地钟） | `responses/wire.go`、`chatcompletions/encode_stream.go` |
| B0-6（#16+#36） | 病态工具入参不再静默换 {}：保留原文（字符串槽位协议）+ lossy 注记 | 四族 `encode_request.go`/`encode_stream.go`、`codec/lossy.go` |

### B1 · 错误归因与 outcome 语义（**P1/P2/P3，须批准后实施**）

| 项 | 内容 | 主要文件 |
| --- | --- | --- |
| B1-1（#49） | 402 → ErrRateLimit（换号） | `codec/errkind.go` |
| B1-2（#51） | 408/425 → ErrTimeout（换目标） | `codec/errkind.go` |
| B1-3（#44） | 流内风控码不可重试特判 + responses 嵌套 error 形态双层回落 | `codec/errkind.go`、`responses/wire.go`、`responses/decode_stream.go` |

### B2 · 不透明往返底座（D1 决策后实施；多数后续批次的前置）

| 项 | 内容 | 依赖 |
| --- | --- | --- |
| B2-1（#3） | IR 增服务端工具响应块型；anthropic 解码放行同族保留；三外族编码 skip 不烧 output_index | 无（可独立于 D1 先做，解多轮 web_search 400/断流的现实缺口） |
| B2-2（#56+#65） | IR 增 Opaque 块（带来源族标记）；三族 decode default 改收 opaque；encode 按族回吐/跳过+注记 | **D1** |
| B2-3（#61） | 托管工具块跨族丢弃三路径统一注记 | B2-1 |
| B2-4（#62） | anthropic document 块 content source 归 opaque+context/citations 配置贯通 | B2-2 |

### B3 · Citation 引用维度（独立成批，改动面大）

| 项 | 内容 |
| --- | --- |
| B3-1（#12） | IR 块增 Citations；四协议双向（anthropic citations_delta、chat/responses annotations、gemini groundingSupports 字节↔字符换算）；ir/citation.go 互推逻辑 |
| B3-2（#59） | 引用五类型同族往返+Portable 判据+三通道损耗注记 |
| 纪律（#60） | RawMessage 字段必带 omitempty；#17/#37 待未来 gemini 入站再议 |

### B4 · anthropic 2026 新面

| 项 | 内容 |
| --- | --- |
| B4-1（#22） | output_config.format 结构化输出贯通 |
| B4-2（#28） | 思考现代化 adaptive/display/output_config.effort（保留 budgetForEffort 作跨族兜底） |
| B4-3（#26+#27） | Tool.Strict 三族 + 工具修饰四维（defer_loading 等） |
| B4-4（#30） | 顶层 cache_control 糖 + inference_geo |
| B4-5（#31+#34） | container 链路 + container_upload 块（两单一起） |
| B4-6（#74+#69 部分） | HostedRaw 同族原文回吐 + HostedParams 五字段 |
| B4-7（#55） | redacted_thinking 同族往返（**D2 决策后**） |

### B5 · chat / responses 2026 新面

| 项 | 内容 |
| --- | --- |
| B5-1（#33+#35） | chat 专属四维（modalities/audio/prediction/web_search_options）+ 音频输出与多轮引用（两单一起，audio 请求/响应闭环） |
| B5-2（#11） | chat refusal 正文解码 |
| B5-3（#21） | responses background 字段 |
| B5-4（#39） | 响应侧 custom_tool_call 自由文本输入 |
| B5-5（#68） | AllowedTools 白名单、chat metadata 全键注记、modalities 输出请求注记 |
| B5-6（#75） | max_tool_calls / include_obfuscation / json_schema description / gemini executableCode 可见性 |

### B6 · 跨族参数与小项（低成本，可穿插）

#23（service_tier 值集映射+prompt_cache_key）、#24（chat verbosity 等三键）、#25（CacheTTL+工具断点）、#29（anthropic 响应侧档位回显）、#41（缓存 TTL 明细）、#50（reasoning summary 子参数）、#53（x-request-id 回传）、#57（数字 message 容忍，低优先）、#70（托管输出项计数注记+结构回执，可先只做注记）、#76（未知帧计数注记+chat name）。

### B7 · responses 帧结构与停止档（R107–R110，保真长尾）

#77（item id 三槽位+reasoning content 通道+未知帧注记）、#78（帧三键+cache_write 双向+typed tool_choice+context_window 独立档）、#79（Created 透传+incomplete reason 细分+废弃 functions 折现代槽位）、#80（steered 档+reasoning 双通道+completed_at 等回执+gemini toolCall/toolResponse part）。其中「incomplete reason 细分 / context_window / steered 独立停止档」三点相互关联，建议连续做。

## 5. 明确不做

- kiro 相关一切（#46、#58、各 commit 的 kiro 分支）：本仓无此协议。
- gemini 入站/客户端方向（#2、#17、#37）：本仓 gemini 纯出站。
- 会话状态透传回退（#19、#32）：本仓有意 400 拒收，维持。
- #60：缺陷条件不存在，仅作为移植引用时的编码纪律。

## 6. 移植执行结果（附录）

> 执行基线：本仓 HEAD `016fd7c`。口径：用户批准「无争议项全量移植，P1–P3 与 D1–D3 搁置」，
> 每个移植点一个 commit，验证绿后提交、不 push。usage 新维度只进 request_log/Redis/agentv1
> 观测面，不动 relay 契约（#41/#70/#80 一致遵守）。
>
> 结果：计划 46 个移植点全部落地为 46 个 commit，另有 1 个补救 commit（`249b3d0`，
> created 经「整份投影→聚合」生产路径的补投影，属 #71b/#70 created 保真的收尾），
> 合计 `016fd7c..HEAD` 47 个 commit。每点 `go build ./...` + 相关包 `go test` 绿后提交；
> 每阶段末全量 `go test -count=1 -p 1 ./...`（PG 5434 / Redis 6381）exit 0。

### 6.1 已移植对照表（计划项 → 旧仓 # → commit）

| 计划项 | 旧仓 # | commit | 主题 |
| --- | --- | --- | --- |
| 1 | #73a | `3900399` | responses 非流式 failed/cancelled 不再伪装 completed |
| 2 | #73b | `651c1a5` | chat 200+error 体不再假成功 |
| 3 | #73c | `36b1a38` | function_call_output 空 output 恒写键（Output 改 *string） |
| 4 | #73d | `1df9522` | 零增量 tool_use 关块补 "{}" delta（anthropic+chat） |
| 5 | #64 | `2c1893b` | responses 媒体块不伪造空 message 烧 output_index + chat 媒体丢弃注记 |
| 6 | #67 | `f2817c1` | 空壳图片跳过 + image_url 双形态 + file_id 建模 |
| 7 | #43 | `cd12aa5` | responses done 终态三通道回补 |
| 8 | #38 | `700e100` | function_call_arguments.done 完整参数按前缀回补 |
| 9 | #71a | `1c0975c` | function_call_output.output 双形态（字符串/数组） |
| 10 | #71b | `c81c877` | created 保真（上游创建时间不被本地钟覆盖） |
| — | （#71b/#70 收尾） | `249b3d0` | 补救：整份响应投影补上 created（非流式重放路径） |
| 11 | #16+#36 | `ed6b47a` | 病态工具入参保留原文 + lossy 注记 |
| 12 | #44a | `e189ac8` | responses 嵌套 error 形态双层回落（不含风控特判） |
| 13 | #3 | `10b08c4` | IR 服务端工具响应块 + anthropic 放行 + 外族 skip 不烧 output_index |
| 14 | #61 | `0f4bb54` | 托管工具块跨族丢弃三路径统一注记 |
| 15 | #12 | `f083070` | IR Citation + 四协议双向 + 互推逻辑 |
| 16 | #59 | `63825cd` | 引用五类型 + Portable 判据 + 三通道注记 |
| 17 | #22 | `a53c7fe` | anthropic output_config.format 结构化输出 |
| 18 | #28 | `8fe43da` | 思考现代化 adaptive/display/effort（保留 budgetForEffort 兜底） |
| 19 | #26 | `40ebb07` | Tool.Strict 三族（gemini 无槽位照实报出） |
| 20 | #27 | `79e06cc` | 工具修饰四维（defer_loading 等） |
| 21 | #30 | `78db749` | 顶层 cache_control 糖 + inference_geo |
| 22 | #31 | `274d5e1` | anthropic container 链路双向 |
| 23 | #34 | `d4617fd` | container_upload 块全链路 |
| 24 | #74 | `4aae710` | HostedRaw（ServerRaw）同族原文回吐 |
| 25 | #69 | `3b89104` | HostedParams（ServerParams）五字段 |
| 26 | #33 | `98007c7` | chat 专属四维（modalities/audio/prediction/web_search_options） |
| 27 | #35 | `251993a` | chat 音频输出 + 多轮音频引用 |
| 28 | #11 | `1dd1d1b` | chat refusal 正文解码（IR 增 BlockRefusal） |
| 29 | #21 | `50a107e` | responses background 可见化 |
| 30 | #39 | `b8b7b6f` | custom_tool_call 自由文本输入全链路 |
| 31 | #68 | `d081521` | AllowedTools 白名单 + chat metadata 全键 + modalities 注记 |
| 32 | #75 | `2643bb4` | max_tool_calls / include_obfuscation / json_schema description（gemini 四项不适用） |
| 33 | #23 | `ef817fd` | service_tier 值集映射 + prompt_cache_key + anthropic 槽位 |
| 34 | #24 | `41e90ae` | chat verbosity / safety_identifier / moderation / prompt_cache_options |
| 35 | #25 | `87961a0` | CacheTTL + 工具断点 |
| 36 | #29 | `6f30c4c` | anthropic 响应侧档位回显 |
| 37 | #41 | `5b0281e` | 缓存 TTL 明细（仅本仓观测面） |
| 38 | #50 | `2f23452` | reasoning summary/context/mode 子参数 |
| 39 | #53 | `66c84be` | x-request-id 改名回传（X-Upstream-Request-Id） |
| 40 | #57 | `438ee02` | 数字 message 容忍（不连累解得好的 code） |
| 41 | #70 | `2524eb1` | 托管输出项注记 + 结构回执（usage 细分仅本仓观测面） |
| 42 | #76 | `8ca6359` | 未知帧计数 + chat name + logprobs/usage 注记 |
| 43 | #77 | `62bad27` | item id 三槽位 + reasoning content 通道 |
| 44 | #78 | `c24d724` | 帧三键 + cache_write 双向 + typed tool_choice + context_window 独立档 |
| 45 | #79 | `f032c1b` | Created 透传 + incomplete reason 细分 + 废弃 functions 折现代槽位 |
| 46 | #80 | `761be15` | steered 独立档 + usage.iterations + completed_at/缓存诊断/审核回执 + reasoning 双通道 |

### 6.2 搁置清单（按用户批准口径，本轮不移植）

| 项 | 旧仓 # | 搁置原因 |
| --- | --- | --- |
| P1 | #49（402 归因） | 改变上报 relay 的 outcome 归因语义，须单独批准 |
| P2 | #51（408/425 归因） | 同上 |
| P3 | #44 风控不可重试特判 | 同上（注：#44 的「嵌套 error 双层回落」纯保真部分已作为计划项 12 移植） |
| D1 | #56 / #65（未知块/part 归 opaque） | 与本仓「未知一律 400」既定设计冲突，须先拍板 |
| — | #62（anthropic document 块保真） | 依赖 D1 的 opaque 底座，随 D1 搁置 |
| D2 | #55（redacted_thinking 同族往返） | 与本仓钉死测试 `TestSameProtocolStillNormalizes` 冲突 |
| D3 | #71 跳帧续流 | 与本仓坏帧 fail-fast 哲学冲突（注：#71 的 output 双形态/created 已作为计划项 9/10 移植） |

### 6.3 #80 子项处置（旧仓 R110 共 10 点）

- 点 1–8（steered 档 / iterations 透传与注记 / prompt_cache_diagnostics / moderation /
  completed_at / reasoning 双通道解码与编码）：**已移植**，见 `761be15`。
- 点 9（anthropic 响应侧 image/document 块跳过）：**未移植**——不在本计划 #80 行
  （doc 第 126 行）的范围内，旧仓另列。
- 点 10（gemini toolCall/toolResponse 归不透明块）：**不适用**——本仓 gemini 纯出站、
  无入站解码路径，且 IR 无 BlockOpaque 型（同 #75 口径）。
