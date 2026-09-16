package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

func report(id string) relayclient.ResultReport {
	return relayclient.ResultReport{
		ReportID:  id,
		RequestID: "req-1",
		ModelID:   "kimi-1/k3",
		Outcome:   relayclient.OutcomeNormal,
		Usage:     relayclient.Usage{InputTokens: 120, OutputTokens: 64, CacheReadTokens: 80},
	}
}

func newOutbox(t *testing.T) *Outbox {
	t.Helper()
	return NewOutbox(open(t).Pool())
}

func TestEnqueueAndDueRoundTrip(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()

	want := report("req-1:0")
	if err := o.Enqueue(ctx, want, "relay unreachable"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	due, err := o.Due(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due entries", len(due))
	}
	if due[0].Report != want {
		t.Errorf("report = %+v, want %+v", due[0].Report, want)
	}
	if due[0].Attempts != 0 || due[0].LastError != "relay unreachable" {
		t.Errorf("entry = %+v", due[0])
	}
}

// report_id 是幂等键：同键已在队列里，重复入队没有意义。
func TestEnqueueIsIdempotentOnReportID(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()

	for range 3 {
		if err := o.Enqueue(ctx, report("req-1:0"), "boom"); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	due, err := o.Due(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d entries, the same report_id must not stack up", len(due))
	}
}

// 未到期的项不该被取出，否则退避就形同虚设。
func TestDueRespectsSchedule(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()

	if err := o.Enqueue(ctx, report("req-1:0"), ""); err != nil {
		t.Fatal(err)
	}
	due, err := o.Due(ctx, time.Now(), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due = %d entries, %v", len(due), err)
	}

	future := time.Now().Add(time.Hour)
	if err := o.Retry(ctx, due[0].ID, future, "still down"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	again, err := o.Due(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a rescheduled entry must not be due yet: %+v", again)
	}
	later, err := o.Due(ctx, future.Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || later[0].Attempts != 1 || later[0].LastError != "still down" {
		t.Fatalf("entry = %+v", later)
	}
}

func TestDueOrdersByScheduleAndClampsBatch(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()
	for i := range 5 {
		if err := o.Enqueue(ctx, report("req-1:"+strconv.Itoa(i)), ""); err != nil {
			t.Fatal(err)
		}
	}
	due, err := o.Due(ctx, time.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 3 {
		t.Fatalf("got %d entries, want the batch size", len(due))
	}
	for i := 1; i < len(due); i++ {
		if due[i].NextAttemptAt.Before(due[i-1].NextAttemptAt) {
			t.Fatalf("entries must come out in schedule order")
		}
	}
}

func TestDoneRemovesEntry(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()

	if err := o.Enqueue(ctx, report("req-1:0"), ""); err != nil {
		t.Fatal(err)
	}
	due, _ := o.Due(ctx, time.Now(), 10)
	if err := o.Done(ctx, due[0].ID); err != nil {
		t.Fatalf("Done: %v", err)
	}
	left, err := o.Due(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d entries left after Done", len(left))
	}
}

// 死信保留行而不删：运维需要看到哪些用量没上报成，并能手动重试。
func TestBuryKeepsTheRowOutOfRotation(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()

	if err := o.Enqueue(ctx, report("req-1:0"), ""); err != nil {
		t.Fatal(err)
	}
	due, _ := o.Due(ctx, time.Now(), 10)
	if err := o.Bury(ctx, due[0].ID, "gave up"); err != nil {
		t.Fatalf("Bury: %v", err)
	}

	// worker 再也取不到它。
	rotation, err := o.Due(ctx, time.Now().Add(365*24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rotation) != 0 {
		t.Fatalf("a buried entry must stay out of rotation: %+v", rotation)
	}
	// 但管理面看得到。
	dead, err := o.List(ctx, StateDead, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || dead[0].LastError != "gave up" {
		t.Fatalf("dead list = %+v", dead)
	}
	if pending, err := o.List(ctx, StatePending, 10); err != nil || len(pending) != 0 {
		t.Fatalf("pending list = %+v, %v", pending, err)
	}
}

func TestReviveReturnsEntryToRotation(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()

	if err := o.Enqueue(ctx, report("req-1:0"), ""); err != nil {
		t.Fatal(err)
	}
	due, _ := o.Due(ctx, time.Now(), 10)
	if err := o.Bury(ctx, due[0].ID, "gave up"); err != nil {
		t.Fatal(err)
	}
	if err := o.Revive(ctx, "req-1:0"); err != nil {
		t.Fatalf("Revive: %v", err)
	}

	back, err := o.Due(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].Attempts != 0 {
		t.Fatalf("revived entry = %+v, attempts must reset", back)
	}
}

func TestReviveMissingReturnsErrNotFound(t *testing.T) {
	o := newOutbox(t)
	if err := o.Revive(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCountsSplitPendingFromDead(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()
	for i := range 5 {
		if err := o.Enqueue(ctx, report("req-1:"+strconv.Itoa(i)), ""); err != nil {
			t.Fatal(err)
		}
	}
	due, _ := o.Due(ctx, time.Now(), 10)
	for _, e := range due[:2] {
		if err := o.Bury(ctx, e.ID, "gave up"); err != nil {
			t.Fatal(err)
		}
	}

	pending, dead, err := o.Counts(ctx)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if pending != 3 || dead != 2 {
		t.Fatalf("pending = %d, dead = %d, want 3 and 2", pending, dead)
	}
}

func TestListAllIncludesBothStates(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()
	for i := range 3 {
		if err := o.Enqueue(ctx, report("req-1:"+strconv.Itoa(i)), ""); err != nil {
			t.Fatal(err)
		}
	}
	due, _ := o.Due(ctx, time.Now(), 10)
	if err := o.Bury(ctx, due[0].ID, "gave up"); err != nil {
		t.Fatal(err)
	}

	all, err := o.List(ctx, StateAll, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d entries, want all 3", len(all))
	}
}

// last_error 可能很长（上游 HTML 页面），必须截断而不是撑爆行。
func TestLastErrorIsTruncated(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()

	if err := o.Enqueue(ctx, report("req-1:0"), ""); err != nil {
		t.Fatal(err)
	}
	due, _ := o.Due(ctx, time.Now(), 10)
	long := ""
	for range 100 {
		long += "0123456789"
	}
	if err := o.Retry(ctx, due[0].ID, time.Now(), long); err != nil {
		t.Fatal(err)
	}
	got, err := o.Due(ctx, time.Now().Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].LastError) > 512 {
		t.Fatalf("last_error = %d bytes, want it capped", len(got[0].LastError))
	}
}
