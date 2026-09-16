// Package httpapi 把客户端协议端点接到数据面。
//
// 公共数据面**没有自身鉴权**：客户端凭据原样转发给调度层比对，本服务只搬运。
// 因此它必须只暴露在受信网络或反代之后。管理面另有一把独立密钥。
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// Server 只负责协议入口的事：认出入站协议、取出客户端模型名与凭据、
// 交给数据面。选目标、重试、上报都在 pipeline 里。
type Server struct {
	Pipeline *pipeline.Pipeline
	Models   ModelLister
	Health   HealthChecker
	// Admin 为 nil 时不暴露管理面。
	Admin *Admin
	Log   *slog.Logger
	// AccessLog 关掉后仍会记 request_log，只是不打访问日志行。
	AccessLog bool
	// MaxBodyBytes 限制请求体大小，0 表示用默认值。
	MaxBodyBytes int64
}

// ModelLister 给出用户模型清单。实现方决定是否走缓存。
type ModelLister interface {
	List(ctx context.Context) ([]relayclient.UserModelSummary, error)
}

// HealthChecker 汇报各依赖的可用性。
type HealthChecker interface {
	Check(ctx context.Context) Health
}

type Health struct {
	Status        string `json:"status"`
	Database      string `json:"database"`
	Cache         string `json:"cache"`
	Relay         string `json:"relay"`
	OutboxPending int    `json:"outbox_pending"`
	OutboxDead    int    `json:"outbox_dead"`
}

// defaultMaxBody 是请求体上限。上下文塞满的请求确实很大，
// 但没有上限意味着一个坏客户端就能把内存吃光。
const defaultMaxBody = 64 << 20

// Handler 装好全部路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	register(mux, http.MethodPost, messagesPaths, s.dataPlane(codec.ProtocolAnthropic))
	register(mux, http.MethodPost, chatPaths, s.dataPlane(codec.ProtocolChatCompletions))
	register(mux, http.MethodPost, responsesPaths, s.dataPlane(codec.ProtocolResponses))
	register(mux, http.MethodPost, countTokensPaths, s.countTokens)

	for path, protocol := range modelPaths {
		mux.HandleFunc(http.MethodGet+" "+path, s.listModels(protocol))
	}
	mux.HandleFunc(http.MethodGet+" /health", s.health)

	// 管理面挂在同一个监听上，但鉴权完全独立：数据面转发客户端凭据给调度层，
	// 管理面用本服务自己的密钥。未配置 Admin 时 /admin/* 就是 404。
	if s.Admin != nil {
		mux.Handle("/admin/", s.Admin.Handler())
	}

	return s.withAccessLog(mux)
}

// dataPlane 处理一个入站协议的对话请求。
func (s *Server) dataPlane(protocol string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inbound, ok := codec.Inbound(protocol)
		if !ok {
			// 只可能是装配漏了 import，让它响而不是静默 404。
			http.Error(w, fmt.Sprintf("inbound codec %q not registered", protocol),
				http.StatusInternalServerError)
			return
		}

		body, err := s.readBody(w, r)
		if err != nil {
			writeIRError(w, inbound, ir.NewError(ir.ErrInvalidRequest, 0, "", err.Error()))
			return
		}
		req, decErr := inbound.DecodeRequest(body)
		if decErr != nil {
			writeIRError(w, inbound, asIRError(decErr))
			return
		}
		if req.Model == "" {
			writeIRError(w, inbound, ir.NewError(ir.ErrInvalidRequest, 0, "model",
				"model is required"))
			return
		}

		s.Pipeline.Serve(r.Context(), w, pipeline.Call{
			RequestID: requestID(r),
			Protocol:  protocol,
			Inbound:   inbound,
			Request:   req,
			UserModel: req.Model,
			ClientKey: clientKey(r),
			// 客户端要不要 SSE 由请求体的 stream 决定；对上游一律流式，与此无关。
			Stream: req.Stream,
		})
	}
}

// countTokens 本地估算，不打上游。
//
// 打上游要先 dispatch 选目标，而客户端调这个接口只是想知道自己的提示多长，
// 为此消耗一次配额并把延迟抬到几百毫秒不值得。估算偏保守（宁多勿少），
// 客户端据此裁剪上下文时不会踩到真正的上限。
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	inbound, _ := codec.Inbound(codec.ProtocolAnthropic)
	body, err := s.readBody(w, r)
	if err != nil {
		writeIRError(w, inbound, ir.NewError(ir.ErrInvalidRequest, 0, "", err.Error()))
		return
	}
	req, decErr := inbound.DecodeRequest(body)
	if decErr != nil {
		writeIRError(w, inbound, asIRError(decErr))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": ir.EstimateRequest(req)})
}

// listModels 代理调度层的清单，按请求路径渲染成对应协议的外形。
func (s *Server) listModels(protocol string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		models, err := s.Models.List(r.Context())
		if err != nil {
			inbound, _ := codec.Inbound(protocol)
			writeIRError(w, inbound, ir.NewError(ir.ErrUpstream, 0, "",
				"cannot list models: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, renderModels(protocol, models))
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	h := s.Health.Check(r.Context())
	status := http.StatusOK
	if h.Status != "ok" {
		// 降级也回 503：编排器与反代按状态码判定，只看 body 的很少。
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, h)
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limit := s.MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBody
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return raw, nil
}

// clientKey 从两个头里取客户端凭据。
//
// 两个都要看：Anthropic 的 SDK 发 x-api-key，OpenAI 的发 Authorization。
// 本服务不校验它，只原样转发给调度层比对。
func clientKey(r *http.Request) string {
	if v := r.Header.Get("x-api-key"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); v != "" {
		return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	}
	return ""
}

// requestID 沿用客户端给的 id，没有才生成。
//
// 沿用是为了让客户端日志与本服务流水能对上；重试时它保持不变，
// 所以 request_id 能把同一次客户端请求的多次尝试串起来。
func requestID(r *http.Request) string {
	for _, h := range []string{"X-Request-Id", "X-Request-ID", "Request-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeIRError 用客户端协议自己的错误形状回错，否则 SDK 会把它当成解码失败。
func writeIRError(w http.ResponseWriter, inbound codec.InboundCodec, err *ir.Error) {
	if inbound == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Message})
		return
	}
	status, body := inbound.RenderError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func asIRError(err error) *ir.Error {
	if e, ok := err.(*ir.Error); ok {
		return e
	}
	return ir.NewError(ir.ErrInvalidRequest, 0, "", err.Error())
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
