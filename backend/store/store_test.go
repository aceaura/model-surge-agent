package store

import (
	"context"
	"os"
	"testing"
)

// dsn 返回测试库 DSN；未配置则跳过（与同族两个仓库同惯例）。
func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("TEST_PG_DSN")
	if v == "" {
		t.Skip("TEST_PG_DSN not set")
	}
	return v
}

// open 打开测试库并清空本包的两张表。
func open(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, dsn(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Pool().Exec(ctx, "TRUNCATE request_log, report_outbox"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	first, err := Open(ctx, dsn(t))
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	first.Close()

	second, err := Open(ctx, dsn(t))
	if err != nil {
		t.Fatalf("second open must not fail on existing tables: %v", err)
	}
	defer second.Close()

	if err := second.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestSchemaCreatesAllTables(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	for _, table := range []string{"request_log", "report_outbox"} {
		var exists bool
		err := s.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = current_schema() AND table_name = $1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s missing", table)
		}
	}
}
