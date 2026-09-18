# ModelSurge Agent API 参考

数据面「协议转换网关」服务的 HTTP 接口规范。

- **文档版本**：2.0（2026-09-17）
- **对应实现**：`backend/httpapi` + `backend/contract/agentv1`（基线 commit `16cf7c1`）
- **服务定位**：接收客户端的 Anthropic / OpenAI 请求，转内部 IR 后按调度层指定的目标协议发给上游，结果按入站协议回编。**不做客户端鉴权（凭据转发调度层比对），不管理账号与配置，不承载调度运行态。**
- **阅读方式**：每个端点按「使用场景 → 请求（字段表：类型、必填、约束与允许值、含义）→ 响应（字段表：取值与含义）→ 错误 → 示例」组织。共享结构在 [§3 数据模型](#3-数据模型)定义一次，端点内引用不重复。

三件套中的另外两个服务各有自己的 API 文档：Upstream（配置中心）见其仓库 `docs/api.md`；Relay（调度）的 `/v1` 面仅供本服务调用，不对终端用户开放。

---

## 目录

1. [概述](#1-概述)
2. [通用约定](#2-通用约定)
3. [数据模型](#3-数据模型)
4. [健康检查](#4-健康检查)
5. [公共数据面](#5-公共数据面)
6. [数据面跨端点行为](#6-数据面跨端点行为)
7. [管理面](#7-管理面)
8. [快速上手](#8-快速上手)
9. [修订记录](#9-修订记录)
- [附录 A 环境变量](#附录-a-环境变量)
- [附录 B 请求结局（outcome）语义](#附录-b-请求结局outcome语义)

---

## 1. 概述

### 1.1 三个面的划分

| 面 | 路径前缀 | 鉴权 | 面向 |
|---|---|---|---|
| 公共数据面 | `/v1/*`、`/anthropic/*`、`/openai/*`、`/messages`、`/models` 等（见 [5.1](#51-路径别名与协议识别)） | 无自身鉴权；客户端凭据转发调度层比对 | 终端客户端（SDK、CLI、IDE 插件） |
| 管理面 | `/admin/*` | `Authorization: Bearer <MSA_ADMIN_KEY>` | 运维与前端观测台 |
| 健康探针 | `/health` | 无 | 编排器、反向代理 |

### 1.2 一次请求的生命周期

```
客户端 ──HTTP──▶ 本服务
  1. 认出入站协议（按路径），取客户端凭据与请求 ID
  2. 解码 → IR（协议无关的中间表示）
  3. 向调度层 dispatch：用户模型名 + 入站协议 + 客户端凭据 + 已试目标 → 得到一个目标
  4. IR 按目标协议编码 → 出站 wire body → 叠加目标参数策略（defaults/overrides）
  5. 发上游（一律 SSE 流式），逐帧解码 → 按入站协议回编给客户端
  6. 结束：上报结果给调度层、写流水
```

关键语义（详见 [§6](#6-数据面跨端点行为)）：

- **对上游永远流式**，客户端要非流式时由本服务聚合。
- **首个成功解码的上游帧到达即「已提交」**，此后目标锁定，不再换目标；流式客户端的错误只能流内表达（见 [6.1](#61-已提交边界committed-boundary)）。
- 提交前的失败会**换目标重试**（对客户端透明）；上游 404、`context_exceeded`、参数/凭据类错误不换（见 [6.2](#62-换目标重试与-tried_ids)）。

### 1.3 本文档不覆盖的内容

- 调度算法与候选池：由 Relay 决定，客户端只提供**用户模型名**（user model）。
- 上游账号、模型集合、参数策略的配置：由 Upstream 配置中心管理。

---

## 2. 通用约定

### 2.1 基础地址与部署形态

- 服务默认监听容器内 `:8080`；随附 compose 部署映射为 `127.0.0.1:8082`。
- 本文档示例统一用 `http://127.0.0.1:8082` 作为 `$BASE`。
- 公共数据面**必须只暴露在受信网络或反向代理之后**（它不做自己的客户端鉴权）。

### 2.2 鉴权

| 面 | 头 | 规则 |
|---|---|---|
| 公共数据面 | `x-api-key` 或 `Authorization: Bearer <key>` | 二选一（`x-api-key` 优先）。本服务**不校验**，原样转发调度层比对；比对失败按入站协议的 401 形状回错 |
| 管理面 | `Authorization: Bearer <MSA_ADMIN_KEY>` | 本服务自己的密钥，固定时间比较；缺失或错误一律 401 |

`MSA_ADMIN_KEY` 与调度密钥 `MSA_RELAY_DISPATCH_KEY` **必须不同**，相同则服务拒绝启动：任何能读管理面的人都能冒充本服务发调度请求。

### 2.3 内容类型、编码与请求体上限

- 请求体：`application/json`，UTF-8。
- 非流式响应：`application/json`。
- 流式响应：`text/event-stream`（SSE），并固定带以下响应头：

| 响应头 | 值 | 作用 |
|---|---|---|
| `Content-Type` | `text/event-stream` | SSE |
| `Cache-Control` | `no-cache` | 防止中间层缓存 |
| `Connection` | `keep-alive` | 长连接 |
| `X-Accel-Buffering` | `no` | 防止反向代理缓冲 SSE 导致逐字输出失效 |

- 请求体上限：**64 MiB**（代码常量）。超限按 `invalid_request` 回错。上限存在是因为上下文塞满的请求确实很大，但没有上限意味着一个坏客户端就能把内存吃光。

### 2.4 请求 ID

服务按以下顺序取请求 ID，取到即沿用；都没有则生成 `req_` + 24 位十六进制：

```
X-Request-Id → X-Request-ID → Request-Id → 自动生成
```

同一客户端请求换目标重试时 `request_id` 保持不变，因此它能把一次请求的多次尝试在流水中串起来。该 ID 同时上报调度层，用于跨服务对账。

### 2.5 类型词汇表

字段表中「类型」列的写法约定：

| 写法 | 含义 |
|---|---|
| `string` | JSON 字符串 |
| `int` / `int64` | JSON 整数（int64 表示可能超出 32 位，如 token 总数） |
| `number` | JSON 数字（可含小数） |
| `bool` | JSON 布尔 |
| `object` | JSON 对象，字段见同节子表或所指数据模型 |
| `array of T` | 元素类型为 T 的 JSON 数组 |
| `string[]` | 字符串数组（等价于 `array of string`） |
| `time` | RFC3339 时间字符串，输出为 UTC 纳秒精度（如 `2026-09-16T15:30:05.000990298Z`） |
| `T1 \| T2` | 联合类型：两种形态都接受 |

补充约定：

- **缺省即零值**：JSON 响应中的可省字段（`omitempty`）在零值时不出现——`false`、`0`、`""`、空数组都可能表现为「字段缺失」。消费者应按「缺失 = 零值」处理，不要按「缺失 = 未采集」处理（例外情况在字段表中单独注明，如 `degraded`）。
- **未列出的请求字段被忽略**：请求体解码不启用严格模式，未知字段不报错。
- **时间入参**用 RFC3339（带时区偏移），如 `2026-09-16T00:00:00+08:00`。

### 2.6 数据面错误响应

数据面错误按**客户端入站协议**的原生错误形状渲染，SDK 能直接解析成自己的异常类型：

| 协议 | 错误体形状 |
|---|---|
| Anthropic Messages | `{"type":"error","error":{"type":"<类型>","message":"<文本>"}}` |
| Chat Completions | `{"error":{"message":"<文本>","type":"<类型>","code":"<错误种类>"}}` |
| Responses | `{"error":{"type":"<类型>","code":"<错误种类>","message":"<文本>"}}` |

Chat / Responses 的 `code` 是错误种类字符串（如 `"invalid_request"`），取自下表「种类」列。

**错误种类 → HTTP 状态码**（各协议一致）：

| 种类 | HTTP | 触发条件 |
|---|---|---|
| `invalid_request` | 400 | 请求体解码失败、`model` 缺失、体超限、非法角色等 |
| `context_exceeded` | 400 | 上游报「输入太长」（按消息文本识别，各家园说法不同） |
| `authentication` | 401 | 调度层比对客户端凭据失败 / 上游 401、403 |
| `not_found` | 404 | 上游 404（目标不可用 → `invalid_model` 结局） |
| `rate_limit` | 429 | 上游 429 |
| `timeout` | 504 | 首字超时（`MSA_FIRST_TOKEN_TIMEOUT`，默认 60s）/ 空闲超时（`MSA_IDLE_TIMEOUT`，默认 120s） |
| `upstream` | 500 | 上游 5xx / 流异常结束 / 不可解码的流 |
| `internal` | 500 | 本服务内部错误 |

**错误种类 → 协议错误类型名**：

| 种类 | Anthropic `error.type` | Chat / Responses `error.type` |
|---|---|---|
| `invalid_request`、`context_exceeded` | `invalid_request_error` | `invalid_request_error` |
| `authentication` | `authentication_error` | `authentication_error` |
| `not_found` | `not_found_error` | `not_found_error` |
| `rate_limit` | `rate_limit_error` | `rate_limit_error` |
| `timeout`、`upstream`、`internal` | `api_error` | `server_error` |

> 流式请求在「已提交」之后（HTTP 200 已写出）发生错误时，状态码不可再改，错误改为写成**流内 error 帧**并补协议终止帧，见 [6.1](#61-已提交边界committed-boundary)。

### 2.7 管理面错误响应

管理面所有非 2xx 响应使用统一信封：

```json
{
  "error": {
    "code": "invalid_request",
    "message": "window must be 1h or 24h"
  }
}
```

| code | HTTP | 含义 |
|---|---|---|
| `unauthorized` | 401 | 管理密钥缺失或错误 |
| `invalid_request` | 400 | 参数校验失败（非法 `window` / `state` / `since` / `cursor`） |
| `not_found` | 404 | 请求或上报记录不存在 |
| `unavailable` | 503 | 依赖不可用（PG 挂、outbox 未装配、上游清单拉取失败） |
| `internal_error` | 500 | 未预期错误 |

### 2.8 枚举值索引

全局使用的枚举集中在此，端点字段表中不再重复展开。

**协议名**（`inbound_protocol` / `outbound_protocol` / `protocol`）：

| 值 | 含义 |
|---|---|
| `anthropic` | Anthropic Messages |
| `chat_completions` | OpenAI Chat Completions |
| `responses` | OpenAI Responses |
| `gemini` | Google Gemini（仅出站） |

**请求结局**（`outcome`）：`normal` / `abnormal` / `retrying` / `invalid_model` / `context_exceeded`，语义见[附录 B](#附录-b-请求结局outcome语义)。

**常用限制常量**（默认值，环境变量可调，见[附录 A](#附录-a-环境变量)）：body 上限 64 MiB；最大尝试次数 3；首字超时 60s；空闲超时 120s；outbox 最大尝试 20 次。

---

## 3. 数据模型

以下结构在多处复用，字段定义只在这里给一次。

### 3.1 Health

`GET /health` 与 `GET /admin/health` 的响应体。

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `status` | string | `ok` 全部关键依赖正常；`degraded` PG 或调度层不可达（缓存挂不降级） |
| `database` | string | `ok` / `down` —— PostgreSQL 可用性 |
| `cache` | string | `ok` / `down` / `disabled`。`disabled` 是**部署选择**（未配 Redis），不是故障 |
| `relay` | string | `ok` / `down` —— 调度层可用性（探其健康端点） |
| `outbox_pending` | int | 结果上报队列待发送条数 |
| `outbox_dead` | int | 死信条数（超过重试上限） |

### 3.2 RequestSummary

一条请求流水（`GET /admin/requests` 的元素、`GET /admin/requests/{request_id}` 的响应体）。**绝不含凭据与对话内容。**

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `request_id` | string | 请求 ID（沿用客户端的或自动生成，重试不变） |
| `at` | time | 请求到达时间（UTC） |
| `inbound_protocol` | string | 客户端使用的协议，见 [2.8](#28-枚举值索引) |
| `outbound_protocol` | string | 发给上游用的协议；未发出时为空 |
| `user_model` | string | 客户端请求的模型名 |
| `model_id` | string | 最终选定的上游 model_id；调度层未给目标时为空 |
| `account` | string | 承载该次请求的上游账号名 |
| `outcome` | string | 请求结局，见[附录 B](#附录-b-请求结局outcome语义) |
| `status_code` | int | 回给客户端的 HTTP 状态码 |
| `attempts` | int | 尝试次数（换目标重试累计） |
| `tried_ids` | string[] | 换目标重试中被放弃的上游 model_id 链；失败结局含最终失败的那个，成功结局只含被放弃的 |
| `committed` | bool | 是否越过已提交边界，见 [6.1](#61-已提交边界committed-boundary) |
| `stream` | bool | 客户端是否要 SSE |
| `usage_estimated` | bool | 用量是否为估算（上游没回 usage 时按字符数兜底） |
| `input_tokens` | int64 | 输入 token 数（上游报的或估算的） |
| `output_tokens` | int64 | 输出 token 数 |
| `cache_read_tokens` | int64 | 命中提示缓存的输入 token 数 |
| `latency_ms` | int | 总耗时（毫秒） |
| `first_token_ms` | int | 首个上游帧解码成功的耗时（毫秒）；流式与非流式都记录；未收到任何帧即结束时为 0 |
| `error_code` | string | 错误种类（成功时为空） |
| `error_message` | string | 错误文本（可能含上游原文；成功时为空） |
| `sanitized` | string[] | 对客户端请求所做的畸形修复说明，如补配对的 `tool_result`、合并连续同角色消息；为空或不出现表示请求本身合法。指向客户端 bug |
| `lossy` | string[] | 出站编码因目标协议表达不了而丢弃或改写的字段说明，形如 `dropped top_k (chat_completions cannot express it: ...)`；为空或不出现表示无损转换。指向路由选型 |

#### 3.2.1 `sanitized` 的说明形态

除请求体畸形修复外，工具声明治理也记在这里（与协议无关，任何目标协议都同样拒收）：

| 形态 | 触发条件 |
| --- | --- |
| `dropped a tool declaration with an empty name` | 工具声明的 `name` 为空 |
| `rewrote tool name %q to %q (illegal characters or over 64 bytes)` | 名字含 `[a-zA-Z0-9_.-]` 之外的字符（逐字符替换为 `_`），或超 64 字节（截为前 59 字符 + `_` + 名字 sha256 前 4 位十六进制；纯截断会让长前缀同名的工具撞车） |
| `dropped a duplicate tool declaration named %q, kept the first` | 同名工具重复声明，保留首个 |

改名会同步改写历史消息里对应的 `tool_use`，配对关系不受影响。

#### 3.2.2 `lossy` 的说明形态

请求侧结构调整按目标协议的能力位进行，写进 `lossy` 的形态：

| 字段 | 形态 | 触发条件 |
| --- | --- | --- |
| `tool schema` | `dropped` | schema 含目标方言不接受的关键字（如 gemini 下的 `minLength`、`additionalProperties`），说明里列出被剔除的关键字名 |
| `tool schema` | `dropped` | schema 嵌套超过归一深度上限，更深的子树原样透传 |
| `tool schema` | `rewrote` | schema 不是合法 JSON，整体替换为空对象 schema（工具降级为无参数可调用） |
| `tool schema` | `rewrote` | schema 顶层缺 `type`，补为 `object`（原有 `properties` 保留） |
| `tool_choice` | `dropped` | 请求最终没有任何工具，而目标协议一律拒收此组合 |
| `tool_choice` | `rewrote` | 指名的工具未在本请求声明，降级为 `auto` |
| `system media` | `rewrote` | 目标协议的系统提示只承载单一字符串，附件改写为说明性文本 |
| `<type> blocks in system` | `dropped` | 同上，且该块类型无文本可降级 |
| `thinking` | `dropped` | `max_tokens` 太小，推理预算无法同时满足「不低于协议下限」与「小于 max_tokens」 |
| `temperature/top_p` | `dropped` | 目标协议要求推理开启时不得带采样参数 |
| `stop_sequences` | `dropped` | 条数超过协议上限，截断至上限 |
| `cache_control` | `dropped` | 缓存断点数超过协议上限，丢弃最靠前的若干个（靠后的断点覆盖更长前缀，命中时省得更多） |

只有真的丢掉约束才记进 `lossy`。把同一约束换个写法不记——`type` 大写化、联合类型折叠成 `nullable` 都属于此类。原因是这个字段的用途是指向路由选型：若每个带工具的 gemini 请求都恒定带一条说明，它就再也指不出哪条路由真的削弱了请求。

### 3.3 LiveEntry

实时环元素（`GET /admin/live`）。是 `RequestSummary` 的子集：为速度省略了 `tried_ids`、`error_message`、`usage_estimated`、`cache_read_tokens`、`sanitized`、`lossy`，其余同名字段含义一致：

`request_id`、`at`、`inbound_protocol`、`outbound_protocol`、`user_model`、`model_id`、`account`、`outcome`、`status_code`、`attempts`、`stream`、`latency_ms`、`first_token_ms`、`input_tokens`、`output_tokens`、`error_code`。

要看全量字段与 `tried_ids` 链，走 `GET /admin/requests/{request_id}`。

### 3.4 Stats / StatBucket / StatTotals

`GET /admin/stats` 的响应结构。分钟桶，桶只保留最近 2 小时。

**StatBucket**

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `minute` | time | 该桶对应的分钟（UTC，整分） |
| `total` | int64 | 该分钟请求总数 |
| `outcomes` | object | 结局 → 计数，如 `{"normal":2,"abnormal":1}`；空桶时为 `{}` 或不出现 |
| `input_tokens` | int64 | 该分钟输入 token 合计 |
| `output_tokens` | int64 | 该分钟输出 token 合计 |
| `avg_latency_ms` | int64 | 该分钟平均总耗时（毫秒）；无请求时为 0 |

**StatTotals**（窗口合计，字段同名字段含义一致）

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `total` | int64 | 窗口内请求总数 |
| `outcomes` | object | 结局 → 计数 |
| `input_tokens` / `output_tokens` | int64 | 窗口内 token 合计 |
| `success_rate` | number | `normal` 占比，0–1；总数为零时为 0 |
| `qps` | number | 窗口内平均每秒请求数；总数为零时为 0 |

### 3.5 OutboxEntry

结果上报队列元素（`GET /admin/outbox`）。上报调度层失败的结果先落库重试，客户端不受影响。

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `report_id` | string | 幂等键，形如 `<request_id>:<attempt>`；重放同一份不会重复计数。outbox 重试端点的路径参数 |
| `request_id` | string | 所属请求 ID |
| `model_id` | string | 该次尝试的目标 model_id |
| `outcome` | string | 该次尝试的结局 |
| `attempts` | int | 已重试次数（达到 `MSA_OUTBOX_MAX_ATTEMPTS` 进死信） |
| `next_attempt_at` | time | 下次重试时间（UTC） |
| `last_error` | string | 最近一次投递失败的原因；从未失败时为空 |
| `created_at` | time | 入队时间（UTC） |

### 3.6 ModelInfo / ModelsPage

`GET /admin/models` 的响应结构。

**ModelInfo**

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `name` | string | 用户模型名（客户端填在 `model` 字段里的值） |
| `collection` | string | 所属模型集合名 |
| `policy` | string | 调度策略名；未配置时为空 |
| `protocol` | string | 目标上游协议；未配置时为空 |
| `enabled` | bool | 是否启用（禁用的模型不出现在公共数据面的清单里，见 [5.6](#56-get-models模型清单)） |
| `outbound_ready` | bool | 本服务是否装配了该协议的出站编解码。`false` 意味着调度层可能路由到本服务做不到的目标，那会以 `invalid_model` 收场 |

**ModelsPage**

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `models` | array of ModelInfo | 模型清单 |
| `inbound` | string[] | 本服务装配的入站协议名列表 |
| `outbound` | string[] | 本服务装配的出站协议名列表（用于解释 `outbound_ready`） |
| `cached` | bool | 清单是否来自缓存（刚从调度层取回时为 false 或不出现） |

---

## 4. 健康检查

### 4.1 GET /health

**使用场景**：编排器（compose healthcheck、K8s probe）与反向代理探活；也可作为客户端探测「服务是否可用」的第一个请求。

**请求**：无参数、无鉴权。

**响应**：body 为 [3.1 Health](#31-health)。

| HTTP | 条件 |
|---|---|
| `200` | `status == "ok"`（`cache == "disabled"` 也算 ok） |
| `503` | `status == "degraded"`（PG 或调度层不可达） |

**示例**

```bash
curl -s $BASE/health
```

```json
{
  "status": "ok",
  "database": "ok",
  "cache": "ok",
  "relay": "ok",
  "outbox_pending": 0,
  "outbox_dead": 0
}
```

### 4.2 GET /admin/health

**使用场景**：管理面前端探活。与 `/health` **body 完全相同**，但**永远返回 200**——把降级表达成 HTTP 错误会让前端分不清「服务降级」与「管理面自己不通」，降级信息看 body 的 `status` 与各依赖字段。

**请求**：`Authorization: Bearer $MSA_ADMIN_KEY`。

**响应**：body 为 [3.1 Health](#31-health)；HTTP 恒为 `200`（除 401）。

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 管理密钥缺失或错误 |

---

## 5. 公共数据面

### 5.1 路径别名与协议识别

三条对话端点，每条提供三个等价路径别名（裸路径、带 `/v1`、带厂商前缀）：

| 入站协议 | 方法 | 路径别名 |
|---|---|---|
| Anthropic Messages | POST | `/v1/messages`、`/anthropic/v1/messages`、`/messages` |
| OpenAI Chat Completions | POST | `/v1/chat/completions`、`/openai/v1/chat/completions`、`/chat/completions` |
| OpenAI Responses | POST | `/v1/responses`、`/openai/v1/responses`、`/responses` |
| Count Tokens（Anthropic 形状） | POST | `/v1/messages/count_tokens`、`/anthropic/v1/messages/count_tokens`、`/messages/count_tokens` |
| 模型清单（Anthropic 形状） | GET | `/v1/models`、`/anthropic/v1/models` |
| 模型清单（OpenAI 形状） | GET | `/openai/v1/models`、`/models` |

别名不是冗余：客户端会把 base_url 配成 `host`、`host/v1`、`host/anthropic` 等各种形态，而多数客户端不允许改它拼在后面的固定路径。**协议按路径识别**，与 `Accept` 头无关。

**公共请求头**（三条对话端点与 count_tokens 相同）：

| 头 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `Content-Type` | string | 是 | `application/json` | 请求体格式 |
| `x-api-key` | string | 二选一 | 非空 | 客户端凭据（Anthropic SDK 发这个），转发调度层比对 |
| `Authorization` | string | 二选一 | `Bearer <key>` | 客户端凭据（OpenAI SDK 发这个）。两者同给时 `x-api-key` 优先 |
| `X-Request-Id` / `X-Request-ID` / `Request-Id` | string | 否 | 非空 | 请求 ID，按顺序取第一个非空；都无则自动生成 |
| `anthropic-version` | string | 否 | 任意 | **不校验、不转发**；出站请求由本服务固定带 `2023-06-01` |

> **`model` 字段填用户模型名**（调度层配置的 user model，如 `demo-pool`），不是上游 model_id。映射关系由调度层决定。
> **响应里的 `model` 是上游回传的模型名**（如 `kimi-k2-turbo`），不是请求里的用户模型名——它来自上游响应的首帧，多目标重试后它反映实际承载目标的身份。

### 5.2 POST /v1/messages（Anthropic Messages）

**使用场景**：为 Anthropic 官方 SDK（`anthropic`）、Claude Code 等客户端提供后端——把它们的 `ANTHROPIC_BASE_URL` 指向本服务，请求即可被路由到任意上游（包括非 Anthropic 协议的模型）。

**请求**：`POST /v1/messages`（含全部别名）

请求头见 [5.1](#51-路径别名与协议识别)。

**请求体**

| 字段 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `model` | string | 是 | 非空 | 用户模型名；空串或缺失 → 400 `model is required` |
| `messages` | array of object | 是 | 每项 `role` + `content`，元素结构见下表 | 对话消息 |
| `max_tokens` | int | 是（协议要求） | ≥ 1 | 响应最大输出 token 数。本服务不校验：缺失或 ≤0 时出站编码**自动补 4096**（跨协议来源可能没有此字段）；目标的 `overrides` 可强制改写 |
| `system` | `string \| array of object` | 否 | 字符串，或 `[{"type":"text","text":"..."}]` | 系统提示 |
| `tools` | array of object | 否 | `[{"name","description"?,"input_schema"}]` | 工具定义；`input_schema` 为 JSON Schema |
| `tool_choice` | object | 否 | `{"type":"auto"\|"any"\|"none"\|"tool","name"?}` | 工具选择策略；`type=tool` 时 `name` 必填 |
| `temperature` | number | 否 | 0–1（Anthropic 约定） | 采样温度。**透传不校验**；新模型已废弃此参数，上游可能拒绝 |
| `top_p` | number | 否 | 0–1 | 核采样 |
| `top_k` | int | 否 | ≥ 0 | Top-K 采样 |
| `stop_sequences` | string[] | 否 | 非空字符串 | 自定义停止序列，命中后 `stop_reason=stop_sequence` |
| `stream` | bool | 否 | 默认 `false` | 是否以 SSE 返回 |
| `thinking` | object | 否 | `{"type":"enabled"\|"disabled","budget_tokens":int}` | 扩展思考。`type` 非 `"enabled"` 视为关闭；`budget_tokens` ≤0 且无 effort 档位时，出站按 `max_tokens` 的 50% 折算（不低于 1024、且小于 `max_tokens`，否则上游会拒绝） |
| `metadata` | object | 否 | `{"user_id": string}` | 元数据；**仅 `user_id` 被读取**并转发上游 |
| 其他 | — | — | — | 未列出的字段被忽略 |

**`messages` 元素**

| 字段 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `role` | string | 是 | `user` / `assistant`（协议约定） | 消息角色，按原值进入 IR（不做角色合并/规范化） |
| `content` | `string \| array of object` | 是 | 字符串（等价单个 text 块），或块数组 | 消息内容 |

**content 块类型**

| `type` | 结构 | 含义 |
|---|---|---|
| `text` | `{"type":"text","text":string}` | 文本 |
| `image` | `{"type":"image","source":{...}}`，source 为 `{"type":"base64","media_type":"image/png","data":"<base64>"}` 或 `{"type":"url","url":"https://..."}` | 图片 |
| `tool_use` | `{"type":"tool_use","id":string,"name":string,"input":object}` | 模型发起的工具调用（回传时用）；`input` 非合法 JSON 的残缺值出站补为 `{}` |
| `tool_result` | `{"type":"tool_result","tool_use_id":string,"content":string\|块数组,"is_error"?:bool}` | 工具执行结果 |
| `thinking` | `{"type":"thinking","thinking":string,"signature":string}` | 扩展思考块。多轮工具调用**必须原样回传**（含 signature），否则上游拒绝 |
| `redacted_thinking` | `{"type":"redacted_thinking","data":string}` | 加密思考块，本服务不解析，**丢弃**（不报错） |
| `cache_control` | 任意块上的附加字段 `{"type":"ephemeral"}` | 提示缓存断点标记；仅在出站协议同为 Anthropic 时保留语义 |

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `id` | string | 上游消息 ID（如 `msg_...`） |
| `type` | string | 固定 `"message"` |
| `role` | string | 固定 `"assistant"` |
| `model` | string | **上游回传的模型名**（不是请求里的用户模型名） |
| `content` | array of object | 内容块，见下表 |
| `stop_reason` | string | 停止原因，见下表 |
| `usage` | object | 用量，见下表 |

**content 块**（响应侧只有三类）

| `type` | 结构 | 含义 |
|---|---|---|
| `text` | `{"type":"text","text":string}` | 文本输出 |
| `tool_use` | `{"type":"tool_use","id":string,"name":string,"input":object}` | 工具调用；`input` 为聚合后的完整 JSON |
| `thinking` | `{"type":"thinking","thinking":string,"signature":string}` | 思考内容（仅开启 thinking 且上游返回时出现；signature 仅在 Anthropic 系目标间透传） |

**`stop_reason` 取值**

| 值 | 含义 |
|---|---|
| `end_turn` | 模型自然结束 |
| `max_tokens` | 触及输出上限被截断 |
| `stop_sequence` | 命中 `stop_sequences` |
| `tool_use` | 模型要求调用工具 |
| `refusal` | 安全拒答 |

**`usage` 字段**

| 字段 | 类型 | 含义 |
|---|---|---|
| `input_tokens` | int | 输入 token 数 |
| `output_tokens` | int | 输出 token 数 |
| `cache_read_input_tokens` | int | 命中提示缓存的输入 token 数（0 或缺失 = 未命中） |
| `cache_creation_input_tokens` | int | 写入缓存的输入 token 数 |

**流式响应**（`stream: true`，响应头见 [2.3](#23-内容类型编码与请求体上限)）：

SSE 帧序列，`event:` 为事件名，`data:` 为 JSON：

| 事件 | 含义 |
|---|---|
| `message_start` | 流开始，`message` 里带初始 `usage` 与模型名 |
| `content_block_start` | 一个内容块开始（`index` + 块骨架；`tool_use` 的 `input` 是占位，实参在增量里） |
| `content_block_delta` | 块内增量，`delta.type` ∈ `text_delta` / `input_json_delta`（`partial_json` 片段，需拼接）/ `thinking_delta` / `signature_delta` |
| `content_block_stop` | 一个内容块结束 |
| `message_delta` | 消息级增量：`stop_reason` 与最终 `usage` |
| `message_stop` | 流正常结束（本服务保证发出；异常路径见 [6.1](#61-已提交边界committed-boundary)） |
| `ping` | 上游保活帧，原样透传 |
| `error` | 流内错误（异常路径），形状同 [2.6](#26-数据面错误响应) |

**错误**：见 [2.6](#26-数据面错误响应)（状态码与类型名两张表）。

**示例**

```bash
curl -s $BASE/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: sk-client-demo' \
  -d '{
    "model": "demo-pool",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "用一个词回答：你好"}]
  }'
```

```json
{
  "id": "msg_01XyZ...",
  "type": "message",
  "role": "assistant",
  "model": "kimi-k2-turbo",
  "content": [{"type": "text", "text": "你好"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 12, "output_tokens": 3}
}
```

### 5.3 POST /v1/chat/completions（OpenAI Chat Completions）

**使用场景**：为 OpenAI 官方 SDK 与各类 IDE 插件提供后端——它们只认 Chat Completions 形状，但希望底下的模型是 Anthropic / Gemini 等非 OpenAI 上游。

**请求**：`POST /v1/chat/completions`（含全部别名）

请求头见 [5.1](#51-路径别名与协议识别)。

**请求体**

| 字段 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `model` | string | 是 | 非空 | 用户模型名；空串或缺失 → 400 `model is required` |
| `messages` | array of object | 是 | 元素结构见下表 | 对话消息 |
| `max_tokens` | int | 否 | ≥ 1 | 输出上限。与 `max_completion_tokens` **同给时后者生效** |
| `max_completion_tokens` | int | 否 | ≥ 1 | 输出上限（新写法，含推理 token） |
| `temperature` | number | 否 | 0–2（OpenAI 约定） | 采样温度，透传不校验 |
| `top_p` | number | 否 | 0–1 | 核采样 |
| `stop` | `string \| string[]` | 否 | 至多 4 条（OpenAI 约定），透传不校验 | 停止序列 |
| `stream` | bool | 否 | 默认 `false` | 是否以 SSE 返回 |
| `stream_options` | object | 否 | `{"include_usage":bool}` | 客户端语义照常；**对上游本服务恒置 `include_usage:true`**（用量须上报调度层） |
| `tools` | array of object | 否 | `[{"type":"function","function":{"name","description"?,"parameters"?}}]` | 工具定义 |
| `tool_choice` | `string \| object` | 否 | `"none"` / `"auto"` / `"required"` / `{"type":"function","function":{"name":string}}` | 工具选择策略 |
| `reasoning_effort` | string | 否 | `none` / `minimal` / `low` / `medium` / `high` / `xhigh` / `max`（各上游支持范围不同） | 推理强度档位，透传 |
| `user` | string | 否 | 非空 | 终端用户标识，透传上游 |
| 其他 | — | — | — | 未列出的字段被忽略 |

**`messages` 元素**

| 字段 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `role` | string | 是 | `system` / `developer` / `user` / `assistant` / `tool`；空串等同 `user` | 角色。`developer` 等同 `system`；**其他值 → 400** |
| `content` | `string \| array \| null` | 否 | 字符串，或 `[{"type":"text","text"}\|{"type":"image_url","image_url":{"url"}}]`，或 null | 消息内容 |
| `reasoning_content` | string | 否 | — | 思维链回传（assistant 消息）。**多轮工具调用必须回传**，否则部分上游丢失推理上下文 |
| `tool_calls` | array of object | 否 | `[{"index"?,"id","type":"function","function":{"name","arguments"}}]`；`arguments` 是 JSON **字符串** | assistant 消息里的工具调用 |
| `tool_call_id` | string | 否 | — | `role=tool` 消息关联的调用 ID |
| `name` | string | 否 | — | 参与者名 |

> `role=tool` 的消息在 IR 中表示为工具结果块，出站时归位到目标协议的工具结果位置（如 Anthropic 的 `tool_result`、Responses 的 `function_call_output`）。

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `id` | string | 上游响应 ID；上游没给时为 `chatcmpl-unknown` |
| `object` | string | 固定 `"chat.completion"` |
| `created` | int | Unix 秒（本服务生成） |
| `model` | string | **上游回传的模型名** |
| `choices` | array | 恒一个元素（`index: 0`） |
| `choices[].message` | object | 见下表 |
| `choices[].finish_reason` | string | 停止原因，见下表 |
| `usage` | object | 用量，见下表 |

**`choices[].message`**

| 字段 | 类型 | 含义 |
|---|---|---|
| `role` | string | 固定 `"assistant"` |
| `content` | `string \| null` | 文本输出 |
| `reasoning_content` | string | 思维链（上游返回时有） |
| `tool_calls` | array of object | 工具调用：`[{"id","type":"function","function":{"name","arguments"}}]`，`arguments` 为 JSON 字符串 |

**`finish_reason` 取值**

| 值 | 含义 |
|---|---|
| `stop` | 自然结束（含命中停止序列） |
| `length` | 触及输出上限被截断 |
| `tool_calls` | 模型要求调用工具 |
| `content_filter` | 被内容过滤中断 |

**`usage` 字段**

| 字段 | 类型 | 含义 |
|---|---|---|
| `prompt_tokens` | int | 输入 token 数 |
| `completion_tokens` | int | 输出 token 数 |
| `total_tokens` | int | 合计 |
| `prompt_tokens_details.cached_tokens` | int | 命中提示缓存的输入 token 数 |
| `prompt_cache_hit_tokens` | int | 同上（DeepSeek 等家的写法，与上一行同义） |

**流式响应**（`stream: true`）：

`data:` 行承载 `chat.completion.chunk` JSON，`choices[0].delta` 逐帧累积（文本在 `content`、思维链在 `reasoning_content`、工具调用在 `tool_calls`，其中 `tool_calls[].index` 标识分片归属）。流尾恒有 **3 帧**：

1. `delta` 为空、带 `finish_reason` 的 chunk；
2. `usage` 帧（`choices: []`，`usage` 完整）；
3. 字面量 `data: [DONE]`。

异常路径在流内发一帧 `data:` 错误 JSON（形状同 [2.6](#26-数据面错误响应)）后补 `[DONE]`。

**错误**：见 [2.6](#26-数据面错误响应)。

**示例**

```bash
curl -s $BASE/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-client-demo' \
  -d '{
    "model": "demo-pool",
    "messages": [{"role": "user", "content": "用一个词回答：你好"}]
  }'
```

```json
{
  "id": "chatcmpl-7f3a...",
  "object": "chat.completion",
  "created": 1786721405,
  "model": "kimi-k2-turbo",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "你好"},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
}
```

### 5.4 POST /v1/responses（OpenAI Responses）

**使用场景**：为使用 Responses API 的客户端（如 Codex 系）提供后端。该协议把消息与工具调用统一成 `input` 条目数组，比 Chat Completions 更细。

**请求**：`POST /v1/responses`（含全部别名）

请求头见 [5.1](#51-路径别名与协议识别)。

**请求体**

| 字段 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `model` | string | 是 | 非空 | 用户模型名；空串或缺失 → 400 `model is required` |
| `input` | `string \| array of object` | 是 | 字符串（等价单条 user 消息），或条目数组，元素结构见下表 | 输入 |
| `instructions` | string | 否 | — | 系统/开发者指令 |
| `max_output_tokens` | int | 否 | ≥ 1 | 输出上限（含推理 token） |
| `temperature` | number | 否 | 0–2（OpenAI 约定） | 采样温度，透传不校验 |
| `top_p` | number | 否 | 0–1 | 核采样 |
| `stream` | bool | 否 | 默认 `false` | 是否以 SSE 返回 |
| `tools` | array of object | 否 | `[{"type":"function","name","description"?,"parameters"?}]`（**平铺，不嵌 `function`**） | 工具定义 |
| `tool_choice` | `string \| object` | 否 | `"none"` / `"auto"` / `"required"` / `{"type":"function","name":string}` | 工具选择策略 |
| `reasoning` | object | 否 | `{"effort": "low"\|"medium"\|"high"\|..., "summary":"auto"\|"concise"\|"detailed"}` | 推理配置。开启思考时本服务出站恒带 `summary:"auto"`（否则推理内容对客户端不可见） |
| `store` | bool | 否 | 默认 `false` | **出站恒为 `false`**：本服务不让上游留存会话（多目标重试时各上游留存状态互不可见，会分叉） |
| `user` | string | 否 | 非空 | 终端用户标识，透传上游 |
| 其他 | — | — | — | 未列出的字段被忽略 |

**`input` 条目类型**（`type` 字段区分）

| `type` | 结构 | 含义 |
|---|---|---|
| `message` | `{"type":"message","role":"user"\|"assistant"\|"system"\|"developer","content":string\|part[]}` | 消息；**未识别的 role 一律按 `user` 处理** |
| `function_call` | `{"type":"function_call","call_id":string,"name":string,"arguments":string}` | 模型发起的工具调用（`arguments` 为 JSON 字符串） |
| `function_call_output` | `{"type":"function_call_output","call_id":string,"output":string}` | 工具执行结果 |
| `reasoning` | `{"type":"reasoning","summary":[{"type":"summary_text","text":string}],"encrypted_content":string}` | 推理条目；`encrypted_content` 只在 Responses 系目标间原样透传 |

**message 的 content part 类型**

| `type` | 结构 | 含义 |
|---|---|---|
| `input_text` | `{"type":"input_text","text":string}` | 输入文本 |
| `input_image` | `{"type":"input_image","image_url":string}` | 输入图片，`image_url` 可为 data URI 或远程链接 |
| `output_text` | `{"type":"output_text","text":string}` | 助手历史输出文本 |
| `refusal` | `{"type":"refusal","refusal":string}` | 安全拒答文本，按普通文本处理 |

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `id` | string | 上游响应 ID |
| `object` | string | 固定 `"response"` |
| `model` | string | **上游回传的模型名** |
| `status` | string | `completed`（正常完成）/ `incomplete`（未完成，见 `incomplete_details`） |
| `output` | array of object | 输出条目：`message`（含 `output_text` / `refusal` part）、`function_call`、`reasoning` |
| `usage` | object | 见下表 |
| `incomplete_details` | object | `{"reason": string}`，如 `max_output_tokens`（因长度截断）；未截断时不出现 |
| `error` | object | 失败时的错误对象；成功时为 null/不出现 |

**`usage` 字段**

| 字段 | 类型 | 含义 |
|---|---|---|
| `input_tokens` | int | 输入 token 数 |
| `output_tokens` | int | 输出 token 数 |
| `total_tokens` | int | 合计 |
| `input_tokens_details.cached_tokens` | int | 命中提示缓存的输入 token 数 |

**流式响应**（`stream: true`）：

带 `type` 字段的 SSE 事件流，客户端可观察到的事件：

| 事件 | 含义 |
|---|---|
| `response.created` | 流开始，带 response 骨架 |
| `response.output_item.added` | 一个新条目开始（`output_index` 为条目序号） |
| `response.content_part.added` | 条目内一个 part 开始（`content_index` 为 part 序号） |
| `response.output_text.delta` | 文本增量（`delta` 字段） |
| `response.reasoning_summary_text.delta` | 推理摘要增量（开启思考时） |
| `response.function_call_arguments.delta` | 工具调用实参增量（`delta` 为 JSON 片段，需拼接） |
| `response.content_part.done` | part 结束 |
| `response.output_item.done` | 条目结束 |
| `response.completed` / `response.incomplete` | 流正常结束（截断时为 `incomplete`），带最终 response 与 usage |
| `error` | 流内错误（异常路径，形状同 [2.6](#26-数据面错误响应)） |

**错误**：见 [2.6](#26-数据面错误响应)。

**示例**

```bash
curl -s $BASE/v1/responses \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-client-demo' \
  -d '{
    "model": "demo-pool",
    "input": "用一个词回答：你好"
  }'
```

```json
{
  "id": "resp_68c1...",
  "object": "response",
  "model": "kimi-k2-turbo",
  "status": "completed",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [{"type": "output_text", "text": "你好"}]
    }
  ],
  "usage": {"input_tokens": 12, "output_tokens": 3, "total_tokens": 15}
}
```

### 5.5 POST /v1/messages/count_tokens（本地估算）

**使用场景**：客户端在发送前估算提示长度，判断是否超出上下文窗口或估算成本。**本地估算，不打上游、不消耗配额**——打上游要先选目标并消耗一次配额，把几百毫秒的延迟压在一次纯计数上不值得。

**请求**：`POST /v1/messages/count_tokens`（含全部别名）

请求头见 [5.1](#51-路径别名与协议识别)。请求体与 [5.2](#52-post-v1messagesanthropic-messages) 相同（Anthropic Messages 形状；`messages`、`system`、`tools` 等参与估算），其中 `model` 仍必填（同一套解码器）。

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `input_tokens` | int64 | 估算的输入 token 数。**偏保守（宁多勿少）**：据此裁剪上下文不会踩到真实上限 |

**错误**：按 Anthropic 形状，见 [2.6](#26-数据面错误响应)。

**示例**

```bash
curl -s $BASE/v1/messages/count_tokens \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: sk-client-demo' \
  -d '{"model":"demo-pool","messages":[{"role":"user","content":"你好"}]}'
```

```json
{"input_tokens": 2}
```

### 5.6 GET /models（模型清单）

**使用场景**：客户端启动或刷新时拉取可选模型清单（如 Claude Code 的 `/model` 列表、OpenAI SDK 的 `models.list()`）。**禁用（`enabled=false`）的模型不列出**——客户端会把清单当可选项展示，列出必然失败的项目只会误导用户。

**请求**：`GET /v1/models`（Anthropic 形状）、`GET /openai/v1/models` 或 `GET /models`（OpenAI 形状）

**形状按请求路径决定**，与 `Accept` 头无关：各家 SDK 解析不出自己认识的形状会直接报错。清单代理自调度层（短缓存）。

**Anthropic 形状响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `data` | array of object | 元素：`{"id":string,"type":"model","display_name":string,"created_at":time}`；`created_at` 为固定值 `2024-01-01T00:00:00Z`（用户模型是配置项，无真实创建时间；固定值避免客户端误判清单变化） |
| `has_more` | bool | 恒 `false`（清单一次给全） |
| `first_id` / `last_id` | string | 首/末元素 id；清单为空时不出现 |

**OpenAI 形状响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `object` | string | 固定 `"list"` |
| `data` | array of object | 元素：`{"id":string,"object":"model","owned_by":string,"created":int}`；`owned_by` 填所属 collection；`created` 为固定 Unix 秒 `1704067200` |

**错误**

| HTTP | 触发条件 |
|---|---|
| `500` `upstream` | 从调度层拉取清单失败 |

**示例**

```bash
curl -s $BASE/openai/v1/models
```

```json
{
  "object": "list",
  "data": [
    {"id": "demo-pool", "object": "model", "owned_by": "demo", "created": 1704067200}
  ]
}
```

---

## 6. 数据面跨端点行为

以下行为对三条对话端点一致，客户端实现重试/流式处理时需要理解。

### 6.1 已提交边界（committed boundary）

**第一个成功解码的上游帧到达后，本次尝试即视为「已提交」**：目标锁定，不再换目标重试。此后发生的一切错误（上游中途断流、读错误、超时）按客户端是否流式区分：

| 客户端 | 已提交后出错的表现 |
|---|---|
| 流式（`stream: true`） | HTTP 200 已在首帧锁定，无法改状态码。错误以按入站协议形状的流内 error 帧表达，随后补协议终止帧保证流正常闭合（Anthropic `message_stop`、Chat `[DONE]`、Responses `response.completed`） |
| 非流式 | 响应头尚未写出，仍能给出正确的 HTTP 错误状态码（聚合到一半的内容丢弃） |

客户端因此总能拿到一个完整闭合的流，不会吊死等待。

### 6.2 换目标重试与 tried_ids

提交之前的失败会触发换目标重试：把已失败的 `model_id` 追加进 `tried_ids` 重新向调度层要目标，最多 `MSA_MAX_ATTEMPTS` 次（默认 3）。重试对客户端完全透明，在流水中体现为 `attempts > 1` 与 `tried_ids` 链。

**触发换目标 / 不换目标**的分类：

| 失败情形 | 是否换目标 | 原因 |
|---|---|---|
| `rate_limit`（429）、`upstream`（5xx / 流异常）、`timeout`（首字/空闲超时） | 换 | 目标暂时不好，换一个可能行 |
| 上游一帧未出即结束 | 换（`retrying`） | 目标没真正响应 |
| 出站协议未装配 / 出站编码失败 / 参数策略叠加失败 | 换（`invalid_model`） | 这个目标做不到，别的目标可能可以 |
| `not_found`（上游 404） | **不换** | 目标本身不可用，记为 `invalid_model` 直接回错 |
| `context_exceeded` | **不换** | 输入太长是客户端的问题，换目标没有意义 |
| `invalid_request` / `authentication` | **不换** | 参数与凭据问题与目标无关 |
| 已提交（响应已开始写出） | **不换** | 换目标会让客户端看到两段拼接的回答 |

每次尝试一条上报（`report_id = request_id:attempt`），上报失败进 outbox 重试，见 [7.6](#76-get-adminoutbox上报队列)。

### 6.3 参数后处理：defaults 与 overrides

请求参数**不透传**：IR 按目标协议编码成出站 wire body 后，按上游配置中心定义的参数策略处理，然后才发上游。

| 层 | 语义 | 作用对象 |
|---|---|---|
| `defaults` | **只填缺失**：键不存在时才写入；已存在（含客户端显式传值）则保留 | 出站协议的原生字段名 |
| `overrides` | **无条件压盖**：直接覆写同名键 | 同上 |

规则细节：

- 两层都发生在**出站协议的语境内**，不做跨协议字段翻译——anthropic 入站转 gemini 出站时，override 写的是 gemini 的字段名（如 `{"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}`）。
- 对象对对象时递归下钻（不会整块替换），数组与标量整体替换。
- **`overrides` 会覆盖客户端的显式值**：客户端传 `temperature: 0.7` 而目标配了 `{"temperature": 0.2}`，上游收到 0.2。这是设计行为（强制锁定关键参数），不是 bug。

### 6.4 跨协议能力差异

入站协议与目标协议不同时，个别字段无法无损表达，按以下方式处理：

| 字段 | 行为 |
|---|---|
| `stop_sequences` | 目标为 Responses 时丢弃（该协议无此字段）；确有需要可用 `overrides` 注入目标兼容字段（是否生效取决于上游） |
| `top_k` | 目标为 Chat Completions / Responses 时丢弃 |
| `cache_control` | 仅 Anthropic 目标会写回该标记；其他出站协议不表达它（缓存行为由各家自动机制决定） |
| thinking 签名 | 只在同族协议间透传：Anthropic `signature` 与 Responses `encrypted_content` 互不翻译，跨族时降级为纯文本推理（丢掉签名后仍可被接受） |
| thinking 预算 ↔ 档位 | Anthropic 的 `budget_tokens` 转其他协议时折成 effort 档位（<4096→`low`，<16384→`medium`，否则 `high`）；反向转换按 `max_tokens` 比例折算并保证 1024 ≤ budget < max_tokens |

### 6.5 用量估算兜底

上游没回 usage 时（少数上游流末不发 usage），本服务按响应字符数估算输出 token 数，此时流水里 `usage_estimated: true`。估算仅用于调度层用量统计；`/admin/requests` 的 `input_tokens` / `output_tokens` 会标注其可信度。

---

## 7. 管理面

全部挂在 `/admin` 下。除 outbox 重试外全部**只读**——管理面能改的越少，误操作的后果就越小。

### 7.1 管理面鉴权

**请求头**

| 头 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `Authorization` | string | 是 | `Bearer <MSA_ADMIN_KEY>` | 管理密钥。固定时间比较（防按前缀试密钥） |

缺失或错误 → `401` `{"error":{"code":"unauthorized","message":"invalid admin key"}}`（信封见 [2.7](#27-管理面错误响应)）。未配置管理面的部署（`Admin == nil`）下 `/admin/*` 一律 404。

### 7.2 GET /admin/requests（流水查询）

**使用场景**：排查「某次请求为什么失败」「某个用户模型或上游目标最近的请求」，按时间倒序回看历史流量。数据来自 PostgreSQL 的请求流水表。

**请求**：`GET /admin/requests`

**查询参数**

| 参数 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `outcome` | string | 否 | `normal` / `abnormal` / `retrying` / `invalid_model` / `context_exceeded` | 只返回该结局的流水。按值精确过滤、不校验枚举：未知值不报错，自然返回空页 |
| `model_id` | string | 否 | 精确匹配 | 按上游 model_id 过滤 |
| `user_model` | string | 否 | 精确匹配 | 按用户模型名过滤 |
| `since` | string | 否 | RFC3339 时间 | 只返回此时间之后的记录；格式非法 → 400 |
| `limit` | int | 否 | ≥ 1，默认 50 | 页大小。**非法值或 ≤0 回退为默认值、不报错** |
| `cursor` | string | 否 | 上一页返回的 `next_cursor` 原样回传 | 翻页游标；解码失败 → 400 `cursor is not a valid token` |

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `requests` | array of [RequestSummary](#32-requestsummary) | 当前页流水，时间倒序 |
| `next_cursor` | string | 下一页游标；**缺失或空串 = 没有下一页**。游标只编码翻页位置、不含过滤条件——改变过滤条件后应从首页重新开始 |

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |
| `400` | `invalid_request` | `since` 非 RFC3339 / `cursor` 解码失败 |
| `503` | `unavailable` | 流水存储不可用（PG 挂） |

**示例**

```bash
curl -s "$BASE/admin/requests?user_model=demo-pool&limit=1" \
  -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{
  "requests": [
    {
      "request_id": "req_fcb7839ac6c6cb8810820d35",
      "at": "2026-09-16T15:30:05.000990298Z",
      "inbound_protocol": "anthropic",
      "outbound_protocol": "anthropic",
      "user_model": "demo-pool",
      "model_id": "kimi-k2-turbo",
      "account": "kimi-1",
      "outcome": "abnormal",
      "status_code": 404,
      "attempts": 1,
      "tried_ids": ["kimi-k2-turbo"],
      "committed": false,
      "stream": true,
      "input_tokens": 0,
      "output_tokens": 0,
      "latency_ms": 812,
      "first_token_ms": 0,
      "error_code": "not_found",
      "error_message": "upstream returned 404"
    }
  ],
  "next_cursor": "WyIyMDI2LTA5LTE2VDE1OjMwOjA1WiIsInJlcV9mY2I3ODM5YWNjNmNjYjg4MTA4MjBkMzUiXQ"
}
```

### 7.3 GET /admin/requests/{request_id}（单条详情）

**使用场景**：从流水列表点进单条，查看完整的换目标链路（`tried_ids`）与最终错误。

**请求**：`GET /admin/requests/{request_id}`

**路径参数**

| 参数 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `request_id` | string | 是 | 非空 | 请求 ID（流水中 `request_id` 字段），需 URL 编码（如 `/` 编成 `%2F`） |

**响应** `200`：[RequestSummary](#32-requestsummary)（单对象，不是数组）。

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |
| `404` | `not_found` | 该 `request_id` 不存在 |
| `503` | `unavailable` | 流水存储不可用 |

**示例**

```bash
curl -s "$BASE/admin/requests/req_fcb7839ac6c6cb8810820d35" \
  -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{
  "request_id": "req_fcb7839ac6c6cb8810820d35",
  "at": "2026-09-16T15:30:05.000990298Z",
  "inbound_protocol": "anthropic",
  "outbound_protocol": "anthropic",
  "user_model": "demo-pool",
  "model_id": "kimi-k2-turbo",
  "outcome": "normal",
  "status_code": 200,
  "attempts": 1,
  "stream": true,
  "input_tokens": 120,
  "output_tokens": 340,
  "latency_ms": 1520,
  "first_token_ms": 230
}
```

### 7.4 GET /admin/live（实时环）

**使用场景**：实时盯屏——新请求逐条冒出来，用于观察发布/切流是否正常。数据来自 Redis 的定长环（最新在前）。

**请求**：`GET /admin/live`

**查询参数**

| 参数 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `limit` | int | 否 | ≥ 1，默认 100 | 最多返回条数。非法值或 ≤0 回退默认值、不报错 |

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `entries` | array of [LiveEntry](#33-liveentry) | 最新在前 |
| `degraded` | bool | `true` = 缓存不可用，`entries` 是「看不到」而非「没有流量」。前端必须据此区分两种空 |

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |

> 缓存不可用**不报错**（回空列表 + `degraded: true`）：管理面是用来观察状态的，观测渠道自己报错会把「没流量」和「看不到」搅在一起。

**示例**

```bash
curl -s "$BASE/admin/live?limit=2" -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{
  "entries": [
    {
      "request_id": "req_efc777bf2ee1eeb429a028de",
      "at": "2026-09-16T16:03:05.000990298Z",
      "inbound_protocol": "anthropic",
      "outbound_protocol": "anthropic",
      "user_model": "demo-pool",
      "model_id": "kimi-k2-turbo",
      "account": "kimi-1",
      "outcome": "abnormal",
      "status_code": 401,
      "attempts": 1,
      "stream": true,
      "latency_ms": 8,
      "first_token_ms": 0,
      "error_code": "authentication"
    }
  ],
  "degraded": false
}
```

### 7.5 GET /admin/stats（分钟桶统计）

**使用场景**：看吞吐与成功率的趋势图（前端总览页的三个 Spark 卡片）。数据来自 Redis 分钟桶，**桶只保留最近 2 小时**。

**请求**：`GET /admin/stats`

**查询参数**

| 参数 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `window` | string | 否 | `1h`（默认）/ `24h` | 统计窗口。**`24h` 实际返回最近 2 小时**（桶只存 2 小时），保留该档位只为前端档位切换，不做额外承诺；其他值 → 400 |

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `window` | string | 回显生效档位（`1h` / `24h`） |
| `buckets` | array of [StatBucket](#34-stats--statbucket--stattotals) | 分钟桶，时间升序 |
| `totals` | [StatTotals](#34-stats--statbucket--stattotals) | 窗口合计 |
| `degraded` | bool | `true` = 缓存不可用，数字是「看不到」而非真的为零 |

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |
| `400` | `invalid_request` | `window` 不是 `1h` / `24h` |

**示例**

```bash
curl -s "$BASE/admin/stats?window=1h" -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{
  "window": "1h",
  "buckets": [
    {
      "minute": "2026-09-16T16:03:00Z",
      "total": 3,
      "outcomes": {"normal": 2, "abnormal": 1},
      "input_tokens": 120,
      "output_tokens": 340,
      "avg_latency_ms": 820
    }
  ],
  "totals": {
    "total": 3,
    "outcomes": {"normal": 2, "abnormal": 1},
    "input_tokens": 120,
    "output_tokens": 340,
    "success_rate": 0.6667,
    "qps": 0.000833
  },
  "degraded": false
}
```

### 7.6 GET /admin/outbox（上报队列）

**使用场景**：结果上报调度层失败时先落库重试（客户端不受影响）；此端点查看积压与死信。只有埋在这里的上报才看得到——直报成功的不入队。

**请求**：`GET /admin/outbox`

**查询参数**

| 参数 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `state` | string | 否 | `pending`（默认）/ `dead` | 队列状态。`pending` = 待重试；`dead` = 超过重试上限的死信（不删除，可复活）；其他值 → 400 |
| `limit` | int | 否 | ≥ 1，默认 100 | 最多返回条数。非法值或 ≤0 回退默认值、不报错 |

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `entries` | array of [OutboxEntry](#35-outboxentry) | 队列元素 |

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |
| `400` | `invalid_request` | `state` 不是 `pending` / `dead` |
| `503` | `unavailable` | outbox 存储不可用 |

**示例**

```bash
curl -s "$BASE/admin/outbox?state=dead" -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{
  "entries": [
    {
      "report_id": "req_fcb7839ac6c6cb8810820d35:0",
      "request_id": "req_fcb7839ac6c6cb8810820d35",
      "model_id": "kimi-k2-turbo",
      "outcome": "abnormal",
      "attempts": 20,
      "next_attempt_at": "2026-09-16T16:05:00Z",
      "last_error": "relay unreachable: connection refused",
      "created_at": "2026-09-16T16:03:05Z"
    }
  ]
}
```

### 7.7 POST /admin/outbox/{report_id}/retry（复活死信）

**使用场景**：死信通常是调度层短暂不可用造成的；调度层恢复后手动复活，避免人工补报。**这是管理面唯一的写操作。**

**请求**：`POST /admin/outbox/{report_id}/retry`

**路径参数**

| 参数 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `report_id` | string | 是 | 非空，来自 [7.6](#76-get-adminoutbox上报队列) 的 `report_id` | 上报幂等键（`<request_id>:<attempt>`），需 URL 编码（`:` 编成 `%3A`） |

**行为**：只重置 `next_attempt_at` 与 `attempts` 使其重新入队；**上报内容本身不可编辑**。

**响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `revived` | bool | 固定 `true`（成功复活） |

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |
| `404` | `not_found` | `report_id` 不存在 |
| `503` | `unavailable` | outbox 存储不可用 |

**示例**

```bash
curl -s -X POST "$BASE/admin/outbox/req_fcb7839ac6c6cb8810820d35%3A0/retry" \
  -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{"revived": true}
```

### 7.8 GET /admin/models（模型与出站就绪度）

**使用场景**：核对用户模型清单，并**提前发现**「调度层可能把请求路由到本服务没实现出站编解码的协议」的目标（`outbound_ready: false`）——否则要等线上报 `invalid_model` 才知道。

**请求**：`GET /admin/models`

**响应** `200`：[ModelsPage](#36-modelinfo--modelspage)。

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |
| `503` | `unavailable` | 从调度层拉取清单失败 |

**示例**

```bash
curl -s "$BASE/admin/models" -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{
  "models": [
    {
      "name": "demo-pool",
      "collection": "demo",
      "policy": "round-robin",
      "protocol": "anthropic",
      "enabled": true,
      "outbound_ready": true
    }
  ],
  "inbound": ["anthropic", "chat_completions", "responses"],
  "outbound": ["anthropic", "chat_completions", "gemini", "responses"],
  "cached": true
}
```

---

## 8. 快速上手

### 8.1 启动

```bash
# 起 agent + PostgreSQL + Redis；调度层地址与密钥由环境变量传入
export MSA_RELAY_BASE_URL=http://host.docker.internal:8081
export MSA_RELAY_DISPATCH_KEY=msr-dispatch-xxx
export MSA_ADMIN_KEY=msa-admin-xxx
docker compose up -d
curl -s http://127.0.0.1:8082/health
```

调度层（Relay）与配置中心（Upstream）不在本 compose 内，需另行部署或指向已有实例。环境变量与默认值见[附录 A](#附录-a-环境变量)。

### 8.2 非流式对话（Anthropic 协议）

```bash
curl -s http://127.0.0.1:8082/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: sk-client-demo' \
  -d '{"model":"demo-pool","max_tokens":256,"messages":[{"role":"user","content":"你好"}]}'
```

### 8.3 流式对话（Chat Completions 协议）

```bash
curl -N -s http://127.0.0.1:8082/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-client-demo' \
  -d '{"model":"demo-pool","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

### 8.4 查流水与统计

```bash
curl -s "http://127.0.0.1:8082/admin/requests?limit=5" -H "Authorization: Bearer $MSA_ADMIN_KEY"
curl -s "http://127.0.0.1:8082/admin/stats?window=1h"   -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

---

## 9. 修订记录

| 版本 | 日期 | 变更 |
|---|---|---|
| 2.0 | 2026-09-17 | 每个端点补齐「使用场景 / 请求字段表（类型·必填·约束与允许值·含义）/ 响应字段表（取值与含义）/ 错误表 / 示例」；新增类型词汇表、枚举值索引；修正响应 `model` 字段含义（上游回传模型名，非用户模型名）；修正已提交边界描述（非流式客户端仍能拿到真实 HTTP 错误码）；补充跨协议能力差异表 |
| 1.0 | 2026-09-17 | 初稿 |

---

## 附录 A 环境变量

| 变量 | 默认 | 含义 |
|---|---|---|
| `MSA_LISTEN` | `:8080` | 监听地址（容器内；compose 映射 `127.0.0.1:8082`） |
| `MSA_PG_DSN` | 必填 | PostgreSQL 连接串（请求流水） |
| `MSA_REDIS_ADDR` | 可选 | Redis 地址（实时环与统计）；未配置时实时/统计降级为 `disabled` |
| `MSA_REDIS_PASSWORD` | 空 | Redis 密码 |
| `MSA_REDIS_DB` | `0` | Redis 库号 |
| `MSA_CACHE_TTL` | `1m` | 清单等缓存 TTL |
| `MSA_RELAY_BASE_URL` | 必填 | 调度层地址 |
| `MSA_RELAY_DISPATCH_KEY` | 必填 | 本服务向调度层发请求的密钥 |
| `MSA_ADMIN_KEY` | 必填 | 管理面密钥；**必须不同于 `MSA_RELAY_DISPATCH_KEY`**，相同则拒绝启动 |
| `MSA_MAX_ATTEMPTS` | `3` | 单次客户端请求的最大尝试次数（换目标）；< 1 拒绝启动 |
| `MSA_FIRST_TOKEN_TIMEOUT` | `60s` | 首帧超时（提交前） |
| `MSA_IDLE_TIMEOUT` | `120s` | 空闲超时（提交后帧间隔） |
| `MSA_OUTBOX_INTERVAL` | `1s` | outbox 重试间隔 |
| `MSA_OUTBOX_MAX_ATTEMPTS` | `20` | outbox 最大重试次数，超过进死信 |
| `MSA_ESTIMATE_USAGE` | `true` | 上游没回 usage 时按字符数估算兜底（见 [6.5](#65-用量估算兜底)） |
| `MSA_ACCESS_LOG` | `true` | 是否打访问日志行（关掉仍记请求流水） |
| `MSA_LOG_RETENTION` | `336h`（14 天） | 请求流水保留期，过期清理 |

---

## 附录 B 请求结局（outcome）语义

结局决定**调度层如何更新目标运行态**，语义由 relay 定义：

| outcome | 调度层动作 | 产生条件 |
|---|---|---|
| `normal` | 清零失败计数，累计用量 | 成功完成 |
| `abnormal` | 累计失败，可能触发冷却 | 最终仍失败；committed 后失败；不再重试时 `retrying` 降级而来 |
| `retrying` | 同 abnormal，但本次会继续换目标 | 提交前失败且可重试（`rate_limit` / `upstream` / `timeout`）；上游一帧未出即结束 |
| `invalid_model` | 目标本身记为不可用 | 上游 404；出站协议未装配或编码失败 |
| `context_exceeded` | **完全不改运行态** | 输入太长是客户端的问题，不该记作目标的失败 |

每次尝试一条上报，`report_id = request_id:attempt` 保证幂等；上报失败进 outbox 重试（见 [7.6](#76-get-adminoutbox上报队列)）。
