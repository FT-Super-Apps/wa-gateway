package gateway

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// testDB opens a Postgres connection from TEST_DATABASE_URL, skipping the test
// when it is not configured or unreachable. Tests run against the public schema
// and truncate their own tables for isolation.
func testDB(t *testing.T) *pgDB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres-backed test")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := raw.Ping(); err != nil {
		_ = raw.Close()
		t.Skipf("cannot reach TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &pgDB{raw}
}
