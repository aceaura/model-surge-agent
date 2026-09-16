// Package relaymock 是可编程的假调度层，供数据面测试与本地跑通用。
//
// 它按预设脚本逐次回答 dispatch，并记录收到的每份请求与结果上报，
// 这样测试可以断言「换目标时 tried_ids 确实累积了」这类跨调用的行为。
package relaymock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// Step 是一次 dispatch 的预设答案。Err 非空则返回错误，否则返回 Target。
type Step struct {
	Target relayclient.Target
	Err    *relayclient.Error
}

// Mock 记录收到的请求，供测试断言。
type Mock struct {
	// DispatchKey 非空时校验 Authorization。
	DispatchKey string
	// Models 是 /internal/v1/models 的答案。
	Models []relayclient.UserModelSummary

	mu        sync.Mutex
	steps     []Step
	served    int
	dispatch  []relayclient.DispatchRequest
	reports   []relayclient.ResultReport
	reportErr *relayclient.Error
}

func New(steps ...Step) *Mock { return &Mock{steps: steps} }

// FailReports 让后续 results 调用返回错误，用来驱动 outbox 入队路径。
func (m *Mock) FailReports(err *relayclient.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reportErr = err
}

// Dispatches 返回收到的全部 dispatch 请求，按顺序。
func (m *Mock) Dispatches() []relayclient.DispatchRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]relayclient.DispatchRequest(nil), m.dispatch...)
}

// Reports 返回收到的全部结果上报，按顺序。
func (m *Mock) Reports() []relayclient.ResultReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]relayclient.ResultReport(nil), m.reports...)
}

// Start 起一个 httptest 服务器；调用方负责 Close。
func (m *Mock) Start() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+relayclient.PathDispatch, m.handleDispatch)
	mux.HandleFunc("POST "+relayclient.PathResults, m.handleResults)
	mux.HandleFunc("GET "+relayclient.PathModels, m.handleModels)
	return httptest.NewServer(m.authed(mux))
}

func (m *Mock) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.DispatchKey != "" && r.Header.Get("Authorization") != "Bearer "+m.DispatchKey {
			writeErr(w, http.StatusUnauthorized, &relayclient.Error{
				Code: relayclient.CodeUnauthorized, Message: "bad dispatch key"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Mock) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var req relayclient.DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, &relayclient.Error{
			Code: relayclient.CodeInvalidRequest, Message: err.Error()})
		return
	}

	m.mu.Lock()
	m.dispatch = append(m.dispatch, req)
	step, ok := m.nextStepLocked()
	m.mu.Unlock()

	if !ok {
		// 脚本用尽等价于候选耗尽：重试循环见到它会停下，
		// 这样测试里跑飞的循环会立刻暴露而不是无限打下去。
		writeErr(w, http.StatusServiceUnavailable, &relayclient.Error{
			Code: relayclient.CodeTargetUnavailable, Retryable: true,
			Message: "mock ran out of steps"})
		return
	}
	if step.Err != nil {
		writeErr(w, statusFor(step.Err.Code), step.Err)
		return
	}
	writeJSON(w, http.StatusOK, relayclient.DispatchResponse{
		RequestID: req.RequestID,
		Target:    step.Target,
		Decision: relayclient.Decision{
			Policy: "mock", Collection: "mock", Group: "primary", GroupType: "fast",
			Candidates: []string{step.Target.ModelID},
		},
	})
}

func (m *Mock) nextStepLocked() (Step, bool) {
	if m.served >= len(m.steps) {
		return Step{}, false
	}
	step := m.steps[m.served]
	m.served++
	return step, true
}

func (m *Mock) handleResults(w http.ResponseWriter, r *http.Request) {
	var rep relayclient.ResultReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		writeErr(w, http.StatusBadRequest, &relayclient.Error{
			Code: relayclient.CodeInvalidRequest, Message: err.Error()})
		return
	}

	m.mu.Lock()
	failure := m.reportErr
	if failure == nil {
		m.reports = append(m.reports, rep)
	}
	m.mu.Unlock()

	if failure != nil {
		writeErr(w, statusFor(failure.Code), failure)
		return
	}
	writeJSON(w, http.StatusOK, relayclient.ReportResponse{Applied: true})
}

func (m *Mock) handleModels(w http.ResponseWriter, _ *http.Request) {
	models := m.Models
	if models == nil {
		models = []relayclient.UserModelSummary{}
	}
	writeJSON(w, http.StatusOK, relayclient.ModelsResponse{Models: models})
}

func statusFor(code string) int {
	switch code {
	case relayclient.CodeUnauthorized:
		return http.StatusUnauthorized
	case relayclient.CodeNotFound:
		return http.StatusNotFound
	case relayclient.CodeInvalidRequest:
		return http.StatusBadRequest
	case relayclient.CodeTargetUnavailable, relayclient.CodePolicyTimeout:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err *relayclient.Error) {
	writeJSON(w, status, map[string]*relayclient.Error{"error": err})
}
