package store

import (
	"context"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
)

// 两段落库的往返。
//
// 两个值刻意不同：dispatch_ms 与 upstream_ms 同为 INT，列顺序与 Scan 顺序
// 错位后照样能扫成功，只是两个值互换。给相同的值就测不出错位——
// 而错位的症状是运维把「慢在调度层」读成「慢在上游」，方向全反。
func TestSegmentsRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	in := pipeline.Record{
		RequestID:       "seg-1",
		InboundProtocol: "anthropic",
		UserModel:       "kimi-k3",
		Outcome:         "normal",
		LatencyMS:       900,
		DispatchMS:      111,
		UpstreamMS:      222,
	}
	if err := log.Insert(ctx, in); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := log.Get(ctx, "seg-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DispatchMS != 111 {
		t.Errorf("dispatch_ms = %d，要 111（读到 222 说明与 upstream_ms 列序错位）",
			got.DispatchMS)
	}
	if got.UpstreamMS != 222 {
		t.Errorf("upstream_ms = %d，要 222（读到 111 说明与 dispatch_ms 列序错位）",
			got.UpstreamMS)
	}
	if got.LatencyMS != 900 {
		t.Errorf("latency_ms = %d，要 900：新加两列不该挤走既有的时延列",
			got.LatencyMS)
	}
}

// List 走的是另一条 SQL（recordColumns + 分页），两段同样要出来。
// Insert 与 Get 对了而 List 漏了是很可能的：那两处共用 recordColumns，
// 而 INSERT 的列清单是单独写的。
func TestSegmentsSurviveList(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	if err := log.Insert(ctx, pipeline.Record{
		RequestID: "seg-2", InboundProtocol: "anthropic", UserModel: "m",
		Outcome: "normal", DispatchMS: 333, UpstreamMS: 444,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	recs, _, err := log.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("list 出 %d 条，要 1 条", len(recs))
	}
	if recs[0].DispatchMS != 333 || recs[0].UpstreamMS != 444 {
		t.Errorf("list 里两段 = (%d, %d)，要 (333, 444)：recordColumns 或 scanRecord 漏了列",
			recs[0].DispatchMS, recs[0].UpstreamMS)
	}
}

// 同 request_id 覆盖时两段也要被更新。
//
// 漏在 DO UPDATE SET 里的症状最隐蔽：首次写入是对的，只有覆盖那次留着旧值，
// 而覆盖只在重放 request_id 时才发生。
func TestSegmentsUpdatedOnConflict(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	base := pipeline.Record{
		RequestID: "seg-3", InboundProtocol: "anthropic", UserModel: "m",
		Outcome: "normal", DispatchMS: 1, UpstreamMS: 2,
	}
	if err := log.Insert(ctx, base); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	base.DispatchMS, base.UpstreamMS = 555, 666
	if err := log.Insert(ctx, base); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	got, err := log.Get(ctx, "seg-3")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DispatchMS != 555 || got.UpstreamMS != 666 {
		t.Errorf("覆盖后两段 = (%d, %d)，要 (555, 666)：DO UPDATE SET 里漏了这两列",
			got.DispatchMS, got.UpstreamMS)
	}
}

// 老表补列后两段默认为零而不是 NULL：它们声明了 NOT NULL DEFAULT 0，
// 扫进 int 不需要指针。
func TestSegmentsDefaultToZero(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	if err := log.Insert(ctx, pipeline.Record{
		RequestID: "seg-4", InboundProtocol: "anthropic", UserModel: "m", Outcome: "normal",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := log.Get(ctx, "seg-4")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DispatchMS != 0 || got.UpstreamMS != 0 {
		t.Errorf("未填时两段 = (%d, %d)，要 (0, 0)", got.DispatchMS, got.UpstreamMS)
	}
}

// 老库升级路径：两列必须由 ALTER 补上，而不是只出现在 CREATE TABLE 里。
//
// CREATE TABLE IF NOT EXISTS 对已存在的表整段跳过，所以先于这两列建起的
// 库只靠 CREATE 拿不到它们。而漏掉 ALTER 的症状在本机测试里完全看不见——
// 测试库是新建的，CREATE 那一段就把列建全了。探针实测删掉
// upstream_ms 的 ALTER 一行，整套测试照样通过。
func TestSegmentColumnsExistInSchema(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	// 把列删掉，模拟一个先于这两列建起的老库。
	for _, col := range []string{"dispatch_ms", "upstream_ms"} {
		if _, err := s.Pool().Exec(ctx,
			"ALTER TABLE request_log DROP COLUMN IF EXISTS "+col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	// 重开一次：migrate 会整段重跑，CREATE 跳过、ALTER 必须把列补回来。
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, col := range []string{"dispatch_ms", "upstream_ms"} {
		var exists bool
		err := s.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			 WHERE table_name = 'request_log' AND column_name = $1)`, col).Scan(&exists)
		if err != nil {
			t.Fatalf("query column %s: %v", col, err)
		}
		if !exists {
			t.Errorf("列 %s 没被补回来：老库升级后这一列会一直缺，"+
				"而 INSERT 会整条失败——流水从此一条都记不下", col)
		}
	}
}
