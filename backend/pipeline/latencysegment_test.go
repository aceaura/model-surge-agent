package pipeline_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 本文件守时延分段归因：dispatch_ms 与 upstream_ms 两段是否贴在正确的
// 跨进程边界上、重试时是否累加、失败路径是否仍被记下。
//
// 断言用注入的步进时钟而不是真实耗时：本机一次 mock dispatch 常常是零毫秒，
// 靠真耗时的断言在快机器上会退化成 0 == 0 的空断言——那时计时点整块删掉
// 都测不出来。

// step 是步进时钟每被读一次前进的量。取 1 秒而非 1 毫秒：
// msSince 向下取整到毫秒，用毫秒级步长会让断言贴着取整边界。
const step = time.Second

// stepClock 每次被读就前进一个 step。
//
// 这让每一对「前后各读一次」的计时恰好得到 step 毫秒，于是可以对两段做
// **精确**断言而不是范围断言。精确断言才能测出「计时点被挪到了别的地方」：
// 范围断言（>0）在计时点包住了更多代码时照样通过。
type stepClock struct {
	t time.Time
}

func newStepClock() *stepClock {
	return &stepClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *stepClock) now() time.Time {
	c.t = c.t.Add(step)
	return c.t
}

const stepMS = int(step / time.Millisecond)

func TestSegmentsMeasureOneCrossProcessCallEach(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Now = newStepClock().now

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	rec := f.col.record(t)
	if rec.DispatchMS != stepMS {
		t.Errorf("dispatch_ms = %d，要 %d：计时必须紧贴 Dispatch 调用两侧，"+
			"包进更多代码会把本服务自己的耗时算成调度层的",
			rec.DispatchMS, stepMS)
	}
	if rec.UpstreamMS != stepMS {
		t.Errorf("upstream_ms = %d，要 %d：计时必须紧贴 client.Do 两侧，"+
			"终点是响应头到达而不是首帧解码出来——首帧里含上游的思考时间，"+
			"那是生成成本不是连接成本",
			rec.UpstreamMS, stepMS)
	}
}

// 不变式：两段都是 latency_ms 所覆盖区间内的真子段。
//
// 这条是整个分段的意义所在：latency_ms 减去两段才能被解读为
// 「本服务自身 + 上游生成」，不成立的话那个减法就得不出任何结论。
func TestSegmentsFitInsideTotalLatency(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Now = newStepClock().now

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if rec.DispatchMS+rec.UpstreamMS > rec.LatencyMS {
		t.Fatalf("dispatch_ms(%d) + upstream_ms(%d) > latency_ms(%d)："+
			"两段必须落在总耗时之内，否则 latency-dispatch-upstream 会算出负数",
			rec.DispatchMS, rec.UpstreamMS, rec.LatencyMS)
	}
	if rec.LatencyMS <= rec.DispatchMS+rec.UpstreamMS {
		t.Errorf("latency_ms(%d) 没有超出两段之和(%d)：桥流那段应当也花了时间，"+
			"相等说明总耗时的计时点被谁挪到了两段之内",
			rec.LatencyMS, rec.DispatchMS+rec.UpstreamMS)
	}
}

// 换目标重试时两段累加而非覆盖。
//
// 覆盖的话，「三次重试各 20 秒」会显示成一次 20 秒的请求——
// 而那正是最需要被看见的那种慢。
func TestSegmentsAccumulateAcrossRetries(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	good := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-1/k3")},
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-2/k3")},
		relaymock.Step{Target: target(good, "kimi-3/k3")},
	)
	f.p.Now = newStepClock().now

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d，前两个目标连不上应当换到第三个: %s", w.Code, w.Body)
	}

	rec := f.col.record(t)
	if rec.Attempts != 3 {
		t.Fatalf("attempts = %d，要 3：装置没走到三次尝试，累加语义测不到", rec.Attempts)
	}
	if want := 3 * stepMS; rec.DispatchMS != want {
		t.Errorf("dispatch_ms = %d，要 %d（三次要目标之和）："+
			"只记最后一次会让多次重试的等待整段消失", rec.DispatchMS, want)
	}
	if want := 3 * stepMS; rec.UpstreamMS != want {
		t.Errorf("upstream_ms = %d，要 %d（三次发请求之和）："+
			"连不上的那两次也花了时间，漏掉它们就看不出慢在连接上",
			rec.UpstreamMS, want)
	}
}

// dispatch 阶段就失败时 dispatch_ms 仍要有值、upstream_ms 必须为零。
//
// 「调度层超时后才报错」正是要看见的形态：把累加写在 err 分支之后
// 就会把这种最慢的一类记成零。
func TestDispatchFailureStillRecordsDispatchSegment(t *testing.T) {
	// 不给任何 Step：mock 会答没有候选，Dispatch 直接失败。
	f := newFixture(t)
	f.p.Now = newStepClock().now

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))
	if w.Code < 400 {
		t.Fatalf("status = %d，没有候选时应当回错: %s", w.Code, w.Body)
	}

	rec := f.col.record(t)
	if rec.DispatchMS != stepMS {
		t.Errorf("dispatch_ms = %d，要 %d：失败的那次要目标同样花了时间，"+
			"累加写在判错之后就会把「调度层慢到超时」记成零",
			rec.DispatchMS, stepMS)
	}
	if rec.UpstreamMS != 0 {
		t.Errorf("upstream_ms = %d，要 0：根本没拿到目标，一个字节都没发给上游",
			rec.UpstreamMS)
	}
}

// 两段是 Record 上独立的两个字段，不是同一个数的两种叫法。
func TestSegmentsAreSeparateFields(t *testing.T) {
	rec := pipeline.Record{DispatchMS: 11, UpstreamMS: 22}
	if rec.DispatchMS == rec.UpstreamMS {
		t.Fatal("两段共用了同一个字段")
	}
	if rec.DispatchMS != 11 || rec.UpstreamMS != 22 {
		t.Fatalf("rec = %+v", rec)
	}
}

// ---- 计时点的位置（真实时钟） ----
//
// 步进时钟能验证「一对前后各读一次」的形状，但验证不了这对括号**落在哪里**：
// 把它整段挪到 client.Do 之后，形状不变，读到的仍是一个 step。
// 探针实测这一形态在步进时钟下完全测不出来（变异 M5 未被检出），
// 所以位置必须用真实延迟来钉。

// slowDispatcher 在转交前睡一段，把「调度层慢」这件事做实。
type slowDispatcher struct {
	inner pipeline.Dispatcher
	delay time.Duration
}

func (s slowDispatcher) Dispatch(ctx context.Context,
	req relayclient.DispatchRequest) (*relayclient.DispatchResponse, error) {
	time.Sleep(s.delay)
	return s.inner.Dispatch(ctx, req)
}

// 上游在发响应头之前压住一段：这段必须落进 upstream_ms。
//
// 计时起点挪到 Do 之后的话，Do 已经等完了响应头，这段延迟就整段丢失。
func TestUpstreamSegmentCoversWaitForResponseHeader(t *testing.T) {
	const delay = 300 * time.Millisecond
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		// 睡在 WriteHeader 之前：响应头都还没出去，正是 upstream_ms 要量的那段。
		time.Sleep(delay)
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	rec := f.col.record(t)
	floor := int(delay/time.Millisecond) - 50
	if rec.UpstreamMS < floor {
		t.Errorf("upstream_ms = %d，上游压了 %v 才发响应头，至少要 %dms——"+
			"计时起点挪到 client.Do 之后的话这段等待会整段丢失",
			rec.UpstreamMS, delay, floor)
	}
	// 上游只压了这一段，别的地方不该被算进来。
	if ceil := int(delay/time.Millisecond) + 2000; rec.UpstreamMS > ceil {
		t.Errorf("upstream_ms = %d，超出 %dms：计时括号包进了不属于上游的代码",
			rec.UpstreamMS, ceil)
	}
	// 调度层没被拖慢，它那段必须明显小于上游那段。
	if rec.DispatchMS >= rec.UpstreamMS {
		t.Errorf("dispatch_ms(%d) >= upstream_ms(%d)：慢的是上游，两段搞反了",
			rec.DispatchMS, rec.UpstreamMS)
	}
}

// 调度层慢时那段必须落进 dispatch_ms，且不该漏进 upstream_ms。
func TestDispatchSegmentCoversDispatcherDelay(t *testing.T) {
	const delay = 300 * time.Millisecond
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Dispatch = slowDispatcher{inner: f.p.Dispatch, delay: delay}

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	rec := f.col.record(t)
	floor := int(delay/time.Millisecond) - 50
	if rec.DispatchMS < floor {
		t.Errorf("dispatch_ms = %d，调度层压了 %v，至少要 %dms",
			rec.DispatchMS, delay, floor)
	}
	if rec.UpstreamMS >= rec.DispatchMS {
		t.Errorf("upstream_ms(%d) >= dispatch_ms(%d)：慢的是调度层，"+
			"上游那段不该把它算进来",
			rec.UpstreamMS, rec.DispatchMS)
	}
}
