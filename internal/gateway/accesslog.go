package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"

	"wa-gateway/internal/config"
)

// AccessLogEntry adalah satu record akses API.
type AccessLogEntry struct {
	ID         int64  `json:"id"`
	KeyID      string `json:"keyId"`
	KeyName    string `json:"keyName"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	StatusCode int    `json:"statusCode"`
	LatencyMs  int64  `json:"latencyMs"`
	IP         string `json:"ip"`
	CreatedAt  int64  `json:"createdAt"`
}

// AccessLogQuery adalah parameter query akses log.
type AccessLogQuery struct {
	KeyID  string
	Since  int64 // unix timestamp; 0 = semua
	Limit  int   // 0 = default 100
	Offset int
}

// accessLogStore mengelola penyimpanan dan query akses log di PostgreSQL.
type accessLogStore struct {
	db            *pgDB
	log           waLog.Logger
	retentionDays int
	enabled       bool

	mu   sync.Mutex
	buf  []AccessLogEntry // buffer pending flush
	quit chan struct{}
	once sync.Once
}

func newAccessLogStore(db *pgDB, cfg *config.Config) *accessLogStore {
	return &accessLogStore{
		db:            db,
		log:           waLog.Stdout("AccessLog", cfg.LogLevel, true),
		retentionDays: cfg.AccessLogRetentionDays,
		enabled:       cfg.AccessLogRetentionDays != 0,
		quit:          make(chan struct{}),
	}
}

func (s *accessLogStore) ensureSchema(ctx context.Context) error {
	if !s.enabled {
		return nil
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS gw_access_logs (
			id          BIGSERIAL PRIMARY KEY,
			key_id      TEXT NOT NULL DEFAULT '',
			key_name    TEXT NOT NULL DEFAULT '',
			method      TEXT NOT NULL DEFAULT '',
			path        TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL DEFAULT 0,
			latency_ms  BIGINT NOT NULL DEFAULT 0,
			ip          TEXT NOT NULL DEFAULT '',
			created_at  BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gw_access_logs_key ON gw_access_logs(key_id)`,
		`CREATE INDEX IF NOT EXISTS idx_gw_access_logs_ts  ON gw_access_logs(created_at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create gw_access_logs schema: %w", err)
		}
	}
	s.purge(ctx)
	return nil
}

// Record enqueue satu access log entry (non-blocking, di-flush berkala).
func (s *accessLogStore) Record(e AccessLogEntry) {
	if !s.enabled {
		return
	}
	e.CreatedAt = time.Now().Unix()
	s.mu.Lock()
	s.buf = append(s.buf, e)
	s.mu.Unlock()
}

func (s *accessLogStore) flush() {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	entries := s.buf
	s.buf = nil
	s.mu.Unlock()

	for _, e := range entries {
		_, err := s.db.Exec(
			`INSERT INTO gw_access_logs
				(key_id, key_name, method, path, status_code, latency_ms, ip, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			e.KeyID, e.KeyName, e.Method, e.Path, e.StatusCode, e.LatencyMs, e.IP, e.CreatedAt)
		if err != nil {
			s.log.Errorf("flush access log: %v", err)
		}
	}
}

func (s *accessLogStore) purge(ctx context.Context) {
	if s.retentionDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.retentionDays).Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gw_access_logs WHERE created_at < ?`, cutoff); err != nil {
		s.log.Errorf("purge access logs: %v", err)
	}
}

// Query mengembalikan access log sesuai filter, diurutkan terbaru dulu.
func (s *accessLogStore) Query(ctx context.Context, q AccessLogQuery) ([]AccessLogEntry, error) {
	if !s.enabled {
		return nil, nil
	}
	// Flush dulu agar data terkini ikut terquery.
	s.flush()

	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	args := []any{}
	where := "1=1"
	if q.KeyID != "" {
		where += " AND key_id = ?"
		args = append(args, q.KeyID)
	}
	if q.Since > 0 {
		where += " AND created_at >= ?"
		args = append(args, q.Since)
	}
	args = append(args, limit, q.Offset)

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key_id, key_name, method, path, status_code, latency_ms, ip, created_at
		 FROM gw_access_logs WHERE `+where+
			` ORDER BY created_at DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query access logs: %w", err)
	}
	defer rows.Close()

	var out []AccessLogEntry
	for rows.Next() {
		var e AccessLogEntry
		if err := rows.Scan(&e.ID, &e.KeyID, &e.KeyName, &e.Method, &e.Path,
			&e.StatusCode, &e.LatencyMs, &e.IP, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *accessLogStore) start() {
	if !s.enabled {
		return
	}
	go func() {
		flush := time.NewTicker(5 * time.Second)
		purge := time.NewTicker(24 * time.Hour)
		defer flush.Stop()
		defer purge.Stop()
		for {
			select {
			case <-s.quit:
				s.flush()
				return
			case <-flush.C:
				s.flush()
			case <-purge.C:
				s.purge(context.Background())
			}
		}
	}()
}

func (s *accessLogStore) stop() {
	s.once.Do(func() { close(s.quit) })
}
