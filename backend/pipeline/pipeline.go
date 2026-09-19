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

	"github.com/aceaura/model-surge-agent/backend/capture"
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
	RequestID       string
	At              time.Time
	InboundProtocol string
	// Path 是客户端打的请求路径，不含 query。
	//
	// 记它是因为同一入站协议有多条路径别名共用一个处理函数：不记就看不出
	// 客户端把 base_url 配成了哪一种，别名相关的接入问题无从定位。
	// 不记 query：数据面不读任何 query 参数，而一些客户端会把凭据塞进去，
	// 不记就不需要脱敏。
	Path             string
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
	// DispatchMS 是问调度层要目标的累计耗时，UpstreamMS 是发出上游请求到
	// 响应头到达的累计耗时。两段都只切跨进程边界：进程内的编码与 sanitize
	// 是纯 CPU，给它们各记一列只会让表变宽而没有一次排查会用到。
	//
	// 换目标重试时累加而非覆盖，与 Lossy 的覆盖语义刻意相反：Lossy 描述
	// 最终发出去的那次编码，而时延描述客户端等了多久——客户端确实等了
	// 全部尝试的时间。只记最后一次会让「三次重试各 20 秒」看起来像一次
	// 20 秒的请求，而那正是最需要被看见的那种慢。
	//
	// 不变式：DispatchMS + UpstreamMS <= LatencyMS。
	// 差额里含三样：本服务自身的编解码、上游的生成时间、以及终态上报的
	// 一次往返。不写成「= 本服务 + 上游生成」这个等式——重试路径上的上报
	// 已经异步化不占等待，终态那次仍在客户端等待之内。
	DispatchMS int
	UpstreamMS int
	// AttemptsTrail 是逐次尝试的轨迹，按尝试顺序排列。
	//
	// 行内一列而不是另建一张 per-attempt 表：流水已是每请求一行，建表就是
	// 每请求 N 行。参考实现 sub2api 建过那样一张表（033 迁移）又整表删掉
	// （136 迁移），理由原文是写入宽度、内存驻留与库体积。代价是这一列
	// 不便索引；收益是零新表、零新写入路径、随流水一起被保留期清理。
	AttemptsTrail []AttemptRecord
	// attemptDispatchMS/attemptUpstreamMS 是**本次**尝试各自的耗时，
	// 与累计的 DispatchMS/UpstreamMS 并存，每次尝试开头清零。
	//
	// 不用「累计值做差」算本次值：做差要求追加轨迹的地方知道上一次的累计
	// 是多少，那是一个隐式的顺序依赖，改动顺序就会静默算错。分开累加则
	// 每次尝试的清零点在代码里直接可见。
	attemptDispatchMS int
	attemptUpstreamMS int
	ErrorCode         string
	ErrorMessage      string
	// RetryAfter 是上游明示的该目标最早可重试时刻，零值表示上游没说。
	// 落库供事后回答「那次为什么换了目标」。
	RetryAfter time.Time
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

// AttemptRecord 是一次尝试的轨迹项。
//
// 只含标识与结果标量，绝不含凭据、BaseURL 或请求体。不记 BaseURL：它的排查
// 价值等于 model_id+account 的组合（同一账号恒定一个 base），而它是一条带
// 路径的 URL，一些部署会把 key 放在 query 里。
type AttemptRecord struct {
	// N 是尝试序号，从 1 起。
	//
	// 不带 omitempty：从 1 起意味着零值本身就是 bug 信号，让它消失会把
	// 那个 bug 一起藏掉。
	N int `json:"n"`
	// ModelID/Account/OutboundProtocol 在调度层没给出目标时为空——
	// 那次尝试确实没有目标，缺省比填空字符串更诚实。
	ModelID          string `json:"model_id,omitempty"`
	Account          string `json:"account,omitempty"`
	OutboundProtocol string `json:"outbound_protocol,omitempty"`
	Outcome          string `json:"outcome"`
	StatusCode       int    `json:"status_code,omitempty"`
	// DispatchMS/UpstreamMS 是**本次**尝试的耗时，不是累计值。
	//
	// 都不带 omitempty：0 是有意义的观测值（快到不足 1 毫秒），
	// 省掉它会让读的人分不清「很快」与「没记」。
	DispatchMS int `json:"dispatch_ms"`
	UpstreamMS int `json:"upstream_ms"`
	// ErrorCode/ErrorMessage 只在这次尝试失败时有值。
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// RetryAfter 是上游对这次尝试明示的最早可重试时刻。
	RetryAfter time.Time `json:"retry_after,omitzero"`
}

// addAttempt 追加一条轨迹项。
func (r *Record) addAttempt(item AttemptRecord) {
	r.AttemptsTrail = append(r.AttemptsTrail, item)
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
	// HeartbeatInterval 是流式响应静默期内向客户端发保活帧的间隔。
	// 零值取默认，负值表示显式关闭。
	HeartbeatInterval time.Duration
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
	// RequestID 是回显给客户端的那个 id，可能来自客户端自己的请求头。
	RequestID string
	// RecordKey 是本服务自己的记录键，空表示与 RequestID 相同。
	//
	// 两者分列是因为 RequestID 可能撞号：它会成为 request_log 的主键
	// （ON CONFLICT DO UPDATE，撞号即覆盖别人那一行）与上报幂等键
	// （ON CONFLICT DO NOTHING，撞号即整条上报被当重放丢弃）。而回显仍必须
	// 是客户端给的那个值，否则客户端对不上自己的日志。
	RecordKey string
	// Protocol 是入站协议名，转发给调度层供策略脚本参考。
	Protocol string
	// Path 是客户端打的请求路径，只进流水，不参与任何判定。
	Path    string
	Inbound codec.InboundCodec
	Request *ir.Request
	// UserModel 是客户端请求里的模型名（user model，不是上游 model_id）。
	UserModel string
	// ClientKey 原样转发给调度层比对，本服务不做客户端鉴权。
	ClientKey string
	// Stream 表示客户端要不要 SSE。对上游一律流式，与此无关。
	Stream bool
	// Declarations 是客户端的协议声明（版本与 beta 特性）。
	// 承载不了它的出站协议会把它记成有损，而不是静默丢弃。
	Declarations codec.Declarations
	// Capture 是这次请求的四体捕获会话，nil 表示捕获关闭。
	//
	// 挂在 Call 而不是 Record 上：Record 会进 PG 与 Redis，而四体是 MB 级
	// 的字节，把它们塞进一条流水等于让每次请求都往库里写一份请求全文。
	Capture *capture.Session
}

// RecordID 是本服务落库与上报该用的键。
//
// 回落到 RequestID 而不是要求调用方必填：绝大多数请求不撞号，两者相同，
// 要求必填只会让每个构造点多写一行重复赋值，而漏写的那处会静默换形态。
func (c Call) RecordID() string {
	if c.RecordKey != "" {
		return c.RecordKey
	}
	return c.RequestID
}

// Serve 处理一次客户端请求，自行把响应或错误写进 w。
func (p *Pipeline) Serve(ctx context.Context, w http.ResponseWriter, call Call) {
	start := p.now()
	// 先修畸形再估算：sanitize 会增删块，而 est_tokens 只算一次并在重试间复用。
	sanitized := ir.Sanitize(call.Request)
	// est_tokens 只在首次 dispatch 前算一次，重试复用：请求没变，重算没意义。
	est := int(ir.EstimateRequest(call.Request))

	rec := Record{
		// 落库用记录键而非回显 id：撞号时后者会覆盖别人那一行。
		RequestID:       call.RecordID(),
		At:              start,
		InboundProtocol: call.Protocol,
		Path:            call.Path,
		UserModel:       call.UserModel,
		Stream:          call.Stream,
		Sanitized:       sanitized,
	}
	defer func() {
		rec.LatencyMS = msSince(start, p.now())
		// 判 ErrorCode 而不是 Outcome：retrying 是中间态，而 committed 之后
		// 的失败仍带着正常的 usage，按 outcome 判会两头都错。ErrorCode
		// 恰好在且仅在走过错误路径时非空。
		call.Capture.Finish(rec.ErrorCode != "")
		p.record(rec)
	}()

	// 上报队列：重试路径把投递交给它，终态路径等它排空后同步投递。
	// 收尾时 close 并等排空，确保 Serve 返回时这次请求的全部上报已投出。
	rq := newReportQueue(p.Reporter, max(p.Opts.MaxAttempts, 1))
	defer rq.close()

	var tried []string
	var lastErr *ir.Error

	for attempt := range max(p.Opts.MaxAttempts, 1) {
		rec.Attempts = attempt + 1
		rec.TriedIDs = tried
		// 本次尝试的两段耗时从零起算。放在循环体开头而不是结尾：
		// 中途 return 的路径不会执行结尾的清零。
		rec.attemptDispatchMS = 0
		rec.attemptUpstreamMS = 0

		dispatchStart := p.now()
		disp, err := p.Dispatch.Dispatch(ctx, relayclient.DispatchRequest{
			Model:           call.UserModel,
			InboundProtocol: call.Protocol,
			ClientKey:       call.ClientKey,
			// 给调度层的也是记录键：它那边的记录要能与本服务的流水对上，
			// 用会撞号的回显 id 就对不上。
			RequestID: call.RecordID(),
			TriedIDs:  tried,
			EstTokens: est,
		})
		// 累加在判错之前：失败的那次要目标同样花了时间，而「调度层超时后
		// 才报错」正是要看见的形态，记到 err 分支之后就会漏掉它。
		dispatchMS := msSince(dispatchStart, p.now())
		rec.DispatchMS += dispatchMS
		rec.attemptDispatchMS = dispatchMS
		if err != nil {
			// 调度层没给出目标，没有 model_id 可上报，只能回错。
			lastErr = dispatchError(err)
			// 这次尝试仍要留痕，但目标三项留空：它不是「某个目标失败了」，
			// 把它伪装成一次目标失败会让「哪个账号总失败」的排查算进一个
			// 不存在的账号。
			rec.addAttempt(AttemptRecord{
				N:            attempt + 1,
				Outcome:      relayclient.OutcomeAbnormal,
				DispatchMS:   dispatchMS,
				ErrorCode:    string(lastErr.Kind),
				ErrorMessage: lastErr.Message,
			})
			p.fail(w, call, &rec, lastErr)
			return
		}

		target := disp.Target
		rec.ModelID = target.ModelID
		rec.Account = target.Account
		rec.OutboundProtocol = target.Protocol

		outcome, res := p.attempt(ctx, w, call, target, attempt, start, &rec)
		if res.err == nil {
			p.report(rq, call, target, attempt, relayclient.OutcomeNormal, res.usage, nil, &rec)
			rec.Outcome = relayclient.OutcomeNormal
			rec.Usage = res.usage
			return
		}
		lastErr = res.err
		// 在这里统一记：所有失败路径都汇到这一行，
		// 分散到各个分支填会有人忘。
		rec.RetryAfter = res.err.RetryAfter

		// committed 之后目标已锁定：响应已经开始写出，
		// 换目标会让客户端看到两段拼接的回答。
		if res.committed {
			p.report(rq, call, target, attempt, outcome, res.usage, res.err, &rec)
			rec.Outcome = outcome
			rec.Usage = res.usage
			return
		}
		// 换目标也不会好（参数错、上下文超限），直接回错。
		if !res.err.Retryable {
			p.report(rq, call, target, attempt, outcome, res.usage, res.err, &rec)
			rec.Outcome = outcome
			rec.Usage = res.usage
			p.fail(w, call, &rec, res.err)
			return
		}

		tried = append(tried, target.ModelID)
		last := attempt == max(p.Opts.MaxAttempts, 1)-1
		if last {
			// 不再重试，所以是 abnormal 而非 retrying：后者表示还会换目标。
			p.report(rq, call, target, attempt, downgrade(outcome), res.usage, res.err, &rec)
			rec.Outcome = downgrade(outcome)
			// 与其余三个终态分支一致地设 usage：重试耗尽的请求上游照样计了费，
			// 不设会让流水记 0 token 而调度层记非零，事后无法判断哪边错。
			rec.Usage = res.usage
			rec.TriedIDs = tried
			break
		}
		// 这条路径还要再试一次，上报走异步：report 的投递是一次阻塞 HTTP POST
		// （默认 10s 超时），同步做等于让客户端为每次重试白等一个上报往返。
		p.report(rq, call, target, attempt, outcome, res.usage, res.err, &rec)
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

	// 标在编码之前：这一次尝试从此刻起写入的上游字节都归到它名下，
	// 包括编码或建流阶段就失败时上游回的错误体。
	call.Capture.MarkAttempt(attempt + 1)

	req := call.Request.Clone()
	// native model 在编码前替换：出站请求体里必须是上游认识的名字。
	req.Model = target.NativeModel

	body, lossy, err := encodeWithLossy(outbound, req)
	if err != nil {
		return relayclient.OutcomeInvalidModel, attemptResult{
			err: retryableErr(asIRError(err, ir.ErrInternal)),
		}
	}
	// 声明的丢弃与请求体的丢弃并列：客户端声明的 beta 特性没送到上游，
	// 与它的某个字段没送到，对客户端是同一类损失。
	lossy = codec.MergeNotes(lossy, codec.DescribeDeclarationLoss(call.Declarations, outbound))
	// 换目标重试时覆盖而非累加：诊断描述的是最终发出去的那次编码，
	// 混入上一个目标的丢弃项会把排查引向一个没被采用的路径。
	rec.Lossy = lossy
	// 参数覆盖作用在编码之后的 wire body 上，因此运维配的是**出站协议的
	// 原生字段名**，出站协议特有的嵌套结构天然可表达。
	body, err = paramover.Apply(body, target.Defaults, target.Overrides)
	// 覆盖而非累加：换目标重试时要看的是最终发出去的那一份。
	// 捕获点在 paramover 之后，因为那之后的字节才是真正写进请求的。
	call.Capture.Set(capture.UpstreamRequest, body)
	if err != nil {
		return relayclient.OutcomeInvalidModel, attemptResult{
			err: retryableErr(ir.NewError(ir.ErrInternal, 0, "",
				fmt.Sprintf("apply param layers: %v", err))),
		}
	}

	stream, irErr := p.open(ctx, outbound, target, body, call.Declarations, rec, call.Capture)
	if irErr != nil {
		return outcomeFor(irErr), attemptResult{err: irErr}
	}
	defer stream.Close()
	// 建流阶段的说明并进 rec.Lossy 而不是响应侧那一列：后者是累加的，
	// 换目标重试时会留下一个没被采用的目标的说明。
	rec.Lossy = codec.MergeNotes(rec.Lossy, stream.notes)

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
	// 错误信封也是回给客户端的字节，而且正是 errors 档要留的那一类：
	// 漏掉它会让唯一被保留的那些捕获恰好缺第四体。
	call.Capture.Add(capture.ClientResponse, body)
}

// recordAttempt 追加本次尝试的轨迹，并给出该投递的上报（没装配上报实现时
// 第二个返回值为假）。err 为 nil 表示成功。
//
// 拆成「记轨迹」与「投递」两步：轨迹必须同步（它写 rec，而 Serve 的 defer
// 会读同一个 rec），投递则不一定。合成一个函数的话重试路径想异步就只能连
// 轨迹一起异步。
//
// 收 err 而不是单收一个 retryAfter 参数：到期时刻是错误的属性，
// 让六处调用点各自从 res.err 里取会有人忘。
func (p *Pipeline) recordAttempt(call Call, target relayclient.Target, attempt int,
	outcome string, usage relayclient.Usage, err *ir.Error,
	rec *Record) (relayclient.ResultReport, bool) {
	var retryAfter time.Time
	if err != nil {
		retryAfter = err.RetryAfter
	}
	// 轨迹与上报用同一个 outcome 变量，两处永不分歧：各算一次的话，
	// 调度层看到 retrying 而本地记 abnormal 这类分歧会让人不知该信哪个。
	//
	// 追加放在这里而不是五条终止路径各写一遍：那五条路径**每一条都恰好**
	// 调一次 report，是唯一的共同漏斗；分散写迟早漏一条，而漏掉的那条在
	// 读代码时看不出来。也因此这一步排在 Reporter 判空之前——没装配上报
	// 实现时轨迹照样要留。
	item := AttemptRecord{
		N:                attempt + 1,
		ModelID:          target.ModelID,
		Account:          target.Account,
		OutboundProtocol: target.Protocol,
		Outcome:          outcome,
		DispatchMS:       rec.attemptDispatchMS,
		UpstreamMS:       rec.attemptUpstreamMS,
		RetryAfter:       retryAfter,
	}
	if err != nil {
		item.ErrorCode = string(err.Kind)
		item.ErrorMessage = err.Message
		item.StatusCode = err.StatusCode
	} else {
		item.StatusCode = rec.StatusCode
	}
	rec.addAttempt(item)

	if p.Reporter == nil {
		return relayclient.ResultReport{}, false
	}
	return relayclient.ResultReport{
		// attempt 入键：同一客户端请求的多次尝试各自上报，
		// 而重放同一份不会被重复计数。
		ReportID: fmt.Sprintf("%s:%d", call.RecordID(), attempt),
		// 与 ReportID、流水主键、以及给调度层的 dispatch 用同一个键：
		// 四处只要有一处用会撞号的回显 id，事后就串不起同一次请求。
		RequestID: call.RecordID(),
		ModelID:   target.ModelID,
		Outcome:   outcome,
		Usage:     usage,
		// 零值时 omitzero 让它不出现在 JSON 里，调度层据此回落启发式。
		RetryAfter: retryAfter,
	}, true
}

// report 记轨迹并把投递交给队列。
//
// 全部六条路径都走队列，终态那几条也不例外：调度层按到达顺序解释这些上报
// （retrying 之后才是终态），一条绕过队列直投就会插到还没投出的那条前面。
// Serve 的 defer 等队列排空，所以 Serve 返回时这次请求的上报都已投出。
//
// 轨迹仍同步追加：它写 rec，而 Serve 的 defer 会读同一个 rec 落库，
// 异步写就是数据竞争。异步的只有投递，而投递拿的是一份值拷贝。
func (p *Pipeline) report(rq *reportQueue, call Call, target relayclient.Target,
	attempt int, outcome string, usage relayclient.Usage, err *ir.Error, rec *Record) {
	rep, ok := p.recordAttempt(call, target, attempt, outcome, usage, err, rec)
	if ok {
		rq.enqueue(rep)
	}
}

// reportQueue 按入队顺序串行投递一次请求的上报。
//
// 存在的理由是重试路径上的上报不能占客户端的等待时间：投递是一次阻塞
// HTTP POST（默认 10s 超时），三次尝试串着做客户端要白等约两个上报往返，
// 而这段时间落在 latency_ms 里却不在 dispatch_ms 或 upstream_ms 里。
//
// 串行而不是每条一个 goroutine：调度层按到达顺序解释这些上报（retrying
// 之后才是终态），并发投递会让顺序随机。
//
// 每次请求一个队列而不是进程级的一个：进程级的会让一次请求等上别的请求
// 的在途投递，而那与它毫无关系。
type reportQueue struct {
	reporter Reporter
	ch       chan relayclient.ResultReport
	done     chan struct{}
}

func newReportQueue(reporter Reporter, cap int) *reportQueue {
	if reporter == nil {
		// 没装配上报实现时不起 goroutine：enqueue 那边判 nil 直接丢。
		return &reportQueue{}
	}
	q := &reportQueue{
		reporter: reporter,
		// 容量给满尝试次数：enqueue 因此永不阻塞调用方，
		// 而「不阻塞客户端」正是这个队列存在的全部理由。
		ch:   make(chan relayclient.ResultReport, cap),
		done: make(chan struct{}),
	}
	go func() {
		defer close(q.done)
		for rep := range q.ch {
			q.reporter.Report(rep)
		}
	}()
	return q
}

func (q *reportQueue) enqueue(rep relayclient.ResultReport) {
	if q.ch == nil {
		return
	}
	q.ch <- rep
}

// close 关队列并等在途投递做完。
//
// 必须等：不等就在进程忙时留下一批连入队都还没走完的上报，而那批就是
// 凭空缺掉的用量。等的位置在响应写完之后，所以不占客户端等待时间。
func (q *reportQueue) close() {
	if q.ch == nil {
		return
	}
	close(q.ch)
	<-q.done
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
	case ir.ErrTransport:
		return relayclient.OutcomeTransport
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
