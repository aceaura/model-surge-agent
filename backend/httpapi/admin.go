package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"
)

// Admin 是管理面。与公共数据面分开是因为鉴权模型不同：
// 数据面把客户端凭据转给调度层比对，管理面用本服务自己的一把密钥。
//
// 除了 outbox 重试，全部只读。管理面能改的越少，误操作的后果就越小。
type Admin struct {
	Key      string
	Requests RequestStore
	Outbox   OutboxStore
	Cache    *cache.Cache
	Models   ModelLister
	Health   HealthChecker
	// Captures 是转换四体捕获。nil 时两个端点回空列表与 404，而不是崩。
	Captures *capture.Store
}

// RequestStore 是流水查询。接口而非直接用 store：PG 挂了也要能起服务，
// 传 nil 时管理面报 unavailable 而不是崩。
type RequestStore interface {
	List(ctx context.Context, f store.Filter) ([]pipeline.Record, store.Cursor, error)
	Get(ctx context.Context, requestID string) (pipeline.Record, error)
}

type OutboxStore interface {
	List(ctx context.Context, state store.State, limit int) ([]store.Entry, error)
	Revive(ctx context.Context, reportID string) error
}

// Handler 装好 /admin 路由。
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/health", a.health)
	mux.HandleFunc("GET /admin/requests", a.listRequests)
	mux.HandleFunc("GET /admin/requests/{request_id}", a.getRequest)
	mux.HandleFunc("GET /admin/live", a.live)
	mux.HandleFunc("GET /admin/stats", a.stats)
	mux.HandleFunc("GET /admin/outbox", a.listOutbox)
	mux.HandleFunc("POST /admin/outbox/{report_id}/retry", a.retryOutbox)
	mux.HandleFunc("GET /admin/models", a.listModels)
	mux.HandleFunc("GET /admin/captures", a.listCaptures)
	mux.HandleFunc("GET /admin/captures/{request_id}", a.getCapture)
	return a.authed(mux)
}

// authed 用固定时间比较避免按前缀逐字符试出密钥。
func (a *Admin) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Key == "" || !equalKey(bearer(r), a.Key) {
			adminError(w, http.StatusUnauthorized, agentv1.CodeUnauthorized, "invalid admin key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Admin) health(w http.ResponseWriter, r *http.Request) {
	h := a.Health.Check(r.Context())
	// 管理面永远回 200：它是拿来看状态的，把降级表达成 HTTP 错误
	// 会让前端分不清「服务降级」与「管理面自己不通」。
	writeJSON(w, http.StatusOK, agentv1.Health(h))
}

func (a *Admin) listRequests(w http.ResponseWriter, r *http.Request) {
	if a.Requests == nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable,
			"request log is unavailable")
		return
	}
	q := r.URL.Query()
	filter := store.Filter{
		Outcome:   q.Get("outcome"),
		ModelID:   q.Get("model_id"),
		UserModel: q.Get("user_model"),
		Limit:     atoiOr(q.Get("limit"), 50),
	}
	if raw := q.Get("since"); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			adminError(w, http.StatusBadRequest, agentv1.CodeInvalidRequest,
				"since must be an RFC3339 timestamp")
			return
		}
		filter.Since = at
	}
	if raw := q.Get("cursor"); raw != "" {
		cursor, err := decodeCursor(raw)
		if err != nil {
			adminError(w, http.StatusBadRequest, agentv1.CodeInvalidRequest, err.Error())
			return
		}
		filter.Cursor = cursor
	}

	records, next, err := a.Requests.List(r.Context(), filter)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable, err.Error())
		return
	}
	page := agentv1.RequestPage{Requests: make([]agentv1.RequestSummary, 0, len(records))}
	for _, rec := range records {
		page.Requests = append(page.Requests, summaryOf(rec))
	}
	if !next.Zero() {
		page.NextCursor = encodeCursor(next)
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *Admin) getRequest(w http.ResponseWriter, r *http.Request) {
	if a.Requests == nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable,
			"request log is unavailable")
		return
	}
	rec, err := a.Requests.Get(r.Context(), r.PathValue("request_id"))
	if errors.Is(err, store.ErrNotFound) {
		adminError(w, http.StatusNotFound, agentv1.CodeNotFound, "no such request")
		return
	}
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detailOf(rec))
}

// live 读缓存的实时环。缓存不可用时回空集加 degraded 标记，
// 而不是报错：前端要能区分「没有流量」与「缓存挂了」。
func (a *Admin) live(w http.ResponseWriter, r *http.Request) {
	limit := atoiOr(r.URL.Query().Get("limit"), 100)
	entries := a.Cache.Live(r.Context(), limit)
	page := agentv1.LivePage{Entries: make([]agentv1.LiveEntry, 0, len(entries))}
	for _, e := range entries {
		page.Entries = append(page.Entries, agentv1.LiveEntry(e))
	}
	page.Degraded = a.Cache.Ping(r.Context()) != nil
	writeJSON(w, http.StatusOK, page)
}

func (a *Admin) stats(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	dur, label, err := parseWindow(window)
	if err != nil {
		adminError(w, http.StatusBadRequest, agentv1.CodeInvalidRequest, err.Error())
		return
	}
	buckets := a.Cache.Buckets(r.Context(), time.Now(), dur)
	out := agentv1.Stats{Window: label, Buckets: make([]agentv1.StatBucket, 0, len(buckets))}
	totals := agentv1.StatTotals{Outcomes: map[string]int64{}}
	for _, b := range buckets {
		out.Buckets = append(out.Buckets, agentv1.StatBucket{
			Minute:             b.Minute,
			Total:              b.Total,
			Outcomes:           b.Outcomes,
			InputTokens:        b.InputTokens,
			OutputTokens:       b.OutputTokens,
			CacheReadTokens:    b.CacheReadTokens,
			CacheWriteTokens:   b.CacheWriteTokens,
			ReasoningTokens:    b.ReasoningTokens,
			CacheWrite5mTokens: b.CacheWrite5mTokens,
			CacheWrite1hTokens: b.CacheWrite1hTokens,
			// 托管次数与音频/预测明细：逐列直搬，不加权。
			WebSearchRequests:        b.WebSearchRequests,
			WebFetchRequests:         b.WebFetchRequests,
			PromptAudioTokens:        b.PromptAudioTokens,
			CompletionAudioTokens:    b.CompletionAudioTokens,
			AcceptedPredictionTokens: b.AcceptedPredictionTokens,
			RejectedPredictionTokens: b.RejectedPredictionTokens,
			AvgLatencyMS:             avg(b.LatencySumMS, b.Total),
		})
		totals.Total += b.Total
		totals.InputTokens += b.InputTokens
		totals.OutputTokens += b.OutputTokens
		totals.CacheReadTokens += b.CacheReadTokens
		totals.CacheWriteTokens += b.CacheWriteTokens
		totals.ReasoningTokens += b.ReasoningTokens
		totals.CacheWrite5mTokens += b.CacheWrite5mTokens
		totals.CacheWrite1hTokens += b.CacheWrite1hTokens
		totals.WebSearchRequests += b.WebSearchRequests
		totals.WebFetchRequests += b.WebFetchRequests
		totals.PromptAudioTokens += b.PromptAudioTokens
		totals.CompletionAudioTokens += b.CompletionAudioTokens
		totals.AcceptedPredictionTokens += b.AcceptedPredictionTokens
		totals.RejectedPredictionTokens += b.RejectedPredictionTokens
		for k, v := range b.Outcomes {
			totals.Outcomes[k] += v
		}
	}
	if totals.Total > 0 {
		totals.SuccessRate = float64(totals.Outcomes[relayclient.OutcomeNormal]) / float64(totals.Total)
		totals.QPS = float64(totals.Total) / dur.Seconds()
	}
	out.Totals = totals
	out.Degraded = a.Cache.Ping(r.Context()) != nil
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) listOutbox(w http.ResponseWriter, r *http.Request) {
	if a.Outbox == nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable,
			"outbox is unavailable")
		return
	}
	state := store.StatePending
	switch raw := r.URL.Query().Get("state"); raw {
	case "", "pending":
	case "dead":
		state = store.StateDead
	default:
		adminError(w, http.StatusBadRequest, agentv1.CodeInvalidRequest,
			"state must be pending or dead")
		return
	}
	entries, err := a.Outbox.List(r.Context(), state, atoiOr(r.URL.Query().Get("limit"), 100))
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable, err.Error())
		return
	}
	page := agentv1.OutboxPage{Entries: make([]agentv1.OutboxEntry, 0, len(entries))}
	for _, e := range entries {
		page.Entries = append(page.Entries, agentv1.OutboxEntry{
			ReportID:      e.ReportID,
			RequestID:     e.Report.RequestID,
			ModelID:       e.Report.ModelID,
			Outcome:       e.Report.Outcome,
			Attempts:      e.Attempts,
			NextAttemptAt: e.NextAttemptAt,
			LastError:     e.LastError,
			CreatedAt:     e.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, page)
}

// retryOutbox 是管理面唯一的写操作：把死信项重新排进队列。
// 它只改 next_attempt_at 与 attempts，上报内容本身不可编辑。
func (a *Admin) retryOutbox(w http.ResponseWriter, r *http.Request) {
	if a.Outbox == nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable,
			"outbox is unavailable")
		return
	}
	err := a.Outbox.Revive(r.Context(), r.PathValue("report_id"))
	if errors.Is(err, store.ErrNotFound) {
		adminError(w, http.StatusNotFound, agentv1.CodeNotFound, "no such report")
		return
	}
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revived": true})
}

// listModels 把清单与「本服务能否真的发给它」并列。
//
// 调度层可能把请求路由到一个本服务没实现出站编解码的协议上，
// 那必然以 invalid_model 收场。在这里暴露出来，免得等线上报错才发现。
func (a *Admin) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := a.Models.List(r.Context())
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, agentv1.CodeUnavailable, err.Error())
		return
	}
	page := agentv1.ModelsPage{
		Models:   make([]agentv1.ModelInfo, 0, len(models)),
		Inbound:  codec.InboundNames(),
		Outbound: codec.OutboundNames(),
	}
	for _, m := range models {
		ready := false
		if m.Protocol != "" {
			_, ready = codec.Outbound(m.Protocol)
		}
		page.Models = append(page.Models, agentv1.ModelInfo{
			Name:          m.Name,
			Collection:    m.Collection,
			Policy:        m.Policy,
			Protocol:      m.Protocol,
			Enabled:       m.Enabled,
			OutboundReady: ready,
		})
	}
	writeJSON(w, http.StatusOK, page)
}

// --- helpers ---

func summaryOf(rec pipeline.Record) agentv1.RequestSummary {
	return agentv1.RequestSummary{
		RequestID:          rec.RequestID,
		At:                 rec.At,
		InboundProtocol:    rec.InboundProtocol,
		Path:               rec.Path,
		OutboundProtocol:   rec.OutboundProtocol,
		UserModel:          rec.UserModel,
		ModelID:            rec.ModelID,
		Account:            rec.Account,
		Outcome:            rec.Outcome,
		StatusCode:         rec.StatusCode,
		Attempts:           rec.Attempts,
		TriedIDs:           rec.TriedIDs,
		Committed:          rec.Committed,
		Stream:             rec.Stream,
		UsageEstimated:     rec.UsageEstimated,
		InputTokens:        rec.Usage.InputTokens,
		OutputTokens:       rec.Usage.OutputTokens,
		CacheReadTokens:    rec.Usage.CacheReadTokens,
		CacheWriteTokens:   rec.Usage.CacheWriteTokens,
		ReasoningTokens:    rec.Usage.ReasoningTokens,
		CacheWrite5mTokens: rec.Usage.CacheWrite5mTokens,
		CacheWrite1hTokens: rec.Usage.CacheWrite1hTokens,
		// 托管次数与音频/预测明细：逐列直搬，与明细表的列一一对应。
		WebSearchRequests:        rec.Usage.WebSearchRequests,
		WebFetchRequests:         rec.Usage.WebFetchRequests,
		PromptAudioTokens:        rec.Usage.PromptAudioTokens,
		CompletionAudioTokens:    rec.Usage.CompletionAudioTokens,
		AcceptedPredictionTokens: rec.Usage.AcceptedPredictionTokens,
		RejectedPredictionTokens: rec.Usage.RejectedPredictionTokens,
		LatencyMS:                rec.LatencyMS,
		FirstTokenMS:             rec.FirstTokenMS,
		DispatchMS:               rec.DispatchMS,
		UpstreamMS:               rec.UpstreamMS,
		ErrorCode:                rec.ErrorCode,
		ErrorMessage:             rec.ErrorMessage,
		Sanitized:                rec.Sanitized,
		Lossy:                    rec.Lossy,
	}
}

// detailOf 在列表项之外补上逐次尝试轨迹。
func detailOf(rec pipeline.Record) agentv1.RequestDetail {
	out := agentv1.RequestDetail{RequestSummary: summaryOf(rec)}
	for _, it := range rec.AttemptsTrail {
		out.AttemptsTrail = append(out.AttemptsTrail, agentv1.AttemptTrailItem{
			N:                it.N,
			ModelID:          it.ModelID,
			Account:          it.Account,
			OutboundProtocol: it.OutboundProtocol,
			Outcome:          it.Outcome,
			StatusCode:       it.StatusCode,
			DispatchMS:       it.DispatchMS,
			UpstreamMS:       it.UpstreamMS,
			ErrorCode:        it.ErrorCode,
			ErrorMessage:     it.ErrorMessage,
			RetryAfter:       it.RetryAfter,
		})
	}
	return out
}

// 游标编成一个不透明串：内部是 (at, request_id)，但让前端原样回传
// 而不是自己拼，改分页实现时就不必同步改前端。
func encodeCursor(c store.Cursor) string {
	raw, err := json.Marshal([2]string{c.At.UTC().Format(time.RFC3339Nano), c.RequestID})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (store.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.Cursor{}, fmt.Errorf("cursor is not a valid token")
	}
	var pair [2]string
	if err := json.Unmarshal(raw, &pair); err != nil {
		return store.Cursor{}, fmt.Errorf("cursor is not a valid token")
	}
	at, err := time.Parse(time.RFC3339Nano, pair[0])
	if err != nil {
		return store.Cursor{}, fmt.Errorf("cursor is not a valid token")
	}
	return store.Cursor{At: at, RequestID: pair[1]}, nil
}

// parseWindow 只接受两个档位：分钟桶只留 2 小时，更长的窗口读不到数据，
// 给出一个必然半空的图不如直接拒绝。
func parseWindow(raw string) (time.Duration, string, error) {
	switch raw {
	case "", "1h":
		return time.Hour, "1h", nil
	case "24h":
		// 桶只存 2 小时，所以 24h 实际拿到的是最近 2 小时。
		// 保留这个档位是为了前端的档位切换，不做额外承诺。
		return 2 * time.Hour, "24h", nil
	default:
		return 0, "", fmt.Errorf("window must be 1h or 24h")
	}
}

func avg(sum, n int64) int64 {
	if n <= 0 {
		return 0
	}
	return sum / n
}

func atoiOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
}

func adminError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, agentv1.ErrorEnvelope{Error: agentv1.Error{Code: code, Message: message}})
}

// equalKey 用固定时间比较：逐字符短路的比较能让攻击者按前缀试出密钥。
func equalKey(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// captureKindKeys 是四体在 JSON 里的键名，顺序与 capture.Kind 一致。
var captureKindKeys = [...]string{
	capture.ClientRequest:    "client_request",
	capture.UpstreamRequest:  "upstream_request",
	capture.UpstreamResponse: "upstream_response",
	capture.ClientResponse:   "client_response",
}

func (a *Admin) listCaptures(w http.ResponseWriter, r *http.Request) {
	out := agentv1.CaptureList{
		Mode:  string(a.Captures.Mode()),
		Items: []agentv1.CaptureSummary{},
	}
	for _, snap := range a.Captures.List() {
		sizes := map[string]int{}
		for i, key := range captureKindKeys {
			sizes[key] = len(snap.Bodies[i].Bytes)
		}
		out.Items = append(out.Items, agentv1.CaptureSummary{
			RequestID: snap.RequestID, At: snap.At, Sizes: sizes,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) getCapture(w http.ResponseWriter, r *http.Request) {
	snap, ok := a.Captures.Get(r.PathValue("request_id"))
	if !ok {
		// 回 404 而不是空对象：捕获是有上限的环，那一条可能已被淘汰，
		// 空对象会让读的人以为那次请求四体全空。
		adminError(w, http.StatusNotFound, agentv1.CodeNotFound, "capture not found")
		return
	}
	writeJSON(w, http.StatusOK, agentv1.CaptureDetail{
		RequestID:        snap.RequestID,
		At:               snap.At,
		ClientRequest:    captureBody(snap.Bodies[capture.ClientRequest]),
		UpstreamRequest:  captureBody(snap.Bodies[capture.UpstreamRequest]),
		UpstreamResponse: captureBody(snap.Bodies[capture.UpstreamResponse]),
		ClientResponse:   captureBody(snap.Bodies[capture.ClientResponse]),
	})
}

func captureBody(b capture.Body) agentv1.CaptureBody {
	return agentv1.CaptureBody{
		Body: string(b.Bytes), Truncated: b.Truncated, Dropped: b.Dropped,
	}
}
