package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RequestLog 存请求流水。绝不含凭据：写入的字段来自 pipeline.Record，
// 而那个类型本身就没有 headers。
type RequestLog struct {
	pool *pgxpool.Pool
}

func NewRequestLog(pool *pgxpool.Pool) *RequestLog { return &RequestLog{pool: pool} }

// Insert 写一条流水。同 request_id 覆盖：正常情况一个请求只写一次，
// 重复出现说明上游调用方重放了 request_id，保留最后一次更有诊断价值。
func (l *RequestLog) Insert(ctx context.Context, rec pipeline.Record) error {
	tried, err := json.Marshal(nonNil(rec.TriedIDs))
	if err != nil {
		return fmt.Errorf("encode tried_ids: %w", err)
	}
	sanitized, err := json.Marshal(nonNil(rec.Sanitized))
	if err != nil {
		return fmt.Errorf("encode sanitized: %w", err)
	}
	lossy, err := json.Marshal(nonNil(rec.Lossy))
	if err != nil {
		return fmt.Errorf("encode lossy: %w", err)
	}
	at := rec.At
	if at.IsZero() {
		at = time.Now()
	}
	_, err = l.pool.Exec(ctx, `
		INSERT INTO request_log (
			request_id, at, inbound_protocol, path, user_model, outbound_protocol,
			model_id, account, outcome, status_code, attempts, tried_ids,
			committed, stream, usage_estimated,
			input_tokens, output_tokens, cache_read_tokens,
			latency_ms, first_token_ms, error_code, error_message, sanitized, lossy)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
		ON CONFLICT (request_id) DO UPDATE SET
			at = EXCLUDED.at, outbound_protocol = EXCLUDED.outbound_protocol,
			model_id = EXCLUDED.model_id, account = EXCLUDED.account,
			outcome = EXCLUDED.outcome, status_code = EXCLUDED.status_code,
			attempts = EXCLUDED.attempts, tried_ids = EXCLUDED.tried_ids,
			committed = EXCLUDED.committed, usage_estimated = EXCLUDED.usage_estimated,
			input_tokens = EXCLUDED.input_tokens, output_tokens = EXCLUDED.output_tokens,
			cache_read_tokens = EXCLUDED.cache_read_tokens,
			latency_ms = EXCLUDED.latency_ms, first_token_ms = EXCLUDED.first_token_ms,
			error_code = EXCLUDED.error_code, error_message = EXCLUDED.error_message,
			sanitized = EXCLUDED.sanitized, lossy = EXCLUDED.lossy`,
		rec.RequestID, at, rec.InboundProtocol, rec.Path, rec.UserModel, rec.OutboundProtocol,
		rec.ModelID, rec.Account, rec.Outcome, rec.StatusCode, rec.Attempts, tried,
		rec.Committed, rec.Stream, rec.UsageEstimated,
		rec.Usage.InputTokens, rec.Usage.OutputTokens, rec.Usage.CacheReadTokens,
		rec.LatencyMS, rec.FirstTokenMS, rec.ErrorCode, rec.ErrorMessage, sanitized, lossy)
	return err
}

// Filter 是流水查询条件。空字段表示不筛。
type Filter struct {
	Outcome   string
	ModelID   string
	UserModel string
	Since     time.Time
	Limit     int
	// Cursor 是上一页最后一条的位置，用于键集分页。
	Cursor Cursor
}

// Cursor 用 (at, request_id) 做键集分页而非 OFFSET：
// 流水在持续写入，OFFSET 会让翻页时漏条或重条。
type Cursor struct {
	At        time.Time
	RequestID string
}

func (c Cursor) Zero() bool { return c.At.IsZero() && c.RequestID == "" }

const maxLimit = 200

// List 按时间倒序返回流水，并给出下一页游标（无更多则为零值）。
func (l *RequestLog) List(ctx context.Context, f Filter) ([]pipeline.Record, Cursor, error) {
	limit := f.Limit
	if limit <= 0 || limit > maxLimit {
		limit = 50
	}

	var (
		where []string
		args  []any
	)
	add := func(clause string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Outcome != "" {
		add("outcome = $%d", f.Outcome)
	}
	if f.ModelID != "" {
		add("model_id = $%d", f.ModelID)
	}
	if f.UserModel != "" {
		add("user_model = $%d", f.UserModel)
	}
	if !f.Since.IsZero() {
		add("at >= $%d", f.Since)
	}
	if !f.Cursor.Zero() {
		args = append(args, f.Cursor.At, f.Cursor.RequestID)
		where = append(where,
			fmt.Sprintf("(at, request_id) < ($%d, $%d)", len(args)-1, len(args)))
	}

	query := "SELECT " + recordColumns + " FROM request_log"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// 多取一条判断有没有下一页，省一次 COUNT。
	args = append(args, limit+1)
	query += fmt.Sprintf(" ORDER BY at DESC, request_id DESC LIMIT $%d", len(args))

	rows, err := l.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, Cursor{}, err
	}
	defer rows.Close()

	out := []pipeline.Record{}
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, Cursor{}, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, Cursor{}, err
	}

	var next Cursor
	if len(out) > limit {
		last := out[limit-1]
		next = Cursor{At: last.At, RequestID: last.RequestID}
		out = out[:limit]
	}
	return out, next, nil
}

func (l *RequestLog) Get(ctx context.Context, requestID string) (pipeline.Record, error) {
	rows, err := l.pool.Query(ctx,
		"SELECT "+recordColumns+" FROM request_log WHERE request_id = $1", requestID)
	if err != nil {
		return pipeline.Record{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return pipeline.Record{}, err
		}
		return pipeline.Record{}, ErrNotFound
	}
	return scanRecord(rows)
}

// Prune 删除早于 before 的流水，供保留期清理。
func (l *RequestLog) Prune(ctx context.Context, before time.Time) (int64, error) {
	tag, err := l.pool.Exec(ctx, "DELETE FROM request_log WHERE at < $1", before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

const recordColumns = `request_id, at, inbound_protocol, path, user_model, outbound_protocol,
	model_id, account, outcome, status_code, attempts, tried_ids,
	committed, stream, usage_estimated,
	input_tokens, output_tokens, cache_read_tokens,
	latency_ms, first_token_ms, error_code, error_message, sanitized, lossy`

func scanRecord(rows pgx.Rows) (pipeline.Record, error) {
	var (
		rec       pipeline.Record
		tried     []byte
		sanitized []byte
		lossy     []byte
	)
	if err := rows.Scan(&rec.RequestID, &rec.At, &rec.InboundProtocol, &rec.Path, &rec.UserModel,
		&rec.OutboundProtocol, &rec.ModelID, &rec.Account, &rec.Outcome, &rec.StatusCode,
		&rec.Attempts, &tried, &rec.Committed, &rec.Stream, &rec.UsageEstimated,
		&rec.Usage.InputTokens, &rec.Usage.OutputTokens, &rec.Usage.CacheReadTokens,
		&rec.LatencyMS, &rec.FirstTokenMS, &rec.ErrorCode, &rec.ErrorMessage,
		&sanitized, &lossy); err != nil {
		return rec, err
	}
	if len(tried) > 0 {
		if err := json.Unmarshal(tried, &rec.TriedIDs); err != nil {
			return rec, fmt.Errorf("decode tried_ids: %w", err)
		}
	}
	if len(sanitized) > 0 {
		if err := json.Unmarshal(sanitized, &rec.Sanitized); err != nil {
			return rec, fmt.Errorf("decode sanitized: %w", err)
		}
	}
	if len(lossy) > 0 {
		if err := json.Unmarshal(lossy, &rec.Lossy); err != nil {
			return rec, fmt.Errorf("decode lossy: %w", err)
		}
	}
	return rec, nil
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
