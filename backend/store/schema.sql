-- 幂等 DDL，启动时整段执行。用它而非迁移框架：表结构简单，
-- 且这个服务的两张表都是可重建的运行记录，没有需要保留的业务主数据。

CREATE TABLE IF NOT EXISTS request_log (
  request_id        TEXT PRIMARY KEY,
  at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  inbound_protocol  TEXT NOT NULL,
  path              TEXT NOT NULL DEFAULT '',
  user_model        TEXT NOT NULL,
  outbound_protocol TEXT NOT NULL DEFAULT '',
  model_id          TEXT NOT NULL DEFAULT '',
  account           TEXT NOT NULL DEFAULT '',
  outcome           TEXT NOT NULL,
  status_code       INT NOT NULL DEFAULT 0,
  attempts          INT NOT NULL DEFAULT 1,
  tried_ids         JSONB NOT NULL DEFAULT '[]',
  committed         BOOLEAN NOT NULL DEFAULT FALSE,
  stream            BOOLEAN NOT NULL DEFAULT FALSE,
  usage_estimated   BOOLEAN NOT NULL DEFAULT FALSE,
  input_tokens      BIGINT NOT NULL DEFAULT 0,
  output_tokens     BIGINT NOT NULL DEFAULT 0,
  cache_read_tokens BIGINT NOT NULL DEFAULT 0,
  -- 缓存写入与推理消耗：客户端那侧本来就收到这两位，这里不落库会让流水
  -- 与客户端看到的账不一致，而差额随 prompt caching 使用率放大。
  cache_write_tokens BIGINT NOT NULL DEFAULT 0,
  reasoning_tokens   BIGINT NOT NULL DEFAULT 0,
  -- 缓存写入总量的 TTL 细分（anthropic cache_creation 明细）：1h 档单价
  -- 通常是 5m 的 2 倍，分档定价与对账要靠它。非 anthropic 上游恒为 0。
  cache_write_5m_tokens BIGINT NOT NULL DEFAULT 0,
  cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0,
  latency_ms        INT NOT NULL DEFAULT 0,
  first_token_ms    INT NOT NULL DEFAULT 0,
  -- 两段跨进程耗时的累计值（含全部重试）。latency_ms 减去两段
  -- 即「本服务自身 + 上游生成」，据此回答慢在上游还是慢在我们。
  dispatch_ms       INT NOT NULL DEFAULT 0,
  upstream_ms       INT NOT NULL DEFAULT 0,
  error_code        TEXT NOT NULL DEFAULT '',
  error_message     TEXT NOT NULL DEFAULT '',
  sanitized         JSONB NOT NULL DEFAULT '[]',
  lossy             JSONB NOT NULL DEFAULT '[]',
  -- 逐次尝试的轨迹。行内一列而不是另建 per-attempt 表：流水已是每请求
  -- 一行，建表就是每请求 N 行。代价是这一列不便索引。
  attempts_trail    JSONB NOT NULL DEFAULT '[]',
  -- 上游明示的该目标最早可重试时刻。NULL 表示上游没说。
  retry_after       TIMESTAMPTZ
);

-- 给先于这几列建起的旧表补列。
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS sanitized JSONB NOT NULL DEFAULT '[]';
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS lossy JSONB NOT NULL DEFAULT '[]';
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS path TEXT NOT NULL DEFAULT '';
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS retry_after TIMESTAMPTZ;
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS dispatch_ms INT NOT NULL DEFAULT 0;
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS upstream_ms INT NOT NULL DEFAULT 0;
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS attempts_trail JSONB NOT NULL DEFAULT '[]';
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS cache_write_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS reasoning_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS cache_write_5m_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS request_log_at_idx ON request_log(at DESC);
CREATE INDEX IF NOT EXISTS request_log_model_idx ON request_log(model_id, at DESC);

-- 结果上报必须送达，否则调度层的冷却与用量会失真。直报失败就入这张表，
-- 由后台 worker 重放。report_id 唯一约束同时充当幂等键：调度层按同键去重，
-- 所以重放是安全的。
CREATE TABLE IF NOT EXISTS report_outbox (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  report_id       TEXT NOT NULL UNIQUE,
  report_json     JSONB NOT NULL,
  attempts        INT NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_error      TEXT NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- 认领租约。取到期项时原子地写上，处理期间这一行对其它执行流不可见。
  -- 需要它是因为处理（一次真实 HTTP 上报）发生在事务之外：行锁随事务结束
  -- 就释放了，而那时上报还在飞。NULL 表示没人持有。
  lease_until     TIMESTAMPTZ,
  -- 租约的所有权凭据。写回裁决时带上它比对，认不上就说明这一行的所有权
  -- 已经不在调用方手里——可能是租约过期后被重新认领，也可能是管理面的
  -- 重试按钮清掉了它。没有这个凭据，一次迟到的裁决会覆盖掉运维的操作。
  lease_token     UUID
);

ALTER TABLE report_outbox ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
ALTER TABLE report_outbox ADD COLUMN IF NOT EXISTS lease_token UUID;

CREATE INDEX IF NOT EXISTS report_outbox_due_idx ON report_outbox(next_attempt_at);
