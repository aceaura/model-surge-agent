# model-surge-agent 依赖规范

> 本文从代码中提取本服务（数据面）对外部的全部依赖契约，供审核。
> 本服务自身对客户端暴露的接口规范见 `docs/api.md`，不在本文范围。

## 1. 依赖全景

```
客户端 ──三协议──▶ model-surge-agent ──HTTP/JSON──▶ model-surge-relay（调度层，必需）
                        │
                        ├──HTTPS/SSE──▶ 上游 LLM 提供方（四出站协议，按 target 动态寻址）
                        ├──pgx──────▶ PostgreSQL（必需：流水 + 上报队列）
                        └──RESP─────▶ Redis（可选：缓存与观测，缺失降级）
```

| 依赖 | 必要性 | 故障时行为 |
| --- | --- | --- |
| model-surge-relay | 硬依赖 | 数据面无法选目标，整体不可用；`/health` 报 `status: degraded` |
| 上游 LLM 提供方 | 硬依赖（由 relay 寻址） | 单目标失败可换目标重试；全部耗尽才向客户端报错 |
| PostgreSQL | 硬依赖（启动必填 DSN） | 运行时挂掉：数据面仍放行、结果尽力直报，流水可能缺失，`status: degraded` |
| Redis | 可选（`MSA_REDIS_ADDR` 留空即不启用） | 只丢观测数据（实时页/趋势图空并带 `degraded: true`），数据面照常 |

代码位置：`backend/cmd/server/main.go`（装配）、`backend/config/config.go`（配置来源）。

## 2. model-surge-relay 调度面契约

镜像自 `backend/relayclient/contract.go`（注释明确要求字段名与 relay 的 `contract/relayv1` 逐字一致，但不跨仓 import）。

### 2.1 端点

| 方法 | 路径 | 用途 | 调用方 |
| --- | --- | --- | --- |
| POST | `/v1/dispatch` | 问「这次发给谁」；换目标时把失败 `model_id` 追加进 `tried_ids` 重调，无需租约 | 每次客户端请求、每次换目标 |
| POST | `/v1/results` | 上报一次尝试的 outcome 与用量；`report_id` 幂等 | 每次尝试（直报，失败进 outbox 重放） |
| GET | `/v1/models` | 取用户模型清单 | 管理面 `/v1/models`、`/admin/models`、健康探针 |

鉴权：全部请求带 `Authorization: Bearer $MSA_RELAY_DISPATCH_KEY`。

### 2.2 DispatchRequest

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `model` | string | 客户端请求的用户模型名 |
| `inbound_protocol` | string | 入站协议名 |
| `client_key` | string | 从客户端请求头原样转发；本服务不校验，鉴权在 relay |
| `request_id` | string | 本服务生成的请求 ID |
| `tried_ids` | []string, omitempty | 已失败的目标，换目标时追加 |
| `est_tokens` | int, omitempty | 估算输入 token 数 |

### 2.3 DispatchResponse

`request_id` + `target` + `decision`。

**Target**（选中上游的全套信息）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `model_id` / `account` / `provider_id` | string | 标识 |
| `protocol` | string | 出站协议，决定用哪个出站 codec |
| `base_url` | string | 上游地址 |
| `native_model` | string | 上游原生模型名（gemini 要拼进 URL） |
| `context_window` | int, omitempty | 上下文窗口 |
| `headers` | map[string]string | **含凭据头**：只驻内存、只写进出站请求，不入库不落日志；日志经 `RedactHeaders` 脱敏（authorization / x-api-key / x-goog-api-key 遮蔽） |
| `defaults` / `overrides` | json.RawMessage | 两层参数，写出站协议原生字段名；在 wire body 编码之后作用（defaults 缺失才填、overrides 无条件压盖），见 `backend/paramover/paramover.go` |

**Decision**：`policy`、`policy_version`、`collection`、`group`、`group_type`、`note`、`candidates`、`skipped[{model_id, reason, detail}]`。

### 2.4 ResultReport

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `report_id` | string | `request_id:attempt`，幂等键，重放安全 |
| `request_id` / `model_id` | string | |
| `outcome` | string | 见下表 |
| `usage` | object | 五维：`input_tokens` / `output_tokens` / `cache_read_tokens` / `cache_write_tokens` / `reasoning_tokens`（omitempty；后两维 relay 暂不累计但本服务如实上报） |
| `retry_after` | time.Time, omitzero | 上游明示的最早可重试**时刻**；零值表示上游没说，relay 回落启发式 |

**Outcome 取值**（语义由 relay runstate 定义）：

| 值 | relay 侧效果 |
| --- | --- |
| `normal` | 清零失败计数、累计用量 |
| `abnormal` | 累计失败，可触发冷却 |
| `retrying` | 同 abnormal，但本次会继续换目标 |
| `invalid_model` | 目标本身不可用（上游 404、无出站 codec） |
| `transport` | 出站连接层故障，运行态零变更 |
| `context_exceeded` | 输入太长，运行态零变更 |

响应：`{"applied": bool}`。

### 2.5 错误信封与错误码

relay 错误体：`{"error": {"code", "message", "retryable", "field?"}}`。本服务**照用 relay 判定的 `retryable`，不重新推断**。

错误码：`unauthorized` / `not_found` / `invalid_request` / `target_unavailable`（=候选耗尽，重试循环必须停） / `policy_timeout` / `internal_error`。

拿不到信封时按状态码兜底映射（401/403→unauthorized、404→not_found、400→invalid_request、503→target_unavailable、其余→internal_error；5xx/503 判可重试），正文截取 256 字节进 message。

### 2.6 控制面传输层策略

代码：`backend/relayclient/transport.go`、`client.go`。

| 项 | 值 | 环境变量覆盖 |
| --- | --- | --- |
| 重定向 | **一律拒绝**（任何 3xx 视为故障，不读正文，错误文本不含 URL） | 不可配 |
| 响应体上限 | 8 MiB | 不可配 |
| 响应头超时 | 10s | `MSA_RELAY_RESPONSE_HEADER_TIMEOUT` |
| 整次调用总超时 | 30s | `MSA_RELAY_TIMEOUT` |
| 空闲连接池 | 总量 64 / PerHost 32 / 空闲 90s | `MSA_RELAY_MAX_IDLE_CONNS` 等 |
| 代理 | 默认 off（不走 HTTP_PROXY） | `MSA_RELAY_PROXY=off\|environment` |
| 健康探针 | GET /v1/models，3s 超时 | — |

### 2.7 上报送达保证（outbox）

代码：`backend/outbox/outbox.go`、`backend/store/outbox.go`。

- 每次尝试先直报（10s 超时，独立 context，与客户端请求生命周期解耦）；失败即落 `report_outbox` 表，**不可重试的错误也落库**（供管理面看 last_error）。
- 后台 worker 每 `MSA_OUTBOX_INTERVAL`（默认 1s）扫描，单轮最多 100 条，指数退避 1s 起翻倍、5min 封顶。
- 行级认领租约：`lease_until` + `lease_token`（UUID），租约 = max(3×单条超时, 30s)；写回裁决带 token 比对，认不上即放弃（`store.ErrLeaseLost`）。
- 超过 `MSA_OUTBOX_MAX_ATTEMPTS`（默认 20）转**死信**，行保留不删；管理面 `POST /admin/outbox/{report_id}/retry` 可重置。

## 3. 上游 LLM 提供方（出站数据面）

目标由 relay 的 `target` 动态下发，本服务不持有任何上游静态配置。代码：`backend/pipeline/upstream.go`、`backend/codec/*`。

### 3.1 四出站协议端点

对上游**一律请求流式**（客户端要非流式时由本服务聚合 IR 事件再编成一次性响应）。

| 协议 | 端点拼法 | 协议特有头 |
| --- | --- | --- |
| anthropic | `POST {base_url}/v1/messages` | 客户端声明的 `anthropic-version` / beta 令牌可透传；本服务不注入自有 beta 令牌 |
| chat_completions | `POST {base_url}/chat/completions` | — |
| responses | `POST {base_url}/responses` | — |
| gemini | `POST {base_url}/models/{native_model}:streamGenerateContent?alt=sse`；`native_model` 拼不出安全路径段时本次尝试记为可换目标的失败 | API key 走 `headers` 下发的 `x-goog-api-key` |

### 3.2 公共出站形态

- 固定头：`Content-Type: application/json`、`Accept: text/event-stream`。
- 头的三层来源按序写入，均过保护集合（禁止覆盖 `Accept-Encoding` 等）：① 端点定义（codec 自带）→ ② 客户端协议声明（`codec.DeclarationEncoder`）→ ③ relay 下发的 `target.Headers`（凭据，优先级最高）。
- 凭据只存在于 `http.Request`，不入库、不落日志、不进捕获（四体捕获只存 body 不存 header）。
- `target.Defaults` / `target.Overrides` 在出站 codec 编码之后逐层作用于 wire body。

### 3.3 上游响应处理

| 行为 | 规范 |
| --- | --- |
| 非 2xx | 错误体解压后读至多 64 KiB，交出站 codec `DecodeError` 归一化（区分限流/配额/上下文超长等）；3xx 直接判不可跟随 |
| Content-Type 非 SSE | 整份读取（上限 32 MiB），投影成事件序列回放；空体按「一帧没发」处理可换目标；HTML 体单独归因（提示有代理/WAF 拦截） |
| SSE | 逐帧解码；首帧成功解码后目标锁定（committed），此前任何失败都可换目标 |
| 回传响应头 | **白名单前缀**：`anthropic-ratelimit-*`、`x-ratelimit-*`；`Retry-After` 不回传（已由 ratelimit 模块解析后按本服务口径另写） |
| 连接复用 | 关闭前排空响应体（上限 1 MiB），否则连接不放回池 |

### 3.4 出站连接层参数

代码：`backend/pipeline/transport.go`。

| 项 | 默认 | 环境变量 |
| --- | --- | --- |
| 空闲连接池 | 总量 256 / PerHost 32 / 空闲 90s | `MSA_MAX_IDLE_CONNS` / `MSA_MAX_IDLE_CONNS_PER_HOST` / `MSA_IDLE_CONN_TIMEOUT` |
| 响应头超时 | 120s（必须宽于首帧超时） | `MSA_RESPONSE_HEADER_TIMEOUT` |
| 整次调用 Client.Timeout | **不设**（SSE 可跑几分钟；流的时限由首帧 60s / 空闲 120s 两个计时器负责） | `MSA_FIRST_TOKEN_TIMEOUT` / `MSA_IDLE_TIMEOUT` |
| h2 死连接探测 | 空闲 15s 发 PING、15s 无 PONG 判失联（任一负值整组关闭） | `MSA_H2_SEND_PING_TIMEOUT` / `MSA_H2_PING_TIMEOUT` |
| 代理 | 走环境变量（`ProxyFromEnvironment`，刻意保留以支持代理出网部署） | 系统环境 |
| 重定向 | 自定义策略：摘除凭据后的跨 host 跳转被拒并记损 | 不可配 |

## 4. PostgreSQL

驱动 `github.com/jackc/pgx/v5`（pgxpool）。DSN 来自 `MSA_PG_DSN`（必填）；`MSA_PG_MAX_CONNS` 覆盖池上限（0 = 驱动默认 32）。

DDL 由 `backend/store/schema.sql` 经 `//go:embed` 在启动时**幂等整段执行**，无迁移框架；两张表均为可重建的运行记录。

### 4.1 `request_log`（请求流水，每请求一行）

关键列：`request_id`(PK)、`at`、`inbound_protocol`、`path`、`user_model`、`outbound_protocol`、`model_id`、`account`、`outcome`、`status_code`、`attempts`、`tried_ids`(JSONB)、`committed`、`stream`、`usage_estimated`、五维 token（`input/output/cache_read/cache_write/reasoning_tokens`）、`latency_ms` / `first_token_ms` / `dispatch_ms` / `upstream_ms`、`error_code` / `error_message`、`sanitized`(JSONB) / `lossy`(JSONB)、`attempts_trail`(JSONB)、`retry_after`。

索引：`(at DESC)`、`(model_id, at DESC)`。保留期 `MSA_LOG_RETENTION`（默认 336h），每小时清理。**绝不含凭据与对话内容**。

### 4.2 `report_outbox`（上报队列）

`id`(IDENTITY PK)、`report_id`(UNIQUE，幂等键)、`report_json`(JSONB)、`attempts`、`next_attempt_at`、`last_error`、`created_at`、`lease_until`、`lease_token`(UUID)。索引 `(next_attempt_at)`。

## 5. Redis（可选）

驱动 `github.com/redis/go-redis/v9`。未配置即整个 cache 层为 nil，全部方法走降级；任何 Redis 错误按「没有」处理。客户端禁重试（`MaxRetries: -1`），拨号/读/写超时均 2s。

键空间（前缀 `msa:` 防与其它服务撞键），**凭据与 target 永不入 Redis**：

| 键 | 结构 | 内容 | TTL / 容量 |
| --- | --- | --- | --- |
| `msa:models` | String (JSON) | relay 用户模型清单缓存 | `MSA_CACHE_TTL`（默认 1m） |
| `msa:live` | List (JSON 行) | 实时流水环，LPUSH+LTRIM | 500 条封顶 |
| `msa:stat:{YYYYMMDDHHMM}` | Hash | 分钟桶：`total`、`o:{outcome}`、`input`/`output`/`cache_read`/`cache_write`/`reasoning`、`latency` | 2h（每次写入刷新） |

## 6. Go 模块依赖

`backend/go.mod`（Go 1.25）：

| 模块 | 版本 | 用途 |
| --- | --- | --- |
| `github.com/jackc/pgx/v5` | v5.11.0 | PostgreSQL 驱动与连接池 |
| `github.com/redis/go-redis/v9` | v9.22.0 | Redis 客户端 |
| `golang.org/x/sync` | v0.17.0 | 并发原语 |

直接依赖仅此三项，其余均为间接。协议编解码（SSE、JSON）全部基于标准库手写，无第三方 LLM SDK。

## 7. 部署与运行时依赖

- **构建**：`golang:1.25-alpine`；**运行**：`alpine:3.21` + `ca-certificates`（出站 HTTPS 校验）+ `tzdata`，非 root 用户 `msa`（uid 10001），`CGO_ENABLED=0` 静态编译。
- **compose 拓扑**：postgres:16-alpine（宿主 127.0.0.1:5434）、redis:7-alpine（127.0.0.1:6381）、backend（127.0.0.1:8082）；端口与同族两仓（upstream 5432/6379/8080、relay 5433/6380/8081）错开可并存。backend 依赖 postgres/redis 健康检查通过后启动。
- **绑定地址**：compose 默认全绑 127.0.0.1。公共数据面**无自身鉴权**（客户端凭据由 relay 比对 `client_key`），对外服务必须置于反代之后。
- **必填环境变量**（缺失一次性报全、拒绝启动）：`MSA_PG_DSN`、`MSA_RELAY_BASE_URL`、`MSA_RELAY_DISPATCH_KEY`、`MSA_ADMIN_KEY`（且必须与 dispatch key 不同）。
- 其余环境变量与默认值见 README「环境变量」一节及 `backend/config/config.go`；配置间有交叉校验（如 `MSA_HEARTBEAT_INTERVAL` 必须小于两个流超时、`MSA_MAX_REQUEST_DURATION` 不小于单次尝试超时、`MSA_SHUTDOWN_GRACE` 不小于流超时）。
