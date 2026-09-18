// Package pipeline 是数据面核心：向调度层要目标、编码请求、桥接上游流、
// 判定结果并上报。
//
// 每个请求都走 inbound.DecodeRequest → ir.Request → outbound.EncodeRequest →
// paramover.Apply，同协议也不例外。不设字节透传快捷路径：两条代码路径会
// 各自漂移，同协议的 bug 就无法被跨协议测试覆盖。
package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/paramover"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

type Dispatcher interface {
	Dispatch(ctx context.Context, req relayclient.DispatchRequest) (*relayclient.DispatchResponse, error)
}

// Reporter 接收结果上报。实现方负责送达（直报失败则入 outbox），
// 因此这里不返回错误：数据面不该因为上报失败而影响客户端。
type Reporter interface {
	Report(rep relayclient.ResultReport)
}

// Recorder 记录请求流水，供管理面查询。同样不返回错误。
type Recorder interface {
	Record(rec Record)
}

// Record 是一次客户端请求的完整流水。绝不含凭据。
type Record struct {
	RequestID        string
	At               time.Time
	InboundProtocol  string
	UserModel        string
	OutboundProtocol string
	ModelID          string
	Account          string
	Outcome          string
	StatusCode       int
	Attempts         int
	TriedIDs         []string
	Committed        bool
	Stream           bool
	UsageEstimated   bool
	Usage            relayclient.Usage
	LatencyMS        int
	FirstTokenMS     int
	ErrorCode        string
	ErrorMessage     string
	// Sanitized 是 ir.Sanitize 对本次请求做出的修复说明；
	// 为空表示请求本身合法，未被改动。
	Sanitized []string
	// Lossy 是出站编码因目标协议表达不了而丢弃的字段说明。
	//
	// 与 Sanitized 刻意分列两个字段：前者说「客户端发来的请求畸形，我修了」，
	// 指向客户端 bug；后者说「请求本身合法，是这个目标承不住」，指向路由选型。
	// 合成一列就无法据此判断该改客户端还是该换目标。
	Lossy []string
	// responseLossy 是响应侧编码丢弃的说明，落库前并入 Lossy 对外暴露。
	//
	// 与请求侧刻意用两种写法：请求侧每次尝试整体覆盖（换目标重试时，
	// 上一个目标的丢弃项描述的是一条没被采用的路径，混进来会把排查引错），
	// 响应侧则累加——响应侧发生在已 committed 之后，不存在换目标重试，
	// 一个流里同类丢弃出现多次都属于同一次实际响应。
	responseLossy []string
}

// addResponseLossy 累加响应侧说明。合并与去重推迟到落库前统一做。
func (r *Record) addResponseLossy(notes ...string) {
	r.responseLossy = append(r.responseLossy, notes...)
}

// mergedLossy 把请求侧与响应侧说明合成对外的一列，去重并排序。
// 两侧都为空时返回 nil，保持「无丢弃则字段不出现」的既有语义。
func (r *Record) mergedLossy() []string {
	if len(r.Lossy) == 0 && len(r.responseLossy) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(r.Lossy)+len(r.responseLossy))
	out := make([]string, 0, len(r.Lossy)+len(r.responseLossy))
	for _, n := range append(append([]string{}, r.Lossy...), r.responseLossy...) {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

type Options struct {
	MaxAttempts       int
	FirstTokenTimeout time.Duration
	IdleTimeout       time.Duration
	// EstimateUsage 在上游没给 usage 时用字符数估算兜底。
	EstimateUsage bool
}

type Pipeline struct {
	Dispatch Dispatcher
	HTTP     *http.Client
	Reporter Reporter
	Recorder Recorder
	Opts     Options
	// Now 可注入以便测试断言时延字段。
	Now func() time.Time
}

// Call 是一次已解码的客户端请求。
type Call struct {
	RequestID string
	// Protocol 是入站协议名，转发给调度层供策略脚本参考。
	Protocol string
	Inbound  codec.InboundCodec
	Request  *ir.Request
	// UserModel 是客户端请求里的模型名（user model，不是上游 model_id）。
	UserModel string
	// ClientKey 原样转发给调度层比对，本服务不做客户端鉴权。
	ClientKey string
	// Stream 表示客户端要不要 SSE。对上游一律流式，与此无关。
	Stream bool
}

// Serve 处理一次客户端请求，自行把响应或错误写进 w。
func (p *Pipeline) Serve(ctx context.Context, w http.ResponseWriter, call Call) {
	start := p.now()
	// 先修畸形再估算：sanitize 会增删块，而 est_tokens 只算一次并在重试间复用。
	sanitized := ir.Sanitize(call.Request)
	// est_tokens 只在首次 dispatch 前算一次，重试复用：请求没变，重算没意义。
	est := int(ir.EstimateRequest(call.Request))

	rec := Record{
		RequestID:       call.RequestID,
		At:              start,
		InboundProtocol: call.Protocol,
		UserModel:       call.UserModel,
		Stream:          call.Stream,
		Sanitized:       sanitized,
	}
	defer func() {
		rec.LatencyMS = msSince(start, p.now())
		p.record(rec)
	}()

	var tried []string
	var lastErr *ir.Error

	for attempt := range max(p.Opts.MaxAttempts, 1) {
		rec.Attempts = attempt + 1
		rec.TriedIDs = tried

		disp, err := p.Dispatch.Dispatch(ctx, relayclient.DispatchRequest{
			Model:           call.UserModel,
			InboundProtocol: call.Protocol,
			ClientKey:       call.ClientKey,
			RequestID:       call.RequestID,
			TriedIDs:        tried,
			EstTokens:       est,
		})
		if err != nil {
			// 调度层没给出目标，没有 model_id 可上报，只能回错。
			lastErr = dispatchError(err)
			p.fail(w, call, &rec, lastErr)
			return
		}

		target := disp.Target
		rec.ModelID = target.ModelID
		rec.Account = target.Account
		rec.OutboundProtocol = target.Protocol

		outcome, res := p.attempt(ctx, w, call, target, attempt, start, &rec)
		if res.err == nil {
			p.report(call, target, attempt, relayclient.OutcomeNormal, res.usage)
			rec.Outcome = relayclient.OutcomeNormal
			rec.Usage = res.usage
			return
		}
		lastErr = res.err

		// committed 之后目标已锁定：响应已经开始写出，
		// 换目标会让客户端看到两段拼接的回答。
		if res.committed {
			p.report(call, target, attempt, outcome, res.usage)
			rec.Outcome = outcome
			rec.Usage = res.usage
			return
		}
		// 换目标也不会好（参数错、上下文超限），直接回错。
		if !res.err.Retryable {
			p.report(call, target, attempt, outcome, res.usage)
			rec.Outcome = outcome
			rec.Usage = res.usage
			p.fail(w, call, &rec, res.err)
			return
		}

		tried = append(tried, target.ModelID)
		last := attempt == max(p.Opts.MaxAttempts, 1)-1
		if last {
			// 不再重试，所以是 abnormal 而非 retrying：后者表示还会换目标。
			p.report(call, target, attempt, downgrade(outcome), res.usage)
			rec.Outcome = downgrade(outcome)
			rec.TriedIDs = tried
			break
		}
		p.report(call, target, attempt, outcome, res.usage)
		rec.TriedIDs = tried
	}

	p.fail(w, call, &rec, lastErr)
}

type attemptResult struct {
	err *ir.Error
	// committed 表示目标已锁定，不能再换。
	committed bool
	usage     relayclient.Usage
}

// attempt 跑一次目标：编码、发请求、桥流。返回 outcome 与结果。
func (p *Pipeline) attempt(ctx context.Context, w http.ResponseWriter, call Call,
	target relayclient.Target, attempt int, start time.Time, rec *Record) (string, attemptResult) {

	outbound, ok := codec.Outbound(target.Protocol)
	if !ok {
		// 配置里写了本服务没实现的协议：这个目标永远不可用，
		// 但换一个可能可以，所以标成可重试的 invalid_model。
		return relayclient.OutcomeInvalidModel, attemptResult{
			err: retryableErr(ir.NewError(ir.ErrInternal, 0, "",
				fmt.Sprintf("no outbound codec for protocol %q", target.Protocol))),
		}
	}

	req := call.Request.Clone()
	// native model 在编码前替换：出站请求体里必须是上游认识的名字。
	req.Model = target.NativeModel

	body, lossy, err := encodeWithLossy(outbound, req)
	if err != nil {
		return relayclient.OutcomeInvalidModel, attemptResult{
			err: retryableErr(asIRError(err, ir.ErrInternal)),
		}
	}
	// 换目标重试时覆盖而非累加：诊断描述的是最终发出去的那次编码，
	// 混入上一个目标的丢弃项会把排查引向一个没被采用的路径。
	rec.Lossy = lossy
	// 参数覆盖作用在编码之后的 wire body 上，因此运维配的是**出站协议的
	// 原生字段名**，出站协议特有的嵌套结构天然可表达。
	body, err = paramover.Apply(body, target.Defaults, target.Overrides)
	if err != nil {
		return relayclient.OutcomeInvalidModel, attemptResult{
			err: retryableErr(ir.NewError(ir.ErrInternal, 0, "",
				fmt.Sprintf("apply param layers: %v", err))),
		}
	}

	stream, irErr := p.open(ctx, outbound, target, body)
	if irErr != nil {
		return outcomeFor(irErr), attemptResult{err: irErr}
	}
	defer stream.Close()

	return p.bridge(ctx, w, call, stream, start, rec)
}

// fail 在未 committed 时把错误按客户端协议回出去。
func (p *Pipeline) fail(w http.ResponseWriter, call Call, rec *Record, err *ir.Error) {
	if err == nil {
		err = ir.NewError(ir.ErrInternal, 0, "", "request failed without a reason")
	}
	if rec.Outcome == "" {
		rec.Outcome = relayclient.OutcomeAbnormal
	}
	rec.ErrorCode = string(err.Kind)
	rec.ErrorMessage = err.Message

	status, body := renderErrorWithLossy(call.Inbound, err, rec)
	rec.StatusCode = status
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (p *Pipeline) report(call Call, target relayclient.Target, attempt int,
	outcome string, usage relayclient.Usage) {
	if p.Reporter == nil {
		return
	}
	p.Reporter.Report(relayclient.ResultReport{
		// attempt 入键：同一客户端请求的多次尝试各自上报，
		// 而重放同一份不会被重复计数。
		ReportID:  fmt.Sprintf("%s:%d", call.RequestID, attempt),
		RequestID: call.RequestID,
		ModelID:   target.ModelID,
		Outcome:   outcome,
		Usage:     usage,
	})
}

func (p *Pipeline) record(rec Record) {
	if p.Recorder == nil {
		return
	}
	// 合并推迟到此处而非各上报点：对外只有 lossy 一个字段，
	// 在这里合一次就不必让每个上报点都关心两侧的拼接。
	rec.Lossy = rec.mergedLossy()
	p.Recorder.Record(rec)
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// outcomeFor 把错误类别映射成调度层的 outcome。
func outcomeFor(err *ir.Error) string {
	switch err.Kind {
	case ir.ErrContextExceeded:
		return relayclient.OutcomeContextExceeded
	case ir.ErrNotFound:
		return relayclient.OutcomeInvalidModel
	case ir.ErrRateLimit, ir.ErrUpstream, ir.ErrTimeout:
		return relayclient.OutcomeRetrying
	default:
		return relayclient.OutcomeAbnormal
	}
}

// downgrade 把 retrying 降成 abnormal：不再换目标了，
// retrying 会让调度层以为还有后续尝试。
func downgrade(outcome string) string {
	if outcome == relayclient.OutcomeRetrying {
		return relayclient.OutcomeAbnormal
	}
	return outcome
}

// retryableErr 标记为可重试。编码期失败对这个目标是死路，但换一个可能行。
func retryableErr(err *ir.Error) *ir.Error {
	err.Retryable = true
	return err
}

func dispatchError(err error) *ir.Error {
	re, ok := err.(*relayclient.Error)
	if !ok {
		return ir.NewError(ir.ErrInternal, 0, "", err.Error())
	}
	kind := ir.ErrInternal
	switch re.Code {
	case relayclient.CodeUnauthorized:
		kind = ir.ErrAuth
	case relayclient.CodeNotFound, relayclient.CodeTargetUnavailable:
		kind = ir.ErrNotFound
	case relayclient.CodeInvalidRequest:
		kind = ir.ErrInvalidRequest
	}
	out := ir.NewError(kind, 0, re.Code, re.Message)
	// 候选耗尽后重试也没用，不管 relay 怎么标。
	out.Retryable = re.Retryable && !re.Exhausted()
	return out
}

// encodeWithLossy 编码请求，并在出站 codec 支持时顺带取回有损说明。
// 未实现该可选接口等价于「不丢任何字段」。
func encodeWithLossy(outbound codec.OutboundCodec, req *ir.Request) ([]byte, []string, error) {
	if le, ok := outbound.(codec.LossyEncoder); ok {
		return le.EncodeRequestLossy(req)
	}
	body, err := outbound.EncodeRequest(req)
	return body, nil, err
}

// encodeResponseWithLossy 与 encodeWithLossy 对称，处理非流式响应侧。
func encodeResponseWithLossy(inbound codec.InboundCodec, resp *ir.Response) ([]byte, []string, error) {
	if le, ok := inbound.(codec.LossyResponseEncoder); ok {
		return le.EncodeResponseLossy(resp)
	}
	body, err := inbound.EncodeResponse(resp)
	return body, nil, err
}

// renderErrorWithLossy 渲染错误体，并把入站协议表达不了的维度记进流水。
func renderErrorWithLossy(inbound codec.InboundCodec, err *ir.Error, rec *Record) (int, []byte) {
	if lr, ok := inbound.(codec.LossyErrorRenderer); ok {
		status, body, notes := lr.RenderErrorLossy(err)
		rec.addResponseLossy(notes...)
		return status, body
	}
	return inbound.RenderError(err)
}

func asIRError(err error, fallback ir.ErrorKind) *ir.Error {
	if e, ok := err.(*ir.Error); ok {
		return e
	}
	return ir.NewError(fallback, 0, "", err.Error())
}

func msSince(start, now time.Time) int {
	return int(now.Sub(start) / time.Millisecond)
}
