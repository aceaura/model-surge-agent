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
| 公共数据面 | `/v1/*`、`/anthropic/*`、`/openai/*`、`/v1beta/*`、`/messages`、`/models` 等（见 [5.1](#51-路径别名与协议识别)） | 无自身鉴权；客户端凭据转发调度层比对 | 终端客户端（SDK、CLI、IDE 插件、浏览器内 SDK） |
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

- 请求体：`application/json`，UTF-8。请求侧的 `Content-Type` **只挡表单两种**（见 [2.3.3](#233-请求体-media-type)），其余一律放过——能否解码由 JSON 解析器给出确定答案，按白名单提前拒收只会把能正常服务的请求拒掉。
- 请求体允许带 UTF-8 BOM，服务会剥掉它再解码（`encoding/json` 不接受 BOM 前缀，一些 Windows 上的客户端会带）。
- 非流式响应：`application/json`。
- 流式响应：`text/event-stream`（SSE），并固定带以下响应头：

| 响应头 | 值 | 作用 |
|---|---|---|
| `Content-Type` | `text/event-stream` | SSE |
| `Cache-Control` | `no-cache` | 防止中间层缓存 |
| `Connection` | `keep-alive` | 长连接 |
| `X-Accel-Buffering` | `no` | 防止反向代理缓冲 SSE 导致逐字输出失效 |

#### 2.3.1 请求体传输编码

服务受理压缩过的请求体，按 `Content-Encoding` 处理：

| 值 | 行为 |
|---|---|
| 缺失、`identity` | 原样处理（大小写不敏感） |
| `gzip` | 解压后再解码 |
| `deflate` | 解压后再解码 |
| 其他（`br`、`zstd` 等） | 回 400，消息里列出本服务接受的编码 |

不支持的编码**不静默当未压缩处理**：那样客户端只会收到一条 JSON 语法错误，比明确说「不支持这个编码」难查得多。解压失败（流损坏、声明了 gzip 但发的是明文）回 **400** 而非 500——这是客户端发来的数据有问题。

#### 2.3.2 请求体上限

上限为 **64 MiB**（代码常量，可由部署配置覆盖）。上限存在是因为上下文塞满的请求确实很大，但没有上限意味着一个坏客户端就能把内存吃光。

| 情形 | 状态码 | `error_code` |
|---|---|---|
| 请求体超过上限 | **413** | `context_exceeded` |
| 解压**后**的字节数超过上限 | **413** | `context_exceeded` |
| 请求体为空（或只有空白） | 400 | `invalid_request` |
| 读请求体因其他原因失败 | 400 | `invalid_request` |

回 413 而非 400 是为了与 §2.8 的错误分类自洽：上游回 413 时本服务归类为 `context_exceeded`，自己产生同类错误时走同一套语义，客户端据此知道该裁剪输入，而不是去逐个字段检查参数。错误消息里带上上限的字节数，客户端可据此自行分片。

**压缩体不能绕过上限**：限制在解压前与解压后各判一次。只限压缩前等于没限——几百 KB 的 gzip 能解出几百 MB，而内存是按解压后的大小吃掉的。

判定是严格大于：解压后长度**正好等于**上限的请求会被接受。

空请求体单独回一句明确的话，而不是透出 `unexpected end of JSON input`——后者读的人分不清是自己没发 body 还是 body 被中间层吃了。

#### 2.3.3 请求体 media type

`Content-Type` **不做白名单，只挡表单两种**：

| `Content-Type` 的 media type | 结果 |
|---|---|
| `application/x-www-form-urlencoded` | **415** `invalid_request` |
| `multipart/form-data` | **415** `invalid_request` |
| 其余任意值（`application/json`、`text/plain`、`application/vnd.api+json` 等） | 放过，交给 JSON 解析器 |
| 缺失该头 | 放过 |
| 该头解不动（不是合法的 media type） | 放过 |

判定忽略 charset 等参数，media type 大小写不敏感。

**为什么只挡表单**：浏览器的 `fetch` 默认发 `x-www-form-urlencoded`，请求体是 urlencode 过的键值对。让它流到 JSON 解析器只会得到一句语法错误，而真正的问题是「这个端点不吃表单」——前者读的人看不出后者。

**为什么不做白名单**：真实客户端带的 `Content-Type` 五花八门（缺头、`text/plain`、带自家 `+json` 后缀），按「必须是 `application/json`」挡会把一堆本能正常解码的请求挡在门外。缺头或解不动同样放过：这类客户端很多，而它们发的确实是 JSON。

415 而非 400：状态码本身就说明「体的类型不对」而不是「体的字段不对」。这个闸门挂在读请求体的最前面，因此三条对话端点与 count_tokens 都覆盖到，且拒绝会按 §3.2.9 的形态入流水（`attempts` 为 0）。

### 2.4 请求 ID

服务按以下顺序取请求 ID，取到且**通过校验**即沿用；都没有或都不合法则生成 `req_` + 24 位十六进制：

```
X-Request-Id → X-Request-ID → Request-Id → 自动生成
```

沿用客户端给的值必须先校验：

| 规则 | 取值 |
| --- | --- |
| 长度 | 1 – 128 字节 |
| 字符集 | ASCII 字母、数字、`-`、`_`、`.`、`:` |

这两条覆盖 UUID、`req_<hex>`、`trace:span` 三种常见形态。**不合法时只是换成自动生成的值，不拒绝请求**——客户端的业务请求本身没问题，为一个可自愈的卫生问题回错误是把它变成故障。也**不回显客户端给的非法值**：回显等于把它写进响应头与日志，白费一次校验。

校验的必要性在于这个值的去处：它是 `request_log` 的主键，而写入语句是 `ON CONFLICT (request_id) DO UPDATE`——不校验的话，客户端只要发一个已存在的 ID 就能改掉别人那一行流水。它同时进 `X-Request-Id` 响应头、上报 ID（`report_id` 带唯一约束）与结构化日志字段，所以长度也必须有上限。

同一客户端请求换目标重试时 `request_id` 保持不变，因此它能把一次请求的多次尝试在流水中串起来。该 ID 同时上报调度层，用于跨服务对账。

**该 ID 一律回显在响应头 `X-Request-Id` 上**，覆盖全部响应形态：成功的非流式响应、SSE 流、count_tokens，以及受理面的每一种拒绝（404 / 405 / 413 / 400）。客户端自带合法 ID 时回显的是它自己给的那个值，不另生成——否则它日志里的 ID 与响应头里的对不上，两边都查不到。拒绝时同样带这个头，因为那正是客户端要报障的时刻。

### 2.5 入站协议声明

客户端可以在请求头里声明用哪一版协议对话、要开哪些 beta 特性。这两个头会被读取并传递给上游：

| 头 | 读法 | 出站行为 |
| --- | --- | --- |
| `anthropic-version` | 取首个非空值 | anthropic 出站用客户端的值；客户端没声明才用 `2023-06-01` |
| `anthropic-beta` | **取全部同名头行**，逐行按逗号切分 | 去空白、丢空令牌、去重保序后逗号拼接设为出站头；客户端没声明则不设该头 |

三条已定的判据：

- **不校验取值。** 版本号与令牌名都原样转发，未知值也照发。上游是唯一知道哪些版本与令牌有效的一方，我们维护白名单只会把上游新增的特性拒在门外，而那个故障由我们造成。
- **不注入本服务自己的令牌。** 部分同类网关会注入官方客户端的 beta 令牌与 `User-Agent` 以通过上游的「仅官方客户端」指纹检查。那是身份伪造；要那么做应当由运维在调度层的账号头里配置，不该由数据面代劳。
- **`anthropic-beta` 用全部头行而非首行。** 它是列表值头，HTTP 允许客户端分多行发，只读首行会静默丢掉其余声明。

声明与调度层给的凭据头冲突时（例如调度层也配了 `anthropic-version`），**调度层胜出**：那些值来自运维配置，运维意图优先于客户端声明。

出站协议承载不了声明时（chat_completions / responses / gemini 都没有放它的位置），声明被丢弃并**记入 `lossy`**，取值见 §3.2。不静默丢弃的理由与第六轮对错误维度的判据相同：beta 令牌是调用方的功能性契约，静默丢掉会让客户端按声明的特性解析响应、而 HTTP 200 让故障完全不可见。客户端一个声明都没发时不记任何有损说明——没声明就没有损失。

其余入站头一律不转发给上游。全量透传能让客户端影响出站请求的任意维度，而凭据头就在同一个头集合里。

### 2.6 类型词汇表

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
| `transport` | 502 | 出站连接层故障：连接被重置、h2 连接判定失联、拨号或握手失败（见 §6.9） |
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
| `pool` | object | PG 连接池饱和度快照，见下表。**PG 不可用或未配时整个字段不出现**，而不是报一组零——零是「池此刻空闲」的合法状态 |
| `goroutines` | int | 当前 goroutine 数（`runtime.NumGoroutine()`）。永远出现，永远 >= 1 |

`pool` 的字段：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `acquired` | int32 | 此刻被借出的连接数 |
| `idle` | int32 | 池里闲着的连接数 |
| `total` | int32 | 池中连接总数（含已借出与正在建立的）。`total / max` 就是饱和度 |
| `max` | int32 | 池上限 |
| `acquire_waiting` | int64 | **累计**发生过「池空、只能等一条连接」的次数，**不是此刻的排队长度**——pgxpool 没有暴露瞬时排队数。单调递增，两次取样做差才是这段时间的等待次数 |

`acquire_waiting` 的读法见 [6.10](#610-依赖饱和度与时延分段)。

### 3.2 RequestSummary

一条请求流水（`GET /admin/requests` 的元素、`GET /admin/requests/{request_id}` 的响应体）。**绝不含凭据与对话内容。**

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `request_id` | string | 请求 ID（沿用客户端的或自动生成，重试不变） |
| `at` | time | 请求到达时间（UTC） |
| `inbound_protocol` | string | 客户端使用的协议，见 [2.8](#28-枚举值索引) |
| `path` | string | 客户端打的请求路径，**不含 query**。同一协议的多条别名（见 [5.1](#51-路径别名与协议识别)）共用一个处理函数，靠它才能看出客户端把 base_url 配成了哪一种。不记 query 是因为数据面不读任何 query 参数，而一些客户端会把凭据塞进去——不记就不需要脱敏 |
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
| `input_tokens` | int64 | 输入 token 数（上游报的，或上游未报时按调度方向估算的，见 [6.5](#65-用量估算兜底)） |
| `output_tokens` | int64 | 输出 token 数 |
| `cache_read_tokens` | int64 | 命中提示缓存的输入 token 数 |
| `latency_ms` | int | 总耗时（毫秒） |
| `first_token_ms` | int | 首个上游帧解码成功的耗时（毫秒）；流式与非流式都记录；未收到任何帧即结束时为 0 |
| `dispatch_ms` | int | 问调度层要目标的**累计**耗时（毫秒，含全部重试）。调度层失败时同样记录 |
| `upstream_ms` | int | 发出上游请求到**响应头到达**的**累计**耗时（毫秒，含全部重试）。未发出请求时为 0 |
| `error_code` | string | 错误种类（成功时为空） |
| `error_message` | string | 错误文本（可能含上游原文；成功时为空） |
| `sanitized` | string[] | 对客户端请求所做的畸形修复说明，如把孤儿 `tool_result` 降级为文本、丢弃无人应答的 `tool_use`；为空或不出现表示请求本身合法。指向客户端 bug |
| `lossy` | string[] | 出站编码因目标协议表达不了而丢弃或改写的字段说明，形如 `dropped top_k (chat_completions cannot express it: ...)`；为空或不出现表示无损转换。指向路由选型 |

#### 3.2.1 `sanitized` 的说明形态

除请求体畸形修复外，工具声明治理也记在这里（与协议无关，任何目标协议都同样拒收）：

| 形态 | 触发条件 |
| --- | --- |
| `dropped a tool declaration with an empty name` | 工具声明的 `name` 为空 |
| `rewrote tool name %q to %q (illegal characters or over 64 bytes)` | 名字含 `[a-zA-Z0-9_.-]` 之外的字符（逐字符替换为 `_`），或超 64 字节（截为前 59 字符 + `_` + 名字 sha256 前 4 位十六进制；纯截断会让长前缀同名的工具撞车） |
| `dropped a duplicate tool declaration named %q, kept the first` | 同名工具重复声明，保留首个 |

改名会同步改写历史消息里对应的 `tool_use`，配对关系不受影响。

工具调用与结果的配对治理同样记在这里：

| 形态 | 触发条件 |
| --- | --- |
| `orphan tool_result %q demoted to text` | 结果找不到宣告它的 `tool_use`（上下文压缩删掉了调用却留下结果）。降级而非丢弃，因为结果里的内容仍是模型接下来要用的事实 |
| `duplicate tool_result %q demoted to text` | 同一个 id 有多份结果（重连留下的重放），只留最后一份 |
| `dropped unanswered tool_use %q` | 调用没有任何结果应答，上游会拒收这种悬空调用 |
| `dropped %s message left empty by tool_use removal` | 上一条删空了整条消息 |
| `reordered tool_result %q next to its tool_use` | 结果与调用的顺序不一致，上游按顺序绑回会让参数与结果对错 |
| `duplicate tool_use id %q in history (results may be paired to the wrong call)` | 历史里出现同 id 的两次 `tool_use`。这通常意味着上游不给调用 id 且合成 id 未按响应区分，导致跨轮撞号；后果是前一轮的结果被当成重复结果降级，后一轮的结果绑到前一轮的调用上 |

> 相邻的同角色消息**不是**畸形，本服务不合并它们。`user(tool_result)` 紧跟 `user(text)` 正是每一次工具回合的真实形态，合并会在每一轮都破坏上游的 prompt cache 前缀。

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

此外还有两条请求侧取舍：

| 字段 | 形态 | 触发条件 |
| --- | --- | --- |
| `thinking` | `dropped` | 目标协议拒收「推理开启 + 强制工具选择」的组合（`reasoning cannot be combined with a forced tool choice`）。关推理而不是把 `tool_choice` 降级为 `auto`：降级约束的故障不可见，上游会正常回一段文本，调用方以为模型自己决定不调工具 |
| `tool call id` | `rewrote` | 调用 id 超出目标协议的长度上限（`the call id is longer than this protocol accepts`），收敛为「前 N-5 字节 + `_` + 原 id 的 sha256 前 4 位」。改写会同步作用于 `tool_result` 的配对键，因此客户端下一轮回传的 id 与它上一轮收到的不一致——这正是要报出来的原因。当前四个协议的上限都未设值（见下文），故这一条实际不触发 |

只有真的丢掉约束才记进 `lossy`。把同一约束换个写法不记——`type` 大写化、联合类型折叠成 `nullable` 都属于此类。原因是这个字段的用途是指向路由选型：若每个带工具的 gemini 请求都恒定带一条说明，它就再也指不出哪条路由真的削弱了请求。

入站协议声明（§2.5）被丢弃时也记进这一列，形如：

| 说明前缀 | 何时出现 |
| --- | --- |
| `anthropic-version: <协议> carries no protocol version declaration` | 客户端声明了版本，但选中的出站协议没有承载它的位置 |
| `anthropic-beta: <协议> carries no feature declarations` | 同上，针对特性令牌 |

客户端一个声明都没发时不记：没声明就没有损失，恒定出现的说明会把真正的信号淹掉。anthropic 出站承载得了，也不记。

#### 3.2.3 响应侧的说明形态

响应侧（上游 → 客户端）的说明并入同一列 `lossy`，不另开字段：调用方要回答的问题是「这一轮转换有没有削弱内容」，与削弱发生在哪个方向无关。流式与非流式两条路径共用同一套措辞，否则同一份内容在两条路上会得到不同结论。

| 形态 | 触发条件 |
| --- | --- |
| `dropped thinking signature from the response (%s cannot express it: signature is only valid within its own protocol family)` | 上游给的推理签名来源与客户端协议不同族。跨族透传的密文客户端验不了，还会被它存进历史，下一轮带回来令整个请求被上游拒收 |
| `dropped thinking signature from the response (%s cannot express it: signature carries another vendor's ciphertext)` | 签名的前缀就表明它出自别家（如 `gAAAA` 属 responses），来源字段缺失时靠这一层兜住 |
| `dropped thinking signature from the response (%s cannot express it: no signed reasoning)` | 客户端协议根本没有承载签名的字段（如 `chat_completions`） |
| `merged tool call fragments that arrived under different indexes (matched by call id)` | `chat_completions` 上游同一次调用的分片带着不同 `index`，按 `id` 并回一个块。不并的后果是客户端收到两个 `tool_use`、拿着两份半截入参各执行一次，而 HTTP 状态码是 200 |
| `split a stream line that carried several JSON documents` | 一行 SSE `data:` 里首尾相接挤了多个 JSON 文档。只在整帧解码失败后才拆，拆后任一份不合法即整行失败，不接受部分解码 |
| `dropped error param %s (anthropic error envelope has no param field)` | 上游错误体给了 `error.param` 而客户端用的是 anthropic 协议，该信封没有这个位。详见 §3.2.6 |
| `upstream ignored the streaming request and returned a whole response` | 本服务对上游一律请求流式，但上游回的是 `Content-Type` 非 `text/event-stream` 的一整份响应（兼容层网关的常见形态）。内容照原样交付，丢的是逐字输出这一项。详见 §3.2.10 |

签名剥离只丢签名，不丢推理文本：文本对客户端仍然有用，只有签名是它验不了、下一轮会被上游拒收的那部分。

#### 3.2.4 请求体字节预算

出站请求体编码完成后会量一次字节数。超出该协议的预算时记一条说明，**请求仍照常发给上游**：

| 形态 | 触发条件 |
| --- | --- |
| `request body is 631204 bytes, over the 600000-byte budget for gemini (the upstream may reject it with a misleading 400)` | 出站请求体超出该协议的 `MaxPayloadBytes` |

三点需要说明：

- **不在本地拒收。** 上限是按实测推出的估计值，上游可能就接受了；本地拒收会把这份余地一并剥掉。
- **不裁历史。** 裁历史是有损且不可逆的语义改动：静默丢掉几轮对话会让模型失忆，而客户端从 200 响应里看不出任何异常。先让体积问题在诊断里可见，裁不裁由调用方按业务决定。
- **措辞里点明「误导性 400」**是因为实测中这类超限回的是一个 `reason` 为空的 `400 Improperly formed request.`，排查者不会把它与体积联系起来。

**当前四个协议的 `MaxPayloadBytes` 与 `MaxToolIDLen` 都是零值，即不设限、跳过检查。** 这是刻意的：有确凿实测证据的上限只在本服务不用的上游上（Mistral 的 9 位 id 正则、Kiro 的 ~615KB 体积门槛，后者的数据面归 Upstream 服务）。凭空猜一个上限会把本来能过的请求改坏。逻辑已就位，等某个协议实测到上限，改一个常量即可生效——有一条源码级守卫盯着四个出站协议都调用了预检，防止那天填了值却忘了挂。

#### 3.2.5 工具调用 id 的生命周期

上游不给调用 id 时本服务会合成一个（另外三个协议都要 id 才能把工具结果回指到调用）。合成 id 的形态是 `msa_synth_<响应标记>_<工具名>_<序号>`，例如 `msa_synth_3f9a1c_grep_1`。前缀的作用是让出站侧仅凭 id 文本就能判断来源：

| 协议 | id 字段 | 合成 id 的处置 |
| --- | --- | --- |
| anthropic | `tool_use.id` / `tool_result.tool_use_id`，必填 | 保留 |
| chat_completions | `tool_calls[].id` / `tool_call_id`，必填 | 保留 |
| responses | `call_id`，必填 | 保留 |
| gemini | `functionCall.id` / `functionResponse.id`，**可选** | **省略**，交由上游按调用顺序消歧 |

对 gemini 省略是因为合成的 id 上游从未见过，发回去它有权拒绝或错配；对另外三个保留是因为省略会让上游彻底无法配对，比发一个陌生 id 更糟。

**省略不记进 `lossy`**：这是恢复协议的原生形态而非削弱请求，而且 gemini 每个无 id 的调用都会触发，恒定出现的说明会把真正的有损信号淹掉。

省略只发生在写请求体这一步，IR 内部的 id 保持完整——gemini 的 `functionResponse` 只有 name 没有 id，填 name 要靠一张以 id 为键的表，IR 里的 id 一清那张表就查不到了。

**响应标记保证跨轮不撞号。** 标记是上游这次响应的 id（Chat Completions 的 `id`、Gemini 的 `responseId`）取 sha256 前 6 位十六进制，序号是该次响应内的调用序号。两者都必须参与：

- 只有序号时，第二轮的同名调用会拿到与第一轮一样的 id。客户端把两轮都回传进历史，于是历史里出现同 id 的两次 `tool_use`——配对治理据此把前一轮的结果当成重复结果降级成文本，把后一轮的结果绑到前一轮的调用上，参数与结果就对错了。这类历史会附带 `duplicate tool_use id ...` 说明（见 §3.2.1）。
- 只有标记时，同一次响应里的多个并行调用会互相撞。

不原样拼上游的响应 id：Gemini 的 `responseId` 很长，拼进去会把工具 id 顶到各家的长度上限附近，反而触发 id 收敛。

上游连响应 id 都不给时（兼容层网关常见）用一个随机值，而不是固定串：固定串会让所有缺响应 id 的上游退回到「只有序号」那个撞车状态。同一次响应内该值不变，所以该次响应的各调用仍共享同一个标记。

#### 3.2.6 错误的 `param` 维度

上游因某个具体字段拒收请求时，错误体里的 `error.param` 指出是哪个字段。这一维度会被归一进内部错误对象并按入站协议渲染出去：

| 入站协议 | 错误信封的 `param` 位 | 处置 |
| --- | --- | --- |
| anthropic | 无（信封只有 `{type,message}`） | 丢弃，记一条 `lossy` |
| chat_completions | `error.param` | 原样带出 |
| responses | `error.param`，流内 `event: error` 同样带 | 原样带出 |

| 形态 | 触发条件 |
| --- | --- |
| `dropped error param max_tokens (anthropic error envelope has no param field)` | 上游给了 `param` 而客户端用的是 anthropic 协议 |

两点需要说明：

- **这条丢弃要记进 `lossy`**，与「补块闭合帧」不同：它确实丢了信息，且只在上游真的给了 `param` 时才出现，指向性成立。恒定发生的动作若也记进来，这个字段就再也指不出哪条路由真的削弱了诊断。
- **提取只认 `error.param` 与顶层 `param` 两种位置**，不做 `message` 那样的宽松字段名匹配。消息认错了只是文案不准，字段名认错了会让调用方去改一个根本没问题的字段，比不给这个维度更糟。

#### 3.2.7 流式错误收尾的帧形态

`committed` 之后（首帧已解码、HTTP 200 已写出）才失败时，状态码收不回来，错误只能落在流内。收尾遵循一条分界：**闭合已开启的块，但不宣告正常结束**。前者让客户端 SDK 的块状态机收束，后者会让它把这轮当成功、把残缺内容存进历史。

| 入站协议 | 块闭合帧 | 错误帧 | 失败终态 | 不会出现 |
| --- | --- | --- | --- | --- |
| anthropic | 每个开着的块一个 `content_block_stop` | `event: error` | 错误帧自身即终态 | `message_delta`、`message_stop` |
| chat_completions | 无块概念，无需闭合 | 内含 `error` 的 data 帧 | `[DONE]` | 带 `finish_reason` 的 chunk |
| responses | 每个开着的条目一个 `output_item.done`（文本条目另有 `content_part.done`） | `event: error` | `response.failed` | `response.completed`、`response.incomplete` |

三点需要说明：

- **responses 的失败终态是 `response.failed`。** 只发 `error` 帧客户端会一直等一个终态事件，挂到自己的超时。该帧带 `status:"failed"` 与 `error` 对象，但**不重复携带已发出的 output items**：客户端已经逐帧收到过它们。
- **错误收尾时闭合的 responses 条目标 `incomplete` 而非 `completed`。** 那一刻条目里的函数入参可能只有半截 JSON、推理可能缺签名，标成 `completed` 等于告诉客户端这个条目可以用。
- **chat_completions 仍发 `[DONE]`。** 该协议的客户端靠它判定流结束，缺了会挂到超时——这与「不发 `finish_reason`」不矛盾：前者是流的边界，后者才是「正常说完了」的语义。

残缺工具入参或缺签名推理块另有一层处置（见 `error_code` 的 `incomplete_stream`）：补闭合帧不改变那条判定，闭合是为了状态机，不是为了把毒历史包装成可用。

#### 3.2.8 `error_code` 的取值

| 取值 | 含义 | 是否累计目标失败 |
| --- | --- | --- |
| `invalid_request` | 请求本身有问题，换目标也没用 | 是 |
| `authentication` | 凭据无效或权限不足 | 是 |
| `not_found` | 模型不存在 | 是 |
| `rate_limit` | 限流，换目标有意义 | 是 |
| `context_exceeded` | 输入超出模型上下文窗口 | 否（是客户端的问题） |
| `upstream` | 上游 5xx 或响应无法解码 | 是 |
| `transport` | 出站连接层故障（见 §6.9） | **否（坏的是本服务的连接，不是这个目标）** |
| `timeout` | 首字节或空闲超时 | 是 |
| `internal` | 本服务自身出错 | 是 |
| `incomplete_stream` | 流断在不能补闭合的位置（残缺工具入参、缺签名推理块） | 是 |
| `canceled` | **客户端自己取消了请求** | **否** |

`canceled` 单独成类是为了让运维能把它与真实上游故障分开统计。混在一起的后果是：客户端多按几次停止，健康账号的失败计数就会涨到冷却。流式与非流式都覆盖——非流式请求在聚合完成前一个字节都不写客户端，所以「写客户端失败」观察不到取消，只能从请求 context 判定。

判定为客户端取消时：上报 `normal` 不累计失败、按已收事件结算 usage（上游照样计费）、不换目标重试（客户端已经不要这个回答了）、不向客户端写任何字节。

#### 3.2.9 受理面拒绝的流水形态

请求在选目标之前就被拒（路径不存在、方法不允许、体超限、media type 是表单、传输编码不支持、压缩体损坏、体为空、体无法解码、缺模型名）时同样入流水。不入的后果是这类接入问题在管理面完全不可见：客户端报 404，运维查 `GET /admin/requests` 一行都没有，无法判断请求究竟有没有到达本服务。

字段取值与数据面请求不同：

| 字段 | 取值 | 为什么 |
| --- | --- | --- |
| `outcome` | `abnormal` | 请求失败了。但**不向调度层上报** |
| `attempts` | `0` | 与「打了一次上游但失败」（`1`）区分开 |
| `model_id` / `account` / `outbound_protocol` | 空 | 还没选过目标 |
| `user_model` | 能解出就填，否则空 | 缺模型名与解码失败这两条路径解不出 |
| `status_code` | 实际回给客户端的码 | `404` / `405` / `413` / `415` / `400` |
| `error_code` | 对应的 `ir.ErrorKind` | 与数据面同一套取值 |
| `path` | 客户端打的路径 | 别名相关的接入问题只能靠它定位 |

不上报调度层是刻意的：这一刻还没选过目标，硬编一个占位账号会让某个真实账号无端累计失败并被冷却，而它根本没参与过这次请求。

`/admin/` 前缀下的 404/405 不走这套：管理面有自己的错误约定，也不该出现在数据面流水里。

#### 3.2.10 上游忽略流式请求

本服务对上游**一律**请求流式（`Accept: text/event-stream`、请求体 `stream: true` 写死），客户端要不要流是另一件事，两者互不改写。但上游不保证照办：兼容层网关忽略 `stream: true` 回一整份 JSON 是常见形态。

判定只看上游响应的 `Content-Type`：

| 上游响应头 | 处理 |
| --- | --- |
| media type 为 `text/event-stream`（忽略 `charset` 等参数，大小写不敏感） | 按 SSE 逐帧读 |
| 头缺失，或解不出 media type | 按 SSE 处理。绝大多数缺头的上游发的是正常 SSE，而错判成整份响应会把真流缓冲成一整份、毁掉逐字输出 |
| 其余任何 media type（含没听说过的） | 整体读入，用出站协议的非流式解码器解，再投影成事件序列送进与真流完全相同的下游路径 |

采纳整份响应时的行为：

- 内容、`stop_reason`、用量、工具调用的 id 与入参**照原样交付**，流式客户端拿到的是一份内容完整、一次到齐的 SSE，非流式客户端拿到的是等价的整份响应
- 记一条 `lossy`：`upstream ignored the streaming request and returned a whole response`。内容没损失，丢的是逐字输出这一项，客户端有权知道
- 响应体为空或只有空白：按「上游一帧都没发」处理，判为截断，**可换目标重试**
- 响应体解不动：报 `ir.ErrUpstream`，可换目标重试。**不伪造一个内容为空的成功响应**——那样客户端拿到的是 `stop_reason: end_turn` 加零内容、目标被记为健康、用量为零、另外的目标一次都不试
- 响应体超过 32 MiB：报 `ir.ErrUpstream`，不无界读进内存

### 3.3 LiveEntry

实时环元素（`GET /admin/live`）。是 `RequestSummary` 的子集：为速度省略了 `tried_ids`、`error_message`、`usage_estimated`、`cache_read_tokens`、`sanitized`、`lossy`，其余同名字段含义一致：

`request_id`、`at`、`inbound_protocol`、`outbound_protocol`、`user_model`、`model_id`、`account`、`outcome`、`status_code`、`attempts`、`stream`、`latency_ms`、`first_token_ms`、`dispatch_ms`、`upstream_ms`、`input_tokens`、`output_tokens`、`error_code`。

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
  "outbox_dead": 0,
  "pool": {
    "acquired": 1,
    "idle": 3,
    "total": 4,
    "max": 8,
    "acquire_waiting": 0
  },
  "goroutines": 37
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

三条对话端点，每条提供四个等价路径别名（裸路径、带 `/v1`、带重复 `/v1/v1`、带厂商前缀）：

| 入站协议 | 方法 | 路径别名 |
|---|---|---|
| Anthropic Messages | POST | `/v1/messages`、`/v1/v1/messages`、`/anthropic/v1/messages`、`/messages` |
| OpenAI Chat Completions | POST | `/v1/chat/completions`、`/v1/v1/chat/completions`、`/openai/v1/chat/completions`、`/chat/completions` |
| OpenAI Responses | POST | `/v1/responses`、`/v1/v1/responses`、`/openai/v1/responses`、`/responses` |
| Count Tokens（Anthropic 形状） | POST | `/v1/messages/count_tokens`、`/v1/v1/messages/count_tokens`、`/anthropic/v1/messages/count_tokens`、`/messages/count_tokens` |
| 模型清单（外形按客户端推断） | GET | `/v1/models`、`/v1/v1/models`、`/models` |
| 模型清单（Anthropic 形状） | GET | `/anthropic/v1/models` |
| 模型清单（OpenAI 形状） | GET | `/openai/v1/models` |
| 模型清单（Gemini 形状） | GET | `/v1beta/models` |
| 单模型查询 | GET | 上列每条清单路径 + `/{id}`，外形与该清单路径一致 |

别名不是冗余：客户端会把 base_url 配成 `host`、`host/v1`、`host/anthropic` 等各种形态，而多数客户端不允许改它拼在后面的固定路径。**协议按路径识别**，与 `Accept` 头无关。

`/v1/v1` 那一条专为「用户把 base_url 配成 `host/v1`、而 SDK 仍在后面拼一个固定的 `/v1/...`」准备。裸路径别名接不住它：SDK 拼的那一段是它自己加的，用户改不掉。

**路径或方法不被接受时**，服务不回 HTTP 层的纯文本，而是按路径推断出的入站协议渲染错误信封（推断不出时用 Anthropic 形状）。这样客户端 SDK 得到的是一条业务错误，而不是一个 JSON 解码异常：

| 情形 | 状态码 | 响应头 | 响应体 |
|---|---|---|---|
| 路径未注册 | 404 | `X-Request-Id` | `no such endpoint: <方法> <路径>`，协议信封，`error_code` 为 `not_found` |
| 方法不被允许 | 405 | `Allow`、`X-Request-Id` | `method <方法> is not allowed on <路径>`，协议信封，`error_code` 为 `invalid_request` |

`Allow` 头列出该路径接受的方法，客户端据此能直接改对而不必翻文档。`/admin/` 前缀下的拒绝不做此改写——管理面有自己的错误形状（见 §2.7）。

**公共请求头**（三条对话端点与 count_tokens 相同）：

| 头 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `Content-Type` | string | 否 | 建议 `application/json`；表单两种回 415 | 只挡 `application/x-www-form-urlencoded` 与 `multipart/form-data`，其余（含缺失）一律放过，交给 JSON 解析器。详见 §2.3.3 |
| `Content-Encoding` | string | 否 | `gzip`、`deflate`、`identity` | 请求体传输编码。缺失或 `identity` 按未压缩处理；其他值回 400 并列出本服务接受的编码。详见 §2.3 |
| `x-api-key` | string | 二选一 | 非空 | 客户端凭据（Anthropic SDK 发这个），转发调度层比对 |
| `Authorization` | string | 二选一 | `Bearer <key>` | 客户端凭据（OpenAI SDK 发这个）。两者同给时 `x-api-key` 优先 |
| `X-Request-Id` / `X-Request-ID` / `Request-Id` | string | 否 | ≤128 字节，`[A-Za-z0-9-_.:]` | 请求 ID，按顺序取第一个通过校验的值；都无或都不合法则自动生成（不拒绝请求）。**合法值会原样回显在响应头**，见 §2.4 |
| `anthropic-version` | string | 否 | 任意，不校验 | 协议版本声明。anthropic 出站用客户端的值，缺省回落 `2023-06-01`；其余出站协议承载不了，记入 `lossy`。见 §2.5 |
| `anthropic-beta` | string（列表值，可多行） | 否 | 任意，不校验 | 特性声明。全部头行按逗号切分、去重保序后转发给 anthropic 出站；其余出站协议承载不了，记入 `lossy`。见 §2.5 |

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
| `max_tokens` | int | 是（协议要求） | ≥ 1 | 响应最大输出 token 数。本服务不校验，也**不设下限**——客户端要一个极短回答是它的事，静默抬高是无痕改写它的意图。缺失或 ≤0 时**仅在出站为 Anthropic 时**补 4096 并报有损 `filled in max_tokens`（其余三个出站协议此字段可选，直接省略）；目标的 `overrides` 可强制改写 |
| `system` | `string \| array of object` | 否 | 字符串，或 `[{"type":"text","text":"..."}]` | 系统提示 |
| `tools` | array of object | 否 | `[{"name","description"?,"input_schema"}]` | 工具定义；`input_schema` 为 JSON Schema |
| `tool_choice` | object | 否 | `{"type":"auto"\|"any"\|"none"\|"tool","name"?}` | 工具选择策略；`type=tool` 时 `name` 必填 |
| `temperature` | number | 否 | 0–1（Anthropic 约定） | 采样温度。**透传不校验**；新模型已废弃此参数，上游可能拒绝 |
| `top_p` | number | 否 | 0–1 | 核采样 |
| `top_k` | int | 否 | ≥ 0 | Top-K 采样 |
| `stop_sequences` | string[] | 否 | 非空字符串 | 自定义停止序列，命中后 `stop_reason=stop_sequence` |
| `stream` | bool | 否 | 默认 `false` | 是否以 SSE 返回 |
| `thinking` | object | 否 | `{"type":"enabled"\|"disabled","budget_tokens":int}` | 扩展思考。`type` 为 `"enabled"` 是明确开启，其他取值均为明确关闭；**省略整个字段是「没提」，与 `disabled` 不等价**（三态见 [6.4](#64-跨协议能力差异)）。`budget_tokens` ≤0 且无 effort 档位时，出站按 `max_tokens` 的 50% 折算（不低于 1024、且小于 `max_tokens`，否则上游会拒绝） |
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
| `reasoning_effort` | string | 否 | `none` / `minimal` / `low` / `medium` / `high` / `xhigh` / `max`（各上游支持范围不同） | 推理强度档位。`none` 例外：它是**明确关闭**而非档位，不会被折算成某个真实档位；省略整个字段是「没提」（三态见 [6.4](#64-跨协议能力差异)） |
| `presence_penalty` | number | 否 | -2.0–2.0 | 出现惩罚，透传不校验；目标不支持时丢弃并报有损 |
| `frequency_penalty` | number | 否 | -2.0–2.0 | 频率惩罚，同上 |
| `seed` | int | 否 | 任意整数（**含 0**，0 是一个具体种子而非「没给」） | 采样种子；目标不支持时丢弃并报有损 |
| `n` | int | 否 | ≥ 1 | 候选数。目标不支持时丢弃并报有损，**此时只会回一个候选**——本服务不在本地做 n 次扇出 |
| `logprobs` | bool | 否 | — | 是否回对数概率。`false` 是**明确不要**，与省略不等价 |
| `top_logprobs` | int | 否 | 0–20 | 每 token 回多少候选概率 |
| `logit_bias` | object | 否 | `{"<token_id>": number}`，值域 -100–100 | token 偏置。**不做跨协议 token id 重映射**：各家分词器不同，重映射会把偏置加到别的词上 |
| `service_tier` | string | 否 | 透传不校验 | 服务档位 |
| `parallel_tool_calls` | bool | 否 | — | 是否允许并行工具调用。三态：省略=没提，`false`=明确禁止 |
| `response_format` | object | 否 | `{"type":"text"\|"json_object"\|"json_schema","json_schema":{"name","schema","strict"?}}` | 输出格式约束。`type:"text"` 视为**没提**（那是默认形态，不是一项要求）；目标只支持 JSON 不支持 schema 时降级为纯 JSON 并报有损 |
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
| `service_tier` | string | **上游实际执行的档位，原样回显**。可能低于请求里点的那个（点 `flex` 拿到 `default` 就是被降档了）。上游没回时该键不出现——本服务绝不拿请求里的值兜底，那会把降档伪装成按要求执行 |

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
| `reasoning` | object | 否 | `{"effort": "none"\|"low"\|"medium"\|"high"\|..., "summary":"auto"\|"concise"\|"detailed"}` | 推理配置。开启思考时本服务出站恒带 `summary:"auto"`（否则推理内容对客户端不可见）；`effort:"none"` 是**明确关闭**，出站不带 `summary`；省略整个字段是「没提」（三态见 [6.4](#64-跨协议能力差异)） |
| `store` | bool | 否 | 默认 `false` | **出站恒为 `false`**：本服务不让上游留存会话（多目标重试时各上游留存状态互不可见，会分叉） |
| `text` | object | 否 | `{"format":{"type":"text"\|"json_object"\|"json_schema","name"?,"schema"?,"strict"?},"verbosity":"low"\|"medium"\|"high"}` | 输出格式与详尽度。**schema 三项平铺在 `format` 这一层**，不像 Chat Completions 那样再嵌一个 `json_schema` 对象；`type:"text"` 视为没提 |
| `include` | array of string | 否 | 透传不校验 | 要求额外返回的条目；目标不支持时丢弃并报有损 |
| `truncation` | string | 否 | `auto` / `disabled` | 超长时的截断策略 |
| `metadata` | object | 否 | `{"<key>":"<value>"}` | 客户端自定义元数据。与 `user` **各走各的**：后者是用户标识、会被翻译成各协议的用户字段，混进元数据会让它发出去两次 |
| `service_tier` | string | 否 | 透传不校验 | 服务档位 |
| `parallel_tool_calls` | bool | 否 | — | 是否允许并行工具调用，三态同 Chat Completions |
| `top_logprobs` | int | 否 | 0–20 | 每 token 回多少候选概率 |
| `user` | string | 否 | 非空 | 终端用户标识，透传上游 |
| `previous_response_id` | string | 否 | **带值即 400** | 上游托管的会话续接，见下文「托管状态字段」 |
| `conversation` | `string \| object` | 否 | **带值即 400** | 同上 |
| `context_management` | object | 否 | **带值即 400** | 同上 |
| `prompt` | object | 否 | **带值即 400** | 同上 |
| 其他 | — | — | — | 未列出的字段被忽略 |

**托管状态字段**

上表四个字段都把状态放在上游那一侧，本服务表达不了：请求会被分发到任意一个目标账号，那里没有这个 id 指向的历史。收下再忽略等于悄悄丢掉客户端以为已经带上的上下文，因此一律显式拒收：

```json
{"error":{"type":"invalid_request_error","code":"invalid_request","message":"server-side conversation state is not supported: previous_response_id, conversation; send the full conversation in input"}}
```

规则：

- 一次报全所有命中的字段（按 `previous_response_id` → `conversation` → `context_management` → `prompt` 的顺序），不是命中第一个就返回。客户端往往同时带了两三个，一次只报一个会让它改一处再撞一次。
- 显式的 `null` 等于没带这个字段，不触发拒收。
- `context_management` 是上游自己裁剪历史的开关，同样依赖上游那一侧存着历史。收下再忽略会让客户端以为超长上下文已被裁剪，实际整段原样发出并撞上窗口上限。
- 替代做法：把完整历史放进 `input` 自行携带。

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
| `service_tier` | string | **上游实际执行的档位，原样回显**，出现在 `response.created` 与 `response.completed` 两帧的 `response` 对象里。语义与 Chat Completions 同一条，见该端点的说明 |
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

**估算口径**（不是真分词器，见 [6.6](#66-token-估算的两个方向)）：

- CJK（中日韩）按**每字符约 1.25 token** 计。各家实测的 CJK 系数在 0.68–1.21 之间，取上界才能保证不低报。
- 其余字符按每 4 字符约 1 token 计。
- 每个媒体块（图片、音频等）计入 258 token 的**下限**，不按 base64 长度算。真实消耗通常高于这个数。
- `tools` 的名称、描述、schema 全部计入。

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

**请求**：见下表。清单代理自调度层（短缓存），与 `Accept` 头无关。

**外形按客户端身份决定，路径优先于头**：

| 路径 | 外形 |
|---|---|
| `/anthropic/v1/models` | 固定 Anthropic |
| `/openai/v1/models` | 固定 OpenAI |
| `/v1beta/models` | 固定 Gemini |
| `/v1/models`、`/v1/v1/models`、`/models` | 按请求信号推断，见下表 |

推断的次序固定：

| 请求信号 | 外形 |
|---|---|
| 有 `anthropic-version` 或 `x-api-key` | Anthropic |
| 有 `x-goog-api-key` 或 query 参数 `key` | Gemini |
| 以上都没有 | OpenAI |

**为什么要推断**：`/v1/models` 与 `/models` 两族 SDK 都会打——用户把 base_url 配成 `host`，Anthropic SDK 与 OpenAI SDK 各自拼出同一个路径。而两家清单的字段名与包装都不同（`display_name` 对 `owned_by`），给错了对方解析不出来。

**为什么路径优先**：`/anthropic/...` 与 `/openai/...` 是用户显式选的族，头只是推断。让头翻盘会让「我明明配了 `/openai` 前缀」这件事失效，而用户无从判断为什么。

**为什么 OpenAI 垫底**：前两族要有专有头才成立，OpenAI 是「什么专有信号都没有」的那一档（`Authorization: Bearer` 不专属于它），只能垫底。三家的凭据头名互不重叠，所以单看头名就能分族。多族头同时出现（代理链上会发生）时取 Anthropic：`anthropic-version` 没有别的含义，而 Gemini 的 `key` 参数常被中间层顺手加上。

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

**Gemini 形状响应** `200`：

| 字段 | 类型 | 取值与含义 |
|---|---|---|
| `models` | array of object | 元素：`{"name":"models/<id>","displayName":string,"supportedGenerationMethods":array of string}` |

`name` 带 `models/` 前缀是 Gemini 的资源名约定，客户端会把这个串原样回传去做单模型查询（见 §5.7）。`supportedGenerationMethods` 固定为 `["generateContent","streamGenerateContent"]`，客户端据此判断能不能流式。

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

### 5.7 GET /models/{id}（单模型查询）

**使用场景**：SDK 的 `models.retrieve()`。客户端先拿清单再单查某一项，用于校验模型名是否可用、或展示单个模型的元信息。

**请求**：上列每一条清单路径 + `/{id}`。SDK 是在它拿清单的那个 base_url 上拼 `/{id}`，所以两者必须成对存在——少一条就有一类客户端的 `retrieve()` 静默 404。

| 路径参数 | 类型 | 说明 |
|---|---|---|
| `id` | string | 用户模型名。允许带 `models/` 前缀（Gemini 客户端回传的是清单里的资源名），前缀会被剥掉再查 |

**响应** `200`：一个模型对象，字段与该路径的清单元素完全一致，但**不带清单的包装**（没有 `data` / `models` / `object:"list"`）。外形推断与 §5.6 同一套规则。

**错误**

| HTTP | `error_code` | 触发条件 |
|---|---|---|
| `404` | `not_found` | 该 id 不存在，**或存在但已禁用** |
| `500` | `upstream` | 从调度层拉取清单失败 |

禁用的模型按不存在处理：清单不列它，单查却回它会让客户端拿到一个必然失败的 id——而它从清单里根本看不到这个 id，无从判断为什么失败。

404 回的是**该族的错误信封**，不是 200 包一个错误体。SDK 看到 200 会按成功去解析，拿到一个缺字段的对象，报错指向的是字段名而不是「这个模型不存在」。

**示例**

```bash
curl -s $BASE/v1beta/models/models/demo-pool
```

```json
{
  "name": "models/demo-pool",
  "displayName": "demo-pool",
  "supportedGenerationMethods": ["generateContent", "streamGenerateContent"]
}
```

### 5.8 跨域（CORS）与预检

**使用场景**：浏览器里的 SDK（网页版客户端、在线 playground）直接打本服务。CORS 是纯浏览器侧的强制，服务端的全部职责就是把头发对。

**配置**：环境变量 `MSA_CORS_ORIGINS`，逗号分隔的来源列表。空项会被丢掉（容忍尾逗号）。

| 配置 | 行为 |
|---|---|
| 不配，或含 `*` | `Access-Control-Allow-Origin: *`，**不发** `Allow-Credentials` |
| 配具体来源，且请求的 `Origin` 在列 | 回显该 `Origin`，并发 `Allow-Credentials: true` 与 `Vary: Origin` |
| 配具体来源，但请求的 `Origin` 不在列 | 不发 CORS 头（只发 `Vary: Origin`），**请求照常处理** |
| 请求无 `Origin` 头 | 一个 CORS 头都不发 |

`*` 与 `Allow-Credentials` **不能并存**：浏览器会拒绝整个响应。要用凭据就配具体的来源白名单。

不在白名单时仍然处理请求，而不是回 403：CORS 是浏览器侧的强制，服务端多拦一层只会让非浏览器客户端（它们不看 CORS）莫名被拒。回显来源时必须声明 `Vary: Origin`，否则中间缓存会把一个来源的响应喂给另一个来源。

**预检**：任意路径上的 `OPTIONS` 一律回 **204**，空体，不进路由、**不记流水**。预检不是一次业务请求，记进去会让流水里每个浏览器请求都多出一条。

预检必须在路由之前答掉，否则 `OPTIONS` 会落到 404/405 那套改写（§5.1），浏览器据此判定跨域失败。

**放行的请求头**（`Access-Control-Allow-Headers`）用白名单而非 `*`——`*` 与 `Allow-Credentials` 并存时同样被浏览器拒绝：

| 类别 | 头 |
|---|---|
| 通用 | `Content-Type`、`Content-Encoding`、`Content-Length`、`Authorization`、`Accept`、`Accept-Encoding` |
| 厂商专有 | `x-api-key`、`anthropic-version`、`anthropic-beta`、`x-goog-api-key` |
| 请求 ID | `X-Request-Id` |
| OpenAI SDK | `x-stainless-*` 一族（arch、lang、os、package-version、runtime、runtime-version、retry-count、timeout、async、helper-method、poll-helper、custom-event） |

`Access-Control-Expose-Headers` 固定为 `X-Request-Id`：不暴露则浏览器里的 JS 读不到这个头，报障时无从对账。`Access-Control-Max-Age` 为 `86400`——不设它的话浏览器对每个请求都先发一次 `OPTIONS`，延迟直接翻倍。

**出错的响应也带 CORS 头**。没有它，浏览器里的 JS 读不到状态码与错误体，只看到一句 network error——正是最该看清错误的时候。

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
| thinking 开关三态 | 见下表 |
| 服务端工具 | 只有 Anthropic 表达得了「由上游自己执行的工具」；其他目标整条丢弃并报有损诊断，**不降级成普通函数工具**（详见 [6.13](#613-工具意图的保真)） |
| 工具结果失败态 | Anthropic（`is_error`）与 Gemini（`error` 键）有原生表达，原样写出；Chat Completions 与 Responses 的工具消息没有可放标记的键，改写成内容前缀 `[tool error] ` 并报有损诊断（详见 [6.14](#614-响应侧语义维度的保真)） |
| 停止序列身份 | 只有 Anthropic 的响应有 `stop_sequence` 字段；转其他入站协议时不合成、**不报有损**（客户端用的协议本来就没有这个键，那是协议差异而非损失） |
| 图片 `detail` 层级 | Chat Completions 与 Responses 双向透传；Anthropic 与 Gemini 没有这一维，丢弃并报有损（它决定计费与识别精度）。客户端没给时**不合成**——合成会把「按上游默认」变成「按我们猜的」，两者计费可能不同（详见 [6.15](#615-目标协议承载得了却没写出去的维度)） |
| 相邻同角色消息 | 在 IR 层合并成一条并报说明。Anthropic 硬性要求 user/assistant 交替，非交替历史换来不可重试的 400，而相邻两条 user 在 Chat Completions 里完全合法 |
| 调参字段 | 见「调参字段的承载矩阵」 |

#### 推理开关的三态

客户端对推理只有三种表态，本服务逐一区分，**不把「没提」与「明确关闭」合并**：部分上游模型默认开启推理，把明确关闭当成没提会让请求被静默改成开启。

| 客户端表态 | 判定 | 出站行为 |
|---|---|---|
| 请求里完全不提推理字段 | 没提 | 出站**不写**任何推理字段，随上游模型自己的默认；模型配置的 `defaults` 此时可以填进来 |
| 明确关闭 | 关闭 | 出站写出该协议的关闭标记（见下表）；`defaults` 因键已存在而不再生效，**客户端赢**；`overrides` 仍然压得住（那是运维的强制层） |
| 明确开启 | 开启 | 按预算/档位折算后写出开启标记 |

各协议的「明确关闭」写法（入站识别、出站写出，双向同一张表）：

| 协议 | 关闭写法 | 说明 |
|---|---|---|
| Anthropic | `"thinking":{"type":"disabled"}` | 省略 `thinking` 是「没提」，与 `disabled` 不等价 |
| Chat Completions | `"reasoning_effort":"none"` | `none` 是关闭，不是强度档位；不会被折算成某个真实档位 |
| Responses | `"reasoning":{"effort":"none"}` | 关闭时**不带** `summary`：没有推理过程可摘要 |
| Gemini | `"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}` | 出站独有；关闭时不带 `includeThoughts` |

目标协议不支持推理时，明确关闭**不报**有损诊断——那恰好就是客户端要的结果；只有明确开启才报 `dropped thinking`。

#### 调参字段的承载矩阵

采样、候选、输出格式这一类调参字段各协议覆盖面不同。本服务把客户端给的值解进中立表示，能表达的按目标方言写出，不能表达的**丢弃并报有损诊断，请求照常发出**。

| 字段 | Anthropic | Chat Completions | Responses | Gemini |
|---|---|---|---|---|
| `presence_penalty` / `frequency_penalty` | ✗ | ✓ | ✗ | ✗ |
| `seed` | ✗ | ✓ | ✗ | ✗ |
| `n`（候选数） | ✗ | ✓ | ✗ | ✓ `candidateCount` |
| `logprobs` / `top_logprobs` | ✗ | ✓ | 仅 `top_logprobs` | ✓ `responseLogprobs` / `logprobs` |
| `logit_bias` | ✗ | ✓ | ✗ | ✗ |
| `service_tier` | ✗ | ✓ | ✓ | ✗ |
| `parallel_tool_calls` | ✗ | ✓ | ✓ | ✗ |
| `response_format`（纯 JSON） | ✗ | ✓ | ✓ `text.format` | ✓ `responseMimeType` |
| `response_format`（带 schema） | ✗ | ✓ | ✓ | ✓ `responseSchema` |
| `verbosity` | ✗ | ✗ | ✓ `text.verbosity` | ✗ |
| `include` | ✗ | ✗ | ✓ | ✗ |
| `truncation` | ✗ | ✗ | ✓ | ✗ |
| 客户端 `metadata` | ✗ | ✗ | ✓ | ✗ |

要点：

- **只报有损，不拒请求**。拒绝会把一个能用的回答换成零回答；而目标协议是调度层按策略选的、客户端无从预知，让它为此吃一个 400 归因方向是错的。运维确需强制某个值时用目标的 `overrides`。
- **零值都有意义，一律按「给没给」区分**。`seed:0` 是一个具体种子、`presence_penalty:0` 是「不惩罚」、`logprobs:false` 是「明确不要」、`parallel_tool_calls:false` 是「明确禁止」——与省略字段不是一回事。客户端没给的字段**不报**有损。
- **`n` 被丢弃时只会回一个候选**，本服务不在本地做 n 次扇出（那会把一次计费变成 n 次而客户端看不出来）。
- **schema 降级是两档**。目标支持 JSON 但不支持 schema 时降级为纯 JSON 并报 `response_format.schema`——保住「必须是 JSON」这条硬约束比整条丢掉更接近客户端意图。
- **Gemini 的 schema 走工具 schema 同一套方言归一**：方言外的关键字会让上游回 `400 Invalid JSON payload`；归一失败时退回纯 JSON。
- **Gemini 只给 `top_logprobs` 不给开关时本服务补上开关**：该协议的 `logprobs` 字段在 `responseLogprobs` 为假时不生效，不补等于把要求丢掉。
- **`logit_bias` 不做跨协议 token id 重映射**：各家分词器不同，同一个 id 指向不同的词，重映射会把偏置加到别的词上。

#### 调参的请求侧与响应侧分工

同一个参数在两侧各说一半，**两格互斥**，不会重复报：

| 字段 | 目标不支持时 | 目标支持时 |
|---|---|---|
| `n` | 请求侧报 `dropped n`（只会回一个候选） | 请求侧**不报**；上游真回多路时响应侧报 `dropped N extra response candidate(s)`，N 是实际丢掉的路数 |
| `logprobs` | 请求侧报 `dropped logprobs` | 请求侧报 `forwarded logprobs but the result is not returned`——参数照发给了上游、上游也会算，但按 token 的概率不跨协议承载，解码时丢掉 |

两条措辞刻意分开：**`dropped` 是「换个目标就有」，`forwarded ... not returned` 是「换谁都没有」**。混用会让客户端以为换个目标能拿到对数概率。

`n` 的那一格选「响应侧报实际路数」而不是请求侧提前警告，因为实际路数是能对账的数字——`usage.output_tokens` 是上游按**全部**候选算的，客户端为丢掉的那几路付了钱。

#### 响应侧明确不做的维度

以下都看起来像遗漏，其实是判断过的：

| 维度 | 不做的理由 |
|---|---|
| 响应 `logprobs` | Chat Completions 的按 token 数组与 Gemini 的 `topCandidates`/`chosenCandidates` 平行数组结构不同构，逐 token 对齐要求复原上游的分词边界，而本服务不带分词器（见 [6.6](#66-token-估算的两个方向)）。四个参考实现无一承载它。 |
| `system_fingerprint` | 它与 `seed` 配对，供客户端判断「后端配置变了所以同 seed 结果不同」。但本服务的响应来自调度层按策略选出的某个目标，同一 seed 两次请求可能落到**不同上游**——顺延上游的指纹会让客户端以为后端没变，**比不给更糟**。 |
| Gemini `groundingMetadata` / `citationMetadata` | 本服务从不声明搜索类工具，上游不会回这些结构。 |
| Gemini `safetyRatings` | 逐类别的评分在任何目标协议里都没有对应位置，转成文本附注会污染回答正文。整体被安全策略拦截这一情形已由 `promptFeedback.blockReason` 与 `finishReason` 覆盖。 |
| `text.format` / `truncation` 回显 | 回显的就是客户端自己发的值，它已经知道。 |

Gemini 的 `finishMessage`（上游随 `finishReason` 附的人类可读原因，比如具体触发了哪条安全策略）**会以有损说明的形式保留原文**（截断到 200 字节）。它不改 `stop_reason`——枚举值已由 `finishReason` 决定，两个来源打架只会让判定变得不可预测。

### 6.5 用量估算兜底

上游没回 usage 时（少数上游流末不发 usage），本服务按字符数估算填入，此时流水里 `usage_estimated: true`。

**两个维度各自独立判断**：`input_tokens` 与 `output_tokens` 哪个为 0 就兜哪个。上游报了非 0 值的维度**原样透传，绝不覆盖**——上游的数字是唯一权威，用估算盖掉它会让账目与上游对不上且看不出是谁改的。只有一个维度被兜底时 `usage_estimated` 同样为 `true`：它表达「这行数字里有估算成分」，不区分是哪一维。

兜底用**调度方向**的估算（见 [6.6](#66-token-估算的两个方向)）。`MSA_ESTIMATE_USAGE=false` 时两个维度都不兜，上游没报就留 0。

### 6.6 token 估算的两个方向

本服务不带分词器，token 数是按字符类别加权估算的。同一个估算有两个方向，因为用途对偏差方向的要求相反：

| 方向 | 用途 | 偏差要求 | CJK 权重 |
|---|---|---|---|
| 调度 | dispatch 的 `est_tokens`、用量兜底 | 宁可**低**估 | 每字符 0.6 |
| 公开 | `count_tokens` 的回答 | 宁可**高**估 | 每字符 1.25 |

- **调度方向宁可低估**：`est_tokens` 给策略脚本按上下文窗口筛候选，高估会让本装得下的请求被排掉所有目标，客户端拿到「无可用目标」而不是一个回答；用量兜底进的是配额累计，高估等于凭空吃掉用户的额度。
- **公开方向宁可高估**：客户端据 `count_tokens` 裁上下文，低估会让它裁完照样被上游以超长拒掉，而历史已经删了。
- 非 CJK 字符两个方向都按每 4 字符约 1 token，英文下没有上调空间。
- 媒体块两个方向都按每块 258 token 的下限计入。**两个方向只在字符权重上不同，算哪些块是一样的**。
- 不按厂商分表：`count_tokens` 要在选目标之前回答，那时还不知道会落到哪个上游。参考实现能分表是因为它在渠道上下文里。

### 6.7 出站连接层

本服务与上游之间的连接由一个专用客户端管理，**不使用** Go 标准库的默认客户端。

**连接复用**。标准库默认每个 host 只保留 2 条空闲连接（`http.DefaultMaxIdleConnsPerHost`），并发超过它的请求每次都要重新建连并完整 TLS 握手。本服务把这个上限抬到 32、全局上限 256，空闲 90s 后回收。三个值均可配（见[附录 A](#附录-a-环境变量)）。

**四层时限的分工**。这四个计时器各管一段，任一段缺失都会让另外三段管不到的故障变成无限挂起：

| 时限 | 覆盖区间 | 默认 | 触发后 |
|---|---|---|---|
| 响应头等待（`MSA_RESPONSE_HEADER_TIMEOUT`） | 请求发出 → 响应头到达 | 120s | 本次尝试失败，按 `upstream` 处理；提交前可换目标 |
| 首帧超时（`MSA_FIRST_TOKEN_TIMEOUT`） | 建流 → 第一个可解码帧 | 60s | `timeout`（504），提交前可换目标 |
| 空闲超时（`MSA_IDLE_TIMEOUT`） | 帧与帧之间 | 120s | `timeout`，已提交则在流内报错收尾 |
| 单次写 deadline（不可配） | 一次 `Write` 调用 | 30s | 写失败，本轮按客户端已离开收尾 |

要点：

- **响应头等待的默认值必须宽于首帧超时的默认值**。上游接受了连接却永不发响应头这类故障，首帧超时管不到——它的计时器要等建流之后才起。反过来，若把响应头等待设得比首帧超时还短，本该由首帧超时报出的慢上游会被错报成连接层问题，而两者的换目标语义不同。
- **不设整体请求超时**。Go 的 `http.Client.Timeout` 覆盖到读完整个响应体，而 SSE 流会跑几分钟，设了就会从中间掐断，且掐断点落在已提交之后，客户端收到的是残缺的流。
- **写 deadline 逐次推进，不是总时限**。语义是「单次写不得阻塞超过 30s」，读得正常的客户端永远不会触发。不推的后果是慢客户端（比如收了一半就不读的）能无限占住一条上游连接：`Write` 是同步调用、不在任何 `select` 里，空闲超时与客户端取消都观察不到它阻塞。
- **两个超时的负值表示显式不设限**，零值（即不配）取上表默认。零值不表示不设限：那会让忘配环境变量的部署静默回到无限等待。

### 6.8 上游限流的到期时刻

上游被限流时通常会告诉我们**什么时候能再来**。本服务把这句话解析出来，随结果上报交给调度层，由它精确冷却到那一刻，而不是按一个配置的默认时长猜。

**认哪些来源**：

| 来源 | 取值形态 |
|---|---|
| `Retry-After` 响应头 | 十进制秒（**允许小数**，实测有上游回 `1.5`）或 HTTP-date，两种都解 |
| `anthropic-ratelimit-unified-reset` | Unix 时间戳 |
| `anthropic-ratelimit-unified-5h-reset` / `-7d-reset` | 同上，分窗 |
| `x-ratelimit-reset-requests` / `-tokens` | 毫秒 epoch / 秒 epoch / 相对秒数三种形态都出现过 |
| `x-codex-primary-reset-after-seconds` / `x-codex-secondary-...` | **相对秒数**，不是时间戳 |
| Gemini 错误体 `error.details[]` 中 `@type` 为 `type.googleapis.com/google.rpc.RetryInfo` 的那一项 | `retryDelay`，Go duration 串，如 `"0.201506475s"` |

几点定死的语义：

- **量级消歧而非按头名分派。** 同一个头名在不同上游上出现过不同量级（同一个 `x-ratelimit-reset-*` 有的回 epoch、有的回相对秒数），按头名钉死会解错。数值 ≥ 1e11 当毫秒 epoch，≥ 1e9 当秒 epoch，其余当相对秒数——两个边界都远离真实的相对秒数（限流窗口不会有 31 年）与真实的秒级 epoch。
- **多个来源同时给了就取最早的那个。** 取最早只是多一次探测；取最晚会在那个长窗口其实没被拒时白锁数小时。`7d` 窗口真被拒时，那一次探测会带回 `7d` 的头，冷却随之推到正确的时刻。
- **可信性闸门**：不晚于当下、或晚于当下 24 小时的时刻都按坏数据丢弃。上游时钟不一定与本机同步，采信一个半年后的时刻会把可用目标锁到下个季度。`7d` 窗口的真实到期因此不会被采信——那一档靠启发式压制。
- **上游没说就是没有，绝不编造。** 编造一个到期时刻会把其实可用的目标锁住，比不知道更坏。
- **限流头不限于 429。** `503` 带 `Retry-After` 是 RFC 9110 的标准做法，而过载恰恰最需要按上游节奏退避。
- **本服务不在数据面睡等。** 到期时刻再近也只是换下一个目标：数据面睡等会占住入站连接与并发额度，等待是调度层的事。
- **Gemini 只认 `RetryInfo`**，不从错误文案里抠 `"per day"` 之类的说法。前者是 Google 的官方 RPC 结构、稳定；后者一改文案就静默失效，而失效方向是「又开始按默认值瞎猜」，运行时看不出来。`details[]` 里的 `@type` 做全串比对——数组里还有 `ErrorInfo`、`QuotaFailure` 等成员，宽松匹配会把别人的字段读成到期时刻。
- **该时刻落进请求流水**（`retry_after` 列），事后能回答「那次为什么换了目标」。上游没说时该列为 `NULL` 而非一个过去的时刻。

调度层拿到它之后如何改变冷却判定（明示优先、失败计数仍递增、只向后推进、不可信则回落启发式），见 model-surge-relay 的 `POST /v1/results` 一节。

### 6.9 死连接探测与连接层归因

连接池里可能存在**看着活、实际已经不通**的连接：NAT 表项超时、上游 LB 静默回收、网络中途中断之后，本地内核仍认为连接可用。HTTP/2 下更严重——一条 TCP 连接承载全部多路复用的流，它死掉会拖垮所有并发请求。

**主动探测**。出站 transport 配 `HTTP2.SendPingTimeout`（连接空闲多久发一个 PING）与 `HTTP2.PingTimeout`（PING 发出后多久没收到 PONG 判失联）：

| 环境变量 | 默认 | 含义 |
|---|---|---|
| `MSA_H2_SEND_PING_TIMEOUT` | `15s` | 空闲多久后发 PING |
| `MSA_H2_PING_TIMEOUT` | `15s` | 多久没收到 PONG 判失联 |

- **最坏检出耗时是两者之和**（刚探完就变死 → 等一个发送间隔 → 再等一个失联判定）。默认值之和 30s 必须显著小于 `MSA_RESPONSE_HEADER_TIMEOUT` 的 120s，否则被动超时先触发、主动探测等于没配。
- **任一配成负值表示显式关闭探测**，此时整个不配（只配一半是无意义的状态：光有发送间隔没有失联判定，PING 发出去永远等不到结论）。
- 探测**不改变协议协商**：是否走 HTTP/2 仍由 TLS 决定。
- HTTP/1.1 的正常关闭不需要额外处置——标准库检测到复用连接上的 EOF 会自动换连接重放。要挡的是**静默**半开连接。

**连接层单独归因**。请求发送失败时区分两类：

| 判为连接层（`transport`，502） | 判为其他 |
|---|---|
| socket 层读写/拨号失败（`*net.OpError`） | 客户端取消（归 `canceled`） |
| 连接被重置/拒绝/中止、写已关闭的管道 | 请求 context 超时 |
| 响应中途断掉（`io.ErrUnexpectedEOF`） | 裸 `io.EOF`（正常的流结束） |
| TLS 记录头非法（`*tls.RecordHeaderError`） | **证书校验失败**（换连接换目标都一样失败，问题在证书或信任库，归 `upstream`） |
| h2 判定连接失联 | 上游有响应（哪怕是 5xx，连上了就说明连接是好的） |

- `transport` 的结果上报**不计入目标的失败计数**，运行态零变更。上游可能完全健康，坏的是本服务池里那条连接；记成目标失败会让一条死连接把健康账号推向冷却。
- `transport` 可重试：换一个目标一定可以重来。
- **流已经开始之后（committed）读流失败不做连接层归因**，仍归 `upstream`。此时目标已锁定、客户端已收到部分内容，换目标会拼出两段回答——归因再准也无处可用。
- 与 `context_exceeded` 一样零变更，但**是独立的分类**：一个是「请求太大」、一个是「本服务的连接坏了」，合成一类运维就在流水里分不开这两种故障。

### 6.10 依赖饱和度与时延分段

两件事回答的是同一个问题：**故障发生了，故障在谁身上。**

#### 时延的三段读法

流水里三个时延字段构成一个可做减法的分解：

| 读法 | 含义 |
|---|---|
| `dispatch_ms` | 问调度层要目标花了多久 |
| `upstream_ms` | 发出请求到**上游响应头到达**花了多久 |
| `latency_ms - dispatch_ms - upstream_ms` | 本服务自身的处理 + 上游的生成时间 |

回答「慢在上游还是慢在我们」不需要额外字段，做这个减法就够。`first_token_ms` 与 `upstream_ms` 的差则是上游拿到请求之后到吐出第一个可解码帧之间的思考时间。

**两段都只切跨进程边界。** 编码、`Sanitize`、参数覆盖都是纯 CPU，在总耗时里占不到毫秒级，给它们各记一列只会让表变宽而没有任何一次排查会用到。

**两段在换目标重试时累加，不是覆盖。** 这与 `lossy` 的覆盖语义刻意相反：`lossy` 描述「最终发出去的那次编码丢了什么」，上一个目标的丢弃项描述的是一条没被采用的路径；而时延描述「客户端等了多久」，客户端确实等了全部尝试的时间。只记最后一次，「三次重试各 20 秒」会显示成一次 20 秒的请求，而那正是最需要被看见的那种慢。

**`upstream_ms` 的终点是响应头到达，不是首帧解码出来。** 首帧里含上游的思考时间，那是生成成本不是连接成本；而响应头这一刻正是 `MSA_RESPONSE_HEADER_TIMEOUT`（见 [6.7](#67-出站连接层)）约束的那一刻。两者对齐，`upstream_ms` 逼近那个阈值就知道该调哪个参数。

不变式：`dispatch_ms + upstream_ms <= latency_ms`。

#### 连接池饱和度

`/health` 的 `pool` 是**整个服务都慢、而每条流水看上去都正常**时唯一能看的地方——慢的那段在等一条数据库连接上，而那段不在任何一条请求的计时里。

- **现取，不采样。** `Stat()` 是读内存计数器、没有 IO，所以不起后台 goroutine 定时采样：那只会换来一个过时的数字，而调 `/health` 就是想知道此刻的饱和度。
- **`acquire_waiting` 是累计次数，不是排队长度。** pgxpool 没有暴露瞬时排队数。累计值反而更好用——它单调递增，两次取样做差就知道这段时间有没有人等过连接；瞬时值在轮询间隙里等过又等到了会完全看不见。
- **PG 不可用时 `pool` 整体缺省。** 包括「池已被关闭」这一支：那时 `Stat()` 仍会返回最后一刻的残留数字，报出去会让运维以为池还活着。
- `goroutines` 只作原始数字暴露，不设阈值告警：本服务没有告警设施。

#### 明确不做

- **不引入 Prometheus / OpenTelemetry。** 四个参考仓库（cc-switch、new-api、sub2api、kiro-gateway）**无一使用**，跳过它不是落后于这个同行群体。
- **不做 sub2api 的四段划分**（auth / routing / upstream / response，`migrations/033_ops_monitoring_vnext.sql:117-122`）：本服务的入站鉴权是**转发**给调度层做的，没有一段独立的 auth 耗时可计；response 段与生成时间在 SSE 下不可分。
- **不加 `error_owner` / `is_business_limited`**：本服务已有 `error_code` 与 `outcome` 两维足以定位，而 SLA 口径的计算面在调度层。
- **不存请求体 / 响应体**。sub2api 自己把错误表里的 `request_body JSONB` 删掉了（`136_remove_ops_retry_replay.sql:8-11`），代价太高。
- **不做 per-attempt 逐次快照**（每次尝试的绝对时刻、上游 URL、上游 request-id）：本轮两段是累计值；逐次明细要另建表。
- **不用 `context.WithValue` 传计时器**：两段的累加点都在能直接拿到流水记录的函数里，走 ctx 是把编译期可见的数据流变成运行期的类型断言。

---

### 6.11 转换四体捕获

每个请求都要经历四次 wire 形态变换：客户端请求体 → 出站 wire body → 上游原始字节 → 回客户端字节。流水里记的是**结论**——`sanitized` 说修了什么、`lossy` 说丢了什么、`error_code` 说哪类错，而这些结论都是转换层自己判断出来的。当那个判断本身错了的时候，流水里没有任何东西能看。

捕获保留这四段原始字节，用来回答「到底是哪一步坏的」。

#### 四体的语义

| 体 | 取自 | 说明 |
|---|---|---|
| `client_request` | 受理面读体、解压、去 BOM 之后 | 客户端实际发来的 JSON。解码就失败的请求**不开捕获**——它连出站协议都没选过，四体里只会有一体 |
| `upstream_request` | `paramover` 应用之后、写进 `http.Request` 之前 | 真正发给上游的那份 body，含已替换的 native model 与运维配的 defaults/overrides |
| `upstream_response` | 切帧之前，未经任何解码 | 上游回的原始字节。SSE 流旁挂在 body 上逐块累加；非 SSE 的整份响应与非 2xx 的错误体同样捕获 |
| `client_response` | 出站编码之后、`Write` 成功之后 | 与客户端实收一致。流式是 SSE 帧、非流式是一次性 JSON、失败时是错误信封 |

换目标重试时，`upstream_request` **覆盖**（诊断对象是最终发出去的那一次，累加会把一份没被采用的 body 拼在前面），`upstream_response` 与 `client_response` **累加**（同一个流的连续片段，两次尝试各自的响应都要留，否则看不出第一次是怎么坏的）。

#### 三态开关

`MSA_CAPTURE_MODE` 取 `off`（默认）、`errors`、`all`：

- `off`：不分配任何缓冲，`Begin` 恒返回空会话，四个切点全是空操作。
- `errors`：全程缓冲在内存里，请求收尾时按结果决定留还是丢。**建议的常开档**。
- `all`：成功与失败都留。

非法值**拒绝启动**而不是静默回落到 `off`：静默的后果是运维以为捕获开着，等出了故障才发现什么都没留，而那时故障已经过去了。

留还是丢的判据是流水的 `error_code` 是否非空，不是 `outcome`。两者在两处分歧：换目标重试成功的请求中途有过 `retrying` 但整体正常，不该留；而客户端取消的请求 `outcome` 记 `normal`（客户端自己走了不是目标的故障，不该累计它的失败计数）却带着 `error_code: canceled`，要留——「客户端为什么取消」往往正是要看上游当时发了什么才能回答的。

#### 凭据边界

**捕获只含 body，永不含任何请求头。** 上游凭据来自调度层、只写进 `http.Request`，让它进捕获等于把一个排查设施变成凭据泄露面。

因此也**不做 body 内的凭据扫描**：四体里不存在凭据这件事是由「不捕获头」这个结构保证的，再加一层正则脱敏只会给出一种虚假的安全感（它挡不住客户端自己把密钥写进 prompt，而那份内容本来就在客户端手里）。客户端转发给调度层的凭据同样只在头里，不在 body 里。

两个读取端点在管理面密钥之后，与流水查询同一把密钥。

#### 内存上限

| 上限 | 默认 | 超出后 |
|---|---|---|
| 单体字节数（`MSA_CAPTURE_MAX_BODY`） | 1 MiB | 保留**前段**，标 `truncated` 并记 `dropped` 字节数 |
| 保留条数（`MSA_CAPTURE_MAX_ENTRIES`） | 32 | 淘汰最旧的一条 |

截断保留前段而不是后段：请求体的诊断价值在头部的模型名、参数与工具声明上，响应流的头部则是 `message_start` 与首个内容块，而转换 bug 绝大多数在流的开头就已显形。只留尾部会把 `message_start` 挤掉，而那一帧常常正是问题所在。

#### 明确不做

- **不落盘**。只在进程内存里，重启即失。要长期留证据的场景应该在反代层抓包，而不是让数据面兼任存储。
- **不捕获请求头**，因此也不做任何脱敏（见上）。
- **不捕获中立表示（IR）与事件序列**。那是 Go 结构体，序列化它需要一套只为调试存在的编解码，而它的内容可由前后两体推出。
- **不做采样**。三态开关已经给出了「常开而不撑爆内存」的形态（`errors` 档），再加采样率只会让「为什么这一条没留下」多一个说不清的原因。
- **不做 base64**。这两个端点唯一的用途是人眼看哪一步坏了，base64 之后要先解一层才能看。
- **不参考 kiro-gateway 的实现**。它的三态取舍值得学，但它是一个单例、每次请求清空同一个共享目录，并发请求会互相擦掉证据。本服务每个请求独占一个会话。
- **调度层与配置中心零改动**。捕获完全是数据面自己的事。

### 6.12 逐次尝试轨迹（attempts_trail）

一次请求可能尝试多个目标（见 [6.2](#62-换目标重试与-tried_ids)）。流水行上的 `model_id`、`account`、`outcome`、`status_code`、`error_code`、`error_message`、`retry_after` 都只是**最后一次**尝试的值，`dispatch_ms` 与 `upstream_ms` 是全部尝试的**累计**值，`tried_ids` 只有被试过的模型 ID 而没有各自发生了什么。于是「第二个账号是 429 还是 500」「三次都慢还是只有第三次慢」这类问题在流水里查不到答案。

`attempts_trail` 按尝试顺序给出每一次的身份与结果，随单条详情（[7.3](#73-get-adminrequestsrequest_id单条详情)）返回。

**轨迹项字段**

| 字段 | 类型 | 省略条件 | 含义 |
|---|---|---|---|
| `n` | int | 不省略 | 尝试序号，从 1 起。**零值也出现**：从 1 起意味着 `0` 本身就是 bug 信号 |
| `model_id` | string | 空时省略 | 这次尝试的目标模型 ID |
| `account` | string | 空时省略 | 这次尝试的账号名 |
| `outbound_protocol` | string | 空时省略 | 这次尝试的出站协议 |
| `outcome` | string | 不省略 | 这次尝试的结局，取值见[附录 B](#附录-b-请求结局outcome语义)。中途的尝试是 `retrying` |
| `status_code` | int | `0` 时省略 | 这次尝试的上游 HTTP 状态码 |
| `dispatch_ms` | int | 不省略 | **本次**向调度层要目标的耗时。**零值也出现**：`0` 是「快到不足 1 毫秒」这个有意义的观测值 |
| `upstream_ms` | int | 不省略 | **本次**上游连接耗时。未连上上游的那次为 `0` |
| `error_code` | string | 成功时省略 | 这次尝试的错误码 |
| `error_message` | string | 成功时省略 | 上游对这次尝试的错误原文 |
| `retry_after` | string(RFC3339) | 上游未明示时省略 | 上游对这次尝试明示的最早可重试时刻 |

**不变式**：轨迹各项的 `dispatch_ms` 之和等于行上的 `dispatch_ms`，`upstream_ms` 同理。据此可以判断慢在哪一次，而不只是判断总共有多慢。

**调度层没给出目标的那次尝试**也会留一项，此时 `model_id`、`account`、`outbound_protocol` 三项**同时缺省**——那次尝试确实没有目标。把它伪装成一次目标失败，会让「哪个账号总失败」的统计算进一个不存在的账号。

**捕获的尝试边界**。捕获（[6.11](#611-转换四体捕获)）的 `upstream_response` 是累加的，多次尝试的上游字节首尾相接。自本版起两段之间插一行 SSE 注释形式的分隔标记 `: ---- attempt N ----`（仅第二次及之后）：SSE 注释行在语法上合法且被解析器忽略，把捕获物直接喂给 SSE 工具不会报错。

#### 明确不做

- **不建 per-attempt 表**。流水已是每请求一行，建表就是每请求 N 行。参考实现 sub2api 曾建过这样一张表（`033_ops_monitoring_vnext.sql` 的 `ops_retry_attempts`）、扩过一次（`038`），最终整表删掉（`136_remove_ops_retry_replay.sql`），理由原文是写入宽度、内存驻留与库体积。代价是行内 JSONB 列不便索引；收益是零新表、零新写入路径、随流水一起被保留期清理。
- **不记 `base_url`**。它的排查价值等于 `model_id` + `account` 的组合（同一账号恒定一个 base），而它是一条带路径的 URL，一些部署会把 key 放在 query 里。
- **不记请求头与请求体**。轨迹随流水进 PG，而凭据只在内存里活着。要看字节应开捕获。
- **不进列表端点**。`/admin/requests` 一页最多 200 条，每条再挂 N 项会让响应随重试次数膨胀。列表上的 `attempts` 计数是入口：它不为 1 时再点详情。
- **不做「重试链」字符串**。参考实现 new-api 把尝试过的渠道拍平成 `重试：A->B->C` 一行人读文本（`controller/relay.go`），能看出顺序但看不出各自的错误码与耗时，而那正是要查的东西。
- **不给单次尝试省掉轨迹**。只试了一次也留一项：让「查一条流水看它每次尝试」不必先分辨有没有重试过。

### 6.13 工具意图的保真

客户端对工具的声明里含着四层意图：有哪些工具、必须调哪一件、想让模型思考多久、哪些工具由上游自己执行。这四层被整形层削弱时故障都**不可见**——上游正常回一段文本，HTTP 200，客户端看不出自己的声明被改过。本节是这四层的处置规则。

#### 工具改名同步到 tool_choice 与历史

工具名含非法字符或超长时会被改写（见 [6.4](#64-跨协议能力差异) 之前的声明治理）。改写必须同步到另外两处：

| 同步点 | 不同步的后果 |
|---|---|
| `tool_choice` 的具名 | 出站整形随后发现它指向一个未声明的工具，降级成 `auto`。客户端的「必须调这件工具」变成「模型自己决定」，上游正常回一段文本 |
| 消息历史里的 `tool_use` 名字 | 上游看到一次对未声明工具的调用 |

两处同步后，出站整形**不再**报 `downgraded to auto`。若确实指向一件从未声明过的工具，仍然降级并报诊断——同步不等于「凡指不着就随便对上一件」。

#### 推理预算与 max_tokens 的冲突

推理预算是从输出上限里划出来的，因此必须**严格低于** `max_tokens`；相等意味着留给回答本身的 token 为零。客户端同时给出两个数字时它们可能冲突，而这是一个合法的入站形状，原样出站会拿到不可重试的 400（换目标也救不回来）。

| 关系 | 处置 |
|---|---|
| `budget < max_tokens` | 不动。碰它就是无端降低推理质量 |
| `budget == max_tokens` | 预算夹到 `max_tokens - 1`，报 `rewrote thinking.budget_tokens` |
| `budget > max_tokens` | 同上 |
| `max_tokens` 缺席 | 不动。无从比较，也无冲突可解 |
| 夹紧后低于协议下限（Anthropic 是 1024） | 落到「两个约束无解」那一支：整条关掉推理并报 `dropped thinking` |

**夹预算而不是抬 `max_tokens`**。参考实现 sub2api 走的是后者（`request_transformer.go` 的 `ensureMaxTokensGreaterThanBudget`，把上限抬到 `budget + padding`），本服务刻意不照搬：`max_tokens` 是客户端对成本与响应长度的约束，抬它是替客户端花钱，还会让「我只要 4096 个 token」回出更长的内容；预算只是「想多久」，调小它只降质量。

**夹紧排在关推理之前**。反过来先关后夹会漏掉最后一行那条路径。

#### 服务端工具

部分协议允许声明「由上游自己执行」的工具（Anthropic 的 `web_search_20250305`、`code_execution` 等，线上形态是工具对象带一个非 `custom` 的 `type` 且不带 `input_schema`）。

| 目标协议 | 行为 |
|---|---|
| Anthropic | `type` 原样写回，**不带** `input_schema`——参数形状由上游那一版工具自己定义，我方给出的任何 schema 都可能与它冲突 |
| Chat Completions / Responses / Gemini | 整条丢弃，报 `dropped tools[<名字>]`；同一请求里的函数工具不受牵连 |

**丢弃而不是降级成函数工具**。降级后上游会把它当成「等客户端回结果」的函数，而本服务永远不会回那个结果——对话就停在那里，且不报错。丢弃的后果是模型少一件工具可用，可见且有说明。

丢弃排在 `tool_choice` 校正**之前**：若 `tool_choice` 正指向被丢的那一件，校正会把它降级成 `auto`。

服务端工具也**跳过** schema 归一：走一遍只会给它塞上一个空对象 schema 发出去。

#### 被跳过的工具声明出说明

Responses 与 Chat Completions 的入站解码只认函数工具，其余 `type` 的声明本服务表达不了。跳过时留一条说明：

```
skipped tool "ws": unsupported type "web_search"
```

说明走**入站 `sanitized` 通道**而不是出站 `lossy`：这一步发生在解码期，与最终选了哪个目标无关。Chat Completions 的 `type` 省略等同 `function`，**不出**说明——正常形状出说明会让这个字段再也指不出真问题。

#### 明确不做

- **不给非 Anthropic 目标合成服务端工具**。上游不执行它，合成出来的是一件永不返回结果的工具。
- **不抬 `max_tokens` 解冲突**。理由见上。
- **不为服务端工具另立 IR 类型**。除 `type` 一处外，它与函数工具在本服务眼里的处理完全相同（都要参与 `tool_choice` 校正、都要出现在工具集合里），分型会让每个遍历工具的地方都变成两个分支。
- **不把跳过的声明降级成函数工具**。同上一条丢弃的理由。
- **不在出站侧重复报「跳过」**。同一件事报两次会让诊断字段里出现重复条目。
- **不做工具数量上限**。四家协议都没有本服务需要代为执行的硬上限，请求体字节预算（见 [6.4](#64-跨协议能力差异) 之后的说明）已经覆盖了「声明太多」这一形态。

### 6.14 响应侧语义维度的保真

响应方向也会丢语义，且比请求方向更难发现：请求畸形通常换来一个 400，而响应侧的语义丢失一律是 HTTP 200——客户端收到一份看起来完整、实则少了一维的响应。本节是四个这类维度的处置规则。

#### 停止序列的身份

`stop_reason` 说「是被一条停止序列打断的」，但没说是哪一条。用 `stop_sequences` 做分段解析的调用方需要那条序列的原文。

| 方向 | 行为 |
|---|---|
| Anthropic 入站 | 非流式响应的 `stop_sequence` 与流式 `message_delta` 的 `delta.stop_sequence` 都解出 |
| Anthropic 出站 | 两处都写回 |
| 另三个协议出站 | 不合成、不报有损 |

**互斥约束**：`stop_reason` 不是 `stop_sequence` 时这一维必须为空。回填一条未触发的序列会让按它切分输出的客户端切错位置，比拿不到更坏——因此采纳判据在解码与编码两侧各自执行，而不是只在解码时守一次。

#### 工具结果的失败态

工具执行失败的历史发回模型时，`is_error` 这一维决定模型会不会重试。Chat Completions 的 `tool` 消息只有 `role`/`tool_call_id`/`content` 三个键，Responses 的 `function_call_output` 只有 `call_id`/`output`，都没有可放标记的位置。

处置是**改写成内容前缀**而非丢弃：丢掉它模型会把失败当成功，那是跨轮语义被改坏且完全不可见；加前缀模型会看到一行本服务加的文字，可见且有诊断。措辞 `[tool error] `，方括号形态在工具输出里罕见。有损诊断的字段名是 `tool_result.is_error`，措辞用 `rewrote` 而非 `dropped`——读者对这两者的下一步动作不同：丢弃要考虑换目标，改写要考虑上游会怎么读。

前缀作为一个独立文本块前置，而不是把整段内容折成一个字符串：后者会碾平工具结果里的媒体块，而那与失败态无关。

`is_error` 为假时字节完全不变、不报诊断；Anthropic 与 Gemini 有原生表达，不加前缀、不报诊断。

#### 响应内的媒体

Gemini 的响应 part 可以带 `inlineData`/`fileData`。三个下游协议对响应内媒体的表达各不相同且都不通用，因此**不往下游转**这一决定不变，但必须报出来：客户端收到图文混排里只剩文字时，需要能从诊断里区分「图被丢了」与「模型没画」。说明带 media type（丢的是图还是音频决定客户端下一步怎么办），流式与非流式两条路径措辞同一出处。

非流式路径此前连这个分支都没有——媒体 part 落到 `switch` 外面被静默跳过，「丢了」这个事实不在代码里表达。

#### 拒答不得被降级成正常结束

Responses 用一个独立的 part 类型（`refusal`）表达拒答，而响应的 `status` 仍是 `completed`。于是「模型拒绝回答」与「模型答完了」在 `status` 上看不出差别，而 Anthropic 入站有原生的拒答终止原因——同一次拒答在不同入站协议上会得到不同的 `stop_reason`。

判定改为：输出里出现 refusal part 时 `stop_reason` 判 `content_filter`。优先级有两条硬性约束：

| 关系 | 谁胜 | 理由 |
|---|---|---|
| `incomplete_details` vs refusal | `incomplete_details` | 它是上游对「为什么没完成」的明确表态，比从 part 类型推断更可靠 |
| refusal vs `function_call` | refusal | 判成 `tool_use` 会让客户端去执行工具，而模型实际上拒绝了 |

流式路径在解码器上记一个布尔：`response.refusal.delta` 与 `response.content_part.added`（`part.type` 为 `refusal`）两处都置位——上游实现不一，有的只发 delta 不发 part 开启帧。收尾帧读它，且只在收尾帧未给出非正常结束原因时改判。

拒答的文字仍并入文本块，不另立 IR 块类型：客户端要看到拒答说了什么，而它与文本的唯一差别就是终止原因。

#### 明确不做

- **不给非 Anthropic 协议合成 `stop_sequence`**。它们的客户端不读这个键。
- **不把 refusal 做成一种 IR 块类型**。它与文本的唯一差别是终止原因，另立块类型会让四个出站各多一个分支。
- **不往下游转响应内媒体**。三个下游协议的表达各不相同且都不通用，只补说明。
- **不给 `is_error` 在 Chat Completions 里另找字段**。该协议的 `tool` 消息只有三个键，没有可用位置。

### 6.15 目标协议承载得了却没写出去的维度

[6.13](#613-工具意图的保真) 与 [6.14](#614-响应侧语义维度的保真) 讲的是「没有承载位置」。本节相反：位置一直都在，只是没往里写——有的是能力位声明错了，有的是线上结构缺字段，有的是两个字段间的换算没做。这类缺口比缺位更隐蔽，因为诊断会给出一条**事实错误**的说明，排查的人照着它去找一个不存在的原因。

#### 相邻同角色消息的合并

Anthropic 硬性要求 user/assistant 交替，非交替历史换来不可重试的 400（换目标也救不回来）。而相邻两条 user 在 Chat Completions 里完全合法，Agent 框架在一轮里连发两条很常见。

治理放在 IR 的 `Sanitize` 里而非 Anthropic 出站：Gemini 的 `contents` 同样按轮次配对，四个出站都受益于一份交替的历史。合并**只拼接**，不去重也不把块并成一个——并成一个需要决定用什么分隔符，而那个决定会改变模型看到的内容。

**顺序**：合并排在「丢弃空消息」之后。丢掉中间一条空消息会让原本被隔开的两条同角色消息变成相邻，反过来排会漏掉这一形态。

合并会报 `merged N adjacent same-role message(s)`，条数如实——排查的人需要知道「我发了 5 条、上游看到 3 条」不是丢消息。

#### Gemini 的 seed 与两个惩罚项

Gemini 的 `generationConfig` 有 `seed`、`presencePenalty`、`frequencyPenalty`（键名是驼峰，语义与 OpenAI 同）。此前能力位声明为假且线上结构缺字段，于是这三维被丢弃并报出一条「本协议没有该参数」的说明——那句话是错的。

现在三者照原样写出，零值（客户端未给）时键缺席。仍然确实没有对应字段的是 `logit_bias`、`service_tier`、`parallel_tool_calls` 与三个 Responses 专有项。

#### Responses 的推理签名索要

本服务对 Responses 恒发 `store:false`（无状态转发，既有定案）。该模式下上游**只在 `include` 里被明确点名时**才回 `reasoning.encrypted_content`，而签名是推理跨轮接续的唯一载体——摘要不是签名，它不能回传。

因此请求推理时（且仅在请求推理时）向 `include` 追加 `reasoning.encrypted_content`：

| 情形 | 行为 |
|---|---|
| 请求推理、客户端未给 `include` | 追加该项 |
| 请求推理、客户端给了其他项 | 保留原有项并追加 |
| 请求推理、客户端已给该项 | 不重复添加（重复项可能被上游拒收） |
| 不请求推理 | 不追加（为不存在的过程索要签名是自相矛盾的请求） |

追加**不报有损**：有损说明描述的是「你给的东西我送不到」，而这里是「为达成你的意图我多要了一样东西」。

#### 只给 logprobs 时补出 top_logprobs

Responses 没有独立的 `logprobs` 开关，`top_logprobs` 兼任开关与档位。客户端给 `logprobs:true` 表达的是「我要对数概率」，丢掉它等于让一个本协议满足得了的请求落空。

现在在 `top_logprobs` 缺席且 `logprobs` 为真时补 `1`——客户端说了要但没说要几个，取最小值：多取是花上游的算力与响应体，而它没要求。`logprobs` 为假不补，客户端已给 `top_logprobs` 时原样用它。

#### 图片的 detail 层级

`detail` 决定识别精度与计费档位，不是内容。Chat Completions 的 `image_url.detail` 与 Responses 的 `input_image.detail` 同名同义，双向透传；Anthropic 与 Gemini 没有这一维，丢弃并报出带计费后果的说明（笼统的 dropped 读不出「账单会变」）。

客户端没给时**不合成**。这与算价网关的做法（显式兜底 `high`）刻意不同：本服务是转发层，上游的默认才是权威，替它选一个会把「按上游默认」变成「按我们猜的」。

#### 明确不做

- **不给 Gemini 设 `safetySettings`**。没有入站协议有等价字段，客户端没表达过任何东西；替它选阈值是政策决定而非保真修复。
- **不为 `logit_bias` 做跨模型 token 重映射**。词表随模型而变，没有正确答案。
- **不把 `detail` 翻译成媒体降级文字**。它是计费与精度的开关，不是内容。
- **不改 `store:false`**。无状态转发是既有定案。
- **不在 `Sanitize` 里做角色交替之外的结构改写**。「首条必须是 user」那件事只有 Anthropic 需要，已在其出站按需处理。

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
      "dispatch_ms": 12,
      "upstream_ms": 795,
      "error_code": "not_found",
      "error_message": "upstream returned 404"
    }
  ],
  "next_cursor": "WyIyMDI2LTA5LTE2VDE1OjMwOjA1WiIsInJlcV9mY2I3ODM5YWNjNmNjYjg4MTA4MjBkMzUiXQ"
}
```

### 7.3 GET /admin/requests/{request_id}（单条详情）

**使用场景**：从流水列表点进单条，查看完整的换目标链路（`tried_ids`）、逐次尝试各自发生了什么（`attempts_trail`）与最终错误。

**请求**：`GET /admin/requests/{request_id}`

**路径参数**

| 参数 | 类型 | 必填 | 约束 / 允许值 | 含义 |
|---|---|---|---|---|
| `request_id` | string | 是 | 非空 | 请求 ID（流水中 `request_id` 字段），需 URL 编码（如 `/` 编成 `%2F`） |

**响应** `200`：[RequestSummary](#32-requestsummary) 的全部字段，外加一项 `attempts_trail`（单对象，不是数组）。

**详情独有字段**

| 字段 | 类型 | 省略条件 | 含义 |
|---|---|---|---|
| `attempts_trail` | array&lt;object&gt; | 为空时省略 | 逐次尝试的轨迹，按尝试顺序。逐项字段与不变式见 [6.12](#612-逐次尝试轨迹attempts_trail)。**列表端点刻意不带这一列** |

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
  "first_token_ms": 230,
  "dispatch_ms": 9,
  "upstream_ms": 115,
  "attempts_trail": [
    {
      "n": 1,
      "model_id": "kimi-k2-turbo",
      "account": "kimi-1",
      "outbound_protocol": "anthropic",
      "outcome": "normal",
      "status_code": 200,
      "dispatch_ms": 9,
      "upstream_ms": 115
    }
  ]
}
```

换过目标的那条形如：

```json
{
  "request_id": "req_3f1c08b54a2e77d9",
  "outcome": "normal",
  "status_code": 200,
  "attempts": 3,
  "tried_ids": ["kimi-k2-turbo", "doubao-seed"],
  "dispatch_ms": 24,
  "upstream_ms": 903,
  "attempts_trail": [
    {
      "n": 1,
      "model_id": "kimi-k2-turbo",
      "account": "kimi-1",
      "outbound_protocol": "anthropic",
      "outcome": "retrying",
      "status_code": 429,
      "dispatch_ms": 8,
      "upstream_ms": 96,
      "error_code": "rate_limit",
      "error_message": "rate limit exceeded",
      "retry_after": "2026-09-19T03:20:00Z"
    },
    {
      "n": 2,
      "model_id": "doubao-seed",
      "account": "ark-2",
      "outbound_protocol": "chat_completions",
      "outcome": "retrying",
      "status_code": 500,
      "dispatch_ms": 7,
      "upstream_ms": 121,
      "error_code": "upstream",
      "error_message": "internal error"
    },
    {
      "n": 3,
      "model_id": "kimi-k2-turbo",
      "account": "kimi-2",
      "outbound_protocol": "anthropic",
      "outcome": "normal",
      "status_code": 200,
      "dispatch_ms": 9,
      "upstream_ms": 686
    }
  ]
}
```

三项的 `dispatch_ms` 之和 `8 + 7 + 9 = 24` 等于行上的 `dispatch_ms`，`upstream_ms` 同理 `96 + 121 + 686 = 903`。

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
      "dispatch_ms": 3,
      "upstream_ms": 4,
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

### 7.9 GET /admin/captures（捕获列表）

**使用场景**：先看有哪些捕获、四体各有多少字节，再挑出可疑的那条取全文。列表**不带字节**：一条捕获可达数 MB，列 32 条就是上百 MB 的响应。

**请求**：`GET /admin/captures`

**响应** `200`：

| 字段 | 类型 | 允许值 | 含义 |
|---|---|---|---|
| `mode` | string | `off` / `errors` / `all` | 当前开关。列表为空时据此区分「捕获关着」与「开着但还没有符合条件的请求」 |
| `items` | array | — | 捕获摘要，**最新在前** |
| `items[].request_id` | string | — | 与响应头 `X-Request-Id`、流水的 `request_id` 同一个值 |
| `items[].at` | string | RFC3339 | 请求开始时刻 |
| `items[].sizes` | object | — | 四体各自已捕获的字节数，键为 `client_request`、`upstream_request`、`upstream_response`、`client_response`。某一体缺失时其值为 `0` |

捕获未装配或开关为 `off` 时回 `mode: "off"` 与空数组，不是错误。

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |

**示例**

```bash
curl -s "$BASE/admin/captures" -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{
  "mode": "errors",
  "items": [
    {
      "request_id": "0f3a9c1b7d5e4821",
      "at": "2026-09-19T10:02:17.482Z",
      "sizes": {
        "client_request": 412,
        "upstream_request": 486,
        "upstream_response": 0,
        "client_response": 137
      }
    }
  ]
}
```

上面这条的 `upstream_response` 是 `0`：上游一个字节都没回（连接层失败），而 `upstream_request` 有 486 字节——发出去的是什么可以看，回来的什么都没有。这正是四体分开记的用处。

---

### 7.10 GET /admin/captures/{request_id}（四体全文）

**使用场景**：拿到一条可疑请求的四段原始字节，逐段比对定位是哪一次转换坏的。

**请求**：`GET /admin/captures/{request_id}`

**响应** `200`：

| 字段 | 类型 | 含义 |
|---|---|---|
| `request_id` | string | 请求标识 |
| `at` | string | 请求开始时刻（RFC3339） |
| `client_request` | CaptureBody | 客户端发来的请求体 |
| `upstream_request` | CaptureBody | 发给上游的 wire body |
| `upstream_response` | CaptureBody | 上游回的原始字节 |
| `client_response` | CaptureBody | 回给客户端的字节 |

CaptureBody 的三个字段：

| 字段 | 类型 | 含义 |
|---|---|---|
| `body` | string | 原始 wire 字节，按 UTF-8 当字符串交出，**不做 base64** |
| `truncated` | bool | 是否超出单体上限被截断。为真时保留的是**前段** |
| `dropped` | int | 被截断掉的字节数，让读的人知道自己少看了多少 |

**绝不含任何请求头**，见 [6.11](#611-转换四体捕获) 的凭据边界。

**错误**

| HTTP | code | 触发条件 |
|---|---|---|
| `401` | `unauthorized` | 密钥缺失或错误 |
| `404` | `not_found` | 没有这条捕获：从未捕获、已被条数上限淘汰、或开关为 `off`。回 404 而不是空对象——空对象会让读的人以为那次请求四体全空 |

**示例**

```bash
curl -s "$BASE/admin/captures/0f3a9c1b7d5e4821" \
  -H "Authorization: Bearer $MSA_ADMIN_KEY"
```

```json
{
  "request_id": "0f3a9c1b7d5e4821",
  "at": "2026-09-19T10:02:17.482Z",
  "client_request": {
    "body": "{\"model\":\"demo-pool\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}",
    "truncated": false,
    "dropped": 0
  },
  "upstream_request": {
    "body": "{\"model\":\"claude-sonnet-4-5\",\"max_tokens\":64,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}]}",
    "truncated": false,
    "dropped": 0
  },
  "upstream_response": {
    "body": "",
    "truncated": false,
    "dropped": 0
  },
  "client_response": {
    "body": "{\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream connection failed: dial tcp: connection refused\"}}",
    "truncated": false,
    "dropped": 0
  }
}
```

对比这四体即可定位：`upstream_request` 里 `model` 已经是 native 名字、参数也都在，说明转换本身没问题；`upstream_response` 为空、`client_response` 是连接层错误，问题在网络或上游可达性，不在编解码。

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
| 2.13 | 2026-09-19 | 工具意图的保真首次成文（[6.13](#613-工具意图的保真)）：四条修复，全在转换层内，无新增字段、无新增端点、无库变更。**行为变更一**：工具名被改写（非法字符/超长）时，改写此前只同步到消息历史里的 `tool_use`，**不同步** `tool_choice` 的具名——于是出站整形随后发现它指向一个未声明的工具并降级成 `auto`，客户端的「必须调这件工具」静默变成「模型自己决定」，上游正常回一段文本。参考实现 sub2api 在改名时同步三处（`gateway_tool_rewrite.go` 的 `tool_choice.name` 重写），本服务此前只做了两处。**行为变更二**：客户端同时给出 `thinking.budget_tokens` 与 `max_tokens` 且预算不低于上限时（合法的入站形状），此前原样出站，拿到不可重试的 400——换目标也救不回来。现在把预算夹到 `max_tokens - 1`；夹后低于协议下限则落到既有的「关掉推理」那一支，因此夹紧必须排在关推理之前。**刻意不照搬 sub2api**：它抬 `max_tokens`（`request_transformer.go` 的 `ensureMaxTokensGreaterThanBudget`，抬到 `budget + padding`），而 `max_tokens` 是客户端对成本与响应长度的约束，抬它是替客户端花钱，还会让「我只要 4096 个 token」回出更长的内容；预算只是「想多久」，调小它只降质量。**行为变更三**：Anthropic 的服务端工具（`web_search_20250305` 等，带非 `custom` 的 `type`）此前在入站解码时 `type` 被整个丢掉，于是它被当成普通函数工具发给任意目标——上游会等一个永远不来的工具结果，对话停住且不报错。现在 IR 的 `Tool` 带 `ServerType`（字段而非新类型：除 `type` 一处外它与函数工具的处理完全相同），新增能力位 `ServerTools`（仅 Anthropic 为真），承载不了的目标整条丢弃并报 `dropped tools[<名字>]`，丢弃排在 `tool_choice` 校正之前，且服务端工具跳过 schema 归一。**行为变更四**：Responses 与 Chat Completions 的入站解码遇到非函数工具此前静默 `continue`，客户端从响应里分不出「自己的声明被丢了」还是「模型不愿意调」。现在留一条 `skipped tool "X": unsupported type "Y"`，走**入站 `sanitized` 通道**（发生在解码期，与选了哪个目标无关）；Chat Completions 的 `type` 省略等同 `function`，不出说明。为此 IR 的 `Request` 增设内部字段 `DecodeNotes`（不上线、由 `Sanitize` 取走并清空，避免同一条说明被上报两次），并修掉 `Sanitize` 在空消息列表时的早返回——「只声明了工具、还没说话」的第一轮请求此前会丢掉解码说明。**明确不做**：不给非 Anthropic 目标合成服务端工具；不抬 `max_tokens`；不为服务端工具另立 IR 类型；不把跳过的声明降级成函数工具；不在出站侧重复报「跳过」；不做工具数量上限（四家协议无需本服务代为执行的硬上限，请求体字节预算已覆盖「声明太多」）；调度层与配置中心零改动 |
| 2.14 | 2026-09-19 | 响应侧语义维度的保真首次成文（[6.14](#614-响应侧语义维度的保真)）：四条修复，全在转换层与中立表示内，无新增端点、无库变更。四者同型——上游给出了一个语义维度，而中立表示或出站编码根本没有承载它的位置，于是它在转换途中消失，客户端收到一份 HTTP 200 却少一维的响应。**行为变更一**：`stop_reason` 为 `stop_sequence` 时，是哪一条序列触发的此前完全丢失（中立表示与 Anthropic 的线上结构都没有这个位置）。现在响应与流式收尾事件各增一维，Anthropic 双向读写；互斥约束（`stop_reason` 不匹配时必须为空）在解码与编码两侧各自执行——回填一条未触发的序列会让按它分段的客户端切错位置，比拿不到更坏。另三个协议不合成、**不报有损**：请求侧丢的是客户端给过的东西（要报），响应侧少的是客户端读不到的键（那是协议差异）。**行为变更二**：工具结果的 `is_error` 此前只被 Anthropic 与 Gemini 出站读取，Chat Completions 与 Responses 完全不读也不报——模型把一次失败的工具调用当成功，既不重试也不致歉，跨轮语义被改坏且完全不可见。现在改写成内容前缀 `[tool error] ` 并报 `rewrote tool_result.is_error`；措辞用 `rewrote` 而非 `dropped`，因为读者的下一步动作不同。前缀作为独立文本块前置而非把整段折成字符串——后者会碾平工具结果里的媒体块，而那与失败态无关。**行为变更三**：Gemini 响应里的 `inlineData`/`fileData` 此前流式路径静默跳过、非流式路径连分支都没有。现在两处都报带 media type 的说明，措辞同一出处；「不往下游转」这一决定不变。**行为变更四**：Responses 的 `refusal` part 此前被并入文本块而终止原因落到 `end_turn`，于是「模型拒答」与「模型答完了」对客户端无差别，而同一次拒答经 Anthropic 入站会得到 `refusal`——四协议间不对称。现在判 `content_filter`，优先级为 `incomplete_details` > refusal > `function_call`（判成 `tool_use` 会让客户端去执行工具，而模型实际上拒绝了）；流式在 `refusal.delta` 与 `content_part.added` 两处都记标记（上游实现不一，有的只发 delta）。拒答文字仍并入文本块。**明确不做**：不给非 Anthropic 协议合成 `stop_sequence`；不把 refusal 另立 IR 块类型；不往下游转响应内媒体；不给 `is_error` 在 Chat Completions 里另找字段；不改 Gemini 的 `functionResponse` 排序（经查「工具结果先于同条消息正文」是正确时序，不是缺陷）；调度层与配置中心零改动 |
| 2.15 | 2026-09-19 | 目标协议承载得了却没写出去的维度首次成文（[6.15](#615-目标协议承载得了却没写出去的维度)）：五条修复，全在转换层与中立表示内，无新增端点、无库变更。与前两轮相反——位置一直都在，只是没往里写，于是诊断会给出一条**事实错误**的说明，排查的人照着它去找一个不存在的原因。**行为变更一**：相邻同角色消息此前一对一写出，而 Anthropic 硬性要求 user/assistant 交替，非交替历史换来不可重试的 400（换目标也救不回来），且相邻两条 user 在 Chat Completions 里完全合法。现在在 IR 的 `Sanitize` 里合并并报 `merged N adjacent same-role message(s)`；只拼接不并块（并块需要决定分隔符，那会改变模型看到的内容）；顺序排在「丢弃空消息」之后——丢掉中间一条空消息会**制造**新的同角色相邻。参考实现 sub2api 在配对治理前后各跑一次合并。**行为变更二**：Gemini 的 `seed`、`presencePenalty`、`frequencyPenalty` 此前能力位为假且线上结构缺字段，三维被丢弃并报出一条「本协议没有该参数」的假说明。现在三者照原样写出（键名驼峰，蛇形会被上游静默忽略）；仍确实没有的是 `logit_bias`、`service_tier`、`parallel_tool_calls`。**行为变更三**：本服务对 Responses 恒发 `store:false`，该模式下上游只在 `include` 点名时才回 `reasoning.encrypted_content`，而此前 `include` 只是客户端值的透传——签名槽的代码都在却永远收不到值，下一轮推理接不上且无诊断。现在请求推理时追加该项（追加非替换、已有不重复、不请求推理不追加、不报有损）。**行为变更四**：Responses 无独立 `logprobs` 开关，客户端只给开关时此前什么也拿不到，而诊断报的是「已转发但结果不回」——那句话暗示字段送到了。现在补 `top_logprobs:1`（取最小值，客户端没说要几个）。**行为变更五**：图片的 `detail` 此前在所有路径上被丢弃，`detail:"low"` 的请求按上游默认计费，图多的负载上是实打实的成本倍数。现在 Chat Completions 与 Responses 双向透传，另两协议丢弃并报带计费后果的说明；客户端未给时**不合成**——本服务是转发层，上游的默认才是权威。**明确不做**：不给 Gemini 设 `safetySettings`（客户端没表达过任何东西，替它选阈值是政策决定）；不为 `logit_bias` 做跨模型重映射；不把 detail 翻成降级文字；不改 `store:false`；不在 `Sanitize` 里做交替之外的结构改写。**顺带更正两处陈旧测试夹具**：两份被标为「健康请求」的历史里 `user(tool_result)` 紧跟 `user(text)`，本身就是 Anthropic 会拒的非交替形态，已补 assistant 隔开 |
| 2.12 | 2026-09-19 | 逐次尝试轨迹首次成文（[6.12](#612-逐次尝试轨迹attempts_trail)）：流水新增行内 JSONB 列 `attempts_trail`（老库自动补列），随单条详情（[7.3](#73-get-adminrequestsrequest_id单条详情)）返回；捕获的 `upstream_response` 在第二次及之后的尝试前插入分隔标记 `: ---- attempt N ----`。**行为变更**：此前一次请求尝试了多个目标时，流水上的 `model_id`/`account`/`outcome`/`status_code`/`error_code`/`error_message`/`retry_after` 全都只是**最后一次**的值，`dispatch_ms`/`upstream_ms` 是累计值，`tried_ids` 只有模型 ID 而没有各自的结果——「第二个账号是 429 还是 500」「三次都慢还是只有第三次慢」在流水里无从得知，而上一版加入的捕获又把多次尝试的上游字节无边界拼在一起。**形态选择**：行内一列而不是 per-attempt 表，依据是参考实现 sub2api 曾建过 `ops_retry_attempts` 表（`033_ops_monitoring_vnext.sql`）、扩过一次（`038`）、最终整表删掉（`136_remove_ops_retry_replay.sql`），理由原文是写入宽度、内存驻留与库体积；new-api 从未建表，只把尝试过的渠道拍平成 `重试：A->B->C` 一行人读文本（`controller/relay.go`），看不出各自的错误码与耗时。**不变式**：轨迹各项耗时之和恒等于行上的累计值，因此「本次值」与「累计值」两个计时器混用会被立刻发现。**凭据边界**：轨迹随流水进 PG，因此只含标识与结果标量，绝不含请求头、`base_url`（其排查价值等于 `model_id`+`account`，而它是带路径的 URL、可能把 key 放在 query 里）或请求体。**明确不做**：不建 per-attempt 表；不进列表端点（一页 200 条会随重试次数膨胀，`attempts` 计数是入口）；不做「重试链」人读字符串；不给单次尝试省掉轨迹；调度层与配置中心零改动 |
| 2.11 | 2026-09-19 | 转换四体捕获首次成文（[6.11](#611-转换四体捕获)）：新增 `MSA_CAPTURE_MODE` 三态开关（`off`/`errors`/`all`，默认 `off`，非法值拒绝启动）与两个上限变量，新增两个管理面端点（[7.9](#79-get-admincaptures捕获列表)、[7.10](#710-get-admincapturesrequest_id四体全文)）。**行为变更**：此前本服务**没有任何**调试捕获设施——流水只记转换层自己判断出的结论（`sanitized`/`lossy`/`error_code`），当那个判断本身错了时没有任何东西可看。上一次定位 kiro 的 `toolUse` 帧碎裂 bug 就是靠手写一段临时 tee 抓真实上游字节才找到根因，那段代码用完即弃、下一次还得重写。四体的取舍：`upstream_request` 换目标重试时覆盖（诊断对象是最终发出去的那一次），`upstream_response` 与 `client_response` 累加（同一个流的连续片段）；留/丢判据用 `error_code != ""` 而非 `outcome`，两者在「重试后成功」（不留）与「客户端取消」（要留，`outcome` 是 `normal` 但 `error_code` 是 `canceled`）两处分歧。**凭据边界**：捕获只含 body、永不含任何请求头，因此也刻意不做 body 内的正则脱敏——不存在凭据这件事由结构保证，再加一层只会给出虚假的安全感。**明确不做**：不落盘（要长期留证据应在反代层抓包）；不捕获 IR 与事件序列（可由前后两体推出）；不做采样（`errors` 档已是「常开而不撑爆内存」的形态）；不做 base64（唯一用途是人眼直接看）；不照搬 kiro-gateway 的 `debug_logger.py` 实现（它是单例、每请求 `shutil.rmtree` 同一个共享目录，并发请求互相擦掉证据）；调度层与配置中心零改动 |
| 2.10 | 2026-09-19 | 时延分段归因与依赖饱和度首次成文（[6.10](#610-依赖饱和度与时延分段)）：流水新增 `dispatch_ms`、`upstream_ms` 两列（累计值，含全部重试），`/health` 与 `/admin/health` 新增 `pool`（五个数）与 `goroutines`。**行为变更**：此前 `latency_ms` 是一个不可拆的总数，一个 30 秒的请求分不清是调度层要目标要了很久、上游压着响应头不发、还是生成本来就长；`/health` 只对 PG 做 `Ping` 报 `ok`/`down`，连接池被占满时每个请求都慢而**每一条流水看上去都正常**——慢的那段在等连接上，那段不在任何一条请求的计时里。`upstream_ms` 的终点选在响应头到达而不是首帧，与 `MSA_RESPONSE_HEADER_TIMEOUT` 对齐；两段在重试时累加而非覆盖（与 `lossy` 的覆盖语义刻意相反）。**明确不做**：不引入 Prometheus/OpenTelemetry（四个参考仓库无一使用）；不做 sub2api 的 auth/routing/upstream/response 四段划分（本服务的入站鉴权是转发给调度层做的，没有独立的 auth 段；response 段与生成时间在 SSE 下不可分）；不加 `error_owner`/`is_business_limited`（SLA 口径的计算面在调度层）；不存请求体/响应体（sub2api 自己已把错误表里的 `request_body` 删掉）；不做 per-attempt 逐次快照（需另建表）；不起后台 goroutine 采样池指标（换来的是过时数字）；不给 `goroutines` 设阈值告警（本服务没有告警设施） |
| 2.9 | 2026-09-19 | 死连接探测与连接层归因首次成文（[6.9](#69-死连接探测与连接层归因)）：出站 transport 配 HTTP/2 PING 健康检查（`MSA_H2_SEND_PING_TIMEOUT`、`MSA_H2_PING_TIMEOUT`，默认各 15s），新增 `transport` 错误分类（502）与同名结果上报类别，该类别**不计入目标的失败计数**、调度层运行态零变更。**行为变更**：此前所有 `Do` 失败一律归 `upstream`，经 `retrying` 累计到目标的失败计数上——一条静默半开的连接会把一个完全健康的账号推向冷却；且没有主动探测，撞上死连接的请求只能等 120s 的响应头超时，那对一次尝试是整个重试预算（本机探针实测：不配 PING 时挂到 20s ctx 超时都不失败，配了 4s 内明确失败）。**明确不做**：HTTP/1.1 正常关闭不加重放（标准库已自动换连接重放，实测确认）；不自定义 `DialContext` 设 `KeepAlive`（`DefaultTransport` 的 Dialer 已带 30s，重写还会丢掉标准库后续的默认调整）；不做 per-origin 分片 transport（PING 是直接摘掉死连接，分片只缩小爆炸半径）；连接层失败不在同一目标上就地重试（换目标已能恢复，真正的收益是不记这个目标的失败）；committed 之后读流失败仍归 `upstream`（客户端已收到部分内容，换目标会拼出两段回答） |
| 2.8 | 2026-09-19 | 上游限流到期时刻首次成文（[6.8](#68-上游限流的到期时刻)）：从六类限流响应头与 Gemini 的 `google.rpc.RetryInfo` 解出「最早可以再来」的绝对时刻，随结果上报交给调度层精确冷却，并落进请求流水的 `retry_after` 列。**行为变更**：此前 `resp.Header` 在错误路径上被整体丢弃（全仓唯一读过上游响应头的地方是判 SSE 的 `Content-Type`），限流与普通上游错、超时同为 `retrying` 一档，调度层只能按失败计数累积后冷却一个固定时长——上游说「5 小时后再来」时我们一分钟后就又去撞，在整个限流窗口里反复空转，而每次空转都是一次真实的失败上报。`DecodeError` 签名因此从 `(status, body)` 改为 `(status, header, body)`（改签名而非加可选接口：限流头是 HTTP 层的，四个协议全都可能收到，漏一个就是缺口）。**明确不做**：不在数据面为同一目标睡等退避（数据面睡等会占住入站连接）；不做账号级或 (账号,模型) 级限流（账号身份在 upstream 侧，数据面看不到）；不做 `x-ratelimit-remaining-*` 的预测性避让（需要跨请求窗口状态，那是调度层的职责）；不从错误文案里抠 `"try again in 1.5s"` 这类说法（文案一改就静默失效，而失效方向是又开始瞎猜） |
| 2.7 | 2026-09-19 | 响应侧调参保真：`service_tier` 首次原样回显（Chat Completions 顶层、Responses 的 `response` 对象；`created` 与 `completed` 两帧任一带上都认），Anthropic 出站无此位时报 `dropped service_tier from the response`；上游回多路候选而中立表示只装得下一路时报出**实际丢弃路数**（`dropped N extra response candidate(s)`，一个流恒一条），该数字可与 `usage.output_tokens` 对账；Gemini 的 `finishMessage` 原文作为说明保留（截断 200 字节，**不改写 `stop_reason`**，枚举仍只由 `finishReason` 决定）。新增第三类说明措辞 `forwarded X but the result is not returned`——与 `dropped`（换个目标就有）、`filled in`（本服务补的值）区分开，它表示换谁都拿不到。`n` 与 `logprobs` 的请求侧/响应侧分工成文（[6.4](#64-跨协议能力差异)「调参的请求侧与响应侧分工」）。**绝不拿请求里的 `service_tier` 兜底**：点 `flex` 拿到 `default` 是被降档，兜底会把降档伪装成按要求执行，而这一维决定计费。**明确不做**（五项理由成文，见 [6.4](#64-跨协议能力差异)「响应侧明确不做的维度」）：响应 `logprobs`、`system_fingerprint`、Gemini 的 grounding/citation、`safetyRatings`、`text.format` 与 `truncation` 回显 |
| 2.6 | 2026-09-19 | 调参字段保真：13 个采样/候选/输出格式字段（penalties、`seed`、`n`、logprobs、`logit_bias`、`service_tier`、`parallel_tool_calls`、`response_format`、`verbosity`、`include`、`truncation`、客户端 `metadata`）首次进入中立表示，承载矩阵成文（[6.4](#64-跨协议能力差异)「调参字段的承载矩阵」）；Chat Completions 与 Responses 请求字段表补齐这些字段。**行为变更**：这些字段此前在解码阶段就被丢掉，**连同协议往返也丢**（本服务无透传快路径，`chat_completions → chat_completions` 一样经中立表示重建）；`max_tokens` 缺失时 Anthropic 出站的 4096 兜底现在会报有损 `filled in max_tokens`（此前无痕）。**明确不做**：不因目标不支持某字段而拒绝请求；不对小 `max_tokens` 设下限抬高；不做 `logit_bias` 的跨协议 token id 重映射；不在本地为 `n` 做扇出 |
| 2.5 | 2026-09-19 | 推理开关区分三态（没提 / 明确关闭 / 明确开启），首次成文（[6.4](#64-跨协议能力差异)「推理开关的三态」）：四个协议各自的关闭写法在入站被识别、在出站被写出；明确关闭不再被参数层的 `defaults` 翻转成开启；目标协议不支持推理时明确关闭不报有损。**行为变更**：此前客户端的明确关闭在出站一律被省略，上游按自身默认执行，对默认开启推理的模型等于把关闭请求改成了开启；`reasoning_effort:"none"` / `reasoning.effort:"none"` 此前会被当成强度档位折算成某个真实档位。**文档更正**：Anthropic `thinking` 字段曾写「`type` 非 `enabled` 视为关闭」，措辞上把「没提」也读成了关闭——省略该字段与 `disabled` 并不等价 |
| 2.4 | 2026-09-19 | 用量与 token 计数的对外口径：估算区分调度/公开两个方向，CJK 按字符加权（[6.6](#66-token-估算的两个方向)）；`count_tokens` 改用公开方向并补上估算口径说明（[5.5](#55-post-v1messagescount_tokens本地估算)）；`input_tokens` 补上用量兜底（原先只兜 output，上游不报时输入维度恒为 0）；上下文超限新增 `request is too long`、`input token count exceeds` 等文案，`token limit` 改为需伴随上下文语境的组合式判定。**行为变更**：`count_tokens` 对中文提示的回答从「每 4 字符 1 token」抬到「每字符 1.25 token」，带媒体块的请求不再报 0。**文档更正**：`input_tokens` 字段曾写「上游报的或估算的」，而在本轮之前它从不估算 |
| 2.3 | 2026-09-19 | 出站连接层首次成文（[6.7](#67-出站连接层)）：连接复用上限抬高（标准库默认每 host 只留 2 条空闲连接）、新增响应头等待时限、写 deadline 逐次推进挡慢客户端；四层时限的分工与边界一并列明。附录 A 新增 `MSA_MAX_IDLE_CONNS`、`MSA_MAX_IDLE_CONNS_PER_HOST`、`MSA_IDLE_CONN_TIMEOUT`、`MSA_RESPONSE_HEADER_TIMEOUT` |
| 2.2 | 2026-09-19 | 多轮会话状态一致性：合成工具 id 加入响应标记以保证跨轮不撞号（[3.2.5](#325-工具调用-id-的生命周期)）；`sanitized` 新增 `duplicate tool_use id ...` 说明并补齐全部配对治理形态（[3.2.1](#321-sanitized-的说明形态)）；Responses 的 `context_management` 纳入托管状态字段拒收，四个字段与拒收规则首次成文（[5.4](#54-post-v1responsesopenai-responses)）。**文档更正**：`sanitized` 曾写「合并连续同角色消息」，本服务从未有此行为也不应有——`user(tool_result)` 紧跟 `user(text)` 是每次工具回合的真实形态，合并会破坏 prompt cache 前缀 |
| 2.1 | 2026-09-19 | 清单外形改为按客户端身份推断（`/v1/models`、`/models` 两族 SDK 都会打，路径优先于头）；新增 Gemini 清单外形与 `/v1beta/models`；新增单模型查询端点 [5.7](#57-get-modelsid单模型查询)；新增跨域与预检 [5.8](#58-跨域cors与预检)；新增请求体 media type 闸门 [2.3.3](#233-请求体-media-type)（表单两种回 415）。**行为变更**：`/models` 从固定 OpenAI 外形改为按客户端信号推断，无专有头的请求仍得到 OpenAI 外形 |
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
| `MSA_CORS_ORIGINS` | 空（放开所有来源） | 允许跨域的来源，逗号分隔。空或含 `*` 时回 `Allow-Origin: *` 且不发 `Allow-Credentials`。见 [5.8](#58-跨域cors与预检) |
| `MSA_MAX_IDLE_CONNS_PER_HOST` | `32` | 每个上游 host 保留的空闲连接数上限。标准库默认只留 2 条，并发超过它的请求每次都要重新 TLS 握手 |
| `MSA_MAX_IDLE_CONNS` | `256` | 全部 host 合计的空闲连接数上限。账号池横跨多个上游，总量卡太死会让 PerHost 白设 |
| `MSA_IDLE_CONN_TIMEOUT` | `90s` | 空闲连接多久后回收。负值表示不回收 |
| `MSA_RESPONSE_HEADER_TIMEOUT` | `120s` | **只**约束「请求发出 → 响应头到达」这一段；头到了之后读正文不受它影响。负值表示不设限。见 [6.7](#67-出站连接层) |
| `MSA_H2_SEND_PING_TIMEOUT` | `15s` | HTTP/2 连接空闲多久后发一个 PING 探测健康。负值表示关闭探测。见 [6.9](#69-死连接探测与连接层归因) |
| `MSA_H2_PING_TIMEOUT` | `15s` | PING 发出后多久没收到 PONG 判连接失联。负值表示关闭探测。见 [6.9](#69-死连接探测与连接层归因) |
| `MSA_CAPTURE_MODE` | `off` | 转换四体捕获开关，取 `off`/`errors`/`all`。**非法值拒绝启动**（静默回落会让运维以为捕获开着）。见 [6.11](#611-转换四体捕获) |
| `MSA_CAPTURE_MAX_BODY` | `1048576`（1 MiB） | 单体字节上限，超出保留前段并标 `truncated` |
| `MSA_CAPTURE_MAX_ENTRIES` | `32` | 保留的捕获条数上限，超出淘汰最旧 |

---

## 附录 B 请求结局（outcome）语义

结局决定**调度层如何更新目标运行态**，语义由 relay 定义：

| outcome | 调度层动作 | 产生条件 |
|---|---|---|
| `normal` | 清零失败计数，累计用量 | 成功完成 |
| `abnormal` | 累计失败，可能触发冷却 | 最终仍失败；committed 后失败；不再重试时 `retrying` 降级而来 |
| `retrying` | 同 abnormal，但本次会继续换目标 | 提交前失败且可重试（`rate_limit` / `upstream` / `timeout`）；上游一帧未出即结束 |
| `invalid_model` | 目标本身记为不可用 | 上游 404；出站协议未装配或编码失败 |
| `transport` | **完全不改运行态** | 出站连接层故障（见 [6.9](#69-死连接探测与连接层归因)）。上游可能完全健康，坏的是本服务池里那条连接。与 `context_exceeded` 同为零变更但独立成类——两者成因完全不同，合并后运维在流水里分不开 |
| `context_exceeded` | **完全不改运行态** | 输入太长是客户端的问题，不该记作目标的失败 |

每次尝试一条上报，`report_id = request_id:attempt` 保证幂等；上报失败进 outbox 重试（见 [7.6](#76-get-adminoutbox上报队列)）。
