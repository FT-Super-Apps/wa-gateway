package gateway

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// rebind converts `?` placeholders to PostgreSQL `$1, $2, ...` style. The
// project's SQL never contains a literal `?` inside a string literal, so a
// simple sequential replacement is safe and keeps all call sites dialect-free.
func rebind(query string) string {
	if !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// pgDB wraps *sql.DB and rewrites `?` placeholders to Postgres `$n` on every
// query, so the rest of the codebase can keep using `?` regardless of dialect.
type pgDB struct{ *sql.DB }

func (db *pgDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return db.DB.ExecContext(ctx, rebind(q), args...)
}

func (db *pgDB) Exec(q string, args ...any) (sql.Result, error) {
	return db.DB.Exec(rebind(q), args...)
}

func (db *pgDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return db.DB.QueryContext(ctx, rebind(q), args...)
}

func (db *pgDB) Query(q string, args ...any) (*sql.Rows, error) {
	return db.DB.Query(rebind(q), args...)
}

func (db *pgDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return db.DB.QueryRowContext(ctx, rebind(q), args...)
}

func (db *pgDB) QueryRow(q string, args ...any) *sql.Row {
	return db.DB.QueryRow(rebind(q), args...)
}

// BeginTx returns a placeholder-rewriting transaction wrapper.
func (db *pgDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*pgTx, error) {
	tx, err := db.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &pgTx{tx}, nil
}

// pgTx wraps *sql.Tx with the same `?`→`$n` rewriting.
type pgTx struct{ *sql.Tx }

func (tx *pgTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return tx.Tx.ExecContext(ctx, rebind(q), args...)
}

func (tx *pgTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return tx.Tx.QueryContext(ctx, rebind(q), args...)
}

func (tx *pgTx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return tx.Tx.QueryRowContext(ctx, rebind(q), args...)
}
