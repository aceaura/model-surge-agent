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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
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
	// Recorder 记受理面拒绝的流水。与 Pipeline 内部那个是同一个实现，
	// 但这里必须单独持有：pipeline 的记流水发生在选完目标之后，
	// 整条路径都假定有目标，而受理面拒绝时还没选过。
	Recorder pipeline.Recorder
	Log      *slog.Logger
	// AccessLog 关掉后仍会记 request_log，只是不打访问日志行。
	AccessLog bool
	// MaxBodyBytes 限制请求体大小，0 表示用默认值。
	MaxBodyBytes int64
	// CORSOrigins 是允许的跨域来源。空或含 `*` 表示放开所有来源，
	// 此时不发 Allow-Credentials（浏览器拒绝那个组合）。
	CORSOrigins []string
	// Captures 是转换四体捕获。nil 与 off 档等价，两者都不分配缓冲。
	Captures *capture.Store

	// ids 记住近期采纳过的客户端 id，用于撞号检测。
	//
	// 懒初始化而不是要求构造函数：Server 全仓都是结构体字面量装配的，
	// 加一个必填的构造步骤会让每个测试都要改，而漏改的那处会在运行时 nil。
	idsOnce sync.Once
	ids     *idGuard
}

// guard 取撞号检测器，首次调用时初始化。
func (s *Server) guard() *idGuard {
	s.idsOnce.Do(func() { s.ids = newIDGuard() })
	return s.ids
}

// ModelLister 给出用户模型清单。实现方决定是否走缓存。
type ModelLister interface {
	List(ctx context.Context) ([]relayclient.UserModelSummary, error)
}

// HealthChecker 汇报各依赖的可用性。
type HealthChecker interface {
	Check(ctx context.Context) Health
}

// Health 与 agentv1.Health 形状必须一致：admin.go 用整体类型转换把它交出去，
// 少一个字段就编译不过。这正是要的守卫——新加的字段不会只改一边。
type Health struct {
	Status        string `json:"status"`
	Database      string `json:"database"`
	Cache         string `json:"cache"`
	Relay         string `json:"relay"`
	OutboxPending int    `json:"outbox_pending"`
	OutboxDead    int    `json:"outbox_dead"`
	// 直接用 DTO 的指针类型而不是再定义一份：整体类型转换要求逐字段类型
	// 完全相同，两个同形状但不同名的指针类型转不过去。
	Pool       *agentv1.PoolStats `json:"pool,omitempty"`
	Goroutines int                `json:"goroutines"`
}

// Handler 装好全部路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	register(mux, http.MethodPost, messagesPaths, s.dataPlane(codec.ProtocolAnthropic))
	register(mux, http.MethodPost, chatPaths, s.dataPlane(codec.ProtocolChatCompletions))
	register(mux, http.MethodPost, responsesPaths, s.dataPlane(codec.ProtocolResponses))
	register(mux, http.MethodPost, countTokensPaths, s.countTokens)

	for path, family := range modelPaths {
		mux.HandleFunc(http.MethodGet+" "+path, s.listModels(family))
		// 单模型查询与清单共用路径前缀：SDK 的 models.retrieve() 是在
		// 它拿清单的那个 base_url 上拼 /{id}，两者必须成对存在。
		mux.HandleFunc(http.MethodGet+" "+path+modelIDSuffix, s.getModel(family))
	}
	mux.HandleFunc(http.MethodGet+" /health", s.health)

	// 管理面挂在同一个监听上，但鉴权完全独立：数据面转发客户端凭据给调度层，
	// 管理面用本服务自己的密钥。未配置 Admin 时 /admin/* 就是 404。
	if s.Admin != nil {
		mux.Handle("/admin/", s.Admin.Handler())
	}

	// 恢复在最最外：CORS 与访问日志自己也可能 panic。
	// CORS 在其内：预检要在路由之前答掉。访问日志在它之内，预检不进日志。
	return s.withRecover(s.withCORS(s.withAccessLog(s.withRejectEnvelope(mux))))
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
			s.reject(w, r, protocol, err)
			return
		}
		req, decErr := inbound.DecodeRequest(body)
		if decErr != nil {
			s.reject(w, r, protocol, asIRError(decErr))
			return
		}
		if req.Model == "" {
			s.reject(w, r, protocol, ir.NewError(ir.ErrInvalidRequest, 0, "model",
				"model is required"))
			return
		}

		id := requestID(r)
		// 记录键与回显 id 分离：id 可能来自客户端请求头，撞号时它会覆盖
		// 别人的流水并让本次上报被当重放丢弃。回显仍用 id（下面那行），
		// 落库、上报与捕获用 key。
		key, collided := s.guard().claim(id)
		if collided {
			// 运维可见：撞号本身可能是客户端的 id 生成有 bug，
			// 悄悄换个键会让那个 bug 永远不被发现。
			s.log().Warn("request id collision, using a generated record key",
				"client_request_id", id, "record_key", key)
		}
		// Begin 在解码成功之后：解码就失败的请求走 reject 那条路径，
		// 它连出站协议都没选过，四体里只会有一体，留下来只是噪音。
		capt := s.Captures.Begin(key)
		capt.Add(capture.ClientRequest, body)
		// 在 Serve 之前设，而不是让 pipeline 去设：http.Header 在
		// WriteHeader 之前的修改都会生效，无论谁设的。这样 SSE 与非流式
		// 两条路径不必各改一遍，pipeline 也不必知道这件事。
		w.Header().Set(headerRequestID, id)

		s.Pipeline.Serve(r.Context(), w, pipeline.Call{
			RequestID: id,
			RecordKey: key,
			Protocol:  protocol,
			Path:      r.URL.Path,
			Inbound:   inbound,
			Request:   req,
			UserModel: req.Model,
			ClientKey: clientKey(r),
			// 客户端要不要 SSE 由请求体的 stream 决定；对上游一律流式，与此无关。
			Stream:       req.Stream,
			Declarations: readDeclarations(r),
			Capture:      capt,
		})
	}
}

// countTokens 本地估算，不打上游。
//
// 打上游要先 dispatch 选目标，而客户端调这个接口只是想知道自己的提示多长，
// 为此消耗一次配额并把延迟抬到几百毫秒不值得。
//
// 用公开方向（宁多勿少）而非调度方向：客户端据这个数字裁上下文，低估会让它
// 裁完照样被上游以超长拒掉，而那时它已经删掉了历史。
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	inbound, _ := codec.Inbound(codec.ProtocolAnthropic)
	body, err := s.readBody(w, r)
	if err != nil {
		s.reject(w, r, codec.ProtocolAnthropic, err)
		return
	}
	req, decErr := inbound.DecodeRequest(body)
	if decErr != nil {
		s.reject(w, r, codec.ProtocolAnthropic, asIRError(decErr))
		return
	}
	w.Header().Set(headerRequestID, requestID(r))
	writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": ir.EstimateRequestMode(req, ir.ModePublic)})
}

// listModels 代理调度层的清单，按路径或请求头渲染成对应协议族的外形。
func (s *Server) listModels(pathFamily string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		family := clientFamily(r, pathFamily)
		models, ok := s.modelsOrFail(w, r, family)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, renderModels(family, models))
	}
}

// getModel 回单个模型对象。SDK 的 models.retrieve() 会打这个端点。
func (s *Server) getModel(pathFamily string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		family := clientFamily(r, pathFamily)
		models, ok := s.modelsOrFail(w, r, family)
		if !ok {
			return
		}
		id := modelIDFromPath(r.PathValue("id"))
		m, found := findModel(models, id)
		if !found {
			// 404 而非 200 包 error：new-api 是后者（controller/model.go:369），
			// 那让 SDK 看到成功状态码却解不出模型对象，客户端两边对不上。
			s.reject(w, r, envelopeProtocol(family),
				ir.NewError(ir.ErrNotFound, http.StatusNotFound, "model",
					"no such model: "+id))
			return
		}
		writeJSON(w, http.StatusOK, renderModel(family, m))
	}
}

// modelsOrFail 取清单，失败时已把错误写出去。
func (s *Server) modelsOrFail(w http.ResponseWriter, r *http.Request,
	family string) ([]relayclient.UserModelSummary, bool) {

	models, err := s.Models.List(r.Context())
	if err != nil {
		inbound, _ := codec.Inbound(envelopeProtocol(family))
		writeIRError(w, inbound, ir.NewError(ir.ErrUpstream, 0, "",
			"cannot list models: "+err.Error()))
		return nil, false
	}
	return models, true
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

// headerRequestID 是回显请求 ID 的响应头。
//
// 与读取时的第一优先头同名：客户端回显自己给的值时，看到的是同一个键。
const headerRequestID = "X-Request-Id"

// maxRequestIDLen 是沿用客户端请求 ID 的长度上限。
//
// 必须有上限：这个值会成为 request_log 的主键、X-Request-Id 响应头、
// 以及上报 ID（report_id 带 UNIQUE 约束）。128 够装 UUID 与带前缀的
// trace id。
const maxRequestIDLen = 128

// validRequestID 判断客户端给的 id 能否安全沿用。
//
// 不校验的后果是具体的：request_log 的主键写入是 ON CONFLICT DO UPDATE，
// 客户端发一个已存在的 id 就能改掉别人那一行。
//
// 字符集刻意不含空格与 `/`：前者让日志行难切分，后者在按 id 拼路径的
// 管理面查询里会改变 URL 结构。
func validRequestID(s string) bool {
	if s == "" || len(s) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// requestID 沿用客户端给的 id，没有或不合法才生成。
//
// 沿用是为了让客户端日志与本服务流水能对上；重试时它保持不变，
// 所以 request_id 能把同一次客户端请求的多次尝试串起来。
//
// 不合法时只是换掉，不拒绝请求：客户端的业务请求本身没问题，
// 拒绝会把一个可自愈的卫生问题变成故障。
func requestID(r *http.Request) string {
	for _, h := range []string{"X-Request-Id", "X-Request-ID", "Request-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); validRequestID(v) {
			return v
		}
	}
	return randomID()
}

// randomID 生成本服务自己的请求 id。
//
// 单独一个函数而不是内联：撞号换键那条路径也要生成，两处各写一遍会让
// 两种 id 的形状漂移，而运维是按前缀认它们的。
func randomID() string {
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
// 返回实际写出的状态码，供流水记录。
func writeIRError(w http.ResponseWriter, inbound codec.InboundCodec, err *ir.Error) int {
	return writeIRErrorStatus(w, inbound, err, 0)
}

// writeIRErrorStatus 同上，但可指定状态码。
//
// status 传 0 表示让信封自己推导。只有路由层拒绝要指定：405 的 Allow 头
// 已经随状态码发出，体里的码与头不一致会让客户端两边对不上。
func writeIRErrorStatus(w http.ResponseWriter, inbound codec.InboundCodec, err *ir.Error, status int) int {
	if inbound == nil {
		if status == 0 {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]string{"error": err.Message})
		return status
	}
	rendered, body := inbound.RenderError(err)
	if status == 0 {
		status = rendered
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return status
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
