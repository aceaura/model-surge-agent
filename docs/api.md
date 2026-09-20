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
| `skipped response event <类型> (the client protocol could not encode it)` | 上游给的某个事件在客户端协议里编不出来。该事件被跳过、其余内容照发、流正常收束——不留说明的话这件事在诊断里完全不存在。带事件类型：跳掉一段文本与跳掉一次工具调用的后果差得远。同类型重复跳过合并成一条 |
| `dropped outbound header <头名> from configuration (<原因>)` | 配置（端点定义、客户端声明或调度层下发的头）试图写一个由传输层计算的请求头。该头被丢弃、请求照常发出。五个受保护的头见 §6.17 |
| `stripped credential header <头名> on cross-host redirect` | 上游回了一个指向**另一个 host** 的重定向。该凭据头已从后续请求上摧掉，而这次重定向随后被拒绝（见 §6.18）。说明不带头的值也不带目标 URL |

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

### 6.16 数据面自身的完整性

前三轮（[6.13](#613-工具意图的保真)、[6.14](#614-响应侧语义维度的保真)、[6.15](#615-目标协议承载得了却没写出去的维度)）都在转换层里：输入是客户端的请求，输出是给上游的请求，问题都是某个语义维度在途中变了形。本节不在转换层——它讲的是承载这些转换的数据面自己：凭据会不会顺着错误文本漏出去、两个请求的身份会不会撞在一起、账有没有记全、客户端在等谁、以及一段代码 panic 之后连接会怎样。

这一类缺口的共同点是**不改变任何一次成功响应的内容**，所以矩阵测试全绿也发现不了；它们只在出错、并发或收尾时显形。

#### 出站失败的错误文本必须净化

`http.Client.Do` 的失败是 `*url.Error`，它的 `Error()` **内嵌完整请求 URL 含 query**。而 `BaseURL` 由调度层下发，一些部署把 key 放在 query 里（`?api_key=...`）。这句话随后进四个出口：流水的 `error_message` 列、Redis 实时环、管理面、以及**客户端可见的错误体**。

同一个理由下 `attempts_trail` 刻意不记 `base_url`（[6.12](#612-逐次尝试轨迹attempts_trail)），传输错误这条路不能把它放回来。

净化分两步，顺序有意义：

| 步骤 | 做法 | 为什么不能只做另一个 |
|---|---|---|
| 剥 `*url.Error` 外层 | `errors.As` 取内层错误 | 外层文本形如 `Post "URL": inner`，文本扫描拿不掉 `Post ` 与引号，客户端会看到半截文本 |
| 扫 `://` 换占位符 | 向前吃 scheme、向后吃到分隔符 | 上游库换错误类型、或自己把 URL 拼进消息时，剥外层那步就不生效了 |

**归因必须留住**：超时、连接被拒、DNS 失败三者的处置完全不同，只回一句「连接失败」等于把排查推给抓包。归因全在 `*url.Error` 的内层，剥掉外层刚好既去 URL 又留归因。

只认 `://` 这个记号，**不做内容黑名单**（不扫 `sk-`、不扫「像密钥的字符串」）：黑名单会误伤正常内容，且给人虚假的安全感。

净化后若文本为空则回一句 `upstream request failed`——空 `message` 在客户端那边表现成「未知错误」，比一个笼统但确定的说法更难查。

两条出错路径（`transport` 与 `upstream unreachable`）都走净化。

#### 客户端请求 id 撞号

`X-Request-Id` 由客户端给，本服务原样回显（[2](#2-通用约定)）。它同时还是：流水主键、上报给调度层的幂等键、以及捕获的键。客户端重复用同一个值（id 生成有 bug、或重放）时：

| 出口 | 撞号的后果 |
|---|---|
| 流水主键 | `ON CONFLICT DO UPDATE`，第二条**静默覆盖**第一条 |
| 上报幂等键 | `DO NOTHING`，第二次的用量**静默丢弃**，调度层的配额少算 |

因此**回显 id 与记录键分离**：

- 回显仍是客户端给的那个值——改掉它客户端就对不上自己的日志。
- 落库、上报、捕获、以及给调度层的 dispatch 四处统一用记录键。四处只要有一处用了会撞号的回显 id，事后就串不起同一次请求。
- 未撞号时两者是同一个值（绝大多数请求的形态，没有变化）。
- 撞号时记录键换成本服务生成的 `req_<hex>`，并打一条 `WARN request id collision`——悄悄换键会让客户端那个 bug 永远不被发现。

撞号检测只在**进程内**记忆最近 4096 个采纳过的 id（LRU 环）：

- 不进 PG 查重：那是每个请求一次额外往返，换来的只是一个本就罕见的情形。
- **多实例部署的边界**：两个实例各记自己的，跨实例撞号仍可能覆盖。要严格去重应在客户端侧保证 id 唯一（UUID），或在反代层按实例做会话亲和。这条边界写在这里，不在代码注释里——它是部署方需要知道的事。

受理面拒绝那条路径（[3.2.8](#328-受理面拒绝)）同样过撞号检测：它也落流水。

#### 重试耗尽时的 usage

四条终态分支（正常、已提交后失败、不可重试、重试耗尽）都要把这次的 usage 记进流水。耗尽那条此前漏了——而重试耗尽的请求上游**照样计了费**，漏记会让流水记 0 token 而调度层记非零，事后无法判断哪边错。

判据写成「流水里的 usage 与最后一次上报给调度层的 usage 一致」：这正是对账时两边对不上的那个量。

#### 上报不占客户端等待时间

每次尝试结束都要给调度层上报一次结果，而上报是一次阻塞 HTTP POST。重试三次的请求此前要串三次上报的往返，全落在客户端的等待里。

现在每次请求带一个上报队列（缓冲 channel + 单消费者）：

| 决定 | 理由 |
|---|---|
| 六条路径全部入队，终态也不例外 | 调度层按到达顺序解释这些上报（`retrying` 之后才是终态），混用同步与异步会让顺序取决于调度 |
| 串行投递，不是每条一个 goroutine | 同上，顺序是语义的一部分 |
| 每次请求一个队列，不是进程级一个 | 进程级队列在关闭时要等所有请求，且一个慢上报会拖住无关请求 |
| `Serve` 返回前等队列排空 | 返回即代表「这次请求的全部上报已投出」，否则进程退出会丢掉在途上报 |

轨迹（`attempts_trail`）仍同步追加：它写的是随后要落库的那条记录，异步化会让落库读到一半的轨迹。

**时延不变式随之调整**：`dispatch_ms + upstream_ms <= latency_ms`，差额里含三样——本服务自身的编解码、上游的生成时间、以及**终态上报的一次往返**。重试路径上的上报已不占等待，终态那次仍在客户端的等待之内。

#### panic 不得让流无声中断

不兜 panic 的后果具体到一种症状：任一 codec 在流中途 panic，连接直接断，客户端看到的是一个**未闭合的流**——SDK 那边表现成解析卡住或超时，而不是一个能报给人看的错误。两个参考实现都包了恢复中间件。

恢复挂在处理链**最外层**（CORS 与访问日志之外）：那两层自己也可能 panic，尤其访问日志在业务处理返回之后才记录。管理面走同一条链，一并覆盖。

收尾按「响应写到哪一步了」分四种：

| 情形 | 收尾 |
|---|---|
| 数据面、还没写响应头 | 500 + 本协议的错误信封 |
| 数据面、已开始写 | 状态码收不回来，**不再动它**；补一个流内错误帧让客户端的状态机收束 |
| 管理面、还没写 | 500 + 管理面自己的错误形状（不套数据面信封，理由同 [3.2.8](#328-受理面拒绝)） |
| 管理面、已开始写 | 什么都不再写：数据面的错误帧它解不动 |

已开始写之后改状态码不会真的改掉状态码（标准库打一行 `superfluous WriteHeader` 并忽略），它改的是**响应体**：本该是流内错误帧的地方变成一份错误信封 JSON，混在 SSE 流里客户端解不动。

`http.ErrAbortHandler` 原样抛回去：它是标准库表达「故意中断这个响应」的方式，恢复它等于把一个刻意的中断改写成一次错误。

panic 值**不回给客户端**（可能含内部细节），但栈必须打进日志——panic 的定位信息只在栈里。

#### 跳过的事件必须出说明

某个上游事件在客户端协议里编不出来时，跳过它、其余内容照发、流正常收束。这个处置本身是对的（中断整个流更坏），但此前不留任何痕迹：客户端收到的内容缺了一块，而 HTTP 200、流正常结束、诊断里什么都没有。

现在记一条 `skipped response event <类型> (...)`。带事件类型：跳掉一段文本与跳掉一次工具调用的后果差得远。**刻意不拼错误文本**——说明会去重，而错误措辞一变同一类跳过就散成多条。

#### 明确不做

- **不拒绝客户端给的 id**。撞号是客户端的 bug，拒绝请求把它的 bug 变成本服务的故障。换键 + 告警既保住了数据又让 bug 可见。
- **不改 `ON CONFLICT` 语义**。`DO UPDATE` 对同一个请求的多次写入（先落库、后补 usage）是正确的；问题在键不该撞，不在冲突处置。
- **不把 `base_url` 加进轨迹**。理由见 [6.12](#612-逐次尝试轨迹attempts_trail)。
- **不做全局错误文本黑名单扫描**。见上文「只认 `://`」。
- **不为 panic 做重试**。panic 是本服务的 bug，重试一遍只会再 panic 一次，而客户端多等一倍时间。

### 6.17 出站传输层的编码与客户端保活

三条缺口都在 HTTP 传输层，共同点是**症状离原因很远**：请求正常发出、上游
正常回 200，坏掉的东西在别的地方显形。

#### 6.17.1 配置不得覆盖由传输层计算的请求头

出站请求头有五层来源：本服务写死的两个（`Content-Type`、`Accept`）、
出站 codec 的端点定义、客户端的协议声明、调度层下发的凭据头。后三层都来自
配置，此前一律直写。

其中 `Accept-Encoding` 最隐蔽。Go 的 `http.Transport` **只在它自己加过这个
头时才透明解压**；配置里写了这个头，标准库就不再解，压缩字节直接进切帧器，
切不出任何东西——落到「HTTP 200 却一个事件都没解出来」那条可重试路径上，
而所有目标配的都是同一个头，三次全败。另外四个（`Content-Length`、
`Transfer-Encoding`、`Host`、`Connection`）写错会让请求立刻变形或被上游拒收，
一次 400 就暴露了；这一个不会。

现在这三层统一过一个**黑名单**：

| 头 | 丢弃原因 |
| --- | --- |
| `Accept-Encoding` | 传输层计算它；设了会关掉透明解压 |
| `Content-Length` | 传输层按 body 计算 |
| `Transfer-Encoding` | Go 用 `Body` 与 `ContentLength` 决定分块，手写不生效反而让请求头自相矛盾 |
| `Host` | 要改的是 `Request.Host` 字段，写进 `Header` 对标准库无效 |
| `Connection` | 传输层计算它 |

**黑名单而非白名单**：白名单要枚举「所有上游可能需要的头」，那个集合是开放的，
每接一家新上游都得改代码；黑名单只需枚举「写进去一定坏事的」，而那个集合由
一个理由封闭界定——它们由标准库按传输的实际情况计算。

`Content-Type` 与 `Accept` **刻意不在其中**：它们是意图表达而非传输计算，
且确有上游要求 `application/json; charset=utf-8` 或别的 `Accept` 值，
运维覆盖它们是正当的配置行为。本服务写死的那两行也不过这一层：它们是代码，
改它要过评审。

丢弃留一条 lossy 说明，**点名头与后果**（这五个里只有 `Accept-Encoding` 的
症状远离原因，说明文本是运维唯一的线索）。说明用规范化的头名，否则同一个头的
各种大小写形态会散成多条。说明**刻意不拼头的值**：这五个头里不会有凭据，
但「不拼值」是一条更容易守住的规则，将来集合扩大时不必重新判断每一项。

#### 6.17.2 上游返回压缩响应体时必须解压

此前从不看响应的 `Content-Encoding`。正常情况下标准库已经解好了——但它解完
会**把这个头从响应头里删掉**，所以这个头还在，就是标准库没解的可靠信号
（原因通常是 6.17.1 那个头被配过，也可能是上游回了我们没请求过的编码）。

| `Content-Encoding` | 处置 |
| --- | --- |
| 空 / `identity` | 原样透传。`identity` 是 RFC 允许的显式「没压」，当成未知编码报错是错的 |
| `gzip` / `x-gzip` | 流式解压。`x-gzip` 是同一编码的老写法，真实网关里出现过 |
| `deflate` | 流式解压 |
| `br` / `zstd` / 其他 | 明确报 `upstream used an unsupported content encoding (<enc>)` |
| 多重（含 `,`） | 明确报 `upstream used multiple content encodings (...)` |

**流式而非读全再解**：SSE 体是无界的，读全等于把逐字输出退化成一整份，
而这一层恰好在流式路径上。

**明确报错而不是硬塞给切帧器**：后者的症状是「HTTP 200 却一个事件都没解出来」，
从那个症状反推到编码问题要花很久。坏 gzip 同样在建流阶段就报——`gzip.NewReader`
立刻校验头部。解压失败的错误里**不拼任何响应体字节**：那些字节可能是上游回的
任何东西，而这个 message 会流到客户端可见的错误体里。

三条读 body 的路径全部覆盖：SSE 切帧、整份响应采纳、以及**非 2xx 的错误体**。
错误体那条尤其要覆盖：限流与配额耗尽正是靠这个体区分的，解不出会归成一个
笼统的上游错误。它解压失败时退回原始体——归因已经是「上游不行」，
再换一条错误路径只会丢掉状态码这条更硬的信息。

**解压挂在捕获的里侧**：捕获要留的是能读的字节。存压缩字节等于把「上游到底
回了什么」这条最后的线索变成一段谁也看不懂的二进制，而排查这类问题时
恰好只有它可看。

#### 6.17.3 静默期内向客户端发保活帧

反代、负载均衡与云网关普遍在 30~60s 无字节时掐掉连接，而推理模型在长思考
期间可以几分钟不出一个 token。被掐时客户端看到的是连接异常中断，
而上游其实一切正常、还在计费生成。

现在流式响应在静默期内按 `MSA_HEARTBEAT_INTERVAL`（默认 15s）发保活帧。
**形状由客户端协议决定**：

| 入站协议 | 保活帧 |
| --- | --- |
| `anthropic` | `event: ping` 事件 |
| 其余三个 | SSE 注释行 `: keepalive` |

Anthropic 的客户端 SDK 按事件类型分派，`ping` 是它已知且会忽略的一类；
注释帧虽然规范上也该被忽略，但那是对解析器的要求，而按类型分派的实现可能
压根没走到注释分支。

四条约束：

- **首帧之前不发**。响应头还没写出，此时往 `w` 写会把状态码钉死在 200，
  而这个阶段的失败本该换目标重试或回一个正确的 HTTP 错误码。
- **非流式客户端不发**。它的响应是一次性 JSON，中间插字节会把体弄坏。
- **不进 usage、不进聚合器、不进 tail**。它不是内容，混进去会让记账多算、
  让完整性判定看到一个不存在的事件。**进捕获**：客户端确实收到了这些字节。
- **不推进空闲超时**。这一条是加心跳时必须同时改的：空闲超时此前是每轮循环
  新起一个 `time.After`，心跳会让循环多醒几次、把计时器一次次推后，
  于是一条彻底静默的上游流永远等不到空闲超时。现在超时是一个**绝对时刻**，
  只有真帧到达才推进它——心跳是我们自己发的，它不是上游还活着的证据。

`MSA_HEARTBEAT_INTERVAL` 取负值显式关闭，与连接层那几个参数同口径。

#### 明确不做

- **不支持 br 与 zstd**。两者都要引第三方依赖，而实测上游里没有回这两种的。
  报错让它一次暴露，将来真遇到再加。
- **不主动加 `Accept-Encoding`**。让标准库自己加：它加了才会自己解，
  手动加等于把解压责任揽过来而收益是零。
- **不做出站并发上限**。那属于调度层：它知道每个账号的配额，数据面只看到
  单次请求。
- **不读 2xx 响应里的限流剩余量头**。那需要新增列 + 迁移 + 管理面暴露，
  是数据模型那条轴上的事，本轮不碰。
- **保活不得掩盖上游静默**。见上文空闲超时那条——这正是加心跳最容易引入的
  新故障。
- **整份响应不发保活**。上游回一整份 JSON 时没有静默期可言。

---

### 6.18 出站重定向策略

出站腿此前没有任何重定向策略：
客户端只设了 `Transport`，`CheckRedirect` 留空，
于是沿用标准库默认行为。本机探针实测的默认行为：

```
302 后最终方法 = GET，body = ""        ← 方法被改写、请求体被丢
307 后最终方法 = POST，body 完整         ← 靠 GetBody 重放，正确
跳转到另一个 host 时：
  Authorization = ""、Cookie = ""       ← 标准库自己删了
  x-api-key、x-goog-api-key 照带      ← 它不认这两个名字
```

后一条是本轮的核心：标准库只认
`Authorization`/`Www-Authenticate`/`Cookie`/`Cookie2` 四个头名，
而本服务的凭据恰好是 `x-api-key`（Anthropic）与
`x-goog-api-key`（Gemini）。

#### 6.18.1 跳转前先摧凭据

重定向目标的 host（**含端口**，换端口就是换服务）
与最初那个请求不同时，下表的头全部摧除，判定大小写不敏感：

| 头 | 使用方 |
|---|---|
| `Authorization` | OpenAI 族（Chat Completions / Responses） |
| `x-api-key` | Anthropic |
| `x-goog-api-key` | Gemini |
| `api-key` | Azure 形态的兼容层 |
| `Cookie` | 任何带会话的中间层 |

基准取**最初那个请求**的 host 而不是上一跳：
A→B→A 这种链条上逐跳比会认为最后一跳「回到了同 host」
从而把凭据加回去，而它在 B 那一跳已经暴露过了。

摧除排在**所有拒绝之前**。本轮的结论是跳转到另一个 host
一律拒绝，所以摧了之后并不会真的发出去——但摧除必须存在：
它是纵深防御，防的是将来有人把 host 判据放宽时凭据静默跟着走。

#### 6.18.2 三种拒绝

| 情形 | 归因文本 | 判据 |
|---|---|---|
| 301 / 302 / 303 | `upstream redirected in a way that rewrites the request method` | 下一跳的方法与上一跳不同 |
| 跳到另一个 host | `upstream redirected to a different host` | `URL.Host` 与最初那个请求不同 |
| 超过 3 跳 | `upstream redirect chain was too long` | `len(via) > 3` |
| 3xx 但无可用 `Location` | `upstream returned a redirect that cannot be followed` | 状态码闸门之前的 3xx 分支 |

判 301/302/303 用的是**比较方法**而不是读状态码：
`CheckRedirect` 的签名里没有重定向响应的状态码，但标准库
在调它之前已经把方法改写好了，而 `via` 里留的是改写前的方法。
一个判据覆盖三个码，并且天然放过 307/308。

307/308 **同 host** 的重定向照常跟随（`GetBody` 存在，体能正确重放）：
同 host 换路径是上游正当的版本迁移形态，这一路上凭据必须留着，
否则一次正常的 307 会变成 401。

跳数上限取 3，不可配置。上限存在的意义是防回环，
具体取几没有运维要调的理由；不让标准库的 10 跳先触发是因为
它的错误文本里带完整 URL 含 query。

#### 6.18.3 归因与净化

四种情形全部归 `upstream` 且**可重试**。归 `upstream` 而不是
`transport`：后者的语义是「换条连接有意义」，而重定向换连接一定
得到同一个结果，那个 kind 还会影响账号该不该冷却的归因。
可重试是因为这是**这个目标**的路由配置问题，换目标有意义。

归因文本是**四句固定文本**，一个字都不从原始错误里取：
`Do` 返回的 `*url.Error` 内嵌重定向目标 URL 含 query
（探针实测 `Post "/next?key=sk-inquery": ...`）。净化那一层的托底扫描
虽然也能去掉 URL，但那是托底；这条路径上我们确切知道该说什么。

重定向失败的判定必须排在连接层归因**之前**：哨兵被
`*url.Error` 裹着而它不是 `net.Error`，否则会落到「upstream unreachable」
那一支——kind 恰好对了一半而文本是错的。

缺 `Location` 的 3xx 会被标准库**无错误地**原样返回（探针实测），
所以状态码闸门**之前**得有一条 3xx 分支。排在闸门之前而不是闸门内部：
闸门那一段要读体、解压、`DecodeError`，而一个没有可用 `Location`
的 3xx 的体不值得走这一套，走了只会把一个通常为空的体归成
笼统上游错误。

摧凭据的说明直接并进 `lossy`，不搭建流阶段 `notes` 那趟车：
后者只在建流**成功**时才被取走，而重定向的说明几乎总是产生在
失败那一侧——搭那趟车的话，唯一会产生这条说明的场景恰好是
它一定丢掉的场景。

说明的收集口挂在**请求的 ctx** 上：客户端是进程级共享的，
`CheckRedirect` 是它的一个字段，策略函数里存本次请求的状态会让
并发请求互相串。探针实测重定向请求继承原请求的 context。

#### 明确不做

- **不做 SSRF 黑名单**（拒私网地址、拒 userinfo、拒非 http(s) scheme）。
  参考实现 new-api 做这些（`service/http_client.go`、
  `controller/video_proxy.go`）是因为它的 URL 部分来自终端用户输入；
  本服务的 `BaseURL` 由受信的调度层下发。加地址黑名单会让合法的
  内网上游部署（本服务的常见形态）连不上，而那个症状远离原因。
- **不用 `ErrUseLastResponse`**（把 3xx 交回调用方自己看）。
  sub2api 在设备授权流程里那么做（`xai/sso_device.go`）是因为它要读
  `Location` 里的 code；本服务的上游不该重定向一个推理请求，
  能跟随的跟随、不能的报错，没有第三种处置。
- **不追跳到另一个 host 的 307/308**。摧掉凭据再跟随得到的一定是 401，
  等于用两个往返换一个注定失败的结果。
- **不记重定向目标 URL**（不进流水、不进轨迹、不进说明）。
  理由同 `attempts_trail` 刻意不记 `BaseURL`。
- **不做可配置的跳数上限**。

---

### 6.19 关停生命周期

进程收到 `SIGINT` 或 `SIGTERM` 后分三个阶段收尾，**每个阶段一份独立的、
新建的超时预算**。

| 阶段 | 预算 | 做什么 |
| --- | --- | --- |
| 1 等流收尾 | `MSA_SHUTDOWN_GRACE` | 停止接受新连接，等在途的流自然结束 |
| 2 清理等待 | `MSA_SHUTDOWN_LINGER` | 仅当阶段 1 超时：继续等在途 handler 返回 |
| 3 冲队列 | `MSA_SHUTDOWN_FLUSH_TIMEOUT` | 无条件：把 outbox 里到期的上报冲一次 |

#### 为什么每阶段必须新建预算

此前三个阶段共用一个 20 秒的 context。只要有流在途，阶段 1 就会烧光整份
预算并返回 `context deadline exceeded`；阶段 3 随后拿到的是**同一个已经过期的
context**，`outbox` 的扫描立刻失败、打一行 `outbox scan failed` 返回 0。
也就是说这次「退出前把队列冲一次」**在它唯一存在意义的场景里保证是空操作**，
而代码注释声称队列被冲过了。

派生也不行（`WithTimeout(上一阶段的 ctx, ...)`）：派生会把刚刚用尽的 deadline
继承下来，结果与共用同一个完全一样。也不能从信号 context 派生：它在收到
信号那一刻就已经 Done。

#### 为什么宽限期的默认值是派生的

`MSA_SHUTDOWN_GRACE` 未设置时取 `max(MSA_FIRST_TOKEN_TIMEOUT,
MSA_IDLE_TIMEOUT)`，而不是一个写死的常量。写死的后果是：运维把空闲超时调到
5 分钟以适配长思考模型，关停宽限期还留在原处，于是**每次部署都会掐断一条
正当地处于静默思考期的流**，而客户端看到的是连接异常中断而非一个能报给人看的
错误。派生之后「默认配置自相矛盾」这个状态不可能出现。

显式设置且小于那个最大值时**拒绝启动**，错误点名两个变量。这是一条
**联合校验**：两个值各自都合法，组合起来才有害。参照 new-api `main.go:232`
给 120 秒，注释理由同款（SSE 流可能跑几分钟）。

判「是否显式设置」看环境变量在不在，而不是看值是否为零：显式写 `0s` 的人
意图是「不等」，那应当被上面那条规则拒掉并给出理由，而不是被当成未设置从而
静默取一个很大的默认——后者会让运维以为自己关掉了等待，实际等满两分钟。

`MSA_SHUTDOWN_LINGER` 与 `MSA_SHUTDOWN_FLUSH_TIMEOUT` **不接受非正值**。
连接层那几个参数用负值表达「关闭」是因为关掉它们是合理的运维选择，而
「不等在途、不冲队列」不是——那恰好就是本节修掉的两个缺陷的样子。

#### 阶段 2 存在的理由：在途请求的上报

阶段 1 超时后，此前没有任何东西等在途 handler（全仓无 `sync.WaitGroup`）：
`run` 直接返回、进程退出，被掐断的 handler 连同它那条每请求的上报投递
goroutine 一起死掉。那些上报**既没 POST 出去也没落库**，于是调度层对部署
瞬间在途的每一个请求都少记用量。

在途计数装在处理链的**最外层**：要等的是「handler 还没返回」，而每请求的
上报队列由 `Serve` 的 defer 排空——等到 handler 返回就等到了上报投完，
无需把那个队列暴露到装配层。计数用 `defer` 归零，因此 handler panic
也不会让计数永久偏高（否则一次 panic 会让此后每次关停都白等满整个 Linger）。

等待用轮询而不是 `sync.WaitGroup`：`WaitGroup.Wait` 不接受超时，而关停期的
等待必须有界，否则一条卡住的上游会把部署窗口拖到无限。放弃时打一行
`gave up waiting for in-flight requests` 带条数——**这是运维唯一能看到
「这次部署丢了多少上报」的地方**。

阶段 1 干净返回时阶段 2 **不执行**：在途已清零，再等就是白白拖长部署窗口。

#### 阶段顺序不可调换

冲队列必须排在等在途之后。排在前面的话，正在收尾的那些请求的上报还没入队，
这一冲就冲不到它们，而它们恰恰是关停期最可能丢的那批。

#### 明确不做

- **不做连接级的强制关闭**（`Server.Close`）。SSE 流被硬切和进程被 kill
  对客户端是同一件事，多写一条路径不换来新行为。
- **不把在途计数暴露成管理面指标或健康检查字段**。那要新增端点与文档轴，
  本节只解决关停期的丢报；`/health` 的 `goroutines` 已是一个相近的粗指标。
- **不改每请求上报队列的生命周期**。它的语义（每请求一个、串行投递、
  响应写完后等排空）是正确的，问题不在它而在没人等它。
- **不在关停期把在途上报改成同步直投**。那会让关停时间取决于调度层的
  响应速度，而调度层此刻可能也在重启。
- **不给阶段 3 重试**。outbox 的后台重放本来就会在下次启动后继续，
  关停期这一冲只是让常见情形少等一个重试间隔。

---

### 6.20 退避信号的解析与回传

上游说「什么时候能再来」有三条路：`Retry-After` 头、各家的 `*-reset` 头、
错误体里的结构化到期信息。三者归一到一个**绝对时刻**，零值表示上游没说。

#### reset 头的三种值形态

| 形态 | 例 | 谁在用 |
| --- | --- | --- |
| 数字（毫秒 epoch / 秒 epoch / 相对秒数） | `1789772400`、`30` | Anthropic unified、Codex、部分 xAI |
| Go duration | `1s`、`6m0s`、`20ms` | OpenAI 兼容层 |
| RFC3339 时刻 | `2026-09-19T12:01:30Z` | Anthropic 标准限流的三个分窗 |

判定顺序是**数字 → duration → RFC3339**，但这个顺序不影响结果：三种形态
互不相交。`time.ParseDuration` 对无单位数字报 `missing unit in duration`，
并**不**把它当成纳秒（本机探针实测）；唯一的交集是 `"0"`，而数字支与
duration 支都把它判成零值。排成这个顺序只是从最便宜、最常见的一种开始。

> 立项时写的理由是「数字必须排第一，否则秒级 epoch 会被当成 1.7 秒」。
> 探针推翻了它。留下这段是因为读代码的人会有同一个疑问，而错的理由比
> 没有理由更坏：它会让下一个人不敢动这段顺序，或者去修一个不存在的风险。

duration 形态解出的时长不足 1 秒时按 1 秒计。「20 毫秒后重试」在限流语境下
几乎总是错的：上游给这个值是因为它按窗口边界算，而客户端到本服务之间还有
排队。抬到 1 秒的代价是多等不到一秒，不抬的代价是一次注定再被拒的往返。

三种形态都认不出来时返回零值，**不猜**。零值与「上游没说」是同一件事，
而这正是此前 duration 形态的下场——解不出来，于是调度层以为上游什么都没说，
回落到启发式冷却，而上游其实明确说了。

#### 被认的 reset 头

- `anthropic-ratelimit-unified-reset` 及 `-5h-` / `-7d-` 两个分窗
- `anthropic-ratelimit-requests-reset`、`-input-tokens-reset`、
  `-output-tokens-reset`（值是 RFC3339）
- `x-ratelimit-reset-requests`、`x-ratelimit-reset-tokens`
- `x-codex-primary-reset-after-seconds`、`x-codex-secondary-reset-after-seconds`

多个头同时命中取**最早**的那个：取最早只是多一次探测，取最晚会在那个长窗口
其实没被拒时白锁数天。

超过 24 小时的到期时刻一律丢弃。上游时钟不一定与本机同步，一个半年后的
时刻是坏数据而不是真的限流。闸门在多头汇总那一层而不在单个值的解析里：
两处都在回答同一个问题，共用一份判据才不会让某一种形态偷偷绕过。

#### 回给客户端的 `Retry-After`

错误响应上发这个头，值是**向上取整的整数秒**。客户端 SDK 的自动退避读的
正是它——此前它一个出口都没有，上游明示的到期时刻只流向调度层，
客户端收到 429 后按自己的默认节奏立刻重来，把一次限流变成一场风暴。

- **从时刻现算，不转发上游的头字符串。** 上游那个字节串可能含 CRLF 或长得
  离谱，而本服务手上已经有一个解析过、过了地平线闸门的时刻值。
- **向上取整而不是截断。** 截断会把 1.2 秒写成 1，客户端早到 0.2 秒又吃一个
  429，而这个头存在的全部意义就是让它不必再吃那一次。
- **时刻已过去或为零值时不发。** 一个 `0` 或负数会让 SDK 立刻重来，
  比不发这个头更坏。
- **两个出口都发**：数据面的终态失败，以及受理面与路由层的拒绝。
  后者里连「没认出入站协议」那条兜底路径也发——那种情形下的限流仍是限流。
- **已提交的流不发。** 响应头早已写出，此刻设 header 既不报错也不生效
  （标准库静默忽略），留着它只会让读代码的人以为它生效了。

#### 明确不做

- **不在 2xx 上发这个头**。成功响应上它没有语义，即使上游在接近配额时
  在成功响应上带了限流头。
- **不转发上游的其它限流头**（剩余额度、窗口大小等）。那些需要新增列、
  迁移与管理面暴露，是数据模型那条轴上的事。
- **不给到期时刻做出口侧的「合理化」夹紧**。地平线闸门已经在解析侧挡掉了
  不可信的值，出口再夹一次等于两处规则各自漂移。
- **不为此新增环境变量**。这几条都是「本该做对的事」，不是可配置策略。

---

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

---

### 6.21 跨边界的值完整性

一个值在跨越边界时可能被静默改写。本节的边界有三个：**字节截断点**、
**JSON 往返**、**PostgreSQL 的 TEXT 列**。共同症状是状态码与响应体都正常，
坏处显形在别处——那一次故障的流水恰好缺失，或上游按另一个种子生成。

#### 截断必须落在字符边界

本服务有四处为限长而截断文本：

| 位置 | 上限 | 产物流向 |
|---|---|---|
| 上游错误体摘要 | 512 字节 | `ir.Error.Message` → `error_message`（TEXT） |
| 调度层非 JSON 响应摘要 | 256 字节 | `report_outbox.last_error`（TEXT） |
| outbox 上报失败原因 | 512 字节 | `report_outbox.last_error`（TEXT） |
| 上游收尾原因原文 | 200 字节 | `lossy`（JSONB） |

按字节切会切在多字节字符中间。后果**不在客户端那一侧**：Go 的
`json.Marshal` 把非法字节替成 U+FFFD 且不报错，调用方看到的是一条完全正常
的错误信封。后果在 PostgreSQL——非法序列让**整行被拒**
（`SQLSTATE 22021 invalid byte sequence for encoding "UTF8"`，本机真库实测），
于是那一次故障的记录恰好缺失，缺的正是最需要的那条。

截断**往回退**而不是往前补：上限的含义是「最多这么多字节」，为凑齐一个完整
字符而超出会把约束反过来，而 `last_error` 那一处上限对着的是真实存储。
最多退 3 字节，UTF-8 单字符最长 4 字节。

判边界用「整串是否合法」而不是「末尾字符是否为 `RuneError`」：合法的 U+FFFD
本身就解码成 `RuneError`，后一种写法会把上游真的发过来的替换字符误判成截断
残骸砍掉。

#### 落库前还要净化

收边界只解决「我们切坏的」。上游本身就可能回非 UTF-8 字节——本服务明确保留
了「解压失败退回原始字节」这条路，那条路上的内容从不经过任何截断点。所以
写 `error_message`、`error_code`、`last_error` 之前另做一次净化。

净化**只剔坏字节，保留其余内容**：整段丢弃等于把那次故障的全部线索换成空白，
而线索恰恰在剩下的部分里。替换物是空串而不是 U+FFFD：这些文本要么进日志要么
进 TEXT 列，留一串替换字符只是把「坏了」这个事实搬到人眼前占位。参照 sub2api
`internal/server/middleware/request_metadata.go:16` 同样的处置，它的注释点明
理由是这些值会「reaches logs or database columns」。

净化点在**写库前**而不是构造时：构造时净化会让客户端看到的和库里存的是两份
不同文本。现在客户端拿到 `json.Marshal` 的 U+FFFD 版本（能看出坏在哪），库里
拿到干净版本（能存下来），两边都不丢那一行。

JSONB 列（`sanitized`、`lossy`、`attempts_trail`）不另做净化：它们的内容由
本服务构造，而 `json.Marshal` 已经把坏字节替掉了。唯一嵌了上游原文的是收尾
原因说明，它走的是上面那张表里的截断点。

#### 参数合并不得改写请求体的字面

上游模型配置的两层参数（defaults / overrides）作用在已编码的请求体上。此前
的实现把 body 解成 `map[string]any` 再编回去，这一趟有两处静默改写：

**数值精度。** 未开 `UseNumber` 时所有数字过一遍 `float64`：
`"seed":13835058055282163712` 出来变成 `13835058055282164000`（实测）。上游按
另一个种子生成，而请求 200、流水正常、有损诊断为空。更难查的是它**只发生在
配了 defaults/overrides 的目标上**——没配的走空短路逐字节原样返回，于是同一个
请求打到两个目标上行为不同。现在开 `UseNumber`，数字停在原始字面。

**HTML 转义。** `encoding/json` 默认把 `<`、`>`、`&` 编成 `\u003c` 这类六字节
转义。语义上无害，但这是字节层的改写，而本服务有两处按字节办事的东西：
请求体字节预算与转换四体捕获。同一个请求配了 overrides 与没配，量出来的字节
数和抓到的上游请求体就不一样。现在关掉转义。

换用 `json.Decoder` 时必须补一道检查：它比 `json.Unmarshal` 宽松，读完第一个
文档就返回，`{"a":1} {"b":2}` 会被静默当成 `{"a":1}`（实测）。不显式确认尾随
内容就等于顺手放宽一个原本守住的边界。`Encoder` 追加的换行也要去掉——这个
返回值是要当请求体发出去的。

#### 明确不做

- **不引入 sjson 之类的字节级 JSON 改写库。** new-api
  `relay/common/override.go:788-795` 为避免大 base64 字段（Gemini
  `inlineData.data`）的内存放大而弃用了 map 往返，代价是自己处理键转义
  （`escapeSjsonLiteralKey`）与嵌套下钻语义。本服务实测 5MB base64 body 经
  参数合并功能正常、字段字面保留（0.03 秒），**只有内存放大没有正确性缺口**，
  不值得为它多一个依赖和一类 bug。
- **不改上限数值，不改 schema，不加环境变量。**
- **不把净化推到 IR 构造处。** 那会让客户端与库里存的成为两份文本。
- **不对已经合法的输入做任何改动。** 净化与收边界对绝大多数请求都必须是
  恒等的，否则这次改动会悄悄改写全部历史流水的内容。

## 6.22 并发下的共享资源治理

本节记四处共享资源在并发下缺治理的修复。四者的触发形态**都发生在单进程
内部**，与部署副本数无关：`docker-compose.yml` 只起一个 `backend` 服务，
所以凡是「多副本才会出事」的论证在这个服务上都不成立。

### 6.22.1 结果上报队列的行认领

`report_outbox` 的取到期项此前是一条纯 `SELECT`，而处理发生在事务之外
（一次真实 HTTP 上报，最长十秒）。真库探针实测：两次相邻的取项各返回三行、
**重叠三行**；两个执行流各把同一次失败记一笔，`attempts` 从 0 直接到 2。

后果不是重复投递（`report_id` 唯一约束加调度层按同键幂等去重顶得住），
而是**假的判死**：`MSA_OUTBOX_MAX_ATTEMPTS=20` 在两个执行流并存时实际是十次，
一条只是撞上调度层短暂抖动的上报被提前 `Bury` 到 `9999-01-01`，
而运维在管理面看到的是「重试了 20 次仍失败」——一个据此做决策的假结论。

单进程里这两个执行流一直都在：ticker 每秒一次的 `Drain`，与关停时那次
无条件 `Drain`。后者正是在预算紧张时与前者撞上的。

新增两列，幂等 DDL 加 `ADD COLUMN IF NOT EXISTS`（无迁移框架，沿用既有做法）：

| 列 | 类型 | 含义 |
|---|---|---|
| `lease_until` | `TIMESTAMPTZ` | 认领租约的到期时刻。`NULL` 表示无人持有 |
| `lease_token` | `UUID` | 租约的所有权凭据，写回裁决时比对 |

取项改为一条 CTE：`SELECT ... FOR UPDATE SKIP LOCKED` 选出候选，
`UPDATE ... RETURNING` 原子地写上租约并返回。**两道机制各管一段**，缺一不可：

- `FOR UPDATE SKIP LOCKED` 管**同一瞬间**。后到的执行流跳过被锁的行而不是
  排队等——排队等的结果是它阻塞在那条语句的事务上（本轮有一条用
  `pg_sleep` 拖住持锁窗口的测试钉住「立刻返回」这件事）。
- `lease_until` 管**跨瞬间**。行锁随那条语句的事务结束就释放了，而处理是
  之后才做的，所以必须另有一个可见性窗口。

租约到期即重新可见，这是刻意的：持有者可能已随进程被杀，那些行必须回到
队列里而不是永久隐身。租约长度由 worker 给，取**单条上报超时的三倍、
下限 30 秒**：短于上报超时会让一条仍在飞的上报被重新认领（幂等键顶得住，
但 `attempts` 又开始虚涨），过长会让被杀进程留下的那批行长时间隐身。
不按批量条数放大——一批一百条会算出千秒级租约，而那一百条里绝大多数
还没开始处理。

`Retry` / `Bury` / `Done` 三处写入全部加 `AND lease_token = $n` 围栏，
零行受影响即返回 `ErrLeaseLost`。删行也围栏：不围的话无法区分
「另一个执行流已经干完了」与「我的租约被抢了」，而这两种情形下运维该看到的
日志不同。一次裁决落地就**交还租约**（两列置 `NULL`）：不交还的话，
一条排在一秒后重试的上报要等满整个租约才会被再看一眼——排空速度
被租约长度而不是退避曲线支配。

`Revive`（管理面的重试按钮）一并清掉两列。这是那个函数里最要紧的一行：
worker 手里可能正攥着这一行的旧 token。真库探针实测过不清的后果——
`Bury` 到 `Revive` 再到旧快照重新 `Bury`，`next_attempt_at` 回到 `9999`，
运维点了重试，界面上看着回到待发，下一秒又变成死信，而日志里没有任何一行
说明是谁埋的。

worker 侧把 `ErrLeaseLost` 降级为 Debug 并继续处理本批余下条目：它不是故障，
而是「有别人在管这一行」。按 Error 记会让日志周期性报一个不存在的故障。
`Done` 返回 `ErrLeaseLost` 时仍计入送达数——报确实送达了，不计的话关停期
那句「flushed N pending reports」永远是 0。

`Counts`（健康检查用的积压数）不看租约：被认领但仍在飞的行仍算 pending，
因为它确实还没送达。`List` 同理——租约是内部治理状态，不是观测口径。

对照 sub2api `repository/auth_cache_invalidation_outbox_repo.go`：同款
`SKIP LOCKED` 加租约加 `claimed_by` 围栏，且它的设计文档要求一个
`claim_version` 栅栏令牌而实际实现只靠 `claimed_by`。本服务的 `lease_token`
每次认领都换新值，正是那份文档要求的那个东西。new-api 走的是另一条路
（`model/system_task.go` 的双表 compare-and-set 加 `locked_until`，全仓无
`SKIP LOCKED`），可行但需要两张表；本服务只有一张表，CTE 那条路更短。

### 6.22.2 模型清单回源的并发合并

`CachedModels.List` 此前是「读缓存，miss 就回源」，无任何合并。缓存 TTL
默认一分钟，而清单页与每次列模型都走这里：TTL 到期那一瞬间所有在列客户端
一起 miss，调度层收到 N 倍尖峰；而它若正因此不健康，每个请求都要各自等满
自己的超时才失败，尖峰持续整个超时窗口。

现在 miss 之后进 `singleflight`，一组并发只回源一次、结果分发给所有等待者。
三处细节：

- **闭包内二次检查缓存。** 领头者在合并之外已经查过一次，进来再查一次能
  捕获「刚才另一组合并刚写完」这一形态。
- **回源用 `context.WithoutCancel` 派生加独立 10 秒预算。** 这一次回源的
  结果是共享的，不该因为发起它的那个客户端断开就作废掉所有等待者的结果。
- **失败记一个 2 秒负缓存窗口**，窗口内直接返回上次的错误。不记的话调度层
  不可用时每个请求各自去撞一次墙、各自等满超时。窗口用进程内的时刻而非
  Redis：Redis 可能没配，而合并这件事是进程内的。

**不学 new-api 的后台 ticker 全量重建**（`model/channel_cache.go`：定时重建
整张表、读路径永不回源，因此不存在回源合并这件事）。那需要一个长跑 goroutine，
它拿不到「当前是否有人在用」的信息，空闲时段也在轮询调度层；而本服务的读
路径已有 Redis 兜着，按需回源加合并开销更低。这是刻意的分歧。

### 6.22.3 健康检查的代价上界

`/healthz` 免鉴权（容器探针拿不到密钥，受信网络暴露是既定前提），而每次
命中此前都做：一次 PG `Ping`、一次**真实的**调度层 `GET /v1/models`（3 秒
超时）、`report_outbox` 上两个全表 `count(*) FILTER`。容器探针加反代加
监控叠加起来就是按秒计的调度层 QPS 与 PG 顺序扫描；而最贵的那个 `count`
在队列积压时最慢，队列积压恰恰是探针被看得最紧的时候——**自我放大**。

现在探测结果带一个 1 秒窗口，窗口内的命中读快照，并发命中经
`singleflight` 合并成一次探测。探测同样用 `context.WithoutCancel` 派生加
独立 5 秒预算：结果要给窗口内所有命中用，不该因第一个探针超时断开就让
后面的人也拿不到，从而各自再探一次——窗口在最需要它的时候失效。

窗口长度不做成环境变量：1 秒相对于容器探针的十秒级周期不会让运维拿到陈旧
结果，而暴露成配置会让人以为它值得调。零值兜底成 1 秒——生产装配不设这个
字段，不兜底的话这整条治理在生产里是关着的而测试全绿（本轮有一条用例专门
钉这个零值路径）。

全表 `count` **不改成近似计数**：那会让运维拿到一个不准的积压数，而积压数
正是他们据此决定要不要介入的那个数。加了窗口之后它每秒最多一次，对一张
正常态接近空的表可以接受。

对照四个参考仓库：**无一**在健康端点上做真实依赖探测。sub2api `/health`
是静态 `{"status":"ok"}`，真实探测放在 admin 鉴权之后；new-api
`/api/status` 只读内存设置，PG 探测版 `/api/status/test` 走 admin 鉴权。
本服务保留真实探测（运维需要这些状态）但给它加窗口，是第五种做法。

### 6.22.4 连接池上限可配

`pgxpool.New(ctx, dsn)` 此前未设任何上限，走驱动默认（探针实测 pgx 是
**32**）。这个池被四路共用：每请求的流水落库、outbox worker、流水清理循环、
健康检查。PG 侧 `max_connections` 通常是 100，而本服务、调度层、配置中心
可能共用一台。池打满时流水落库只 `Warn`、数据面照常 200，**症状是
「流水随机缺行」——一个不会触发任何告警的症状**。

| 变量 | 默认 | 含义 |
|---|---|---|
| `MSA_PG_MAX_CONNS` | `0` | 连接池上限。`0` 表示沿用驱动默认。负值启动期报错 |

默认**不改成某个具体值**：本服务可能与调度层、配置中心共用一台 PG，
在这里单方面抬高上限只会把连接耗尽点挪到别人身上。可配加 `/healthz` 里
已有的 `pool.max` 与 `pool.acquire_waiting`，运维就有据可调。负值不当成
「不限制」——连接池没有那个语义，写负数的人多半想表达那个不存在的意思，
而症状要到 PG 侧耗尽时才显现。

对照：sub2api 默认 256 并校验 `MaxIdleConns <= MaxOpenConns`；new-api 默认
1000（`SQL_MAX_OPEN_CONNS`），那个值下 PG 侧会先垮。取 sub2api 的形态、
但不照搬它的默认值。

### 明确不做

- **不引入多副本部署形态。** 6.22.1 的修复顺带让多副本安全，但那是副产品，
  不是论证依据，也不改 compose。
- **不把健康检查改成静态 ok，不给它加鉴权。** 前者让运维拿不到依赖状态，
  后者让容器探针拿不到凭据。
- **不改 outbox 的裁决语义**（几次转死信、退避曲线、死信保留行）。本节只管
  「谁有权做裁决」。
- **不引入迁移框架，不改既有列，不加新端点。**
- **不给 outbox 加工作者身份列。** `lease_token` 每次认领换新值已足够围栏，
  再记「是谁」需要一个进程标识，而单进程里那个值恒定、多进程又回到上一条
  不做的范围里。

## 6.23 上游侧失败的归因与幂等边界

出站腿的失败此前只分两档：`transport`（换条连接有意义）与 `upstream`
（换个目标有意义）。这个二分漏掉了一个更要紧的问题——**这次请求到底有没有
发出去**。发出去了的请求，上游可能已经生成完、已经计了费、请求里带副作用的
工具调用可能已经被执行，此时换个目标重发就是第二份账单、第二次执行。

本节四条修复的共同症状是**没有症状**：HTTP 状态码正常、响应体正常，
坏掉的东西显形在账单上、在连接池里、或在排查的人被一句事实错误的说明
带去找一个不存在的原因。

### 6.23.1 请求已发出的判定与幂等边界

判据用 `httptrace.ClientTrace.WroteRequest` 而不是从错误文本反推。
标准库内部有连接复用、h2 多路复用与请求重放，从 `*url.Error` 的文本推
「发出去了吗」每一条都是猜。本机探针实测：

| 失败形态 | `WroteRequest` 触发 | `Do` 返回错误 |
|---|---|---|
| 拨号被拒（ECONNREFUSED） | 否 | 是 |
| DNS 不存在（NXDOMAIN） | 否 | 是 |
| 响应头超时 | **是** | 是 |
| 上游读完请求就断（EOF） | **是** | 是 |
| 上游回 502 | 是 | **否**（走状态码闸门） |

最后一行把一条写进需求稿的判断推翻了：502 不在风险集合里——`Do` 成功返回，
失败是本服务读状态码后自己判的，它走的是既有的状态码路径而不是这条。

`ir.Error` 因此新增 `SideEffectRisk`。**与 `Retryable` 分开而不是直接把它压成
`false`**：两者回答的是不同的问题，`Retryable` 是「换个目标有没有意义」，
这一个是「换个目标会不会产生第二份计费」。压成一个字段后，将来要放宽某一类
（比如上游明确说了「我没开始处理」）就没有可放宽的地方。它**不进 `NewError`
的参数表**：绝大多数错误产生在请求发出之前，加进签名等于让四十余处调用点
都跟着填一个 `false`。

重试闸门排在 `Retryable` 闸门**之前**：这一族失败（响应头超时、上游读完请求
就断）的 kind 恰好都是可重试的那几个，放在后面就永远走不到。命中后把明确的
错误交给客户端由它决定重不重试——**它知道自己这个请求有没有副作用，
我们不知道**。

**重定向失败刻意不标这个位**。请求确实已经发出去了，但上游回的是一个重定向
而不是一次生成——它没有处理这个请求，也就没有计费，与 3xx 走状态码那条路
同一个道理。标上的话，一个 `Location` 配错的目标会让整个请求直接失败而不是
换个目标，反而更糟。（这一条是既有的重定向用例在本轮跑红时抄出来的，
设计稿原先是标的。）

### 6.23.2 目标不可达与连接层归因的分野

NXDOMAIN 的错误链是 `*url.Error → *net.OpError → *net.DNSError`
（`IsNotFound=true`）。`*net.OpError` 这一层会被既有的 `isTransportError`
认下并归成「换条连接就好」，于是**一个域名写错的目标永远不计失败、
永不冷却**，每次调度都还会被选中。

现在新增 `isUnreachableTarget` 并排在 `isTransportError` **之前**。判据：

- **只认 `IsNotFound`。** DNS 超时与临时失败是解析服务的问题而不是这个目标的
  问题，算成目标失败会在 DNS 抖动时把整个账号池一起冷却。
- `EHOSTUNREACH`、`ENETUNREACH` 认。
- **`ECONNREFUSED` 刻意不认**：上游滚动重启时会短暂拒连，那是瞬时的。
  参考实现 sub2api 把它归 Persistent，但它那边的处置是「临时摘出调度」，
  本服务这边是「计入目标失败」，代价不同，不跟。

错误文本仍走既有的净化（`BaseURL` 可能把 key 放在 query 里）。

### 6.23.3 响应体排空与连接复用

读不到底的响应体会让那条连接无法复用。本机探针实测：200KB 的错误体、
五次相同的 429，**不排空得到五条新连接**，排空得到一条。

后果是限流窗口期内每个请求各开一条 TCP+TLS——而限流窗口恰恰是上游最脆弱、
本服务最该省着用连接的时候，方向正好反了。且连接数攀升不触发任何告警。

三个 `Close` 点（3xx 分支、非 2xx 错误体分支、整份响应分支）统一改为
先排空再关。**排空有上界**（1 MiB）：无上界等于允许一个坏掉的上游用一份
无限长的错误体把本服务拖在这里，而复用一条连接的价值远不值那个代价。
超出上界就放弃复用、直接关。

### 6.23.4 「200 但不是 JSON」的归因

解码失败此前一律报一句笼统的上游错误。而这里有一类高频形态：企业代理、
WAF 或运营商门户在 200 上回一整页 HTML。排查的人拿着「解码失败」去查模型和
账号，而原因在网络路径上。

现在按体的开头判：`<!doctype html` 或 `<html`（大小写不敏感、允许前导空白、
只看前 512 字节）命中时报一句点明「像是被代理或 WAF 截了」的说明。
**只认开头不认「文中出现」**——一段正当的模型输出里完全可能包含 HTML 片段，
按「出现即判定」会把正常回答误判成拦截页。

非 HTML 的解码失败则把 `Content-Type` 拼进说明。上游回的 `text/plain`
与回的 `application/json` 但结构不对，是两个不同的问题。

两者都**仍然可重试**：换个目标可能就没有那道代理。

### 6.23.5 限流头的回传白名单

上游的 `anthropic-ratelimit-*` 与 `x-ratelimit-*` 此前一个都不回给客户端。
客户端 SDK 的主动限流规避读的正是这些头，拿不到就只能撞上 429 再退避。

回传用**前缀白名单**而不是黑名单。对照 sub2api 的做法是转发除少数几个之外的
全部响应头（黑名单），那个形态会把 `Set-Cookie` 与上游自己的
`x-request-id` 一起送给客户端——前者是凭据面，后者会让客户端拿着一个
本服务日志里查不到的 id 来问。白名单只需枚举「客户端确实要读的」，
而那个集合由用途封闭界定。

`Retry-After` **不在这个白名单里**：它已由 [6.20](#620-退避信号的解析与回传)
从解析过、过了地平线闸门的时刻现算，两处各发一次会给出两个不一致的值。

写入点必须排在写响应头**之前**：流式那条路上 `writeStreamHeaders` 里就
`WriteHeader` 了，排在后面设 header 静默无效。多值头逐个 `Add` 而不是覆盖。

### 明确不做

- **不做全量响应头转发。** 白名单之外的头要么属凭据面，要么会让客户端拿到
  一个本服务这边查不到的标识。
- **不做动态重试次数。** 幂等边界的处置是「不重试」，不是「少重试几次」。
- **不加连接池指标。** 出站连接数的观测属另一条轴（需要新增列与管理面暴露）。
- **不改 `isEventStream` 的 SSE 默认。** 它在 `Content-Type` 缺失时按 SSE 处理
  是刻意的，与本节的解码失败归因是两件事。
- **不给 `SideEffectRisk` 做「上游声明了无副作用」的放宽通道。** 四个协议
  都没有这个声明，字段留着这个可能性就够了。
- 调度层与配置中心零改动。

### 6.24 客户端意图与出站寻址的保真

本节的三条共性是：请求被正常处理、状态码正常、客户端拿到了内容，而丢掉的
东西显形在别处——客户端明确表过的态被当成没提，模型名里的一个字符改掉了
请求实际打到的 URL，或者一次请求的总耗时没有任何上界。

#### 6.24.1 `include_usage` 的三态

Chat Completions 的 `stream_options.include_usage` 决定流末尾那一帧单独的
usage（`choices` 为空数组、只带 `usage` 的那一帧）发不发。本服务此前在解码
阶段就把整个 `stream_options` 丢掉，于是这一维只有一种行为：恒发。

现在区分三态：

| 客户端写法 | 中立表示 | 发那一帧吗 |
| --- | --- | --- |
| 没给 `stream_options` | `nil`（没提） | 发 |
| `"stream_options":{"include_usage":true}` | `true` | 发 |
| `"stream_options":{}` 或 `include_usage:false` | `false` | **不发** |

「给了 `stream_options` 但没写 `include_usage`」判成明确的 false 而不是没提：
JSON 的零值就是这个字段的语义。三态用指针而不是 bool——没提与明确 true 在
当前行为上相同，合并后就没有位置表达「客户端明确要」，将来若要改默认，那
两种必须分开。

压制**只压那一帧**：带 `finish_reason` 的空 delta 与 `[DONE]` 照发，它们是
协议的终止形状，压掉会让客户端等一个永不到来的结束。压制**不记有损**——
这是照客户端的要求执行，不是丢了它要的东西，记成有损会在流水里留下一条
永远解决不了的诊断。

**对上游一律要 usage**，与客户端的表态无关：记账要用上游报的那个数，把
客户端的 false 透传上去会让流水里的 token 数恒为估算值。

另外三个入站协议没有这个开关，也不为它们合成一个：Anthropic 的 usage 挂在
`message_delta` 上、Responses 的挂在 `response.completed` 上，都是协议固有
形状而不是可选帧。

#### 6.24.2 Gemini 出站寻址里的模型名

四个协议里只有 Gemini 把模型名放进 URL 路径
（`{base}/models/{model}:streamGenerateContent?alt=sse`），因此「模型名里的
字符改变了请求实际打到哪」这个缺口也只有它有。本机探针实测原先的字符串
拼接经 `http.NewRequest` 解析后的结果：

| 模型名 | 解析出的 path | 解析出的 query |
| --- | --- | --- |
| `gemini-2.5-pro` | `/v1beta/models/gemini-2.5-pro:streamGenerateContent` | `alt=sse` |
| `gemini?x` | `/v1beta/models/gemini` | `x:streamGenerateContent?alt=sse` |
| `gemini#x` | `/v1beta/models/gemini` | **（空）** |
| `gemini x` | `/v1beta/models/gemini x:streamGenerateContent` | `alt=sse` |
| `../v1beta/models/other` | `/v1beta/models/../v1beta/models/other:...` | `alt=sse` |

最坏的一格是 `#`：`alt=sse` 整个消失，上游于是回一整份 JSON 而不是 SSE，
落进「HTTP 200 但一帧都没解出来」那条**可重试**的路——三个同名目标会连挂
三次，而运维看到的是三个账号同时坏掉。空格不会让 `NewRequest` 报错。

处置分两类：

- **转义**（`url.PathEscape`）：`?`、`#`、空格这类会被 URL 解析吃掉的字符。
  不列字符白名单——模型名的合法取值由上游定义，白名单只会在上游上新模型时
  误拒。
- **拒绝**：含 `/` 或 `\` 的名字，以及整串就是 `.` 或 `..` 的名字。
  `PathEscape` **不编码** `.` 与 `/`，所以穿越修不了只能拒。斜杠一律拒而不是
  逐段放行：Gemini 模型名里从来没有斜杠，逐段放行等于替上游发明一套路径语法。
  `gemini-2.5-pro` 里的点照常放行——只有整段就是点号时才是穿越。

拒绝时 `Endpoint` 返回空串（签名不变，它不返回 error），数据面把空 URL 变成
一次**可重试**的上游错误：换目标会换模型名，下一个可能是配对的。错误文案
不回显模型名——它会回到客户端手里，而流水的 `model_id` 已经记了它。

`base_url` 配成以 `/models` 结尾时不再拼出 `/models/models`，但**只削一层**：
配成 `.../models/models` 的人要的就是那个路径。

#### 6.24.3 一次请求的总时长上限

`MSA_FIRST_TOKEN_TIMEOUT` 与 `MSA_IDLE_TIMEOUT` 各自只管**一次尝试**，
`MSA_MAX_ATTEMPTS` 次尝试累起来的最坏情形是它们的倍数，而客户端与反代看到的
是那个总数。新增 `MSA_MAX_REQUEST_DURATION` 约束它，**默认 0 表示不限**——
既有部署没配过这个变量，默认收紧会把正常的长生成打断。

上限从客户端请求的 ctx **派生**而不是另起一个：另起会让客户端断开不再传导到
上游读取，一个已经没人要的请求会把重试跑完，而每次重试都是一份上游账单。

启动时校验：负值拒绝（并点明 0 才是关闭的写法）；非 0 时不得小于
`max(MSA_FIRST_TOKEN_TIMEOUT, MSA_IDLE_TIMEOUT)`——小于它的话每个请求都在
同一时刻被总上限掐断，重试与单次超时全部失效，而失效方式看上去像上游整体
故障。恰好等于那个下限是允许的。

#### 6.24.4 明确不做

- **不改响应 `model` 的语义**：它是上游回传的模型名而不是用户模型名，这在
  修订 2.0 已作为刻意决定成文，不是缺口。
- **不采集客户端来源 IP**：客户端身份归调度层，数据面不做入站鉴权。
- **不给另外三个入站协议合成 `include_usage`**：它们的 usage 是协议固有形状。
- **不做模型名取值白名单**：合法取值由上游定义。
- **不做 per-attempt 的时长上限**：单次尝试已由首字与空闲两个超时管住。

### 6.25 责任归属：本服务自己的决定不得伪装成别人的

三条修复的共性不是「请求失败了」，而是**责任被记到了错误的一方**。三者的
状态码与响应体都正常，坏掉的东西只显形在运维视角里。

#### 6.25.1 请求预算到期的归因

`MSA_MAX_REQUEST_DURATION`（见 [6.24.3](#6243-一次请求的总时长上限)）是从
客户端 ctx 派生的，到期后 `ctx.Err()` 与客户端按下停止键**完全同形**。而
桥接层此前用 `ctx.Err() != nil` 作为「是谁断的」的唯一判据，于是：

- 提交后到期 → 记成客户端取消（`error_code=canceled`、`outcome=normal`），
  而那一类**刻意不计目标失败**也不算异常。客户端拿到一个残缺的流且没有任何
  错误帧。
- 提交前到期 → 底层是 `DeadlineExceeded`，落到上游兜底分支归成
  「上游不可达」，**累计一个完全健康的目标的失败计数**把它推向冷却。

两种形态合起来的效果是：一个配得过紧的预算会静默截断所有长生成，而它在所有
指标上都长得像「用户爱按停止键」加「上游偶发抖动」。

现在预算到期用 `context.WithTimeoutCause` 挂一个哨兵，判定走
`context.Cause` 而不是 `ctx.Err()`。归因是一个**不可重试**的超时类错误，
文案点名 `MSA_MAX_REQUEST_DURATION`。不可重试的理由：预算到期恰恰意味着
没有时间再换一个目标了，标成可重试会让最后那点预算全花在注定超时的重试上。

三个判定点（桥接层的 channel 关闭分支、桥接层的读错误分支、建流时的请求
失败分支）都**必须排在既有归因之前**——预算到期时 `ctx.Err()` 也非 nil，
顺序反了这条分支永远走不到。

#### 6.25.2 `overrides` 不得改写本服务算出来的值

参数覆盖作用在出站编码**之后**的 wire body 上（见
[6.4](#64-跨协议能力差异)），而它此前对键没有任何保留集。于是目标配置里的
`overrides` 能压掉两个本服务自己算出来的值：

| 键 | 谁算的 | 被压掉的症状 |
| --- | --- | --- |
| `model` | 编码前换成目标的 native model | 请求打到另一个模型并按它计费，而流水、上报与逐次轨迹三处记的都是调度层派的 `model_id`——事后对不出账 |
| `stream` | 三个出站编码器写死 `true`（对上游一律流式是桥接层与聚合器的前提） | 上游回整份 JSON，逐字输出消失，而诊断里只有一句「上游忽略了流式请求」，把配置错误归因给了上游 |
| `stream_options` | 记账要用的 usage 请求 | 流水的 token 数恒为估算值 |
| `alt` | Gemini 的 SSE 开关 | 与 `stream` 同形 |

现在这四个顶层键落在保留集里：`overrides` 命中时**跳过**并记一条有损说明，
点名键与后果。

- 跳过而不是报错：报错会让一个配错的键把整个目标变成死路，而 `overrides`
  里绝大多数键是正当的。
- **只挡顶层**：这四个键在四个协议里都在顶层，下钻挡会误伤
  `generationConfig` 之类嵌套对象里合法的同名键。
- **`defaults` 不受此约束**：它只在键缺失时填，构不成改写；对它也设保留集
  会把「上游端点要求显式写 `alt`」这类正当配置一起拒掉。
- 说明里**不拼值**，与出站请求头黑名单同口径。
- 保留集不做成可配置：能配就能关，而关掉它就回到现状。

#### 6.25.3 错误终态也要回传限流头

[6.23.5](#6235-限流头的回传白名单) 引入的限流头回传只挂在两个成功路径上
（流式提交与非流式成功），而非 2xx 分支根本不构造上游句柄——于是**所有错误
终态上一个限流头都没有**。最需要这族头的那一刻（429）恰好是它们缺席的那
一刻：客户端 SDK 靠剩余量自适应节流，拿不到就只能靠 `Retry-After` 硬等，
而上游沉默时那个头也不发。

现在可回传的头随 `ir.Error` 带到终态（一个不序列化的字段，既不进流水也不进
上报），由错误写出点在 `WriteHeader` 之前发出。挂在错误上而不是给写出函数
多传一个上游句柄：错误从建流到终态之间经过三层调用与五条终止分支，多传参数
意味着五条分支都要跟改而漏一条不会变红，而错误值本身已经贯穿全程
（`retry_after` 与副作用风险位走的就是这条路）。

白名单沿用同一个（`anthropic-ratelimit-*`、`x-ratelimit-*`），`Retry-After`
仍只由本服务现算那一处产出。重试后失败时，头来自**最终那次**尝试。

#### 6.25.4 明确不做

- 不新增环境变量：三条都没有需要运维调的旋钮。
- 不给 `overrides` 做值级校验（只挡键）——取值合法性由上游定义。
- 不为预算到期新增 `outcome` 类别：`abnormal` 已足够，新增会让调度层跟改。
- 不做全量响应头转发（[6.23.5](#6235-限流头的回传白名单) 已定它会外泄凭据面）。

### 6.26 推理签名与流内错误的往返载体

三条修复的共性是**上游交出来的东西在中立表示里没有落点**，于是它在转换途中
消失。三者的状态码与响应体都正常，缺的那一维只在下一轮（签名）或运维视角
（归因）里显形。

#### 6.26.1 responses 收尾条目上的推理签名

多数 responses 形态的上游只在 `response.output_item.done` 上挂
`encrypted_content`，`added` 那一帧里没有。而解码器此前只在 `added` 上读它，
于是签名整条丢失——块已被 `done` 正常闭合，完整性判据（只看未闭合的块）
因此既不报 `incomplete_stream` 也不出任何说明。客户端把无签名的思考块存进
历史，下一轮推理项接不上，而两轮之间没有任何线索。

现在 `itemDone` 在闭块之前补发一条签名增量事件，来源填本协议名。三点约束：

- **签名必须排在 `block_stop` 之前**：之后到的签名增量聚合器不收（那个块已
  不在 open 列里），会静默丢掉。
- **两帧都给时只发一条**：聚合器对签名增量是**累加**的（`+=`），发两条会把
  两份密文拼成一段上游解不开的垃圾——那比丢一份更坏。解码器因此记住每个块
  已拿到过的签名（`slot.sig`）。
- **两帧给了不同的密文时留说明**：不发第二条也不静默取一份。这种形态说明
  上游行为超出预期，运维得看见它；值仍取先到的那份。

不做「done 那份覆盖 added 那份」——聚合器层面没有覆盖语义（见上一条），
要做覆盖就得改聚合器对所有协议的签名处理，而那会动到已验证的三协议路径。

#### 6.26.2 gemini 函数调用自身的推理签名

本协议把 `thoughtSignature` 挂在 `functionCall` part 上，而不是 `thought`
part 上。中立表示此前只有思考块那一位，于是这一处的签名在解码时就没有落点,
整条丢掉，既不报错也不出说明。工具回合恰恰是最需要它的场景：下一轮要带着
签名回去，上游才认这次调用的推理过程。

现在中立表示的工具调用带**独立的**签名与来源两位（不复用思考块那一位：一条
响应里可以既有思考块的签名又有若干次调用各自的签名，它们分别对应上游的不同
状态，混在一处就对不回去），新增能力位 `ToolCallSig`（只有本协议为真，与
`ThinkingSig` 分开——两者是上游的不同状态，一个协议可以只有其中一个）。
流式与非流式两条解码路径各自读它，出站编码把**本族**签名回写到 part 自身。

**这一轮的交付物是「有记录的丢弃」而不是「往返可用」**。本协议是纯出站的：
它只注册出站编解码器，`/gemini/v1/*` 返回 404（没有客户端需要本服务扮演
gemini），所以不存在 gemini 入站的完整往返。实际价值是两件：

- 三个入站协议回客户端时为这一位出剥离说明（它们的 `tool_use` 都没有对应
  字段，所以与来源无关一律剥离）。运维查一次跨协议工具回合的质量下降时，
  有损列里能看到「上游给过推理凭据，本协议装不下」。
- 载体就位：将来真要支持 gemini 入站时不必再改中立表示。

说明的措辞与思考块那条**刻意不同**（`tool call reasoning signature` 对
`thinking signature`）：一条响应里两处都可能丢，措辞相同的话运维看不出丢的
是哪个。

#### 6.26.3 流内错误优先于随后的断流

上游先用一个流内错误帧说明原因、再把流断掉，是限流与配额耗尽的常见形态。
而桥接层的两条拆流分支此前都只报兜底错误（帧 channel 干净关闭报「流没有
终止符就结束了」、scanner 报读错误报「upstream stream broke」，两者 kind
都是 `upstream`）。于是一次流内 429 被记成**目标故障**并累计它的失败计数，
把一个只是需要退避的健康账号推向冷却；运维看到的是一次上游抖动。

现在归因的单一决策点接收流内错误，判定顺序是：

1. 本服务的预算到期 → 记成预算超时（这是我们自己的决定）
2. 客户端自己走了 → 不记成任何人的故障
3. **上游在流内说明过原因 → 那个原因就是根因**
4. 都不是 → 兜底归因

第 3 步排在 1、2 之后：本服务放弃或客户端离开时责任不在上游，哪怕上游此前
确实说过什么。两条拆流分支共用同一个决策点——上游被掐断时走哪条取决于读
协程当时停在哪里，是竞态，两处各判一遍的话其中一处判错只在另一次运行里
才暴露。

#### 6.26.4 明确不做

- 不新增环境变量或开关：三条都没有需要运维调的旋钮。
- **不合成也不猜签名**：上游没给就是没给，凭空造一个会让上游拒整轮。
- 不解密 `encrypted_content`：本服务不持有密钥，它对我们是不透明字节。
- 不把工具调用签名做跨协议翻译：把别家的密文发过来上游会拒整轮。
- 不扩大完整性判据（`UnsafeToClose` 仍只看未闭合的块）：`done` 正常闭合的
  块不是残缺块，把「缺签名」并进去会让一批可用响应被判成不可用。
- 不为流内错误新增 `outcome` 类别或流水列：分类已能表达它。
- 调度层与配置中心零改动。

### 6.27 边界处的静默削减

第三十六轮修的四处共性：值在中立表示里存在且算对了，但跨过一道边界时丢掉一个维度，而两侧都不报错。

#### 6.27.1 usage 的五个维度跨仓不再截断

`ir.Usage` 有五位（输入、输出、缓存读取、缓存写入、推理），而跨进程契约 `relayclient.Usage` / `relayv1.Usage` 与 `request_log` 表此前只有前三位。后两位在数据面算对了，写进契约那一刻消失，管理面明细与调度层上报都看不到。

现在契约与表都是五位：

| 字段 | JSON 键 | 类型 | 含义 |
| --- | --- | --- | --- |
| InputTokens | `input_tokens` | int64 | 输入计费词元 |
| OutputTokens | `output_tokens` | int64 | 输出计费词元 |
| CacheReadTokens | `cache_read_tokens` | int64 | 命中缓存的输入词元 |
| CacheWriteTokens | `cache_write_tokens` | int64 | 写入缓存的输入词元 |
| ReasoningTokens | `reasoning_tokens` | int64 | 推理（思考）词元 |

`request_log` 新增 `cache_write_tokens` / `reasoning_tokens` 两列，经表尾 `ADD COLUMN IF NOT EXISTS` 回补到已存在的库。

**契约五位、运行态三位是刻意的，不是漏了。** 调度层 `runstate` 仍只累计前三位：缓存写入与推理的单价与输入输出不同，直接加进同一组累计列等于用错的权重记账，而加权需要定价模型（在 upstream 配置中心）。relay 侧有一条测试钉住这个决定，要改它得先读那条测试的理由。

旧版本的上报只带三个键仍可正常解码——两个服务独立发版，拒收会让上报整条丢失，目标的失败计数永不清零，从而被错误地冷却。

#### 6.27.2 媒体嗅探容忍折行与 URL-safe 字母表

`SniffMediaType` 在未声明 media type 时读 base64 载荷的头部魔数。此前直接把头部丢给 `base64.StdEncoding`：

- 载荷按 76 列折行（多数 SDK 的默认）→ 解码报错 → 返回空串
- 载荷用 URL-safe 字母表（`-` `_` 代替 `+` `/`）→ 同样报错

空串随后让 `Capabilities.AcceptsMedia` 恒假，整块媒体被 `DowngradeMedia` 换成文本备注。客户端上传了一份完全可解码的图片，模型什么都没看到，而日志里只有一条有损说明。

现在剥空白与 URL-safe 回落都做了，且剥空白是边扫边剥而非先截后剥——后者在窗口内遇到折行会少解出 3 字节，刚好够不到 RIFF 容器第 8-11 字节的格式标记。顺带修掉一个既存缺陷：RIFF 是容器而不是格式，只看前四字节会把 webp **图片**判成 `audio/wav`，于是 `MediaKindFor` 归到音频块、出站按音频编码。

判不出类型时仍返回空串：容错不等于什么都认。

#### 6.27.3 data URI 的参数列表按末位取 base64 标记

`data:` URI 的参数可以有任意多项，`base64` 恒在末位。此前只切第一个分号，于是合法的 `data:image/png;charset=utf-8;base64,...` 拿到 `charset=utf-8;base64` 去比 `base64`，比不上，整段内联图片被当成远程链接塞进 `Media.URL`——上游去拉一个几百 KB 的伪 URL，或者被降级成文本。

现在取最后一个分号后的段，并且大小写不敏感（`BASE64` 也认）。两种不认的形态保持拒收：`data:base64,...`（base64 占的是 media type 位，载荷并不是 base64）与末位参数不是 base64 的（如 `data:text/plain;charset=utf-8,hello`）。

chat_completions 与 responses 各留一份解析代码是刻意的：合并要把「本协议用单个字符串同时表达内联与远程」这个 wire 层细节上提到中立层，而 anthropic 与 gemini 的 wire 本来就分开给出。一致性由一张共用的跨协议判据表顶住。

#### 6.27.4 心跳间隔与流超时的联合校验

心跳只在流式读循环里发。`MSA_HEARTBEAT_INTERVAL` 不短于 `MSA_FIRST_TOKEN_TIMEOUT` 与 `MSA_IDLE_TIMEOUT` 中较小的那个时，一个心跳都发不出——请求先被超时掐断。那是「配了保活却等于没配」，而运维完全看不出来。

现在这种组合**拒绝启动**并在错误里给出三个变量的名字与实际值：

```
MSA_HEARTBEAT_INTERVAL (1m30s) must be shorter than both MSA_FIRST_TOKEN_TIMEOUT (1m0s) and MSA_IDLE_TIMEOUT (2m0s); otherwise no heartbeat is ever sent and the keepalive is silently off
```

取两者较小者而非较大者：空闲超时比心跳还短时，静默期里同样一个都发不出。恰好相等也拒——谁先到取决于调度，这种配置没有存在的理由。负值或零表示显式关闭保活，跳过这条校验。


### 6.28 调参取值范围的跨协议收敛

第三十七轮修的是一处**维度缺失**而非取值错误：`Capabilities` 有二十多个「能不能承载这个字段」的布尔位，也有计数上限（`MaxStopSequences`）与长度上限（`MaxToolIDLen`、`MaxPayloadBytes`），但**没有任何取值范围维度**。

字段四个协议都能发，问题在同一个数字在不同协议里一个合法、一个是不可重试的 400：Anthropic 的 `temperature` 上限是 1，OpenAI 习惯里是 2。客户端发 `temperature: 1.5` 是完全合法的入站，被调度到 Anthropic 目标才出问题，**而客户端无从预知这一跳会落到哪个协议**——目标是调度层按额度与健康度选的。四个出站编码器此前全都原样透传。

上限的证据是上游 400 原文：

```
{"error":{"message":"temperature: range: 0..1"}}
```

`Capabilities` 新增一维：

| 字段 | 类型 | 零值含义 | 当前取值 |
| --- | --- | --- | --- |
| MaxTemperature | float64 | 不设限且跳过检查 | anthropic 1.0；chat_completions / responses / gemini 均为 0 |

**只有 anthropic 填了值，其余三个留零值是刻意的。** OpenAI 常说的 2.0 与 Gemini 各维上限都没有实测或官方明示的确证，而猜出来的上限会把本来能过的请求改坏——那比偶发的 400 更糟，它无声地改变输出的随机性。这与 `MaxToolIDLen`、`MaxPayloadBytes` 当前全为 0 的判据是同一条。有了实测证据再落值；测试里有一条断言钉住这张表，加值时它会红。

夹紧而不是拒请求：拒掉等于把调度层的内部选择变成客户端的错误。夹紧改变了输出的随机性，所以报一条有损说明让调用方看得见：

```
rewrote temperature: 1.5 exceeds this protocol's maximum 1, clamped
```

下界 0 与上界同出一处证据（`range: 0..1` 两端都在这句话里），所以不另设能力位，只在声明了上限的协议上一并生效。取值**正好等于**上限时不动手——原文是闭区间，改它是白报一条说明。取值 0 也不动手：0 是贪心解码这个明确语义，指针字段区分得了缺席与零值。

阶段顺序：范围夹紧排在采样参数互斥**之后**。anthropic 开推理时会把 `temperature` / `top_p` 整个剥掉，夹紧排在前面就会先夹一个马上要被丢弃的值，白报一条说明并让调用方以为参数还在。有一条测试专门钉这个顺序。

**明确不做**：

- 不给 `top_p` / `top_k` 设范围：四家协议的 `top_p` 都是 0..1、本服务没有超出这个区间的入站来源，`top_k` 的上限无确证。
- 不照搬 new-api 的 `TopP >= 1 → 0.99`（`relay/channel/zhipu/adaptor.go`、`perplexity/adaptor.go`）：那是 zhipu 与 perplexity 的**厂商特例**，不是协议约束，升格成协议维度会把四个协议都改坏。
- 不做 new-api 的按模型名分档能力表（`relaykit/dto/openai_request.go` 的 `SupportsTemperature`）：本服务的模型清单由配置中心给出，把模型名硬编码进转换层会与它漂移。
- 不在入站侧校验取值：入站按客户端自己的协议是合法的，在那里拒等于替目标协议提前报错。
- 调度层与配置中心零改动。


#### 6.28.5 合并相邻同角色消息时的分隔符

`ir.Sanitize` 把相邻的同角色消息合并成**一条消息、多个文本块**（Anthropic 硬性要求 user/assistant 交替，而相邻两条 user 在 Chat Completions 里完全合法）。但三个出站协议的文本拼接是**无分隔的**：responses 的 `instructions`、gemini 的 `systemInstruction`、chat_completions 的字符串形态 `content` 都是直接相加。

于是客户端发两条 user 消息「订单号 10086」与「3 件退货」，上游模型收到的是 `订单号 100863 件退货`——模型算错、答错，全程 200，没有任何说明提示内容被改过。

现在合并边界插一个 `"

"` 文本块。分隔符写在合并点而不是各编码器里：这处相邻是合并制造的，而客户端自己在一条消息里写的多个文本块属于目标协议的语义，不该由我们加料。

三种情形不插：边界任一侧不是文本块（其余组合在出站编码里各自成块或成条，不会被拼进同一个字符串）、任一侧文本为空、前段已以空白结尾或后段以空白开头（客户端自己留了分隔，再加一个空行是改它的排版）。

#### 6.28.6 工具失败态在重入时可恢复

`chat_completions` 与 `responses` 没有 `is_error` 这样的原生字段，失败态是本服务出站时写进正文的前缀 `[tool error] `（见 [6.14](#614-响应侧语义维度的保真)）。此前这两个协议的**入站解码完全不认这个前缀**。

后果是一条闭环：anthropic 客户端 → chat_completions 上游（写入前缀）→ 客户端把整段历史回传 → 再路由到 anthropic 或 gemini，`is_error` 读作 false，模型被告知工具调用成功，而正文写着 `[tool error] connection refused`。多跳后前缀还会叠成 `[tool error] [tool error] …`。计费照常，无报错。

现在两处入站解码都认回前缀并**剥掉**它：留着等于把同一件事说两遍，且下一跳若又落到无原生字段的协议会再加一层。只认块首那一处——正文中间出现同样的字面量是工具自己打的内容（比如它在转述一条日志），改写它会篡改工具输出。

#### 6.28.7 推理预算按协议的有效上限夹紧

[6.13](#613-工具意图的保真) 落地的预算夹紧以客户端显式给出的 `max_tokens` 为条件，而 anthropic 的 `DefaultMaxTokens`（4096）是在整形**之后**由编码器补的。于是「不给 `max_tokens` + 大 budget」这条路径整个逃过夹紧：出站成 `max_tokens=4096` / `budget_tokens=60000`，拿到上游 400 `budget_tokens must be less than max_tokens`。

这是参数错误而非 5xx，重试与换目标都救不回来，且归因指向上游而不是我们。

现在整形阶段先用 `MaxTokensFor` 解析出**有效**上限（客户端给了就用它，没给且协议必填就取协议默认值），再据此夹紧；`MinThinkingBudget` 的无解判定同样改用有效上限。客户端给了数字时仍以它为准——按默认值判断会在「客户端要短回答」时用一个更大的上限，漏掉真实冲突。

#### 6.28.8 gemini 的 stop 序列上限

`shapeParams` 的截断逻辑一直存在且正确，但 gemini 只声明了布尔支持、没声明上限（Gemini 的 `stopSequences` 上限为 5，new-api 在 `relaykit/relayconvert/internal/oai_chat/to_gemini_chat_req.go:52-53` 显式截到 5），截断因此不触发。

客户端给 8 个 stop 序列时，chat_completions 路由被截到 4 并出说明，gemini 路由原样透传 → 400 `INVALID_ARGUMENT`。同一请求换上游即成功，用户看到的是「只有 Gemini 坏了」。

`MaxStopSequences: 5` 已补上。四协议现状：anthropic 无公开上限（留零值）、chat_completions 4、responses 无此字段、gemini 5。截的是尾部：前面的序列是客户端最先声明的，保留它们更可能命中它真正关心的那几个。


### 6.29 出站请求体的结构合法性

前面几轮治的是**取值**（范围、计数、长度上限）与**语义**（角色归置、失败态、签名归属）。第三十八轮治的是**结构**：请求体的形状本身被上游拒收，或某个结构维度在编码时被悄悄碾平。四条落差的共性是**全程 200 之前就已经错了**——要么拿一个不可重试的 400，要么带着残缺内容成功返回。

四条都由一次性探针在本仓实测取证，并在三个参考实现里找到对应处理。

#### 6.29.1 工具 schema 的方言从黑名单改白名单

`schemadialect.Dialect` 此前只有 `Drop` 一张黑名单，列了 12 个标量约束键。它漏掉的是**结构性**关键字：`$ref`、`$defs`、`definitions`、`oneOf`、`allOf`、`prefixItems`。这些键不但没被剔除，`$defs` 与 `oneOf` 还在下钻表里——变换会走进它们的子树，把里面的 `type` 大写化，然后原样发出去。

探针输出（改之前）：

```json
{"parameters":{"$defs":{"X":{"type":"STRING"}},
 "properties":{"a":{"$ref":"#/$defs/X"},"b":{"oneOf":[{"type":"STRING"}]}},
 "type":"OBJECT"}}
```

Gemini 的 `parameters` 只接受 OpenAPI 3.0 子集，表外字段直接 `400 Invalid JSON payload ... Cannot find field`。更坏的是 `DroppedKeys` 为空——**连一条有损说明都报不出来**，排查的人看不到任何线索，只看到一个来自上游的格式错误。

`Dialect` 新增 `Allow`：

| 字段 | 类型 | 零值含义 | 语义 |
| --- | --- | --- | --- |
| Allow | []string | 不按白名单收敛 | 非空时表外关键字一律剔除 |

`Allow` 与 `Drop` **并存**而非取代：`Drop` 表达「这个键我明确知道不行」，`Allow` 表达「这个表之外的我都不认」。黑名单永远追不全，而白名单的失效方向是「误剔一条约束并报说明」，比「原样发出拿 400」轻。

剔除**排在下钻之前**。顺序反了的话，`$defs` 被剔掉后仍会下钻进那棵即将消失的子树，把子树里的键也记进 `DroppedKeys`——说明就再也指不出真正被削掉的是哪一条约束。

gemini 的白名单是 17 键：`anyOf`、`default`、`description`、`enum`、`example`、`format`、`items`、`maxProperties`、`maximum`、`minProperties`、`minimum`、`nullable`、`pattern`、`properties`、`propertyOrdering`、`required`、`type`。

表内刻意**不含** `title` 与 `minLength`/`maxLength`/`minItems`/`maxItems`：new-api 的白名单（`relaykit/relayconvert/internal/shared/gemini/schema.go:9-32`）放行这五个，而我们原来的黑名单剔除它们且上线未见问题。两边冲突时不动已经跑通的行为——放行可能换来一个 400，剔除只丢一条约束且已有说明。拿到能发请求的账号后再实测，参考实现的做法不构成改它的证据。表内也不含 `oneOf`/`allOf`：本协议只接受 `anyOf` 一种联合写法。

#### 6.29.2 enum 成员按目标协议的成员类型归一

`enum` 在「绝不下钻」表里——那张表防的是「把用户数据当子 schema 走一遍」（一个恰好叫 `additionalProperties` 的实例值不该被删）。但不下钻不等于不看：Gemini 的 `Schema.enum` 是 `string[]`，`{"type":"integer","enum":[1,2]}` 原样发出去拿到 `Invalid value at 'enum[0]' (TYPE_STRING)`。

`Dialect` 新增 `StringEnumOnly bool`（仅 gemini 为真）。逐成员转成 JSON 字面量的字符串形态：`1` → `"1"`、`true` → `"true"`、`null` → `"null"`。数字用 `strconv.FormatFloat(v, 'f', -1, 64)` 而不是 `fmt.Sprint`——后者对 `1000000` 输出 `1e+06`，与原 schema 里的字面量对不上，模型按字符串匹配可选值就全都匹配不到。

成员是对象或数组（转不成标量字面量）时**整条删除** `enum` 并报进 `DroppedKeys`。留一个半截的枚举比没有约束更坏：模型会以为可选值只有能转的那几个。

已经全是字符串的 `enum` 不触发改写，请求体逐字节不变——否则每个带枚举的请求都会打掉上游的 prompt cache 前缀。

参考 sub2api `gemini_messages_compat_service.go:3671-3691` 的 `normalizeGeminiEnum`，取的是同一条处置。

#### 6.29.3 消息序列被清空后出站不得是 null

`ir.Sanitize` 会整条删除消息（丢掉无人应答的 `tool_use` 后那条 assistant 消息空了、`pruneEmpty` 删掉空消息）。此前清空后没有任何复查，四个出站编码器各自发出：

| 协议 | 字段 | 改之前的出站值 |
| --- | --- | --- |
| anthropic | messages | `null` |
| chat_completions | messages | `null` |
| responses | input | `[]` |
| gemini | contents | `null` |

四者上游都报字段缺失或类型错的 400。只有 system 没有任何一轮对话的请求同样落进这里——那是完全合法的入站。

归因尤其坏：清空是**我们自己的修复动作**造成的，而错误却来自上游、指向客户端的请求体。客户端看到一个它无法复现的 400。

`ShapeRequest` 新增一步 `ensureNonEmptyMessages`：消息序列为空时补一条最小用户消息，正文是 `(continuing the conversation)`，并报一条有损说明。

**补而不是拒**（new-api `relay/helper/valid_request.go:295-296` 走的是拒收）：按本服务已定的「只报有损，从不拒请求」——客户端无从预知我们会修到一条不剩。

占位文本与 anthropic 原有的「首条不是 user 时补前导占位」**合为一处**导出常量 `codec.ConversationPlaceholder`。两处各写一份字面量会在措辞调整时漂移，而这段文本会进提示词，漂移意味着两条路径打掉不同的缓存前缀。

补位只在序列为空时发生。已有消息时无条件插入会改变正常请求的前缀，同样打掉 prompt cache。

#### 6.29.4 工具结果里的媒体按协议承载力处置

工具返回截图是 agent 场景的常态。四个出站协议的工具结果载荷承载力不同，而此前没有任何一处认这件事：

| 协议 | 工具结果载荷 | 改之前的行为 |
| --- | --- | --- |
| anthropic | `tool_result.content` 块数组 | 原生装得下，正确 |
| chat_completions | `role:tool` 消息 | 编出 `image_url` part，上游按格式错误拒收整个请求 |
| responses | `function_call_output.output` 单字符串 | `joinText` 只取文本块，图片静默消失 |
| gemini | `functionResponse.response` 单键对象 | 同上，图片静默消失 |

后两者比 400 更难查：请求 200、响应正常、模型答错。模型被要求根据一张它从未看到的截图回答问题，而全程没有任何说明。

`Capabilities` 新增 `ToolResultTextOnly bool`（chat_completions / responses / gemini 为真，anthropic 为假）。它与 `Images` 是两件事：这三家在**普通消息**里都能带图，只有工具结果这一处装不下，混用会让两个判断变成同一个。

处置是**抽出改投**：把工具结果里的媒体块摘出来，作为紧随该工具结果**之后**的一条用户消息发出。模型仍能看到图，只是呈现位置从「工具的输出」变成「用户随后给的材料」。这是有损的，报一条说明。

抽出而不是转成文本描述：一句「这里原本有一张图」对模型毫无用处。

抽出逻辑集中在整形阶段（`codec.SplitToolResultMedia` + `moveToolResultMedia`）而不是三个编码器各写一份：三个协议的坏法不同，但正确的处置只有一个，写三份会漂移。anthropic 不设这一位，这一步对它是空操作——它的形态本来就是对的，动手只会弄坏。

同一条消息里多个工具结果的媒体**并进同一条**用户消息。拆成多条会让模型看到一串没有上下文的图，也会多出几轮空洞的角色交替。

改写落在请求的副本上（`ToolResult` 是指针，整形阶段先复制再改）——调用方那份请求要留着换目标重试。

参考 cc-switch `src-tauri/src/proxy/providers/transform.rs:495-524`（`plan_chat_tool_output_media` / `queue_chat_tool_output_media`）与 `:546-548`（"Chat tool messages cannot carry image parts"），取的是同一条处置。

#### 6.29.5 明确不做

- **`max_completion_tokens` 按模型族分支**（o-series / GPT-5 家族对 `max_tokens` 报 `Unsupported parameter`）：需要一张「哪些模型要新写法」的模型族表，而本仓刻意没有任何按模型名分支的设施，且该表会随上游上新持续腐坏。正确落点是配置中心的 `request_overrides`，不是 codec 里硬编一张名单。
- **gemini 的 `safetySettings`**：全仓从未赋值，零值即上游默认，不构成故障。
- **schema 深度上限从 32 提到 64**：32 已是真实 schema 两倍裕量，超限已有 `Truncated` 说明，没有实测证据表明不够。
- **`exclusiveMinimum` 在 INTEGER 上折成 `minimum`+1**（sub2api `incrementIntegralSchemaBound`）：该键本轮在白名单外，会被剔除并报说明；再补一层数值推算是替上游发明语义。
- 调度层与配置中心零改动。

### 6.30 记账与预算的内部一致性

前面几轮治的是**出站方向**：请求体的取值、语义与结构。第三十九轮往回看**本服务自己的账与预算**：四条落差全部发生在请求成功之后（或与请求成败无关），数据面一律 200，客户端拿到的东西完全正确——坏掉的只是**运维与调度层看到的数字**，以及一条能把请求收尾无限挂住的兜底路径。

共性是「没有任何一侧报错」：两个管理面视图各自自洽，只是互相对不上；估算值偏小但形态正常；入队挂住时连日志都不出。这类故障不会被告警发现，只会在有人对账时才浮出来，而那时往往已经积累了数周的偏差。

#### 6.30.1 观测面的用量维度补齐到五位

上游返回的用量有五维：输入、输出、缓存读、缓存写、推理。明细表（`request_log`）五列俱全，而实时摘要与趋势桶此前只带输入与输出两维。

少报的恰好是**计费权重最偏的那三维**：缓存写通常按 1.25× 计价，缓存读按 0.1×，推理 token 计入输出计费。于是 `/admin/requests`（走明细表）与 `/admin/live`、`/admin/stats`（走缓存）长期对不上，而两侧都不报错——看面板的人只能把差异当成自己看错了。

三处补齐：

| 层 | 类型 | 变化 |
| --- | --- | --- |
| 缓存摘要 | `cache.LiveEntry` | 新增 `cache_read_tokens`、`cache_write_tokens`、`reasoning_tokens` |
| 缓存桶 | `cache.Bucket` | 同三维，Redis 哈希新增 `cache_read`/`cache_write`/`reasoning` 三个字段 |
| 对外契约 | `agentv1.LiveEntry`、`StatBucket`、`StatTotals` | 同三维 |

三维**各自独立累计，不加权**。按单价折算需要配置中心的定价模型，把不同单价的维度加进同一个数等于用错权重记账，而错的方向随缓存命中率变化，事后无法拆回。

Redis 侧**不需要迁移**：`HIncRBy` 对不存在的字段按零起算，旧桶少三个字段读出来就是零。JSON 摘要同理——旧记录缺字段，解码后三维为零。两条都由测试钉住。

`Cache.Incr` 的签名从五个标量改成传整个 `relayclient.Usage`。此前一行里有六个 `int64` 参数，调错顺序编译器不报，而记错的是计费维度。传结构体让字段名对位。

#### 6.30.2 明细表的列与摘要的字段必须逐维相等

补齐三维只解决了「现在对得上」，不解决「下一次新增一维时还对得上」。用量维度在三层各自声明：明细表的列、缓存摘要的字段、对外契约的字段。任何一层漏一维，两个视图就重新分叉。

增加一条跨层断言：从 `schema.sql` 里解出 `request_log` 所有以 `_tokens` 结尾的列名，与 `cache.LiveEntry`、`agentv1.LiveEntry` 里所有 json 名以 `_tokens` 结尾的字段逐一比对，集合不等即失败。

从 schema 解列名而不是手写一份清单：手写的清单跟 schema 一样会漏，而漏的方向恰好一致（都忘了新增那维），于是断言与被测对象一起错。解析同时覆盖 `CREATE TABLE` 里的列与 `ALTER TABLE ADD COLUMN IF NOT EXISTS` 补上的列——老库走的是后一条路径，只扫前者会漏掉升级加的维度。断言里还有一条下限检查（解出少于五个列名即失败），防止正则失效后这组断言静默地什么都不测。

#### 6.30.3 落库失败在实时摘要上可见（三态标记）

PG 挂了不拦数据面，只是流水缺一条——这是刻意的取舍，不改。问题在于此前这个失败**只写一行 Warn 日志**，而 Redis 两路照常成功：`/admin/requests` 缺记录、`/admin/live` 有记录，长期不一致且没有任何暴露面。

`cache.LiveEntry` 与 `agentv1.LiveEntry` 新增 `log_persisted`：

| 取值 | 含义 |
| --- | --- |
| 缺省（`null`） | 没配 PG，未尝试落库 |
| `false` | 尝试过且失败——这条记录不在 `/admin/requests` 里 |
| `true` | 落库成功 |

**必须三态**。布尔的零值会让「没配 PG」（正常的测试与单机形态）与「落库失败」（故障）变成同一个值，而看面板的人分不出来。

不为此新增 `outcome` 类别：`outcome` 描述的是这次请求对客户端的结局，而落库是本服务自己的副作用，客户端那侧完全正常。把它并进 `outcome` 会让调度层跟改，且会把一次成功的请求记成异常。

#### 6.30.4 估算计入结构化输出的 schema

`ir.EstimateRequest` 此前累加系统提示、消息正文、工具的名字/描述/schema，漏掉两处：`ResponseFormat` 与 `ToolChoice`。

结构化输出的 schema 与工具的 schema 一样要进提示词，而它常有数千 token。漏掉它让带结构化输出的请求被**系统性低估**。调度层（model-surge-relay）把 `EstTokens` 透传进策略脚本的 `policy.Input`——本服务与调度层都没有内建的窗口闸门，所以低估不会直接导致选错目标，真实后果是**用户自己写的动态策略脚本拿到一个偏小的数**，而脚本作者看不出它偏小。

`ResponseFormat` 的 `Name` 与 `Schema` 都计入：两者都出现在出站请求体里，只算 Schema 会让「同一份 schema 换个长名字」的两个请求估出一样的数。

`ToolChoice` **只计名字**。`Mode` 是 `auto`/`any`/`none`/`tool` 的枚举，出站编成一个结构化字段而不是提示词文本；把枚举名也算进去等于凭空虚增。只有被强制指定的那个工具名会进提示词。

两个字段都是指针，nil 时不贡献任何 token——由测试钉住（裸请求的估算等于正文本身的量，不多不少）。

#### 6.30.5 兜底入队带自己的超时预算

上报的路径是「先直投，失败才入队」。此前入队复用了直投那个已经派生过超时的 `context`，而直投**往往正是因为超时才失败的**——那个预算已经耗尽，拿它入队必然立刻失败，比没有超时更坏：上报既没直投成功也没落进队列，只剩一行 `report lost` 日志。

而完全不设超时也不行。`Report` 跑在每请求上报队列的单消费者里，而请求收尾要等消费者排空。PG **hang 住**（不是拒连）时，无限等待的入队会让客户端越过它自己全部的预算（请求总时长、首字、idle）一直挂住，且服务端不报任何错。拒连那条路径无害——它立刻返回错误。

处置是给入队一个独立的 `context.Background()` 派生超时，长度沿用 `Timeout`（单条上报的超时）。两条断言各守一头：入队永远阻塞时 `Report` 必须在预算内返回；直投超时后入队拿到的 `context` 必须还是可用的（`ctx.Err() == nil`），而不是一个已耗尽的。

#### 6.30.6 明确不做

- **`relayclient` 的传输层加固**（自带 `http.Client` 绕过出站传输配置、`io.ReadAll` 无上限）：已核实成立，但那是出站连接层的事，与记账一致性不同轴，留作后续候选。
- **Redis 与 PG 的双写一致性**（两路写入之间进程崩溃会留下只有一侧有的记录）：需要引入两阶段或对账作业，代价远超收益——摘要本身是有 TTL 的观测数据，明细表才是账本。`log_persisted` 已把分叉暴露出来。
- **按模型定价对五维加权**：定价在配置中心，本服务不持有它。加权要么硬编一张会腐坏的价目表，要么每次记账都去查配置中心。
- **为落库失败新增 `outcome` 类别或流水列**：见 6.30.3。
- 调度层与配置中心零改动。

### 6.31 控制面的 HTTP 传输

出站数据面（[6.7](#67-出站连接层)）的传输层早已按环境变量装配，但**控制面**——本服务调调度层的那条链路——一直走的是裸 `http.Client{}`：`relayclient` 是全仓非测试代码里唯一一处。两者的隔离不是遗漏而是设计，`pipeline` 的传输构造注释原文写着它 `Clone` 标准库默认传输的目的正是「不污染 `relayclient`」，于是数据面每一轮加固都精确地绕过了控制面。

这条链路的调用密度与数据面同阶：**每一次**数据面请求先调一次 `/v1/dispatch`，换目标重试再调一次，收尾还调一次 `/v1/results`。它不可能是低频旁路。

依赖方向决定了不能复用数据面那套：`pipeline` import `relayclient`（`Pipeline.Dispatch` 的类型来自它），反向复用 `pipeline.NewHTTPClient` 会成环。因此控制面有自己的一份 `Options` 与常量，默认值刻意与数据面不同（见下）。

#### 6.31.1 连接复用

标准库的 `http.DefaultMaxIdleConnsPerHost` 是 **2**（本机探针实测）。裸客户端因此对调度层只留两条空闲连接：并发 8 个请求跑 2 轮共 16 次调用，实测**拨号 16 次、复用 0 次**——每一次 dispatch 都是一次全新 TCP 加 TLS 握手，而调度层是同一个 host。

现在控制面自带 `MaxIdleConnsPerHost` 默认 **32**、`MaxIdleConns` 默认 **64**、`IdleConnTimeout` 默认 **90s**。数值刻意低于数据面（那边是 32/256）：数据面横跨整个账号池的多个上游 host，总量卡死会让 PerHost 白设；控制面只有**一个** host，总量给到 64 已是 PerHost 的两倍余量。

零值取内置默认，两个超时取**负值**表示显式不设限——与数据面同一约定（`durationOrDefault` 的三分支：负→0、零→默认、正→原值）。

#### 6.31.2 超时分层

裸客户端只有一个 `Client.Timeout`，它覆盖到读完正文。控制面的响应都是小 JSON，但「响应头迟迟不来」与「正文读到一半卡住」在归因上是两件事，而单一总超时把它们压成同一个文本：本机实测 `Client.Timeout` 在读正文阶段命中时给出的是 `context deadline exceeded (Client.Timeout or context cancellation while reading body)`，与**调用方主动取消**逐字相同——排查的人分不出是调度层慢还是客户端走了。

现在两层各管一段：`ResponseHeaderTimeout` 默认 **10s**（只约束「请求发出 → 响应头到达」），`Client.Timeout` 默认 **30s**（整次调用的总时限）。10s 远低于数据面的 120s——数据面那一段要等上游生成第一个 token，控制面只是查一次目标选择。两者的顺序由测试钉住：头不来时必须在 3 秒内以 `ResponseHeaderTimeout` 失败，而不是等满 30 秒的总超时。

调度层不可达一律标 `Retryable`（网络抖动稍后可能自愈），但归因**不是** `target_unavailable`——那个码表示候选耗尽，会让重试循环停下。

#### 6.31.3 响应体字节上限

`io.ReadAll(resp.Body)` 此前没有任何上限。数据面早有三道（整份响应 32 MiB、上游错误体 64 KiB、入站 `MaxBytesReader`），控制面一道都没有：调度层若因故回一份失控的大体（一个把 SQL 结果整个吐出来的 bug、一个被插在中间的代理的错误页），本服务把它整个读进内存。

现在上限 **8 MiB**，读法是 `io.LimitReader(body, limit+1)` 后比长度——三个参考仓库都用这个 `limit+1` 形态，cc-switch 的注释把理由写得最直白：「先收完再比等于上限没有意义」。**上限必须在读的时候生效而不是读完再判**，这一条不能只断言错误消息：本轮变异验证里把 `LimitReader` 摘掉后消息断言照过（长度检查仍在后面），缺口是「把失控响应读进内存」这件事本身没被覆盖。补测的做法是**从对端侧量**：服务端分块 Flush 写 4 倍上限，客户端停读关连接后服务端写失败，断言实际写出的字节数不超过 2 倍上限（留一倍余量给 TCP 与 http 各层缓冲）。

**超限判定排在状态码分支之前**。顺序反了的话，一个带着失控大体的 500 会先进错误解码，运维看到的是 `relay returned 500: <html>…` 的 256 字节 snippet，而真正的异常——响应体失控——没有任何暴露面。超限**不可重试**：对端行为异常，再试一次它不会变小。

#### 6.31.4 不跟随重定向

标准库默认跟随最多 10 跳。控制面的对端是自家服务，任何 3xx 都是故障或有人在中间插了一跳，不是一次正常寻址。本机实测这条默认的后果：

- 302 把带 body 的 POST 改写成**无体 GET**；
- 同 host 跳转时 `Authorization` 被**保留**，客户端拿到 200 与目标伪造的 `{"models":[{"name":"evil"}]}`——即调度结果可被第三方替换；
- 跨 host 跳转时凭据被标准库删掉（`auth=""`），但请求仍然成功且响应体照样被采纳。

现在 `CheckRedirect` 一律拒绝，归因为**不可重试**的内部错误。

错误文本有一条不显然的约束：**不能拼包装后的 `err`**。标准库把 `CheckRedirect` 的错误包进 `*url.Error` 时，打进去的是**重定向之后**那个 URL 且含 query（实测 `Get "http://host/v1/models?leaked_key=…": relay returned a redirect`）——于是攻击者构造的 query 顺着错误消息进日志与流水。`*url.Error` 会遮 userinfo 里的口令，但不遮 query。处置是判 `errors.Is(err, errRedirect)` 后**只用哨兵自己的文本**，而哨兵文本里刻意不含任何 URL。设计阶段我曾假设标准库只打第一个请求的 URL，测试直接把这个假设打翻，代码随之改成现在这样。

#### 6.31.5 代理策略

`ProxyFromEnvironment` 是标准库默认传输的一部分，`Clone` 会把它一起带过来。实测它对 compose 内的服务名 `http://modelsurge-replay:18101` **生效**（走本机的 7897），对 `127.0.0.1`/`localhost` 不生效——也就是说 compose 形态下控制面的每一次 dispatch 都可能被一台外部代理看见并改写，而部署的人从不认为「内部服务调用」会出网。

现在 `MSA_RELAY_PROXY` 二选一：`off`（默认，`Clone` 带来的 `Proxy` 被显式清掉）或 `environment`（显式承认要走环境变量里的代理）。**非法值拒绝启动**并在错误里点名变量名——与 `MSA_CAPTURE_MODE` 同口径，静默回落会让运维以为自己配的那一档生效了。默认取 `off` 而不是保留标准库行为：控制面的对端在部署里总是内网地址，出网是例外而不是常态。

#### 6.31.6 明确不做（已核实，勿再提）

- **h2 ping 探测不给控制面**：数据面那两个参数（[6.9](#69-死连接探测与连接层归因)）针对的是长时间静默的流式连接，而控制面每次调用都是秒级往返，10s 的响应头超时已经覆盖了死连接。
- **不加 `InsecureSkipVerify` 开关**：sub2api 把它做成硬错误，本服务连开关都不给。
- **不做 DNS rebinding 校验**：拒绝重定向已挡掉「被引到别处」这一族；校验解析出的 IP 要维护一张内网网段表，而 compose 的服务名解析结果本身就在私网。
- **不做客户端缓存或按配置分池**：控制面只有一个对端、一份配置，一个进程一个客户端。
- **不做分片 transport**：同上。
- **响应体上限不做成可配**：8 MiB 对一份目标选择结果是三个数量级的余量，能配就能配坏。

## 9. 修订记录

| 版本 | 日期 | 变更 |
|---|---|---|
| 2.13 | 2026-09-19 | 工具意图的保真首次成文（[6.13](#613-工具意图的保真)）：四条修复，全在转换层内，无新增字段、无新增端点、无库变更。**行为变更一**：工具名被改写（非法字符/超长）时，改写此前只同步到消息历史里的 `tool_use`，**不同步** `tool_choice` 的具名——于是出站整形随后发现它指向一个未声明的工具并降级成 `auto`，客户端的「必须调这件工具」静默变成「模型自己决定」，上游正常回一段文本。参考实现 sub2api 在改名时同步三处（`gateway_tool_rewrite.go` 的 `tool_choice.name` 重写），本服务此前只做了两处。**行为变更二**：客户端同时给出 `thinking.budget_tokens` 与 `max_tokens` 且预算不低于上限时（合法的入站形状），此前原样出站，拿到不可重试的 400——换目标也救不回来。现在把预算夹到 `max_tokens - 1`；夹后低于协议下限则落到既有的「关掉推理」那一支，因此夹紧必须排在关推理之前。**刻意不照搬 sub2api**：它抬 `max_tokens`（`request_transformer.go` 的 `ensureMaxTokensGreaterThanBudget`，抬到 `budget + padding`），而 `max_tokens` 是客户端对成本与响应长度的约束，抬它是替客户端花钱，还会让「我只要 4096 个 token」回出更长的内容；预算只是「想多久」，调小它只降质量。**行为变更三**：Anthropic 的服务端工具（`web_search_20250305` 等，带非 `custom` 的 `type`）此前在入站解码时 `type` 被整个丢掉，于是它被当成普通函数工具发给任意目标——上游会等一个永远不来的工具结果，对话停住且不报错。现在 IR 的 `Tool` 带 `ServerType`（字段而非新类型：除 `type` 一处外它与函数工具的处理完全相同），新增能力位 `ServerTools`（仅 Anthropic 为真），承载不了的目标整条丢弃并报 `dropped tools[<名字>]`，丢弃排在 `tool_choice` 校正之前，且服务端工具跳过 schema 归一。**行为变更四**：Responses 与 Chat Completions 的入站解码遇到非函数工具此前静默 `continue`，客户端从响应里分不出「自己的声明被丢了」还是「模型不愿意调」。现在留一条 `skipped tool "X": unsupported type "Y"`，走**入站 `sanitized` 通道**（发生在解码期，与选了哪个目标无关）；Chat Completions 的 `type` 省略等同 `function`，不出说明。为此 IR 的 `Request` 增设内部字段 `DecodeNotes`（不上线、由 `Sanitize` 取走并清空，避免同一条说明被上报两次），并修掉 `Sanitize` 在空消息列表时的早返回——「只声明了工具、还没说话」的第一轮请求此前会丢掉解码说明。**明确不做**：不给非 Anthropic 目标合成服务端工具；不抬 `max_tokens`；不为服务端工具另立 IR 类型；不把跳过的声明降级成函数工具；不在出站侧重复报「跳过」；不做工具数量上限（四家协议无需本服务代为执行的硬上限，请求体字节预算已覆盖「声明太多」）；调度层与配置中心零改动 |
| 2.14 | 2026-09-19 | 响应侧语义维度的保真首次成文（[6.14](#614-响应侧语义维度的保真)）：四条修复，全在转换层与中立表示内，无新增端点、无库变更。四者同型——上游给出了一个语义维度，而中立表示或出站编码根本没有承载它的位置，于是它在转换途中消失，客户端收到一份 HTTP 200 却少一维的响应。**行为变更一**：`stop_reason` 为 `stop_sequence` 时，是哪一条序列触发的此前完全丢失（中立表示与 Anthropic 的线上结构都没有这个位置）。现在响应与流式收尾事件各增一维，Anthropic 双向读写；互斥约束（`stop_reason` 不匹配时必须为空）在解码与编码两侧各自执行——回填一条未触发的序列会让按它分段的客户端切错位置，比拿不到更坏。另三个协议不合成、**不报有损**：请求侧丢的是客户端给过的东西（要报），响应侧少的是客户端读不到的键（那是协议差异）。**行为变更二**：工具结果的 `is_error` 此前只被 Anthropic 与 Gemini 出站读取，Chat Completions 与 Responses 完全不读也不报——模型把一次失败的工具调用当成功，既不重试也不致歉，跨轮语义被改坏且完全不可见。现在改写成内容前缀 `[tool error] ` 并报 `rewrote tool_result.is_error`；措辞用 `rewrote` 而非 `dropped`，因为读者的下一步动作不同。前缀作为独立文本块前置而非把整段折成字符串——后者会碾平工具结果里的媒体块，而那与失败态无关。**行为变更三**：Gemini 响应里的 `inlineData`/`fileData` 此前流式路径静默跳过、非流式路径连分支都没有。现在两处都报带 media type 的说明，措辞同一出处；「不往下游转」这一决定不变。**行为变更四**：Responses 的 `refusal` part 此前被并入文本块而终止原因落到 `end_turn`，于是「模型拒答」与「模型答完了」对客户端无差别，而同一次拒答经 Anthropic 入站会得到 `refusal`——四协议间不对称。现在判 `content_filter`，优先级为 `incomplete_details` > refusal > `function_call`（判成 `tool_use` 会让客户端去执行工具，而模型实际上拒绝了）；流式在 `refusal.delta` 与 `content_part.added` 两处都记标记（上游实现不一，有的只发 delta）。拒答文字仍并入文本块。**明确不做**：不给非 Anthropic 协议合成 `stop_sequence`；不把 refusal 另立 IR 块类型；不往下游转响应内媒体；不给 `is_error` 在 Chat Completions 里另找字段；不改 Gemini 的 `functionResponse` 排序（经查「工具结果先于同条消息正文」是正确时序，不是缺陷）；调度层与配置中心零改动 |
| 2.15 | 2026-09-19 | 目标协议承载得了却没写出去的维度首次成文（[6.15](#615-目标协议承载得了却没写出去的维度)）：五条修复，全在转换层与中立表示内，无新增端点、无库变更。与前两轮相反——位置一直都在，只是没往里写，于是诊断会给出一条**事实错误**的说明，排查的人照着它去找一个不存在的原因。**行为变更一**：相邻同角色消息此前一对一写出，而 Anthropic 硬性要求 user/assistant 交替，非交替历史换来不可重试的 400（换目标也救不回来），且相邻两条 user 在 Chat Completions 里完全合法。现在在 IR 的 `Sanitize` 里合并并报 `merged N adjacent same-role message(s)`；只拼接不并块（并块需要决定分隔符，那会改变模型看到的内容）；顺序排在「丢弃空消息」之后——丢掉中间一条空消息会**制造**新的同角色相邻。参考实现 sub2api 在配对治理前后各跑一次合并。**行为变更二**：Gemini 的 `seed`、`presencePenalty`、`frequencyPenalty` 此前能力位为假且线上结构缺字段，三维被丢弃并报出一条「本协议没有该参数」的假说明。现在三者照原样写出（键名驼峰，蛇形会被上游静默忽略）；仍确实没有的是 `logit_bias`、`service_tier`、`parallel_tool_calls`。**行为变更三**：本服务对 Responses 恒发 `store:false`，该模式下上游只在 `include` 点名时才回 `reasoning.encrypted_content`，而此前 `include` 只是客户端值的透传——签名槽的代码都在却永远收不到值，下一轮推理接不上且无诊断。现在请求推理时追加该项（追加非替换、已有不重复、不请求推理不追加、不报有损）。**行为变更四**：Responses 无独立 `logprobs` 开关，客户端只给开关时此前什么也拿不到，而诊断报的是「已转发但结果不回」——那句话暗示字段送到了。现在补 `top_logprobs:1`（取最小值，客户端没说要几个）。**行为变更五**：图片的 `detail` 此前在所有路径上被丢弃，`detail:"low"` 的请求按上游默认计费，图多的负载上是实打实的成本倍数。现在 Chat Completions 与 Responses 双向透传，另两协议丢弃并报带计费后果的说明；客户端未给时**不合成**——本服务是转发层，上游的默认才是权威。**明确不做**：不给 Gemini 设 `safetySettings`（客户端没表达过任何东西，替它选阈值是政策决定）；不为 `logit_bias` 做跨模型重映射；不把 detail 翻成降级文字；不改 `store:false`；不在 `Sanitize` 里做交替之外的结构改写。**顺带更正两处陈旧测试夹具**：两份被标为「健康请求」的历史里 `user(tool_result)` 紧跟 `user(text)`，本身就是 Anthropic 会拒的非交替形态，已补 assistant 隔开 |
| 2.16 | 2026-09-19 | 数据面自身的完整性首次成文（[6.16](#616-数据面自身的完整性)）：六条修复，全在数据面与受理面内，无新增端点、无新增列、无库变更。与前三轮不同——这些缺口**不改变任何一次成功响应的内容**，矩阵测试全绿也发现不了，只在出错、并发或收尾时显形。**行为变更一**：出站请求失败的 `message` 此前是 `*url.Error` 的原文，它内嵌完整请求 URL 含 query，而 `BaseURL` 由调度层下发、一些部署把 key 放在 query 里——这句话进流水 `error_message`、Redis 实时环、管理面与**客户端可见的错误体**四个出口，等于把凭据送给调用方。现在两步净化：先 `errors.As` 剥 `*url.Error` 外层（文本扫描拿不掉 `Post ` 与引号），再扫 `://` 换占位符兜底（上游库换错误类型时剥外层不生效）；归因留在内层文本里（超时/拒连/DNS 三者处置不同，只回「连接失败」等于把排查推给抓包）；净化后为空则回 `upstream request failed`。刻意**不做内容黑名单**（不扫 `sk-` 之类）：会误伤正常内容且给人虚假的安全感。两条出错路径都走净化。**行为变更二**：`X-Request-Id` 由客户端给且同时是流水主键、上报幂等键与捕获键，客户端重复用同一个值时流水 `ON CONFLICT DO UPDATE` 静默覆盖、上报 `DO NOTHING` 静默丢掉第二次的用量（调度层配额少算）。现在**回显 id 与记录键分离**：回显仍是客户端那个值（改掉它客户端对不上自己的日志），落库/上报/捕获/dispatch 四处统一用记录键；撞号时记录键换成 `req_<hex>` 并打 `WARN request id collision`（悄悄换键会让客户端那个 bug 永不被发现）；未撞号时两者同值。检测只在进程内记最近 4096 个 id（LRU 环，不进 PG 查重：那是每请求一次额外往返换一个罕见情形）；**多实例边界**：两个实例各记自己的，跨实例撞号仍可能覆盖，要严格去重应由客户端保证 id 唯一或在反代做实例亲和。受理面拒绝那条路径同样过检测。**行为变更三**：四条终态分支里「重试耗尽」此前漏设 usage，而耗尽的请求上游照样计了费——流水记 0 token 而调度层记非零，事后无法判断哪边错。**行为变更四**：每次尝试的结果上报是一次阻塞 HTTP POST，重试三次的请求此前把三次往返全串在客户端的等待里。现在每请求一个上报队列（缓冲 channel + 单消费者），六条路径全部入队（终态也入队：调度层按到达顺序解释`retrying` 与终态，混用同步异步会让顺序取决于调度）、串行投递、`Serve` 返回前等排空（否则进程退出丢在途上报）；轨迹仍同步追加（它写的是随后要落库的那条记录）。**时延不变式随之调整**为 `dispatch_ms + upstream_ms <= latency_ms`，差额含编解码、上游生成时间与终态上报的一次往返。**行为变更五**：此前没有恢复中间件，任一 codec 在流中途 panic 则连接直接断，客户端看到一个未闭合的流——SDK 那边表现成解析卡住或超时而非一个能报给人看的错误。现在挂在处理链最外层（CORS 与访问日志之外：那两层自己也可能 panic，访问日志尤其在业务返回之后才记），按「写到哪一步」分四种收尾（数据面未写→500+错误信封；数据面已写→不动状态码、补流内错误帧；管理面未写→管理面自己的错误形状；管理面已写→什么都不再写）。已开始写后改状态码不会真改掉它（标准库打 `superfluous WriteHeader` 并忽略），改的是响应体——本该是流内错误帧的地方变成一份信封 JSON，混在 SSE 里客户端解不动。`http.ErrAbortHandler` 原样抛回（它是标准库「故意中断」的表达）；panic 值不回客户端但栈必须进日志。**行为变更六**：某事件在客户端协议里编不出来时跳过它、其余照发、流正常收束——此前不留任何痕迹，客户端内容缺一块而 HTTP 200、诊断里什么都没有。现在记一条 `skipped response event <类型>`，带类型（跳文本与跳工具调用的后果差得远），刻意不拼错误文本（说明会去重，措辞一变同类跳过就散成多条）。**明确不做**：不拒绝客户端给的 id（撞号是客户端的 bug，拒绝请求把它变成本服务的故障）；不改 `ON CONFLICT` 语义（`DO UPDATE` 对同一请求的多次写入是正确的，问题在键不该撞）；不把 `base_url` 加进轨迹；不做全局错误文本黑名单扫描；不为 panic 做重试（panic 是本服务的 bug，重试一遍只会再 panic 一次而客户端多等一倍）；调度层与配置中心零改动 |
| 2.20 | 2026-09-19 | 退避信号的解析形态与回传首次成文（[6.20](#620-退避信号的解析与回传)）：三条修复，无新增环境变量、无新增端点、无新增列、无库变更。三者的共同症状是**零值**——HTTP 状态码与响应体逐字节正常，坏掉的东西显形在另一个进程（调度层的冷却时长不对）或另一台机器（客户端压上一场 429 风暴），因此矩阵测试全绿也发现不了。**行为变更一**：reset 头此前只做一次 `ParseFloat`，而 OpenAI 兼容层普遍回 `1s`、`6m0s`、`20ms` 这种 Go duration 形态——解不出来，而解不出来与「上游根本没说」不可区分，于是调度层回落到启发式冷却，而上游其实明确说了什么时候能再来。现在识别 duration 形态，亚秒抬到 1 秒（参照 sub2api `xai/quota.go` 同样处置：「20 毫秒后重试」在限流语境下几乎总是错的）。判定顺序是数字 → duration → RFC3339，但**顺序不影响结果**：三种形态互不相交，`ParseDuration` 对无单位数字报 `missing unit in duration` 而不是当成纳秒（本机探针实测，推翻了立项时写下的相反判断），唯一的交集 `"0"` 被两条支路判成同一个零值。钉顺序的那条测试保留为回归护栏，但它护的是形态识别本身，不是一个曾被误判为存在的风险。**行为变更二**：同一函数此前没有任何日期分支（`Retry-After` 走的是 `http.ParseTime`，它不接受 RFC3339），而 `resetHeaders` 只列了 unified 三兄弟，`anthropic-ratelimit-requests-reset` / `-input-tokens-reset` / `-output-tokens-reset`（值是 RFC3339）一个都没登记——补进列表也解不出来，两条必须一起修。现在两者都补上，新增形态仍走**汇总层**的 24h 地平线闸门（闸门刻意不下沉到单值解析：多头取最早必须共用一份判据，下沉会让某一种形态偷偷绕过，这一条也有测试钉住）。**行为变更三**：`Retry-After` 此前**一个出口都没有**——全仓唯一出现`Header().Set("Retry-After", ...)` 的地方是一个测试夹具。上游明示的到期时刻只流向调度层，而客户端 SDK 的自动退避读的正是这个头，拿不到它就按自己的默认节奏立刻重来，把一次限流变成一场风暴。现在两个错误出口都发：数据面终态与受理面/路由层拒绝，后者含「连入站协议都没认出来」那条兜底路径（本轮的测试先抄出了漏掉它这一点）。值**从时刻现算而不是转发上游那个字符串**（后者可能含 CRLF 或长得离谱，而本服务手上已有一个解析过、过了闸门的时刻），**向上取整**（截断会把 1.2 秒写成 1，客户端早到 0.2 秒又吃一个 429），过期或零值**不发**（一个 0 或负数会让 SDK 立刻重来，比不发更坏），已提交的流**不发**（响应头早已写出，此刻设 header 静默无效，留着它只会让读代码的人以为它生效了）。对照 sub2api `handler/openai_gateway_handler.go` 的 `copyFailoverRetryAfter`——它转发上游字符串并为此加了 128 字长度、CRLF 与 7 天上限三道校验，本服务从时刻现算，这三道校验都不需要。**明确不做**：不在 2xx 上发这个头（即使上游在接近配额时在成功响应上带了限流头）；不转发上游的其它限流头（剩余额度、窗口大小需要新增列+迁移+管理面暴露，是数据模型那条轴）；不给到期时刻做出口侧的「合理化」夹紧（地平线闸门已在解析侧挡掉不可信值，两处各夹一次会漂移）；不新增环境变量。调度层与配置中心零改动 |
| 2.21 | 2026-09-19 | 跨边界的值完整性首次成文（[6.21](#621-跨边界的值完整性)）：四条修复，无新增环境变量、无新增端点、无新增列、无库变更、无新增依赖。四者同一形状：**值在跨越一个边界时被静默改写**，而状态码与响应体都正常。**行为变更一**：四处按字节截断（上游错误体摘要 512、调度层响应摘要 256、outbox 失败原因 512、上游收尾原因原文 200）都会切在多字节字符中间，而全仓此前**零处** UTF-8 相关代码。后果不在客户端那一侧——`json.Marshal` 把非法字节替成 U+FFFD 且不报错，调用方看到一条完全正常的错误信封——而在 PostgreSQL：非法序列让**整行被拒**（`SQLSTATE 22021`，本机真库实测），于是那一次故障的流水恰好缺失，缺的正是最需要的那条。现在统一走一个叶子包收边界，**往回退而不是往前补**（上限的含义是「最多这么多字节」，为凑齐一个字符而超出会把约束反过来，而 `last_error` 那一处上限对着的是真实存储），判定用「整串是否合法」而不是「末尾字符是否 RuneError」（合法的 U+FFFD 本身就解码成 RuneError，后者会把上游真发来的替换字符当成截断残骸砍掉）。**行为变更二**：收边界只管「我们切坏的」，上游本身就可能回非 UTF-8 字节——本服务明确保留了「解压失败退回原始字节」这条路，那条路上的内容从不经过任何截断点。现在写 `error_message`/`error_code`/`last_error` 之前另做一次净化，**只剔坏字节保留其余内容**（整段丢弃等于把线索换成空白，而线索恰恰在剩下的部分里），替换物是空串而非U+FFFD（这些文本进日志或 TEXT 列，留一串替换字符只是把「坏了」搬到人眼前占位；参照 sub2api `middleware/request_metadata.go:16` 同款处置与同款理由）。净化点选在**写库前**而非构造时：构造时净化会让客户端看到的与库里存的成为两份文本，现在客户端拿 U+FFFD 版本（看得出坏在哪）、库里拿干净版本（存得下），两边都不丢那一行。JSONB 列不另做净化（内容由本服务构造，`json.Marshal` 已替掉坏字节；唯一嵌上游原文的收尾原因说明走截断点那条路）。**行为变更三**：参数合并此前把body 解成 `map[string]any` 再编回去且未开 `UseNumber`，所有数字过一遍 float64：`"seed":13835058055282163712` 出来变成 `13835058055282164000`（实测），上游按另一个种子生成，而请求 200、流水正常、有损诊断为空。更难查的是它**只发生在配了defaults/overrides 的目标上**——没配的走空短路逐字节原样返回，同一个请求打到两个目标行为不同。现在开 `UseNumber`，数字停在原始字面。**行为变更四**：同一趟往返还把 `<`、`>`、`&` 编成 `\u003c` 这类六字节转义。语义无害但这是字节层改写，而本服务有两处按字节办事的东西（请求体字节预算、转换四体捕获），配了 overrides 与没配量出来的字节数就不一样。现在关掉 HTML 转义。换用 `json.Decoder` 顺带补了一道检查：它比 `json.Unmarshal` 宽松，读完第一个文档就返回，`{"a":1} {"b":2}` 会被静默当成 `{"a":1}`（实测），不显式确认尾随内容就等于顺手放宽一个原本守住的边界；`Encoder` 追加的换行也要去掉，这个返回值是要当请求体发出去的。**明确不做**：不引 sjson 之类字节级改写库——new-api `relay/common/override.go:788-795` 为避免大base64 字段（Gemini `inlineData.data`）的内存放大而弃用 map 往返，代价是自理键转义与嵌套下钻语义，而本服务实测 5MB base64 body 经参数合并功能正常、字段字面保留（0.03 秒），**只有内存放大没有正确性缺口**，不值得多一个依赖和一类 bug；不改上限数值、schema、环境变量；不把净化推到 IR 构造处；不对已经合法的输入做任何改动（净化与收边界对绝大多数请求必须是恒等的，否则会悄悄改写全部历史流水）。调度层与配置中心零改动 |
| 2.22 | 2026-09-19 | 并发下的共享资源治理首次成文（[6.22](#622-并发下的共享资源治理)）：四条修复，新增一个环境变量（`MSA_PG_MAX_CONNS`）、两列（`report_outbox.lease_until` 与 `.lease_token`，幂等 DDL 加 `ADD COLUMN IF NOT EXISTS`）、一个直接依赖（`golang.org/x/sync`，此前已在模块图里作为间接依赖）、无新增端点。四者的触发形态**都在单进程内部**——`docker-compose.yml` 只起一个 `backend` 服务，所以「多副本才会出事」在这个服务上不成立，本轮刻意不采用那条论证。**行为变更一**：`report_outbox` 的取到期项此前是纯 `SELECT` 而处理在事务之外，真库探针实测两次相邻取项**重叠三行**、两个执行流各记一笔后 `attempts` 从 0 直接到 2。后果不是重复投递（`report_id` 唯一约束加调度层同键幂等顶得住）而是**假的判死**：`MSA_OUTBOX_MAX_ATTEMPTS=20` 在两流并存时实际是十次，一条只是撞上调度层抖动的上报被提前埋到 `9999-01-01`，而管理面显示「重试了 20 次仍失败」。单进程里这两个执行流一直都在：ticker 的 `Drain` 与关停时那次无条件 `Drain`。现在改为 CTE 认领（`FOR UPDATE SKIP LOCKED` 管同一瞬间的不阻塞，`lease_until` 管跨瞬间的可见性，缺一不可），`Retry`/`Bury`/`Done` 三处写入全部按 `lease_token` 围栏并在零行受影响时返回 `ErrLeaseLost`，裁决落地即交还租约（不交还则排空速度被租约长度而非退避曲线支配），租约取单条上报超时的三倍、下限 30 秒。`Revive` 一并清租约——真库探针实测不清的后果是先 `Bury` 再 `Revive` 再被旧快照 `Bury`，运维点了重试下一秒又变死信且日志里没人认领这件事。worker 侧把 `ErrLeaseLost` 降级为 Debug 并继续本批余下条目（它是「有别人在管」而非故障），`Done` 丢租约仍计入送达数（报确实送达了）。`Counts`/`List` 不看租约：在飞的行仍算 pending，租约是内部治理状态而非观测口径。对照 sub2api 的同款实现，其设计文档要求的 `claim_version` 栅栏令牌它自己没做，本服务的 `lease_token` 每次认领换新值正是那个东西；new-api 走双表 compare-and-set（全仓无 `SKIP LOCKED`），需要两张表，本服务只有一张。**行为变更二**：`CachedModels.List` 此前无任何回源合并，TTL（默认一分钟）到期瞬间所有在列客户端一起 miss，调度层收到 N 倍尖峰；而它若正因此不健康，每个请求各自等满超时，尖峰持续整个超时窗口。现在 miss 后进 `singleflight`，闭包内二次检查缓存（捕获「另一组刚写完」），回源用 `context.WithoutCancel` 派生加独立预算（共享结果不该因发起方断开而作废），失败记 2 秒负缓存（不记则每个请求各撞一次墙）。**不学 new-api 的后台 ticker 全量重建**：那需要长跑 goroutine 且空闲时段也在轮询，而本服务读路径已有 Redis 兜着。**行为变更三**：`/healthz` 免鉴权而每次命中做一次 PG `Ping` 加一次**真实**调度层 HTTP 调用加两个全表 `count`，探针、反代、监控叠加即按秒计的调度层 QPS，且最贵的 `count` 在队列积压时最慢——队列积压恰是探针被盯最紧的时候，自我放大。现在探测结果带 1 秒窗口、并发命中合并成一次、探测同样 `WithoutCancel` 派生（否则窗口在最需要它的时候失效）。窗口不做成环境变量（1 秒对十秒级探针周期不会陈旧，暴露出去会让人以为值得调），零值兜底 1 秒——生产装配不设它，不兜底则治理在生产里是关着的而测试全绿。全表 `count` 不改近似计数：积压数正是运维据此决定是否介入的那个数。四个参考仓库**无一**在健康端点做真实探测（sub2api 静态 ok、new-api 只读内存，真实探测都在 admin 鉴权后），本服务保留探测但加窗口。**行为变更四**：连接池此前未设上限，走驱动默认（探针实测 pgx 是 32），而这个池被流水落库、outbox worker、清理循环、健康检查四路共用，PG 侧 `max_connections` 通常 100 且可能被三个服务共用；打满时落库只 `Warn`、数据面照常 200，症状是「流水随机缺行」——不触发任何告警。现在 `MSA_PG_MAX_CONNS` 可配，默认 0 沿用驱动默认（不单方面抬高：会把耗尽点挪到共用同一台 PG 的别人身上），负值启动期报错（连接池没有「不限制」语义）。**明确不做**：不引入多副本形态、不改 compose；不把健康检查改静态 ok、不给它加鉴权；不改 outbox 的裁决语义（几次转死信、退避曲线、死信保留行）；不引迁移框架、不改既有列、不加端点；不给 outbox 加工作者身份列（`lease_token` 换新值已足够围栏）。调度层与配置中心零改动 |
| 2.25 | 2026-09-19 | 责任归属首次成文（[6.25](#625-责任归属本服务自己的决定不得伪装成别人的)）：三条修复，无新增环境变量、无新增端点、无新增列、无库变更、无新增依赖。共性不是「请求失败了」而是**责任被记到了错误的一方**——三者的状态码与响应体都正常，坏掉的东西只显形在运维视角里。**行为变更一**：上一版新增的`MSA_MAX_REQUEST_DURATION` 是从客户端 ctx 派生的，到期后 `ctx.Err()` 与客户端按停止键完全同形，而桥接层用 `ctx.Err() != nil` 作为「是谁断的」的唯一判据。于是提交后到期被记成客户端取消（`canceled`/`normal`，而那一类刻意不计目标失败也不算异常，客户端拿到一个残缺流且没有任何错误帧），提交前到期落到上游兜底分支归成「上游不可达」并**累计一个完全健康目标的失败计数**。两者合起来：一个配得过紧的预算会静默截断所有长生成，而它在所有指标上都长得像「用户爱按停止键」加「上游偶发抖动」。现在改用 `context.WithTimeoutCause` 挂哨兵、判定走 `context.Cause`，归因是**不可重试**的超时类错误并点名变量名（不可重试的理由：预算到期恰恰意味着没有时间再换目标，标成可重试会让最后那点预算全花在注定超时的重试上）。用 Cause 而不是多留一个「预算之前的 ctx」字段：后者要三层调用各自多传一个参数，漏传一处静默退回现状。三个判定点都**必须排在既有归因之前**——预算到期时 `ctx.Err()` 也非 nil，顺序反了那条分支永远走不到。**行为变更二**：参数覆盖作用在出站编码之后的 wire body 上，而此前对键没有任何保留集，于是目标配置的 `overrides` 能压掉本服务自己算出来的值：`model`（编码前已换成目标的 native model）被压掉后请求打到另一个模型并按它计费，而流水、上报与逐次轨迹三处记的都是调度层派的 `model_id`，事后对不出账；`stream`（三个出站编码器写死 `true`，对上游一律流式是桥接层与聚合器的前提）被压掉后上游回整份 JSON、逐字输出消失，而诊断里只有一句「上游忽略了流式请求」，把配置错误归因给了上游。现在 `model`/`stream`/`stream_options`/`alt` 四个顶层键落在保留集里，命中即**跳过**并记一条有损说明。跳过而不是报错（报错会让一个配错的键把整个目标变成死路，而 overrides 里绝大多数键正当）；**只挡顶层**（四个协议里它们都在顶层，下钻挡会误伤 `generationConfig` 里合法的嵌套同名键）；**`defaults` 不受约束**（只在键缺失时填，构不成改写；对它也设保留集会把「上游端点要求显式写 `alt`」这类正当配置一起拒掉）；说明**不拼值**（与出站请求头黑名单同口径——这几个值目前都不是凭据，但一旦开始拼，下一个加进保留集的键就得有人重新判断一次安不安全）；保留集不做成可配置（能配就能关，关掉就回到现状）。**行为变更三**：上一版的限流头回传只挂在流式提交与非流式成功两处，而非 2xx 分支根本不构造上游句柄——所有错误终态上一个限流头都没有。最需要这族头的那一刻（429）恰好是它们缺席的那一刻：客户端 SDK 靠剩余量自适应节流，拿不到就只能靠 `Retry-After` 硬等，而上游沉默时那个头也不发。现在可回传的头随 `ir.Error` 带到终态（不序列化字段，既不进流水也不进上报），由错误写出点在 `WriteHeader` 之前发出；挂在错误上而不是给写出函数多传一个上游句柄，因为错误值本身已贯穿全程（`retry_after` 与副作用风险位走的就是这条路），而中间隔着三层调用与五条终止分支，多传参数意味着五条都要跟改而漏一条不会变红。白名单沿用同一个，`Retry-After` 仍只由本服务现算那一处产出，重试后失败时头来自**最终那次**尝试。**明确不做**：不新增环境变量；不给 overrides 做值级校验；不为预算到期新增 `outcome` 类别（`abnormal` 已足够，新增会让调度层跟改）；不做全量响应头转发。调度层与配置中心零改动 |
| 2.26 | 2026-09-19 | 推理签名与流内错误的往返载体首次成文（[6.26](#626-推理签名与流内错误的往返载体)）：三条修复，无新增环境变量、无新增端点、无新增列、无库变更、无新增依赖。共性是**上游交出来的东西在中立表示里没有落点**，于是它在转换途中消失，而状态码与响应体都正常。**行为变更一**：多数 responses 形态的上游只在 `response.output_item.done` 上挂 `encrypted_content`，而解码器此前只在 `added` 上读它——签名整条丢失，且因为块已被 `done` 正常闭合，完整性判据（只看未闭合的块）既不报 `incomplete_stream` 也不出任何说明。客户端把无签名的思考块存进历史，下一轮推理项接不上，两轮之间没有任何线索。现在 `itemDone` 在闭块之前补发签名增量（**必须在 `block_stop` 之前**：之后到的增量聚合器不收，会静默丢掉），来源填本协议名。两帧都给时**只发一条**——聚合器对签名增量是累加的（`+=`），发两条会把两份密文拼成一段上游解不开的垃圾，比丢一份更坏；解码器因此记住每个块已拿到过的签名。两帧给了**不同**的密文时保留先到的那份并留说明（这种形态说明上游行为超出预期，运维得看见它）。刻意**不做「done 覆盖 added」**：聚合器层面没有覆盖语义，要做就得改它对所有协议的签名处理，会动到已验证的三协议路径。**行为变更二**：Gemini 把 `thoughtSignature` 挂在 `functionCall` part 上而不是 `thought` part 上，而中立表示此前只有思考块那一位——这一处的签名在解码时就没有落点，整条丢掉且不出说明。现在中立表示的工具调用带**独立的**签名与来源两位（不复用思考块那一位：一条响应里可以既有思考块的签名又有若干次调用各自的签名，分别对应上游的不同状态，混在一处就对不回去），新增能力位 `ToolCallSig`（仅 Gemini 为真，与 `ThinkingSig` 分开——两者是上游的不同状态，一个协议可以只有其中一个），流式与非流式两条解码路径各自读它，出站把**本族**签名回写到 part 自身。**这一轮的交付物是「有记录的丢弃」而不是「往返可用」**：Gemini 是纯出站协议（只注册出站编解码器，`/gemini/v1/*` 返回 404），不存在它的入站往返。实际价值是三个入站协议回客户端时为这一位出剥离说明（它们的 `tool_use` 都没有对应字段，与来源无关一律剥离），以及载体就位。说明措辞与思考块那条**刻意不同**（`tool call reasoning signature` 对 `thinking signature`）：一条响应里两处都可能丢，措辞相同运维看不出丢的是哪个。**行为变更三**：上游先用流内错误帧说明原因再断流（限流与配额耗尽的常见形态）时，桥接层的两条拆流分支此前都只报兜底错误（kind 均为 `upstream`），于是一次流内 429 被记成**目标故障**并累计失败计数，把一个只需退避的健康账号推向冷却，而运维看到的是一次上游抖动。现在归因的单一决策点接收流内错误，顺序为「本服务预算到期 → 客户端自己走了 → 上游在流内说明过原因 → 兜底」；第三步排在前两步之后，因为本服务放弃或客户端离开时责任不在上游，哪怕上游此前说过什么。两条拆流分支共用同一决策点——上游被掐断时走哪条取决于读协程当时停在哪里，是竞态，两处各判一遍的话其中一处判错只在另一次运行里才暴露（上一轮已因此返工一次）。**明确不做**：不新增环境变量或开关；**不合成也不猜签名**（凭空造一个会让上游拒整轮）；不解密 `encrypted_content`（本服务不持有密钥）；不把工具调用签名做跨协议翻译；不扩大完整性判据（`done` 正常闭合的块不是残缺块，把「缺签名」并进去会让一批可用响应被判成不可用）；不为流内错误新增 `outcome` 类别或流水列。调度层与配置中心零改动 |
| 2.27 | 2026-09-20 | 边界处的静默削减（首次成文，[6.27](#627-边界处的静默削减)）：usage 五维跨仓不再截断、媒体嗅探容忍折行与 URL-safe、data URI 参数列表按末位取 base64、心跳与流超时联合校验 |
| 2.28 | 2026-09-20 | 调参取值范围的跨协议收敛（首次成文，[6.28](#628-调参取值范围的跨协议收敛)）：`Capabilities` 新增 `MaxTemperature` 取值范围维度，anthropic 落 1.0（上游 400 原文 `temperature: range: 0..1`），其余三协议留零值待实测；出站夹紧而非拒请求并报有损说明，夹紧排在采样参数互斥之后。另四条同轴修复（[6.28.5](#6285-合并相邻同角色消息时的分隔符)–[6.28.8](#6288-gemini-的-stop-序列上限)）：**行为变更一**：合并相邻同角色消息时在边界插空行分隔块——三个出站协议的文本拼接无分隔，此前 `订单号 10086` 与 `3 件退货` 会粘成 `订单号 100863 件退货`，模型算错且全程 200 无说明。**行为变更二**：`chat_completions` 与 `responses` 入站解码认回并剥掉 `[tool error] ` 前缀——此前重入后 `is_error` 读作 false，模型把失败当成功，多跳还会把前缀叠加。**行为变更三**：推理预算改按协议**有效**上限夹紧（`MaxTokensFor`）——此前以客户端显式 `max_tokens` 为条件，而协议默认值在整形之后才补，「不给 max_tokens + 大 budget」整条路径逃过夹紧，直落上游 400 `budget_tokens must be less than max_tokens`。**行为变更四**：gemini 补 `MaxStopSequences: 5`——此前只声明布尔支持，超限直落 400 `INVALID_ARGUMENT`，同一请求换上游即成功。**明确不做**：不给 `top_p`/`top_k` 设范围；不照搬 new-api 的厂商特例与按模型名分档；不在入站侧校验取值；调度层与配置中心零改动 |
| 2.29 | 2026-09-20 | 出站请求体的结构合法性（首次成文，[6.29](#629-出站请求体的结构合法性)）：四条落差全由本仓探针实测取证。**行为变更一**：工具 schema 方言从黑名单改白名单（`Dialect.Allow`），此前 `$ref`/`$defs`/`definitions`/`oneOf`/`allOf`/`prefixItems` 既不剔除也不报说明，原样发出直落 gemini 不可重试的 400 `Cannot find field`，而 `DroppedKeys` 为空意味着连线索都没有；白名单 17 键刻意不含 `title` 与四个长度键（new-api 放行，本仓原来剔除且上线未见问题，冲突时不动跑通的行为）。**行为变更二**：`Dialect.StringEnumOnly`（仅 gemini），`enum` 非字符串成员转 JSON 字面量字符串，此前 `{"type":"integer","enum":[1,2]}` 直落 400 `Invalid value at enum[0] (TYPE_STRING)`；数字走 `FormatFloat(.., f, -1, 64)` 而非 `fmt.Sprint`（后者把 1000000 写成 1e+06，模型匹配不到可选值）；非标量成员整条删 `enum` 并报说明。**行为变更三**：`ShapeRequest` 新增 `ensureNonEmptyMessages`，`ir.Sanitize` 清空消息后此前四协议分别发 `messages:null`/`messages:null`/`input:[]`/`contents:null` 全落 400，而清空是本服务自己的修复动作造成的、归因却指向客户端；按「只报有损，从不拒请求」补一条最小 user 轮（new-api 走的是拒收），占位文本与 anthropic 的前导占位合为 `codec.ConversationPlaceholder` 一处。**行为变更四**：`Capabilities` 新增 `ToolResultTextOnly`（cc/responses/gemini 为真），工具结果里的媒体抽出改投为紧随其后的一条 user 消息并报说明，此前 cc 编出 `role:tool` 带 `image_url` 被上游拒收整个请求，responses/gemini 则被 `joinText` 静默碾掉——请求 200、响应正常、模型据一张从未看到的截图答错且无任何说明；该位与 `Images` 是两件事（三家在普通消息里都能带图）；抽出集中在整形层而非三个编码器各写一份，anthropic 不设此位故为空操作。**明确不做**：`max_completion_tokens` 按模型族分支（本仓刻意无按模型名分支设施，正确落点是配置中心 `request_overrides`）；gemini `safetySettings`（全仓未赋值，零值即上游默认）；schema 深度上限从 32 提到 64；`exclusiveMinimum` 在 INTEGER 上折成 `minimum`+1；调度层与配置中心零改动 |
| 2.24 | 2026-09-19 | 客户端意图与出站寻址的保真首次成文（[6.24](#624-客户端意图与出站寻址的保真)）：三条修复，新增一个环境变量`MSA_MAX_REQUEST_DURATION`（默认 0 = 不限），无新增端点、无新增列、无库变更、无新增依赖。**行为变更一**：Chat Completions 的 `stream_options` 此前在解码阶段整个丢掉，于是「客户端明确说不要那一帧单独的 usage」与「客户端没提」在本服务里是同一件事——恒发。现在`ir.Request` 新增三态 `IncludeUsage *bool`，「给了 `stream_options` 但没写 `include_usage`」判成明确 false（JSON 零值就是它的语义）；压制**只压那一帧**，`finish_reason` 的空 delta 与 `[DONE]` 照发（压掉会让客户端等一个永不到来的结束）；压制**不记有损**（照要求执行不是丢东西）；**对上游一律仍要 usage**（记账要用上游报的数，透传客户端的 false 会让流水的 token 数恒为估算值）。`codec.InboundCodec.NewStreamEncoder` 因此改签名带上请求——改签名而不是加可选 setter：setter 形态多出一个「什么时候必须调它」的时序问题，而漏调是静默的。**行为变更二**：四个协议里只有 Gemini 把模型名放进 URL 路径，而原先是裸字符串拼接。本机探针实测`gemini#x` 让整个 `alt=sse` **彻底消失**，上游于是回整份 JSON 而不是 SSE，落进「HTTP 200 但一帧都没解出来」那条可重试的路——三个同名目标连挂三次，运维看到的是三个账号同时坏掉；`gemini?x` 把方法名推进查询串；空格**不会**让 `http.NewRequest` 报错（这一条推翻了调研给出的说法）。现在分两类处置：会被 URL 解析吃掉的字符走 `url.PathEscape`（不列字符白名单——合法取值由上游定义，白名单只会在上游上新模型时误拒），含 `/` 或 `\\` 与整串为 `.`/`..` 的名字**拒绝**（`PathEscape` 不编码 `.` 与 `/`，穿越修不了只能拒；斜杠一律拒而不逐段放行，逐段放行等于替上游发明一套路径语法）。拒绝时 `Endpoint` 返回空串（签名不变，它不返回 error），数据面把空 URL 变成**可重试**的上游错误（换目标会换模型名），文案不回显模型名（它会回到客户端手里，而流水的 `model_id` 已记了它）。`base_url` 以 `/models` 结尾时不再拼出 `/models/models`，但**只削一层**。**行为变更三**：首字与空闲两个超时各自只管一次尝试，`MAX_ATTEMPTS` 次累起来的最坏情形是它们的倍数，而客户端与反代看到的是那个总数——此前没有任何东西约束它。新增 `MSA_MAX_REQUEST_DURATION`，从客户端 ctx **派生**而不是另起（另起会让客户端断开不再传导，一个没人要的请求会把重试跑完，每次重试都是一份账单）；负值拒绝启动并点明 0 才是关闭写法；非 0 时不得小于 `max(首字, 空闲)`（小于它则每个请求都在同一时刻被掐断，重试与单次超时全部失效，而失效方式看上去像上游整体故障），恰好等于放行。**明确不做**：不改响应 `model` 的语义（修订 2.0 已把「上游回传模型名」定为刻意决定，不是缺口）；不采集客户端来源 IP（客户端身份归调度层）；不给另外三个入站协议合成 `include_usage`（它们的 usage 是协议固有形状而非可选帧）；不做模型名取值白名单；不做 per-attempt 时长上限。调度层与配置中心零改动 |
| 2.23 | 2026-09-19 | 上游侧失败的归因与幂等边界首次成文（[6.23](#623-上游侧失败的归因与幂等边界)）：五条修复，无新增环境变量、无新增端点、无新增列、无库变更、无新增依赖。共同症状是**没有症状**——状态码与响应体逐字节正常，坏掉的东西显形在账单上、连接池里，或把排查的人带去找一个不存在的原因。**行为变更一**：出站失败此前只分 `transport`/`upstream` 两档，两者都会换目标重发，而这个二分漏掉了「这次请求到底有没有发出去」。发出去了的请求上游可能已经生成完、已经计了费、请求里带副作用的工具调用可能已经被执行，重发就是第二份账单、第二次执行。现在 `ir.Error` 新增 `SideEffectRisk`，判据取 `httptrace.ClientTrace.WroteRequest` 而不是从 `*url.Error` 的文本反推（标准库内部有连接复用、h2 多路复用与请求重放，反推每一条都是猜）；本机探针实测它在响应头超时与「上游读完请求就断」两种失败上已触发，在拨号被拒与 DNS 失败上没有。**探针推翻了需求稿里的一句判断**：上游回 502 时该回调虽已触发但 `Do` 不返回错误，那条路走的是既有的状态码闸门，不在风险集合里。字段**与 `Retryable` 分开**而不是直接压成 `false`（两者回答不同的问题：一个是「换目标有没有意义」，一个是「换目标会不会产生第二份计费」，压成一个就没有将来放宽某一类的位置），且**不进 `NewError` 的参数表**（绝大多数错误产生在请求发出之前，加进签名等于让四十余处调用点跟着填一个 `false`）。重试闸门排在 `Retryable` 闸门**之前**——这一族失败的 kind 恰好都是可重试的那几个，放在后面永远走不到；命中后把明确的错误交给客户端，由它决定重不重试（它知道自己这个请求有没有副作用，本服务不知道）。**重定向失败刻意不标这个位**：请求确实发出去了，但上游回的是重定向而不是一次生成，它没处理也没计费，标上会让一个 `Location` 配错的目标直接把请求打失败而不是换目标——这一条是既有重定向用例在本轮跑红时抄出来的，设计稿原先是标的。**行为变更二**：NXDOMAIN 的错误链是 `*url.Error → *net.OpError → *net.DNSError`，而 `*net.OpError` 这一层会被既有的 `isTransportError` 认下并归成「换条连接就好」，于是一个域名写错的目标**永远不计失败、永不冷却**，每次调度还会被选中。现在新增 `isUnreachableTarget` 并排在 `isTransportError` 之前，**只认 `IsNotFound`**（DNS 超时与临时失败是解析服务的问题，算成目标失败会在 DNS 抖动时把整个账号池一起冷却）加 `EHOSTUNREACH`/`ENETUNREACH`；**`ECONNREFUSED` 刻意不认**——上游滚动重启时会短暂拒连，那是瞬时的，参考实现 sub2api 把它归 Persistent 但它那边的处置是「临时摘出调度」而本服务是「计入目标失败」，代价不同，不跟。**行为变更三**：非 2xx 错误体与整份响应此前直接 `Close` 而不读到底，本机探针实测 200KB 错误体、五次相同 429 换来**五条新连接**（排空则一条）。后果是限流窗口期内每个请求各开一条 TCP+TLS，而限流窗口恰恰是上游最脆弱、最该省着用连接的时候，方向正好反了，且连接数攀升不触发任何告警。现在三个 `Close` 点统一先排空再关，**排空有 1 MiB 上界**（无上界等于允许一个坏掉的上游用无限长的错误体把本服务拖住，而复用一条连接不值那个代价），超界就放弃复用。**行为变更四**：解码失败此前一律报一句笼统的上游错误，而其中一类高频形态是企业代理、WAF 或运营商门户在 200 上回一整页 HTML——排查的人拿着「解码失败」去查模型和账号，原因在网络路径上。现在按体的开头判 `<!doctype html`/`<html`（大小写不敏感、允许前导空白、只看前 512 字节）并报「像是被代理或 WAF 截了」；**只认开头不认「文中出现」**（正当的模型输出完全可能包含 HTML 片段）；非 HTML 的解码失败把 `Content-Type` 拼进说明（回 `text/plain` 与回 `application/json` 但结构不对是两个问题）；两者仍可重试。**行为变更五**：上游的 `anthropic-ratelimit-*` 与 `x-ratelimit-*` 此前一个都不回给客户端，而客户端 SDK 的主动限流规避读的正是这些头，拿不到就只能撞上 429 再退避。现在按**前缀白名单**回传——对照 sub2api 的黑名单式全量转发会把 `Set-Cookie` 与上游自己的 `x-request-id` 一起送出去（前者是凭据面，后者会让客户端拿一个本服务日志里查不到的 id 来问），白名单只需枚举客户端确实要读的那一族。`Retry-After` **不在白名单里**：它已由 6.20 从过了地平线闸门的时刻现算，两处各发一次会给出两个不一致的值。写入点必须排在写响应头之前（流式那条路 `writeStreamHeaders` 里就 `WriteHeader` 了，排在后面设 header 静默无效），多值头逐个 `Add`。**明确不做**：不做全量响应头转发；不做动态重试次数（幂等边界的处置是「不重试」而非「少重试几次」）；不加连接池指标；不改 `isEventStream` 在 `Content-Type` 缺失时按 SSE 处理的默认；不给 `SideEffectRisk` 做「上游声明了无副作用」的放宽通道（四个协议都没有这个声明）。调度层与配置中心零改动 |
| 2.19 | 2026-09-19 | 关停生命周期首次成文（[6.19](#619-关停生命周期)）：三条修复，新增三个环境变量 `MSA_SHUTDOWN_GRACE`/`MSA_SHUTDOWN_LINGER`/`MSA_SHUTDOWN_FLUSH_TIMEOUT`，无新增端点、无新增列、无库变更。三者相互加重，共同后果是**每次部署都会静默丢掉在途请求的用量上报**，而流水、健康检查与任何一次成功响应里都看不出来。**行为变更一**：关停给 20 秒，而本服务自己的首字超时默认 60 秒、空闲超时默认 120 秒——一条正当地处于静默思考期的流每次部署都会被掐断，客户端看到的是连接异常中断而非一个能报给人看的错误，且**没有任何旋钮可调**（`config.Config` 里不存在关停相关变量）。现在宽限期可配，且未设置时**派生自** `max(首字, 空闲)` 而不是取一个写死的常量：写死会随流超时被调大而重新变得短于它，于是「默认配置自相矛盾」这个状态又回来了。显式设小于那个最大值时拒绝启动——这是一条**联合校验**，两个值各自合法、组合起来才有害。参照 new-api `main.go:232` 给 120 秒，注释理由同款。判「是否显式设置」看环境变量在不在而不是看值是否为零：显式写 `0s` 的人意图是「不等」，那应当被拒并给出理由，而不是被当成未设置从而静默取一个很大的默认。**行为变更二**：三个阶段此前共用一个 context。只要有流在途，`Shutdown` 就会烧光整份预算并返回 `DeadlineExceeded`，随后 `Drain` 拿到**同一个已过期的 context**，`Queue.Due` 立刻失败、打一行 `outbox scan failed` 返回 0——那次「退出前冲一次队列」**在它唯一存在意义的场景里保证是空操作**，而代码注释声称队列被冲过了（已用探针实测：`Shutdown 耗时 400ms err=context deadline exceeded`，随后同一个 ctx 的 `Err()` 非空）。现在每阶段 `WithTimeout(context.Background(), ...)` 新建；派生也不行（继承已用尽的 deadline 等价于共用），从信号 context 派生更不行（它在收到信号那一刻就已 Done）。**行为变更三**：宽限期超时后没有任何东西等在途 handler（全仓无 `sync.WaitGroup`）——`run` 直接返回、进程退出，被掐断的 handler 连同它那条每请求的上报投递 goroutine 一起死掉，那些上报既没 POST 出去也没落库，调度层对部署瞬间在途的每一个请求都少记用量。现在在途计数装在处理链**最外层**（要等的是 handler 还没返回，而每请求上报队列由 `Serve` 的 defer 排空，等到 handler 返回就等到了上报投完，无需把那个队列暴露到装配层），超时后最多再等 `MSA_SHUTDOWN_LINGER`，放弃时打 `gave up waiting for in-flight requests` 带条数——这是运维唯一能看到「这次部署丢了多少上报」的地方。计数用 `defer` 归零，因此 handler panic 不会让它永久偏高（否则一次 panic 会让此后每次关停都白等满整个 Linger）；等待用轮询而非 `WaitGroup.Wait`（后者不接受超时，而关停期的等待必须有界）；宽限期干净返回时这一阶段**不执行**（在途已清零，再等就是白拖部署窗口）。**阶段顺序不可调换**：冲队列排在等在途之前的话，正在收尾的那些请求的上报还没入队，这一冲就冲不到它们，而它们恰恰是关停期最可能丢的那批。**明确不做**：不做连接级强制关闭（`Server.Close`——SSE 流被硬切和进程被 kill 对客户端是同一件事）；不把在途计数暴露成管理面指标（`/health` 的 `goroutines` 已是相近的粗指标）；不改每请求上报队列的生命周期（问题不在它而在没人等它）；不在关停期把上报改成同步直投（会让关停时间取决于调度层的响应速度，而它此刻可能也在重启）；不给冲队列阶段做重试（outbox 的后台重放下次启动后会继续）。调度层与配置中心零改动 |
| 2.18 | 2026-09-19 | 出站重定向策略首次成文（[6.18](#618-出站重定向策略)）：四条修复，无新增环境变量、无新增端点、无新增列、无库变更。出站腿此前完全没有重定向策略（全仓 `CheckRedirect` 零命中），而四个参考实现都在这里做过明确决定。**行为变更一**：跳转到另一个 host 时标准库只删 `Authorization`/`Www-Authenticate`/`Cookie`/`Cookie2` 四个头名，而本服务的凭据恰好是 `x-api-key`（Anthropic）与 `x-goog-api-key`（Gemini）——探针实测这两个跳转后照带，于是一个配错的 `BaseURL` 或一次被劫持的重定向能把账号凭据送到第二个 host 上。现在跳转前摧掉五个凭据头（含 `api-key` 与 `Cookie`），判据取含端口的 Host 且比**最初那个请求**（逐跳比会在 A→B→A 链条上把凭据加回去，而它在 B 那一跳已经暴露过）；摧除排在拒绝之前是纵深防御，防的是将来有人把 host 判据放宽。**行为变更二**：301/302/303 标准库会把 POST 改写成**无体的 GET**（探针实测），上游随后回的 4xx 会被归成模型失败、把一个健康账号推向冷却，而真正的原因在路由。现在拒绝跟随并报一个点明「改写了方法」的可重试错误；判据用**比较方法**而不是读状态码（`CheckRedirect` 拿不到重定向响应的状态码，但标准库在调它之前已把方法改写好，`via` 里留的是改写前的），一个判据覆盖三个码且天然放过 307/308。**行为变更三**：跳数限 3（不可配置）并走自己的错误，不让标准库的 10 跳先触发——后者的文本里带完整 URL 含 query。四种重定向失败全部归 `upstream` 且可重试，**不归 `transport`**（后者的语义是「换条连接有意义」，而重定向换连接一定得到同一结果，那个 kind 还会影响账号冷却的归因），归因文本是固定句子、一个字不从原始错误里取，且判定排在连接层归因之前（哨兵被 `*url.Error` 裹着而它不是 `net.Error`）。**行为变更四**：缺 `Location` 的 3xx 会被标准库无错误地原样返回，此前落到状态码闸门、一份通常为空的体被 `DecodeError` 归成笼统上游错误。现在闸门**之前**先抦 3xx。摧凭据的说明**直接并进 `lossy`** 而不搭建流阶段 `notes`：后者只在建流成功时才被取走，而这条说明几乎总产生在失败那一侧（本轮的端到端用例先抄出了这一点）。说明的收集口挂在**请求的 ctx** 上而不是策略函数的字段：客户端是进程级共享的，存本次请求的状态会让并发请求互相串（探针实测重定向请求继承原请求的 context）。**明确不做**：不做 SSRF 黑名单（new-api 做是因为它的 URL 来自终端用户输入，而加地址黑名单会让合法的内网上游部署连不上）；不用 `ErrUseLastResponse`；不追跳到另一个 host 的 307/308（摧掉凭据再跟随得到的一定是 401）；不记重定向目标 URL；不做可配置的跳数上限。调度层与配置中心零改动 |
| 2.17 | 2026-09-19 | 出站传输层的编码与客户端保活首次成文（[6.17](#617-出站传输层的编码与客户端保活)）：三条修复，新增一个环境变量 `MSA_HEARTBEAT_INTERVAL`，无新增端点、无新增列、无库变更。三者共同点是**症状离原因很远**——请求正常发出、上游正常回 200，坏掉的东西在别的地方显形。**行为变更一**：出站请求头有三层来自配置（端点定义、客户端声明、调度层下发），此前一律直写。其中 `Accept-Encoding` 最隐蔽：Go 的 `http.Transport` 只在**它自己加过这个头时**才透明解压，配置里写了它标准库就不再解，压缩字节直接进切帧器、切不出任何东西，落到「HTTP 200 却一个事件都没解出来」那条可重试路径上——而所有目标配的是同一个头，三次全败。另外四个（`Content-Length`、`Transfer-Encoding`、`Host`、`Connection`）写错会让请求立刻变形或被拒收，一次 400 就暴露了；这一个不会。现在这三层统一过一个**黑名单**（白名单要枚举「所有上游可能需要的头」，那个集合是开放的；黑名单只需枚举「写进去一定坏事的」，而那个集合由一个理由封闭界定——它们由标准库按传输的实际情况计算）。`Content-Type` 与 `Accept` **刻意放行**：它们是意图表达而非传输计算，且确有上游要求特定取值，运维覆盖是正当配置。丢弃留一条 lossy 说明，点名头与后果（五个里只有 `Accept-Encoding` 的症状远离原因，说明是运维唯一的线索），用规范化头名（否则各种大小写形态散成多条），**刻意不拼头的值**（这五个不会有凭据，但「不拼值」是更容易守住的规则）。**行为变更二**：此前从不看响应的 `Content-Encoding`。正常情况标准库已解好，但它解完会**把这个头删掉**，所以头还在就是它没解的可靠信号。现在按头解压：空/`identity` 原样透传（`identity` 是 RFC 允许的显式「没压」，当成未知编码报错是错的）、`gzip`/`x-gzip`/`deflate` 流式解压（**流式而非读全再解**：SSE 体无界，读全等于把逐字输出退化成一整份）、`br`/`zstd`/多重编码明确报错（硬塞给切帧器的症状是「200 却一帧没解出」，反推要花很久）。坏 gzip 在建流阶段就报，错误里**不拼任何响应体字节**（那些字节可能是上游回的任何东西，而 message 会流到客户端可见的错误体里）。三条读 body 的路径全覆盖，含**非 2xx 错误体**（限流与配额耗尽靠这个体区分，解不出会归成笼统的上游错误；它解压失败时退回原始体——归因已是「上游不行」，再换路径只会丢掉状态码）。**解压挂在捕获的里侧**：存压缩字节等于把最后的线索变成看不懂的二进制。**行为变更三**：反代与云网关普遍在 30~60s 无字节时掐连接，而推理模型长思考可以几分钟不出一个 token，被掐时客户端看到连接异常中断而上游一切正常、还在计费。现在流式响应静默期内按 `MSA_HEARTBEAT_INTERVAL`（默认 15s，负值关闭）发保活帧，形状由**客户端协议**决定：anthropic 用它自己的 `ping` 事件（其 SDK 按事件类型分派，注释帧那条分支可能压根不走），其余三个用 SSE 注释 `: keepalive`。四条约束——首帧之前不发（响应头未写出，此时写会把状态码钉死在 200，而这阶段的失败本该换目标重试或回正确的 HTTP 错误码）；非流式不发（一次性 JSON 中间插字节会把体弄坏）；不进 usage/聚合器/tail 但**进捕获**（客户端确实收到了这些字节）；**不推进空闲超时**——这一条是加心跳时必须同时改的：空闲超时此前每轮新起一个 `time.After`，心跳让循环多醒几次、把计时器一次次推后，于是彻底静默的上游流永远等不到超时。现在超时是一个**绝对时刻**，只有真帧到达才推进它。**明确不做**：不支持 br/zstd（要引第三方依赖而实测上游没有回这两种的）；不主动加 `Accept-Encoding`（让标准库自己加，它加了才会自己解）；不做出站并发上限（属调度层）；不读 2xx 里的限流剩余量头（需新增列+迁移+管理面暴露，是数据模型那条轴）；保活不得掩盖上游静默；整份响应不发保活。调度层与配置中心零改动 |
| 2.12 | 2026-09-19 | 逐次尝试轨迹首次成文（[6.12](#612-逐次尝试轨迹attempts_trail)）：流水新增行内 JSONB 列 `attempts_trail`（老库自动补列），随单条详情（[7.3](#73-get-adminrequestsrequest_id单条详情)）返回；捕获的 `upstream_response` 在第二次及之后的尝试前插入分隔标记 `: ---- attempt N ----`。**行为变更**：此前一次请求尝试了多个目标时，流水上的 `model_id`/`account`/`outcome`/`status_code`/`error_code`/`error_message`/`retry_after` 全都只是**最后一次**的值，`dispatch_ms`/`upstream_ms` 是累计值，`tried_ids` 只有模型 ID 而没有各自的结果——「第二个账号是 429 还是 500」「三次都慢还是只有第三次慢」在流水里无从得知，而上一版加入的捕获又把多次尝试的上游字节无边界拼在一起。**形态选择**：行内一列而不是 per-attempt 表，依据是参考实现 sub2api 曾建过 `ops_retry_attempts` 表（`033_ops_monitoring_vnext.sql`）、扩过一次（`038`）、最终整表删掉（`136_remove_ops_retry_replay.sql`），理由原文是写入宽度、内存驻留与库体积；new-api 从未建表，只把尝试过的渠道拍平成 `重试：A->B->C` 一行人读文本（`controller/relay.go`），看不出各自的错误码与耗时。**不变式**：轨迹各项耗时之和恒等于行上的累计值，因此「本次值」与「累计值」两个计时器混用会被立刻发现。**凭据边界**：轨迹随流水进 PG，因此只含标识与结果标量，绝不含请求头、`base_url`（其排查价值等于 `model_id`+`account`，而它是带路径的 URL、可能把 key 放在 query 里）或请求体。**明确不做**：不建 per-attempt 表；不进列表端点（一页 200 条会随重试次数膨胀，`attempts` 计数是入口）；不做「重试链」人读字符串；不给单次尝试省掉轨迹；调度层与配置中心零改动 |
| 2.11 | 2026-09-19 | 转换四体捕获首次成文（[6.11](#611-转换四体捕获)）：新增 `MSA_CAPTURE_MODE` 三态开关（`off`/`errors`/`all`，默认 `off`，非法值拒绝启动）与两个上限变量，新增两个管理面端点（[7.9](#79-get-admincaptures捕获列表)、[7.10](#710-get-admincapturesrequest_id四体全文)）。**行为变更**：此前本服务**没有任何**调试捕获设施——流水只记转换层自己判断出的结论（`sanitized`/`lossy`/`error_code`），当那个判断本身错了时没有任何东西可看。上一次定位 kiro 的 `toolUse` 帧碎裂 bug 就是靠手写一段临时 tee 抓真实上游字节才找到根因，那段代码用完即弃、下一次还得重写。四体的取舍：`upstream_request` 换目标重试时覆盖（诊断对象是最终发出去的那一次），`upstream_response` 与 `client_response` 累加（同一个流的连续片段）；留/丢判据用 `error_code != ""` 而非 `outcome`，两者在「重试后成功」（不留）与「客户端取消」（要留，`outcome` 是 `normal` 但 `error_code` 是 `canceled`）两处分歧。**凭据边界**：捕获只含 body、永不含任何请求头，因此也刻意不做 body 内的正则脱敏——不存在凭据这件事由结构保证，再加一层只会给出虚假的安全感。**明确不做**：不落盘（要长期留证据应在反代层抓包）；不捕获 IR 与事件序列（可由前后两体推出）；不做采样（`errors` 档已是「常开而不撑爆内存」的形态）；不做 base64（唯一用途是人眼直接看）；不照搬 kiro-gateway 的 `debug_logger.py` 实现（它是单例、每请求 `shutil.rmtree` 同一个共享目录，并发请求互相擦掉证据）；调度层与配置中心零改动 |
| 2.10 | 2026-09-19 | 时延分段归因与依赖饱和度首次成文（[6.10](#610-依赖饱和度与时延分段)）：流水新增 `dispatch_ms`、`upstream_ms` 两列（累计值，含全部重试），`/health` 与 `/admin/health` 新增 `pool`（五个数）与 `goroutines`。**行为变更**：此前 `latency_ms` 是一个不可拆的总数，一个 30 秒的请求分不清是调度层要目标要了很久、上游压着响应头不发、还是生成本来就长；`/health` 只对 PG 做 `Ping` 报 `ok`/`down`，连接池被占满时每个请求都慢而**每一条流水看上去都正常**——慢的那段在等连接上，那段不在任何一条请求的计时里。`upstream_ms` 的终点选在响应头到达而不是首帧，与 `MSA_RESPONSE_HEADER_TIMEOUT` 对齐；两段在重试时累加而非覆盖（与 `lossy` 的覆盖语义刻意相反）。**明确不做**：不引入 Prometheus/OpenTelemetry（四个参考仓库无一使用）；不做 sub2api 的 auth/routing/upstream/response 四段划分（本服务的入站鉴权是转发给调度层做的，没有独立的 auth 段；response 段与生成时间在 SSE 下不可分）；不加 `error_owner`/`is_business_limited`（SLA 口径的计算面在调度层）；不存请求体/响应体（sub2api 自己已把错误表里的 `request_body` 删掉）；不做 per-attempt 逐次快照（需另建表）；不起后台 goroutine 采样池指标（换来的是过时数字）；不给 `goroutines` 设阈值告警（本服务没有告警设施） |
| 2.9 | 2026-09-19 | 死连接探测与连接层归因首次成文（[6.9](#69-死连接探测与连接层归因)）：出站 transport 配 HTTP/2 PING 健康检查（`MSA_H2_SEND_PING_TIMEOUT`、`MSA_H2_PING_TIMEOUT`，默认各 15s），新增 `transport` 错误分类（502）与同名结果上报类别，该类别**不计入目标的失败计数**、调度层运行态零变更。**行为变更**：此前所有 `Do` 失败一律归 `upstream`，经 `retrying` 累计到目标的失败计数上——一条静默半开的连接会把一个完全健康的账号推向冷却；且没有主动探测，撞上死连接的请求只能等 120s 的响应头超时，那对一次尝试是整个重试预算（本机探针实测：不配 PING 时挂到 20s ctx 超时都不失败，配了 4s 内明确失败）。**明确不做**：HTTP/1.1 正常关闭不加重放（标准库已自动换连接重放，实测确认）；不自定义 `DialContext` 设 `KeepAlive`（`DefaultTransport` 的 Dialer 已带 30s，重写还会丢掉标准库后续的默认调整）；不做 per-origin 分片 transport（PING 是直接摘掉死连接，分片只缩小爆炸半径）；连接层失败不在同一目标上就地重试（换目标已能恢复，真正的收益是不记这个目标的失败）；committed 之后读流失败仍归 `upstream`（客户端已收到部分内容，换目标会拼出两段回答） |
| 2.8 | 2026-09-19 | 上游限流到期时刻首次成文（[6.8](#68-上游限流的到期时刻)）：从六类限流响应头与 Gemini 的 `google.rpc.RetryInfo` 解出「最早可以再来」的绝对时刻，随结果上报交给调度层精确冷却，并落进请求流水的 `retry_after` 列。**行为变更**：此前 `resp.Header` 在错误路径上被整体丢弃（全仓唯一读过上游响应头的地方是判 SSE 的 `Content-Type`），限流与普通上游错、超时同为 `retrying` 一档，调度层只能按失败计数累积后冷却一个固定时长——上游说「5 小时后再来」时我们一分钟后就又去撞，在整个限流窗口里反复空转，而每次空转都是一次真实的失败上报。`DecodeError` 签名因此从 `(status, body)` 改为 `(status, header, body)`（改签名而非加可选接口：限流头是 HTTP 层的，四个协议全都可能收到，漏一个就是缺口）。**明确不做**：不在数据面为同一目标睡等退避（数据面睡等会占住入站连接）；不做账号级或 (账号,模型) 级限流（账号身份在 upstream 侧，数据面看不到）；不做 `x-ratelimit-remaining-*` 的预测性避让（需要跨请求窗口状态，那是调度层的职责）；不从错误文案里抠 `"try again in 1.5s"` 这类说法（文案一改就静默失效，而失效方向是又开始瞎猜） |
| 2.7 | 2026-09-19 | 响应侧调参保真：`service_tier` 首次原样回显（Chat Completions 顶层、Responses 的 `response` 对象；`created` 与 `completed` 两帧任一带上都认），Anthropic 出站无此位时报 `dropped service_tier from the response`；上游回多路候选而中立表示只装得下一路时报出**实际丢弃路数**（`dropped N extra response candidate(s)`，一个流恒一条），该数字可与 `usage.output_tokens` 对账；Gemini 的 `finishMessage` 原文作为说明保留（截断 200 字节，**不改写 `stop_reason`**，枚举仍只由 `finishReason` 决定）。新增第三类说明措辞 `forwarded X but the result is not returned`——与 `dropped`（换个目标就有）、`filled in`（本服务补的值）区分开，它表示换谁都拿不到。`n` 与 `logprobs` 的请求侧/响应侧分工成文（[6.4](#64-跨协议能力差异)「调参的请求侧与响应侧分工」）。**绝不拿请求里的 `service_tier` 兜底**：点 `flex` 拿到 `default` 是被降档，兜底会把降档伪装成按要求执行，而这一维决定计费。**明确不做**（五项理由成文，见 [6.4](#64-跨协议能力差异)「响应侧明确不做的维度」）：响应 `logprobs`、`system_fingerprint`、Gemini 的 grounding/citation、`safetyRatings`、`text.format` 与 `truncation` 回显 |
| 2.6 | 2026-09-19 | 调参字段保真：13 个采样/候选/输出格式字段（penalties、`seed`、`n`、logprobs、`logit_bias`、`service_tier`、`parallel_tool_calls`、`response_format`、`verbosity`、`include`、`truncation`、客户端 `metadata`）首次进入中立表示，承载矩阵成文（[6.4](#64-跨协议能力差异)「调参字段的承载矩阵」）；Chat Completions 与 Responses 请求字段表补齐这些字段。**行为变更**：这些字段此前在解码阶段就被丢掉，**连同协议往返也丢**（本服务无透传快路径，`chat_completions → chat_completions` 一样经中立表示重建）；`max_tokens` 缺失时 Anthropic 出站的 4096 兜底现在会报有损 `filled in max_tokens`（此前无痕）。**明确不做**：不因目标不支持某字段而拒绝请求；不对小 `max_tokens` 设下限抬高；不做 `logit_bias` 的跨协议 token id 重映射；不在本地为 `n` 做扇出 |
| 2.5 | 2026-09-19 | 推理开关区分三态（没提 / 明确关闭 / 明确开启），首次成文（[6.4](#64-跨协议能力差异)「推理开关的三态」）：四个协议各自的关闭写法在入站被识别、在出站被写出；明确关闭不再被参数层的 `defaults` 翻转成开启；目标协议不支持推理时明确关闭不报有损。**行为变更**：此前客户端的明确关闭在出站一律被省略，上游按自身默认执行，对默认开启推理的模型等于把关闭请求改成了开启；`reasoning_effort:"none"` / `reasoning.effort:"none"` 此前会被当成强度档位折算成某个真实档位。**文档更正**：Anthropic `thinking` 字段曾写「`type` 非 `enabled` 视为关闭」，措辞上把「没提」也读成了关闭——省略该字段与 `disabled` 并不等价 |
| 2.4 | 2026-09-19 | 用量与 token 计数的对外口径：估算区分调度/公开两个方向，CJK 按字符加权（[6.6](#66-token-估算的两个方向)）；`count_tokens` 改用公开方向并补上估算口径说明（[5.5](#55-post-v1messagescount_tokens本地估算)）；`input_tokens` 补上用量兜底（原先只兜 output，上游不报时输入维度恒为 0）；上下文超限新增 `request is too long`、`input token count exceeds` 等文案，`token limit` 改为需伴随上下文语境的组合式判定。**行为变更**：`count_tokens` 对中文提示的回答从「每 4 字符 1 token」抬到「每字符 1.25 token」，带媒体块的请求不再报 0。**文档更正**：`input_tokens` 字段曾写「上游报的或估算的」，而在本轮之前它从不估算 |
| 2.31 | 2026-09-20 | 控制面 HTTP 传输加固（首次成文，[6.31](#631-控制面的-http-传输)）：五条修复，新增六个环境变量（`MSA_RELAY_MAX_IDLE_CONNS`、`MSA_RELAY_MAX_IDLE_CONNS_PER_HOST`、`MSA_RELAY_IDLE_CONN_TIMEOUT`、`MSA_RELAY_RESPONSE_HEADER_TIMEOUT`、`MSA_RELAY_TIMEOUT`、`MSA_RELAY_PROXY`），无新增端点、无新增列、无库变更、无新增依赖。轴线本身是上一轮留下的候选：数据面的出站传输层（[6.7](#67-出站连接层)）已经加固过五轮，而**控制面**——本服务调调度层那条链路——一直走裸 `http.Client{}`，`relayclient` 是全仓非测试代码里唯一一处。两者的隔离**不是遗漏而是设计**：`pipeline` 的传输构造注释原文写着它 `Clone` 标准库默认传输的目的正是「不污染 `relayclient`」，于是数据面每一轮加固都精确地绕过了控制面。而这条链路的调用密度与数据面同阶——**每一次**数据面请求先调一次 `/v1/dispatch`，换目标重试再调一次，收尾还调一次 `/v1/results`。依赖方向决定了不能复用数据面那套（`pipeline` import `relayclient`，反向复用会成环），因此控制面有自己的一份 `Options` 与常量，默认值刻意不同。**行为变更一**：`http.DefaultMaxIdleConnsPerHost` 是 **2**（本机探针实测），裸客户端因此对调度层只留两条空闲连接——并发 8 个请求跑 2 轮共 16 次调用，实测**拨号 16 次、复用 0 次**，每一次 dispatch 都是一次全新 TCP 加 TLS 握手而对端是同一个 host。现在 PerHost 默认 32、总量默认 64、`IdleConnTimeout` 默认 90s；数值刻意低于数据面的 32/256（数据面横跨整个账号池的多个上游 host，总量卡死会让 PerHost 白设；控制面只有**一个** host，64 已是 PerHost 的两倍余量）。零值取内置默认、负值表示显式不设限，与数据面同一约定。**行为变更二**：裸客户端只有一个覆盖到读完正文的 `Client.Timeout`，而「响应头迟迟不来」与「正文读到一半卡住」在归因上是两件事——本机实测 `Client.Timeout` 在读正文阶段命中时给出的文本是 `context deadline exceeded (Client.Timeout or context cancellation while reading body)`，与**调用方主动取消**逐字相同，排查的人分不出是调度层慢还是客户端走了。现在两层各管一段：`ResponseHeaderTimeout` 默认 **10s**、总 `Timeout` 默认 **30s**；10s 远低于数据面的 120s（数据面那一段要等上游生成第一个 token，控制面只是查一次目标选择），顺序由测试钉住（头不来时必须在 3 秒内以响应头超时失败，而不是等满 30 秒）。不可达一律标 `Retryable` 但归因**不是** `target_unavailable`——那个码表示候选耗尽，会让重试循环停下。**行为变更三**：`io.ReadAll(resp.Body)` 此前没有任何上限，而数据面早有三道（整份响应 32 MiB、上游错误体 64 KiB、入站 `MaxBytesReader`）；调度层若因故回一份失控的大体（一个把 SQL 结果整个吐出来的 bug、一个被插在中间的代理的错误页），本服务把它整个读进内存。现在上限 **8 MiB**，读法是 `io.LimitReader(body, limit+1)` 后比长度——三个参考仓库都用这个形态，cc-switch 的注释把理由写得最直白：「先收完再比等于上限没有意义」。**上限必须在读的时候生效而不是读完再判**，而这一条不能只断言错误消息：本轮变异验证里把 `LimitReader` 摘掉后消息断言照过（长度检查仍在后面），缺口是「把失控响应读进内存」这件事本身没被覆盖，补测改为**从对端侧量**实际写出的字节数。**超限判定排在状态码分支之前**——顺序反了的话，一个带着失控大体的 500 会先进错误解码，运维看到的是 `relay returned 500: <html>…` 的 256 字节 snippet 而真正的异常没有任何暴露面；超限**不可重试**（对端行为异常，再试一次它不会变小）。**行为变更四**：标准库默认跟随最多 10 跳，而控制面的对端是自家服务，任何 3xx 都是故障或有人在中间插了一跳。本机实测这条默认的三重后果：302 把带 body 的 POST 改写成**无体 GET**；同 host 跳转时 `Authorization` 被**保留**，客户端拿到 200 与目标伪造的 `{"models":[{"name":"evil"}]}`——即调度结果可被第三方替换；跨 host 跳转时凭据被标准库删掉但请求仍然成功且响应体照样被采纳。现在 `CheckRedirect` 一律拒绝并归因为不可重试的内部错误。错误文本有一条不显然的约束：**不能拼包装后的 `err`**——标准库把 `CheckRedirect` 的错误包进 `*url.Error` 时打的是**重定向之后**那个 URL 且含 query（实测 `Get "http://host/v1/models?leaked_key=…": relay returned a redirect`），于是攻击者构造的 query 顺着错误消息进日志与流水；`*url.Error` 会遮 userinfo 里的口令但**不遮 query**。处置是判哨兵后只用哨兵自己的文本，而哨兵文本里刻意不含任何 URL。设计阶段我曾假设标准库只打第一个请求的 URL，测试直接把这个假设打翻，代码随之改成现在这样。**行为变更五**：`ProxyFromEnvironment` 是标准库默认传输的一部分，`Clone` 会把它一起带过来，实测它对 compose 内的服务名 `http://modelsurge-replay:18101` **生效**（走本机的 7897）、对 `127.0.0.1`/`localhost` 不生效——也就是说 compose 形态下控制面的每一次 dispatch 都可能被一台外部代理看见并改写，而部署的人从不认为「内部服务调用」会出网。现在 `MSA_RELAY_PROXY` 二选一（`off` 默认清掉 `Clone` 带来的 `Proxy`、`environment` 显式承认走代理），**非法值拒绝启动**并在错误里点名变量名（与 `MSA_CAPTURE_MODE` 同口径，静默回落会让运维以为自己配的那一档生效了）；默认取 `off` 而不是保留标准库行为——控制面的对端在部署里总是内网地址，出网是例外。**明确不做（已核实）**：h2 ping 探测不给控制面（那两个参数针对长时间静默的流式连接，而控制面每次调用都是秒级往返，10s 的响应头超时已覆盖死连接）；不加 `InsecureSkipVerify` 开关（sub2api 把它做成硬错误，本服务连开关都不给）；不做 DNS rebinding 校验（拒绝重定向已挡掉「被引到别处」这一族，而校验解析出的 IP 要维护一张内网网段表，compose 的服务名解析结果本身就在私网）；不做客户端缓存或按配置分池、不做分片 transport（只有一个对端、一份配置）；响应体上限不做成可配（8 MiB 对一份目标选择结果是三个数量级的余量，能配就能配坏）。调度层与配置中心零改动 |
| 2.30 | 2026-09-20 | 记账与预算的内部一致性（首次成文，[6.30](#630-记账与预算的内部一致性)）：五条落差全部发生在请求成功之后或与请求成败无关，数据面一律 200、客户端拿到的东西完全正确，坏掉的只是运维与调度层看到的数字，以及一条能把请求收尾无限挂住的兜底路径。**行为变更一**：实时摘要与趋势桶补齐用量三维（`cache_read_tokens`/`cache_write_tokens`/`reasoning_tokens`），此前只带输入与输出两维——少报的恰好是计费权重最偏的那三维（缓存写通常 1.25×、缓存读 0.1×、推理计入输出计费），于是 `/admin/requests`（走明细表，五列俱全）与 `/admin/live`、`/admin/stats`（走缓存）长期对不上而两侧都不报错。三维**各自独立累计不加权**（按单价折算需要配置中心的定价模型，把不同单价的维度加进同一个数等于用错权重记账，且错的方向随缓存命中率变化、事后无法拆回）；Redis **不需要迁移**（`HIncrBy` 对缺字段按零起算，旧 JSON 摘要缺字段解出来也是零，两条都由测试钉住）；`Cache.Incr` 的签名从五个标量改成传整个 `relayclient.Usage`（此前一行里有六个 `int64` 参数，调错顺序编译器不报，而记错的是计费维度）。**行为变更二**：新增跨层断言——从 `schema.sql` 正则解出 `request_log` 所有以 `_tokens` 结尾的列名，与 `cache.LiveEntry`、`agentv1.LiveEntry` 的同后缀 json 名逐一比对，集合不等即报红。从 schema 解而不是手写清单（手写的会与 schema 一起漏同一维，于是断言与被测对象一起错）；解析同时覆盖 `CREATE TABLE` 与 `ALTER TABLE ADD COLUMN IF NOT EXISTS`（老库的维度走后一条路径，只扫前者会漏）；另有一条下限检查（解出少于五列即失败）防正则失效后断言静默空转。**行为变更三**：`LiveEntry` 新增 `log_persisted` 三态（缺省=没配 PG、未尝试落库；`false`=尝试过且失败，这条记录不在 `/admin/requests` 里；`true`=成功）。此前落库失败只写一行 Warn 而 Redis 两路照常成功，两个视图静默分叉且没有任何暴露面，看面板的人只能把差异当成自己看错了。**必须三态**：布尔零值会让「没配 PG」（正常的测试与单机形态）与「落库失败」（故障）变成同一个值。不为此新增 `outcome` 类别（`outcome` 描述的是这次请求对客户端的结局，而落库是本服务自己的副作用、客户端那侧完全正常；并进去会让调度层跟改，还会把一次成功的请求记成异常）。**行为变更四**：`ir.EstimateRequest` 补计 `ResponseFormat` 与 `ToolChoice`。结构化输出的 schema 与工具 schema 一样要进提示词且常有数千 token，漏掉它让这类请求被系统性低估；调度层把 `EstTokens` 透传进策略脚本的 `policy.Input`，而两仓都没有内建的窗口闸门，所以真实后果不是选错目标，而是**用户自己写的动态策略脚本拿到一个偏小的数、且看不出它偏小**。`Name` 与 `Schema` 都计入（只算 Schema 会让「同一份 schema 换个长名字」的两个请求估出一样的数）；`ToolChoice` **只计名字**（`Mode` 是 `auto`/`any`/`none`/`tool` 的枚举，出站编成结构化字段而非提示词文本，算进去等于凭空虚增）；两者都是指针，nil 时不贡献任何 token，由测试钉住。**行为变更五**：兜底入队改用独立的 `context.Background()` 派生超时，长度沿用 `Timeout`。此前复用直投那个已派生的 context，而直投**往往正是因为超时才失败的**——预算已耗尽，拿它入队必然立刻失败，上报既没直投成功也没落进队列，只剩一行 `report lost` 日志；而完全不设超时也不行：`Report` 跑在每请求上报队列的单消费者里而请求收尾要等它排空，PG **hang 住**（不是拒连，拒连会立刻返回错误）时客户端会越过它自己全部的预算（请求总时长、首字、idle）一直挂住，且服务端不报任何错。**明确不做**：`relayclient` 的传输层加固（自带 `http.Client` 绕过出站传输配置、`io.ReadAll` 无上限，已核实成立但不同轴，留作后续候选）；Redis 与 PG 的双写一致性（摘要是有 TTL 的观测数据，明细表才是账本，`log_persisted` 已把分叉暴露出来）；按模型定价对五维加权（定价在配置中心，本服务不持有它）；不新增环境变量、端点、列或库变更、依赖。调度层与配置中心零改动 |
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
| `MSA_HEARTBEAT_INTERVAL` | `15s` | 流式响应静默期内的客户端保活帧间隔；负值关闭（见 [6.17](#617-出站传输层的编码与客户端保活)） |
| `MSA_MAX_REQUEST_DURATION` | `0` | 一次请求的总时长上限，含全部重试。`0` 表示不限。非 `0` 时不得小于 `max(MSA_FIRST_TOKEN_TIMEOUT, MSA_IDLE_TIMEOUT)`，负值拒绝启动 |
| `MSA_SHUTDOWN_GRACE` | `max(首字超时, 空闲超时)` | 关停阶段 1：等在途流收尾的预算；显式设小于该最大值则拒绝启动（见 [6.19](#619-关停生命周期)） |
| `MSA_SHUTDOWN_LINGER` | `30s` | 关停阶段 2：宽限期用尽后继续等在途请求的上限；非正值拒绝启动 |
| `MSA_SHUTDOWN_FLUSH_TIMEOUT` | `10s` | 关停阶段 3：冲 outbox 的预算；非正值拒绝启动 |
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
| `MSA_RELAY_MAX_IDLE_CONNS_PER_HOST` | `32` | **控制面**（调调度层）每 host 空闲连接上限。标准库默认只留 2 条，实测并发 8 × 2 轮拨号 16 次复用 0 次。见 [6.31](#631-控制面的-http-传输) |
| `MSA_RELAY_MAX_IDLE_CONNS` | `64` | 控制面空闲连接总量上限。对端只有一个 host，给到 PerHost 的两倍余量即可 |
| `MSA_RELAY_IDLE_CONN_TIMEOUT` | `90s` | 控制面空闲连接多久后回收。负值表示不回收 |
| `MSA_RELAY_RESPONSE_HEADER_TIMEOUT` | `10s` | 控制面**只**约束「请求发出 → 响应头到达」这一段。远低于数据面的 120s：控制面只是查一次目标选择，不等上游生成。负值表示不设限 |
| `MSA_RELAY_TIMEOUT` | `30s` | 控制面单次调用的总时限，覆盖到读完正文。负值表示不设限 |
| `MSA_RELAY_PROXY` | `off` | 控制面代理策略，取 `off`/`environment`。**非法值拒绝启动**。默认 `off`：`Clone` 标准库传输会带上 `ProxyFromEnvironment`，实测它对 compose 服务名生效，于是内部调用会静默出网 |
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
