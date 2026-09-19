package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 含非法 UTF-8 的错误消息必须写得进去。
//
// PG 对非法序列直接拒收整行（实测 SQLSTATE 22021），而客户端那侧
// json.Marshal 把坏字节替成 U+FFFD 且不报错——所以症状是「客户端看到一条
// 正常的错误信封，而这次故障的流水恰好缺失」，缺的正是最需要的那条。
//
// 坏字节的来源不是我们切出来的：pipeline 明确保留了「解压失败退回原始
// 字节」这条路，那条路上的内容从不经过任何截断点。
func TestInsertAcceptsInvalidUTF8ErrorMessage(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()

	rec := fullRecord()
	rec.RequestID = "req-bad-utf8"
	rec.ErrorMessage = "upstream returned 502: 网关错误\xe9详情"
	rec.ErrorCode = "bad\xffcode"

	if err := l.Insert(ctx, rec); err != nil {
		t.Fatalf("含非法字节的流水写不进去，这次故障的记录就丢了: %v", err)
	}

	got, err := l.Get(ctx, rec.RequestID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 净化只剔坏字节，坏字节以外的内容必须还在——线索恰恰在那里。
	if !strings.Contains(got.ErrorMessage, "网关错误") {
		t.Errorf("error_message = %q，丢了坏字节之前的内容", got.ErrorMessage)
	}
	if !strings.Contains(got.ErrorMessage, "详情") {
		t.Errorf("error_message = %q，丢了坏字节之后的内容", got.ErrorMessage)
	}
	if !strings.Contains(got.ErrorCode, "code") {
		t.Errorf("error_code = %q，净化把它整段吞了", got.ErrorCode)
	}
}

// 合法的错误消息一个字节都不该被动。
//
// 净化在写库路径上，对绝大多数请求必须是恒等的，否则这次改动会悄悄改写
// 全部历史流水的内容。
func TestInsertLeavesValidErrorMessageIntact(t *testing.T) {
	l := NewRequestLog(open(t).Pool())
	ctx := context.Background()

	const msg = "upstream returned 429: 配额不足，请稍后再试"
	rec := fullRecord()
	rec.RequestID = "req-good-utf8"
	rec.ErrorMessage = msg
	rec.ErrorCode = "rate_limit"

	if err := l.Insert(ctx, rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := l.Get(ctx, rec.RequestID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ErrorMessage != msg {
		t.Errorf("error_message = %q, want %q", got.ErrorMessage, msg)
	}
	if got.ErrorCode != "rate_limit" {
		t.Errorf("error_code = %q, want rate_limit", got.ErrorCode)
	}
}

// outbox 的 last_error 同样落 TEXT 列，同样要能写进去。
//
// 这一列存在的意义就是记下这次上报为什么失败；因为切坏了字符而写不进去，
// 等于让重试的人无从判断该不该继续重试。
func TestOutboxRetryAcceptsInvalidUTF8LastError(t *testing.T) {
	o := newOutbox(t)
	ctx := context.Background()

	if err := o.Enqueue(ctx, report("rep-bad-utf8"), ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	due, err := o.Due(ctx, time.Now(), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %v, err = %v", len(due), err)
	}

	// 坏字节必须落在上限**之内**：落在后面的话它压根到不了净化那一步，
	// 这条用例就退化成只测截断。
	bad := "relay returned 502: 坏\xe9字节 " + strings.Repeat("网", 400)
	if err := o.Retry(ctx, due[0].ID, time.Now().Add(time.Minute), bad); err != nil {
		t.Fatalf("含非法字节的 last_error 写不进去: %v", err)
	}
	if err := o.Bury(ctx, due[0].ID, bad); err != nil {
		t.Fatalf("Bury 也要能写: %v", err)
	}

	got, err := lastError(ctx, o, due[0].ID)
	if err != nil {
		t.Fatalf("读 last_error: %v", err)
	}
	if !strings.Contains(got, "字节") {
		t.Errorf("last_error = %q，净化把坏字节之后的内容也吞了", got)
	}
	// 上限必须仍然生效：这一列是真实存储，不能因为净化就不截了。
	if len(got) > 512 {
		t.Errorf("last_error 长 %d 字节，超过 512 的上限", len(got))
	}
}

func lastError(ctx context.Context, o *Outbox, id int64) (string, error) {
	var s string
	err := o.pool.QueryRow(ctx,
		"SELECT last_error FROM report_outbox WHERE id = $1", id).Scan(&s)
	return s, err
}
