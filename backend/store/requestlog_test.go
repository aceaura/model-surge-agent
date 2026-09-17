package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

func fullRecord() pipeline.Record {
	return pipeline.Record{
		RequestID:        "req-1",
		At:               time.Now().UTC().Truncate(time.Millisecond),
		InboundProtocol:  "anthropic",
		UserModel:        "kimi-k3",
		OutboundProtocol: "chat_completions",
		ModelID:          "ark-1/ds",
		Account:          "ark-1",
		Outcome:          relayclient.OutcomeNormal,
		StatusCode:       200,
		Attempts:         2,
		TriedIDs:         []string{"kimi-1/k3"},
		Committed:        true,
		Stream:           true,
		UsageEstimated:   true,
		Usage:            relayclient.Usage{InputTokens: 120, OutputTokens: 64, CacheReadTokens: 80},
		LatencyMS:        1500,
		FirstTokenMS:     320,
		ErrorCode:        "",
		ErrorMessage:     "",
	}
}

func TestInsertAndGetRoundTrip(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()

	want := fullRecord()
	if err := l.Insert(ctx, want); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := l.Get(ctx, "req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// at 经过 PG 往返会换时区表示，比时刻而非结构。
	if !got.At.Equal(want.At) {
		t.Errorf("at = %v, want %v", got.At, want.At)
	}
	got.At, want.At = time.Time{}, time.Time{}
	if !sameRecord(got, want) {
		t.Fatalf("round trip drifted:\n got = %+v\nwant = %+v", got, want)
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	_, err := l.Get(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// 同 request_id 重复写入取最后一次：重放说明调用方复用了 ID，
// 保留最新状态比保留最初的更有诊断价值。
func TestInsertOverwritesSameRequestID(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()

	rec := fullRecord()
	if err := l.Insert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.Outcome = relayclient.OutcomeAbnormal
	rec.ErrorCode = "upstream"
	rec.Attempts = 3
	if err := l.Insert(ctx, rec); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	got, err := l.Get(ctx, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != relayclient.OutcomeAbnormal || got.Attempts != 3 || got.ErrorCode != "upstream" {
		t.Fatalf("record = %+v, want the later state", got)
	}
}

// tried_ids 是排查换目标链路的关键，空切片不能变成 NULL。
func TestEmptyTriedIDsRoundTripsAsEmptyList(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()

	rec := fullRecord()
	rec.TriedIDs = nil
	if err := l.Insert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := l.Get(ctx, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TriedIDs) != 0 {
		t.Fatalf("tried_ids = %v, want empty", got.TriedIDs)
	}
}

func seed(t *testing.T, l *RequestLog, n int) time.Time {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)
	for i := range n {
		rec := fullRecord()
		rec.RequestID = "req-" + strconv.Itoa(i)
		// 时间递增，序号越大越新。
		rec.At = base.Add(time.Duration(i) * time.Second)
		// 偶数条换成另一组标签，让每个筛选维度各命中一半。
		if i%2 == 0 {
			rec.Outcome = relayclient.OutcomeAbnormal
			rec.ModelID = "kimi-1/k3"
			rec.UserModel = "kimi-k3"
		} else {
			rec.UserModel = "ds-v4"
		}
		if err := l.Insert(ctx, rec); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	return base
}

// 键集分页而非 OFFSET：流水在持续写入，OFFSET 会漏条或重条。
func TestListPagesByCursorWithoutGapsOrDuplicates(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()
	seed(t, l, 10)

	seen := map[string]bool{}
	var cursor Cursor
	for page := range 5 {
		got, next, err := l.List(ctx, Filter{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, rec := range got {
			if seen[rec.RequestID] {
				t.Fatalf("page %d returned %s twice", page, rec.RequestID)
			}
			seen[rec.RequestID] = true
		}
		if next.Zero() {
			break
		}
		cursor = next
	}
	if len(seen) != 10 {
		t.Fatalf("paged through %d records, want 10", len(seen))
	}
}

func TestListOrdersNewestFirst(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	seed(t, l, 5)

	got, _, err := l.List(context.Background(), Filter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d records", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].At.After(got[i-1].At) {
			t.Fatalf("record %d is newer than %d, order must be descending", i, i-1)
		}
	}
}

func TestListFilters(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()
	base := seed(t, l, 10)

	cases := []struct {
		name   string
		filter Filter
		want   int
	}{
		{"by outcome", Filter{Outcome: relayclient.OutcomeAbnormal, Limit: 100}, 5},
		{"by model_id", Filter{ModelID: "kimi-1/k3", Limit: 100}, 5},
		{"by user_model", Filter{UserModel: "kimi-k3", Limit: 100}, 5},
		{"since", Filter{Since: base.Add(7 * time.Second), Limit: 100}, 3},
		{"combined", Filter{Outcome: relayclient.OutcomeAbnormal,
			Since: base.Add(4 * time.Second), Limit: 100}, 3},
		{"no match", Filter{Outcome: "nonexistent", Limit: 100}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := l.List(ctx, c.filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != c.want {
				t.Fatalf("got %d records, want %d", len(got), c.want)
			}
		})
	}
}

func TestListClampsLimit(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()
	seed(t, l, 5)

	for _, limit := range []int{0, -1, 10000} {
		got, _, err := l.List(ctx, Filter{Limit: limit})
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(got) != 5 {
			t.Errorf("limit %d returned %d records", limit, len(got))
		}
	}
}

func TestPruneDropsOldRows(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()
	base := seed(t, l, 10)

	n, err := l.Prune(ctx, base.Add(5*time.Second))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 5 {
		t.Errorf("pruned %d rows, want 5", n)
	}
	got, _, err := l.List(ctx, Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("%d records survived, want 5", len(got))
	}
}

// 凭据绝不入库。Record 类型本身没有 headers 字段，
// 这条测试守的是「将来有人加了字段」这种回归。
func TestRecordCarriesNoCredentialShapedFields(t *testing.T) {
	raw, err := json.Marshal(fullRecord())
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"header", "Header", "api_key", "authorization", "Authorization"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("Record must not carry %q: %s", banned, raw)
		}
	}
}

// sameRecord 比较两条记录。tried_ids 单独用 slices.Equal 比，
// 因为 nil 与空切片在这里语义相同（PG 存的是空数组）。
func sameRecord(a, b pipeline.Record) bool {
	// 两个 JSONB 列往返后是空切片而非 nil，DeepEqual 会把它与未设值判为不同。
	if !slices.Equal(a.TriedIDs, b.TriedIDs) || !slices.Equal(a.Sanitized, b.Sanitized) {
		return false
	}
	a.TriedIDs, b.TriedIDs = nil, nil
	a.Sanitized, b.Sanitized = nil, nil
	return reflect.DeepEqual(a, b)
}
