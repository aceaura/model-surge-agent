# model-surge-agent

数据面：接住客户端的对话请求，问调度层要一个上游目标，把请求翻译成那个目标的协议发出去，再把上游的流翻译回客户端要的协议。

## 三向对接关系

```
客户端 ──anthropic / chat_completions / responses──▶ model-surge-agent ──POST /internal/v1/dispatch──▶ model-surge-relay
                                                        (本服务)         ◀──── target + decision ─────    (调度层)
                                                            │            ──POST /internal/v1/results─▶
                                                            ▼
                                              上游（四出站协议，含 gemini）
```

- **model-surge-relay** 决定「这次发给谁」并持有运行态（冷却、失败计数、用量）。本服务不做路由决策，只搬运。
- **客户端鉴权也在调度层**：本服务从请求头取出 `client_key` 原样塞进 dispatch，由 relay 比对。本服务不校验任何客户端凭据。
- 上游凭据由 relay 随 target 下发，只驻内存、只写进出站 HTTP 请求，不入库、不落日志。

## 协议转换

入站三个协议，出站四个：

| | anthropic | chat_completions | responses | gemini |
| --- | --- | --- | --- | --- |
| 入站 | ✅ | ✅ | ✅ | — |
| 出站 | ✅ | ✅ | ✅ | ✅ |

每个请求都走 `inbound.DecodeRequest → ir.Request → outbound.EncodeRequest → paramover.Apply`，**同协议也不例外**。不设字节透传快捷路径：两条代码路径会各自漂移，同协议的 bug 就无法被跨协议测试覆盖。

对上游**一律请求流式**。客户端要非流式时由本服务聚合 IR 事件再编成一次性响应，这样只需维护一套解码逻辑。

IR 以 Anthropic 的流式事件为超集。转换保真度由 `codec/crossmatrix_test.go` 的三入站 × 四出站矩阵守着，每格断言 tool_call / thinking / usage / stop_reason 四类语义。

### 参数覆盖作用在 wire body 上

target 带两层参数，`defaults`（缺失才填）与 `overrides`（无条件压盖）。它们在出站 codec 编码**之后**才作用上去，因此运维配的键是**出站协议的原生字段名**，协议特有的嵌套结构天然可表达：

```json
gemini:    {"generationConfig": {"thinkingConfig": {"thinkingBudget": 8192}}}
responses: {"reasoning": {"effort": "high"}}
anthropic: {"thinking": {"type": "enabled", "budget_tokens": 16384}}
```

### committed 边界

首帧成功解码之前，任何失败都能换目标重试（把失败的 `model_id` 追加进 `tried_ids` 再调一次 dispatch，无需租约）。首帧解码出事件的那一刻目标锁定：HTTP 状态码已定为 200，此后的错误只能作为流内事件表达，并补齐未闭合的块——否则客户端会一直等一个不会来的结束帧。

## 客户端入口

每个入站协议给三条路径别名。三份不是冗余：客户端把 base_url 配成 `host`、`host/v1`、`host/anthropic` 的都有，而多数客户端不允许改它拼在后面的固定路径。

| 协议 | 路径 |
| --- | --- |
| anthropic | `/v1/messages` · `/anthropic/v1/messages` · `/messages` |
| anthropic | `/v1/messages/count_tokens`（本地估算，不打上游） |
| chat_completions | `/v1/chat/completions` · `/openai/v1/chat/completions` · `/chat/completions` |
| responses | `/v1/responses` · `/openai/v1/responses` · `/responses` |
| 模型清单 | `/v1/models` · `/anthropic/v1/models`（anthropic 外形）· `/openai/v1/models` · `/models`（openai 外形） |
| 健康 | `/health`（降级时 503） |

`count_tokens` 本地估算：客户端调它只是想知道提示多长，为此 dispatch 一次、消耗配额并把延迟抬到几百毫秒不值得。

## 环境变量

| 变量 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- |
| `MSA_PG_DSN` | 是 | | PostgreSQL DSN，存请求流水与上报队列 |
| `MSA_RELAY_BASE_URL` | 是 | | model-surge-relay 的调度面地址 |
| `MSA_RELAY_DISPATCH_KEY` | 是 | | 调度面 Bearer 密钥 |
| `MSA_ADMIN_KEY` | 是 | | `/admin/*` 的 Bearer 密钥，必须与 dispatch key 不同 |
| `MSA_LISTEN` | 否 | `:8080` | 监听地址 |
| `MSA_REDIS_ADDR` | 否 | 空 | 留空则不启用缓存；实时页与趋势图为空，数据面照常工作 |
| `MSA_REDIS_PASSWORD` / `MSA_REDIS_DB` | 否 | 空 / `0` | |
| `MSA_CACHE_TTL` | 否 | `1m` | 模型清单缓存过期 |
| `MSA_MAX_ATTEMPTS` | 否 | `3` | 单次客户端请求最多换几个目标 |
| `MSA_FIRST_TOKEN_TIMEOUT` | 否 | `60s` | 首字节超时，超时前可换目标 |
| `MSA_IDLE_TIMEOUT` | 否 | `120s` | committed 之后的空闲超时 |
| `MSA_OUTBOX_INTERVAL` | 否 | `1s` | 上报队列扫描周期 |
| `MSA_OUTBOX_MAX_ATTEMPTS` | 否 | `20` | 超过则转死信，保留行供管理面查看 |
| `MSA_ESTIMATE_USAGE` | 否 | `true` | 上游没给 usage 时按字符数估算，否则调度层的用量统计永远是 0 |
| `MSA_ACCESS_LOG` | 否 | `true` | 关掉仍记 request_log，只是不打访问日志行 |
| `MSA_LOG_RETENTION` | 否 | `336h` | 流水保留时长，超期每小时清理一次 |

必填项缺失时启动失败，并一次列出全部缺失名称。

## 运行

```bash
export MSA_RELAY_BASE_URL=http://host.docker.internal:8081 \
       MSA_RELAY_DISPATCH_KEY=... \
       MSA_ADMIN_KEY=...
docker compose up -d --build
```

宿主端口取 5434 / 6381 / 8082，与 model-surge-upstream 的 5432 / 6379 / 8080 及 model-surge-relay 的 5433 / 6380 / 8081 并存不冲突。

数据库 DDL 由 `//go:embed schema.sql` 在启动时幂等执行，没有迁移框架。

### 公共数据面无自身鉴权

**这一点必须在部署时处理。** 本服务不校验客户端凭据，鉴权由调度层比对 `client_key` 完成。裸暴露到公网虽然仍需正确的 client_key 才能调通，但请求路径上的一切（限流、体积上限、错误信息）都没有额外防线。compose 默认把三个端口都绑 `127.0.0.1`，要对外服务请置于反代之后并自行加一层。

`/admin/*` 用独立的 `MSA_ADMIN_KEY`，与 dispatch key 隔离：相同的话任何能读管理面的人都能冒充本服务发调度请求，服务因此拒绝启动。

## 管理面 API

密钥：`Authorization: Bearer $MSA_ADMIN_KEY`。全部只读，除 outbox 重试。

> 完整的接口规范（含公共数据面三协议的请求/响应字段表、错误码、流式行为）见 [docs/api.md](docs/api.md)。下表是速查。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/admin/health` | 永远 200：它是拿来看状态的，把降级表达成 HTTP 错误会让前端分不清「服务降级」与「管理面自己不通」 |
| GET | `/admin/requests?limit&cursor&outcome&model_id&user_model&since` | 流水分页，游标不透明（内部是 `at,request_id`） |
| GET | `/admin/requests/{request_id}` | 详情含 tried_ids 与错误 |
| GET | `/admin/live?limit` | Redis 实时环，最新在前 |
| GET | `/admin/stats?window=1h\|24h` | 分钟桶：QPS、成功率、outcome 分布、token 量 |
| GET | `/admin/outbox?state=pending\|dead` | 待上报 / 死信 |
| POST | `/admin/outbox/{report_id}/retry` | 唯一写操作：重置死信的下次尝试时间 |
| GET | `/admin/models` | 调度层清单 + 各协议出站是否可用 |

`/admin/models` 的 `outbound_ready` 为假意味着调度层可能把请求路由到本服务没有实现出站编解码的协议上，那会以 `invalid_model` 收场。让它在管理面直接可见，而不是等线上报错才发现。

流水与实时环**绝不含凭据与对话内容**，访问日志同样不打请求体与凭据头。

### 降级语义

- Redis 挂了或未配置：`cache: down` / `disabled`，但 `status: ok`——只丢观测数据，数据面照常工作。实时页与趋势图返回空并带 `degraded: true`，以便前端区分「没流量」和「看不到」。
- PG 挂了：`status: degraded`。数据面仍放行（结果尽力直报），但这段时间的账可能对不上。
- relay 不可达：`status: degraded`，此时数据面无法选目标，等于完全不可用。

## 结果上报

每次尝试都向调度层上报一次 outcome 与用量，`report_id = request_id:attempt`，幂等。直报失败不算完事：落 `report_outbox`，由后台 worker 指数退避（1s 起翻倍、5min 封顶）重放。超过上限转死信而非删除——运维需要看到哪些用量没能上报。

outcome 语义由 relay 的 runstate 定义：`normal` 清零失败计数并累计用量；`abnormal` / `retrying` / `invalid_model` 累计失败可触发冷却；`context_exceeded` 完全不改运行态（输入太长是客户端的问题，不该记作目标的失败）。

## 测试

```bash
cd backend
TEST_PG_DSN="postgres://msa:msa@127.0.0.1:5434/msa?sslmode=disable" \
TEST_REDIS_ADDR=127.0.0.1:6381 \
go test -count=1 -p 1 ./...
```

`TEST_PG_DSN` / `TEST_REDIS_ADDR` 缺失时对应的包跳过。`-p 1` 是因为 PG 测试共用一个库。

- `codec/crossmatrix_test.go`：三入站 × 四出站矩阵
- `e2e/`：真 httpapi + 真 pipeline + 真 outbox worker + relaymock + 假上游，覆盖正常 / 换目标 / context_exceeded / committed 后断流 / 上报重放 / 参数覆盖生效
