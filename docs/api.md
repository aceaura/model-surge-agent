# ModelSurge Agent API 参考

数据面「协议转换网关」服务的 HTTP 接口规范。

- **文档版本**：1.0（2026-09-17）
- **对应实现**：`backend/httpapi` + `backend/contract/agentv1`（commit `0abc6a4`）
- **服务定位**：接收客户端的 Anthropic / OpenAI 协议请求，转内部 IR 后按调度层指定的目标协议发给上游，结果按入站协议回编。**不做客户端鉴权（凭据转发调度层比对），不管理账号与配置，不承载调度运行态。**

三件套中的另外两个服务各有自己的 API 文档：Upstream（配置中心）见其仓库 `docs/api.md`；Relay（调度）的 `/internal/v1` 面仅供 Agent 调用，不对终端用户开放。

---

## 目录

1. [通用约定](#1-通用约定)
2. [公共数据面](#2-公共数据面)
3. [流式响应行为](#3-流式响应行为)
4. [健康检查](#4-健康检查)
5. [管理面](#5-管理面)
6. [数据模型](#6-数据模型)
7. [错误码](#7-错误码)
8. [请求结局（outcome）语义](#8-请求结局outcome语义)

---

## 1. 通用约定

### 1.1 两个鉴权域

| 面 | 鉴权 | 说明 |
|---|---|---|
| 公共数据面 | **无自身鉴权** | 客户端凭据（`x-api-key` 或 `Authorization: Bearer`）原样转发给调度层比对。凭据错误在调度层被拒绝后，按入站协议的 401 形状回错。 |
| 管理面 | `Authorization: Bearer <MSA_ADMIN_KEY>` | 本服务自己的密钥，与调度层密钥完全隔离。缺失或错误一律 401。 |

> 公共数据面不做客户端鉴权意味着它**必须只暴露在受信网络或反代之后**。compose 默认只绑 `127.0.0.1`。

### 1.2 内容类型与编码

- 服务默认监听 `:8080`（容器内）；compose 部署映射为 `127.0.0.1:8082`
- 请求体：`application/json`，UTF-8
- 非流式响应：`application/json`
- 流式响应：`text/event-stream`（SSE），并带 `Cache-Control: no-cache`、`X-Accel-Buffering: no`（防止反代缓冲 SSE 导致逐字输出失效）
- 请求体上限：默认 **64 MiB**。超限按 `invalid_request` 回错。

### 1.3 请求 ID

服务按以下顺序取请求 ID，取到即沿用；都没有则生成 `req_` + 24 位十六进制：

```
X-Request-Id → X-Request-ID → Request-Id → 自动生成
```

同一客户端请求换目标重试时 `request_id` 保持不变，因此它能把一次请求的多次尝试在流水中串起来。

### 1.4 管理面错误信封

管理面所有非 2xx 响应使用统一信封：

```json
{
  "error": {
    "code": "invalid_request",
    "message": "window must be 1h or 24h"
  }
}
```

数据面的错误不用这个信封——它按客户端的入站协议渲染错误体（见 [2.2](#22-错误渲染)）。

---

## 2. 公共数据面

### 2.1 端点总表

三条对话端点，每条提供三个等价路径别名（裸路径、带 `/v1`、带厂商前缀）：

| 入站协议 | 方法 | 路径别名 |
|---|---|---|
| Anthropic Messages | POST | `/v1/messages`、`/anthropic/v1/messages`、`/messages` |
| OpenAI Chat Completions | POST | `/v1/chat/completions`、`/openai/v1/chat/completions`、`/chat/completions` |
| OpenAI Responses | POST | `/v1/responses`、`/openai/v1/responses`、`/responses` |
| Count Tokens（Anthropic 形状） | POST | `/v1/messages/count_tokens`、`/anthropic/v1/messages/count_tokens`、`/messages/count_tokens` |
| 模型清单（Anthropic 形状） | GET | `/v1/models`、`/anthropic/v1/models` |
| 模型清单（OpenAI 形状） | GET | `/v1/models`、`/openai/v1/models`、`/models` |

路径别名不是冗余：客户端会把 base_url 配成 `host`、`host/v1`、`host/anthropic` 等各种形态，而多数客户端不允许改它拼在后面的固定路径。

> **`model` 字段填用户模型名**（调度层配置的 user model，如 `demo-pool`），不是上游 model_id。映射关系由调度层决定。

### 2.2 错误渲染

数据面错误按**客户端入站协议**的原生错误形状渲染，SDK 能直接解析：

| 协议 | 错误体形状 | 示例 |
|---|---|---|
| Anthropic | `{"type":"error","error":{"type":"...","message":"..."}}` | HTTP 400 |
| Chat Completions | `{"error":{"message":"...","type":"...","code":...}}` | HTTP 400 |
| Responses | `{"error":{"code":"...","message":"..."}}` | HTTP 400 |

错误种类到 HTTP 状态码的映射（各协议一致）：

| 错误种类 | HTTP |
|---|---|
| `invalid_request` | 400 |
| `context_exceeded` | 400 |
| `authentication` | 401 |
| `not_found` | 404 |
| `rate_limit` | 429 |
| `timeout` | 504 |
| `upstream` / `internal` | 500 |

### 2.3 Anthropic Messages

`POST /v1/messages`（含全部别名）

**请求头**

| 头 | 必填 | 说明 |
|---|---|---|
| `Content-Type: application/json` | 是 | |
| `x-api-key` 或 `Authorization: Bearer <key>` | 是 | 二选一，原样转发调度层比对 |
| `anthropic-version` | 否 | 不校验，可带可不带 |

**请求体（支持的字段）**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `model` | string | 是 | 用户模型名 |
| `messages` | array | 是 | `role` + `content`；content 支持 string 与分块数组（text / image / tool_use / tool_result / thinking / document） |
| `max_tokens` | int | 是* | Anthropic 协议必填；缺失时可由目标的 defaults 参数策略补齐 |
| `system` | string \| array | 否 | |
| `tools` | array | 否 | |
| `tool_choice` | object | 否 | |
| `temperature` | number | 否 | |
| `top_p` | number | 否 | |
| `top_k` | int | 否 | |
| `stop_sequences` | string[] | 否 | |
| `stream` | bool | 否 | 默认 false |
| `thinking` | object | 否 | `{type:"enabled", budget_tokens:N}` |
| `metadata` | object | 否 | |

**非流式响应示例**

```json
{
  "id": "msg_01ABC...",
  "type": "message",
  "role": "assistant",
  "model": "demo-pool",
  "content": [
    {"type": "text", "text": "你好"}
  ],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 12, "output_tokens": 34}
}
```

**流式响应**：标准 Anthropic SSE 帧序列（`message_start` → `content_block_start` → `content_block_delta` … → `content_block_stop` → `message_delta` → `message_stop`）。异常行为见 [3. 流式响应行为](#3-流式响应行为)。

### 2.4 Chat Completions

`POST /v1/chat/completions`（含全部别名）

**请求体（支持的字段）**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `model` | string | 是 | 用户模型名 |
| `messages` | array | 是 | 支持 `content`（string 或分块）、`tool_calls`、`tool_call_id`、`reasoning_content` 回传 |
| `max_tokens` / `max_completion_tokens` | int | 否 | 后者优先 |
| `temperature` | number | 否 | |
| `top_p` | number | 否 | |
| `stop` | string \| string[] | 否 | |
| `stream` | bool | 否 | |
| `stream_options.include_usage` | bool | 否 | |
| `tools` | array | 否 | |
| `tool_choice` | any | 否 | |
| `reasoning_effort` | string | 否 | low / medium / high |
| `user` | string | 否 | |

**非流式响应示例**

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "model": "demo-pool",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "你好"},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 12, "completion_tokens": 34, "total_tokens": 46}
}
```

**流式响应**：标准 `chat.completion.chunk` SSE 序列，最后以 `data: [DONE]` 收尾。

### 2.5 Responses

`POST /v1/responses`（含全部别名）

**请求体（支持的字段）**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `model` | string | 是 | 用户模型名 |
| `input` | string \| array | 是 | item 数组支持 message / function_call / function_call_output |
| `instructions` | string | 否 | |
| `max_output_tokens` | int | 否 | |
| `temperature` | number | 否 | |
| `top_p` | number | 否 | |
| `stream` | bool | 否 | |
| `tools` | array | 否 | |
| `tool_choice` | any | 否 | |
| `reasoning` | object | 否 | `{effort:"..."}` |
| `store` | bool | 否 | 不存储，字段仅为兼容而接受 |
| `user` | string | 否 | |

**非流式响应示例**

```json
{
  "id": "resp_...",
  "object": "response",
  "model": "demo-pool",
  "status": "completed",
  "output": [
    {"type": "message", "role": "assistant",
     "content": [{"type": "output_text", "text": "你好"}]}
  ],
  "usage": {"input_tokens": 12, "output_tokens": 34}
}
```

**流式响应**：标准 Responses SSE 事件序列（`response.created` → `response.output_item.added` → `response.output_text.delta` … → `response.completed`）。

### 2.6 Count Tokens

`POST /v1/messages/count_tokens`（含全部别名）

本地估算，**不打上游、不消耗配额**。请求体是 Anthropic Messages 形状（只需 `messages` 等内容字段）。估算偏保守（宁多勿少），据此裁剪上下文不会踩到真实上限。

**响应**

```json
{"input_tokens": 2}
```

### 2.7 模型清单

`GET /v1/models`（Anthropic 形状）、`GET /openai/v1/models` 或 `GET /models`（OpenAI 形状）

返回形状按**请求路径**决定而非 `Accept` 头——各家 SDK 解析不出自己认识的形状会直接报错。代理自调度层清单；**禁用的模型不列出**。

**Anthropic 形状**

```json
{
  "data": [
    {"id": "demo-pool", "type": "model", "display_name": "demo-pool",
     "created_at": "2024-01-01T00:00:00Z"}
  ],
  "has_more": false,
  "first_id": "demo-pool",
  "last_id": "demo-pool"
}
```

**OpenAI 形状**（`owned_by` 填所属 collection）

```json
{
  "object": "list",
  "data": [
    {"id": "demo-pool", "object": "model", "owned_by": "demo",
     "created": 1704067200}
  ]
}
```

### 2.8 参数后处理

请求参数不透传：编码成**目标协议的出站 wire body** 后，按上游配置中心定义的两层参数策略处理，然后才发上游：

| 层 | 语义 | 字段名 |
|---|---|---|
| `defaults` | 只填缺失字段 | 目标协议的原生字段名（如 anthropic 的 `max_tokens`、chat_completions 的 `max_completion_tokens`） |
| `overrides` | 无条件覆盖 | 同上 |

两层都发生在出站协议的语境内，不做跨协议字段翻译——anthropic 入站转 gemini 出站时，override 写的是 gemini 的字段名。**`overrides` 会覆盖客户端的显式值**：客户端传 `temperature: 0.7` 而目标配了 `{"temperature": 0.2}`，上游收到 0.2。这是设计行为（强制锁定关键参数），不是 bug。

参数在多跳链路（如 thinking budget）中的表达差异由出站 codec 处理，客户端无感。

---

## 3. 流式响应行为

### 3.1 已提交边界（committed boundary）

**第一个成功解码的上游帧到达后，本次尝试即视为已提交**：目标锁定，不再换目标重试。此后发生的一切错误（上游中途断流、读错误、超时）按客户端是否流式区分：

- **流式客户端**：HTTP 状态码已在首帧时锁定为 200，无法再改。错误以两种帧表达——一个按入站协议形状的 `error` 事件，随后补一个协议终止帧（Anthropic 为 `message_stop`，Chat Completions 为 `[DONE]`，Responses 为 `response.completed`）保证流正常闭合。
- **非流式客户端**：响应头尚未写出，仍能给出正确的 HTTP 错误状态码。

客户端因此总能拿到一个完整闭合的流，不会吊死等待。

### 3.2 换目标重试

提交之前的目标失败会触发换目标重试：把已失败的 `model_id` 追加进 `tried_ids` 重新向调度层要目标。重试对客户端完全透明，流水中体现为 `attempts > 1` 与 `tried_ids` 链。

以下情况**不**换目标，直接把错误回给客户端：

- `context_exceeded`（输入太长是客户端的问题，换目标没有意义）
- `invalid_model`（协议不可达，如出站 codec 未装配）

### 3.3 对上游永远流式

无论客户端 `stream` 与否，对上游一律以 SSE 请求；客户端要非流式时由本服务聚合成完整 JSON 响应。

---

## 4. 健康检查

`GET /health` — 无鉴权，供编排器与反代探活。

| 依赖状态 | HTTP | body |
|---|---|---|
| 全部正常 | 200 | `{"status":"ok",...}` |
| PG 或调度层不可达 | **503** | `{"status":"degraded",...}` |

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

- `cache` 可能取值：`ok` / `down` / `disabled`。缓存挂了**不算** degraded——它是全量降级而非服务不可用。
- `outbox_pending` / `outbox_dead`：结果上报队列的积压数。

管理面的 `GET /admin/health` 返回相同 body 但**永远 200**：管理面是拿来看状态的，把降级表达成 HTTP 错误会让前端分不清「服务降级」与「管理面自己不通」。

---

## 5. 管理面

全部挂在 `/admin` 下，鉴权头 `Authorization: Bearer <MSA_ADMIN_KEY>`。除 outbox 重试外全部只读。

### 5.1 GET /admin/health

永远 200。body 同 [4. 健康检查](#4-健康检查)。

### 5.2 GET /admin/requests

请求流水查询（PG 持久化的 `request_log`，按时间倒序）。

**查询参数**

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `outcome` | string | - | 按结局过滤：`normal` / `abnormal` / `retrying` / `invalid_model` / `context_exceeded` |
| `model_id` | string | - | 按上游 model_id 过滤 |
| `user_model` | string | - | 按用户模型名过滤 |
| `since` | RFC3339 | - | 只返回此时间之后的记录 |
| `limit` | int | 50 | 页大小 |
| `cursor` | string | - | 上一页返回的 `next_cursor`，原样回传 |

`next_cursor` 是不透明串（内部编码 `(at, request_id)`，base64）；**空串或缺失表示没有下一页**。游标只编码翻页位置、不含过滤条件，改变过滤条件后应从首页重新开始。

**响应**

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
      "outcome": "abnormal",
      "status_code": 404,
      "attempts": 1,
      "tried_ids": ["kimi-k2-turbo"],
      "committed": false,
      "stream": true,
      "input_tokens": 0,
      "output_tokens": 0,
      "latency_ms": 812,
      "error_code": "not_found",
      "error_message": "..."
    }
  ],
  "next_cursor": "WyIyMDI2LTA5LTE2VDE1OjMwOjA1WiIsInJlcV9mY2I3ODM5YWNjNmNjYjg4MTA4MjBkMzUiXQ"
}
```

字段完整定义见 [6.1 RequestSummary](#61-requestsummary)。**绝不含凭据与对话内容。**

### 5.3 GET /admin/requests/{request_id}

单条流水详情。404 时回 `{"error":{"code":"not_found","message":"no such request"}}`。

### 5.4 GET /admin/live

实时环（Redis 缓存，最新在前）。**缓存不可用时不报错**，回空列表加降级标记——前端必须区分「没有流量」与「看不到流量」。

**查询参数**：`limit`（默认 100）

**响应**

```json
{
  "entries": [
    {
      "request_id": "req_efc777bf2ee1eeb429a028de",
      "at": "2026-09-16T16:03:05.000990298Z",
      "inbound_protocol": "anthropic",
      "user_model": "demo-pool",
      "outcome": "abnormal",
      "status_code": 401,
      "attempts": 1,
      "latency_ms": 8,
      "error_code": "authentication"
    }
  ],
  "degraded": false
}
```

`degraded: true` 表示数字来自不可用的缓存，是「看不到」而非「没有」。

### 5.5 GET /admin/stats

分钟桶统计（Redis，桶保留 2 小时）。

**查询参数**：`window` — 只接受 `1h`（默认）或 `24h`。`24h` 实际返回最近 2 小时（桶只存 2 小时），保留该档位只为前端档位切换，不做额外承诺。其他值 400。

**响应**

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

- `success_rate` 是 `normal` 占比（0–1），总数为零时为 0
- `qps` 是窗口内平均每秒请求数

### 5.6 GET /admin/outbox

结果上报队列（上报调度层失败的结果先落库重试，客户端不受影响）。

**查询参数**

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `state` | string | `pending` | `pending` 或 `dead`，其他值 400 |
| `limit` | int | 100 | |

**响应**

```json
{
  "entries": [
    {
      "report_id": "req_fcb7839ac6c6cb8810820d35:0",
      "request_id": "req_fcb7839ac6c6cb8810820d35",
      "model_id": "kimi-k2-turbo",
      "outcome": "abnormal",
      "attempts": 3,
      "next_attempt_at": "2026-09-16T16:05:00Z",
      "last_error": "relay unreachable: connection refused",
      "created_at": "2026-09-16T16:03:05Z"
    }
  ]
}
```

`report_id` 是幂等键（`request_id:attempt`），重放同一份不会重复计数。超过 `MSA_OUTBOX_MAX_ATTEMPTS`（默认 20）进死信（dead），不删除——管理面可见、可复活。

### 5.7 POST /admin/outbox/{report_id}/retry

管理面**唯一的写操作**：把死信项重新排进队列。只重置 `next_attempt_at` 与 `attempts`，上报内容本身不可编辑。

**响应**：`{"revived": true}`；`report_id` 不存在时 404。

### 5.8 GET /admin/models

用户模型清单与「本服务能否真的发给它」并列。

**响应**

```json
{
  "models": [
    {"name": "demo-pool", "collection": "demo", "policy": "",
     "protocol": "anthropic", "enabled": true, "outbound_ready": true}
  ],
  "inbound": ["anthropic", "chat_completions", "responses"],
  "outbound": ["anthropic", "chat_completions", "gemini", "responses"],
  "cached": true
}
```

`outbound_ready: false` 意味着调度层可能把请求路由到本服务没实现出站编解码的协议上，那会以 `invalid_model` 收场——在这里直接可见，而不是等线上报错。

---

## 6. 数据模型

### 6.1 RequestSummary

| 字段 | 类型 | 说明 |
|---|---|---|
| `request_id` | string | 请求 ID（沿用客户端给定的或自动生成） |
| `at` | time | 请求到达时间 |
| `inbound_protocol` | string | `anthropic` / `chat_completions` / `responses` |
| `outbound_protocol` | string | 上游协议（四选一），未发出时空 |
| `user_model` | string | 客户端请求的模型名 |
| `model_id` | string | 最终选定的上游 model_id |
| `account` | string | 使用的上游账号名 |
| `outcome` | string | 见 [8](#8-请求结局outcome语义) |
| `status_code` | int | 回给客户端的 HTTP 状态码 |
| `attempts` | int | 尝试次数（换目标重试累计） |
| `tried_ids` | string[] | 已试过的上游 model_id 链 |
| `committed` | bool | 是否越过已提交边界（见 [3.1](#31-已提交边界committed-boundary)） |
| `stream` | bool | 客户端是否要流式 |
| `usage_estimated` | bool | 用量是否为估算 |
| `input_tokens` / `output_tokens` / `cache_read_tokens` | int | 用量 |
| `latency_ms` | int | 总耗时 |
| `first_token_ms` | int | 首个上游帧解码成功的耗时（流式与非流式都记录）；未收到任何帧即结束时为 0 |
| `error_code` / `error_message` | string | 错误信息（成功时空） |

### 6.2 LiveEntry

`RequestSummary` 的实时子集：`request_id`、`at`、`inbound_protocol`、`outbound_protocol`、`user_model`、`model_id`、`account`、`outcome`、`status_code`、`attempts`、`stream`、`latency_ms`、`first_token_ms`、`input_tokens`、`output_tokens`、`error_code`。不含 `tried_ids`、`error_message` 与 token 明细的完整度——实时环为速度牺牲了这些，要看全量走 `/admin/requests`。

---

## 7. 错误码

### 7.1 管理面错误码

| code | HTTP | 含义 |
|---|---|---|
| `unauthorized` | 401 | 管理密钥缺失或错误 |
| `invalid_request` | 400 | 参数校验失败（非法 window / state / since / cursor） |
| `not_found` | 404 | 请求或上报记录不存在 |
| `unavailable` | 503 | 依赖不可用（PG 挂、outbox 未装配、查询失败） |
| `internal_error` | 500 | 未预期错误 |

### 7.2 数据面错误种类（ir.ErrorKind）

| kind | HTTP | 典型来源 |
|---|---|---|
| `invalid_request` | 400 | 请求体解码失败、`model` 缺失、体超限 |
| `context_exceeded` | 400 | 上游报「输入太长」（按消息文本识别，各家园说法不同） |
| `authentication` | 401 | 调度层比对 client_key 失败 / 上游 401、403 |
| `not_found` | 404 | 上游 404（目标不可用 → `invalid_model` 结局） |
| `rate_limit` | 429 | 上游 429 |
| `timeout` | 504 | 首字超时（`MSA_FIRST_TOKEN_TIMEOUT`，默认 60s）/ 空闲超时（`MSA_IDLE_TIMEOUT`，默认 120s） |
| `upstream` | 500 | 上游 5xx / 不可解码的流 |
| `internal` | 500 | 本服务内部错误 |

---

## 8. 请求结局（outcome）语义

结局决定**调度层如何更新目标运行态**，语义由 relay 定义：

| outcome | 调度层动作 | 产生条件 |
|---|---|---|
| `normal` | 清零失败计数，累计用量 | 成功完成 |
| `abnormal` | 累计失败，可能触发冷却 | 最终仍失败 |
| `retrying` | 同 abnormal，但本次会继续换目标 | 中途失败、换目标重试中 |
| `invalid_model` | 目标本身记为不可用 | 上游 404、出站 codec 未装配 |
| `context_exceeded` | **完全不改运行态** | 输入太长是客户端的问题，不该记作目标的失败 |

每次尝试一条上报，`report_id = request_id:attempt` 保证幂等。
