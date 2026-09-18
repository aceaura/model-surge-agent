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
  latency_ms        INT NOT NULL DEFAULT 0,
  first_token_ms    INT NOT NULL DEFAULT 0,
  error_code        TEXT NOT NULL DEFAULT '',
  error_message     TEXT NOT NULL DEFAULT '',
  sanitized         JSONB NOT NULL DEFAULT '[]',
  lossy             JSONB NOT NULL DEFAULT '[]',
  -- 上游明示的该目标最早可重试时刻。NULL 表示上游没说。
  retry_after       TIMESTAMPTZ
);

-- 给先于这几列建起的旧表补列。
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS sanitized JSONB NOT NULL DEFAULT '[]';
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS lossy JSONB NOT NULL DEFAULT '[]';
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS path TEXT NOT NULL DEFAULT '';
ALTER TABLE request_log ADD COLUMN IF NOT EXISTS retry_after TIMESTAMPTZ;

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
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS report_outbox_due_idx ON report_outbox(next_attempt_at);
