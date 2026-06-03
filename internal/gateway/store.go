package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"

	"wa-gateway/internal/config"
)

// StoredMessage is a persisted incoming or outgoing message record.
type StoredMessage struct {
	ID        string `json:"id"`
	Session   string `json:"session"`
	Chat      string `json:"chat"`
	Sender    string `json:"sender,omitempty"`
	Direction string `json:"direction"` // "in" or "out"
	FromMe    bool   `json:"fromMe"`
	IsGroup   bool   `json:"isGroup"`
	Type      string `json:"type"`
	Body      string `json:"body,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// MessageQuery describes filters for listing stored messages.
type MessageQuery struct {
	Session string
	Chat    string
	Limit   int
	Before  int64 // only messages with timestamp < Before (unix seconds); 0 = no bound
}

// messageStore persists messages to the shared SQLite database when enabled.
type messageStore struct {
	db      *sql.DB
	enabled bool
	log     waLog.Logger

	quit chan struct{}
	once sync.Once
}

func newMessageStore(db *sql.DB, cfg *config.Config) *messageStore {
	return &messageStore{
		db:      db,
		enabled: cfg.StoreMessages,
		log:     waLog.Stdout("MessageStore", cfg.LogLevel, true),
		quit:    make(chan struct{}),
	}
}

// ensureSchema creates the gw_messages table and its indexes.
func (s *messageStore) ensureSchema(ctx context.Context) error {
	if !s.enabled {
		return nil
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS gw_messages (
			session   TEXT NOT NULL,
			id        TEXT NOT NULL,
			chat      TEXT NOT NULL,
			sender    TEXT,
			direction TEXT NOT NULL,
			from_me   INTEGER NOT NULL DEFAULT 0,
			is_group  INTEGER NOT NULL DEFAULT 0,
			type      TEXT,
			body      TEXT,
			timestamp INTEGER NOT NULL,
			PRIMARY KEY (session, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gw_messages_session_ts ON gw_messages(session, timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_gw_messages_chat_ts ON gw_messages(session, chat, timestamp)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create gw_messages schema: %w", err)
		}
	}
	return nil
}

// save persists a single message, ignoring duplicates by (session, id).
func (s *messageStore) save(rec StoredMessage) {
	if !s.enabled {
		return
	}
	if rec.Timestamp == 0 {
		rec.Timestamp = time.Now().Unix()
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO gw_messages
			(session, id, chat, sender, direction, from_me, is_group, type, body, timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Session, rec.ID, rec.Chat, rec.Sender, rec.Direction,
		boolToInt(rec.FromMe), boolToInt(rec.IsGroup), rec.Type, rec.Body, rec.Timestamp,
	)
	if err != nil {
		s.log.Errorf("save message %s: %v", rec.ID, err)
	}
}

// query returns stored messages matching the filter, newest first.
func (s *messageStore) query(ctx context.Context, q MessageQuery) ([]StoredMessage, error) {
	if !s.enabled {
		return nil, fmt.Errorf("message storage is disabled (set STORE_MESSAGES=true)")
	}

	var (
		where []string
		args  []any
	)
	if q.Session != "" {
		where = append(where, "session = ?")
		args = append(args, q.Session)
	}
	if q.Chat != "" {
		where = append(where, "chat = ?")
		args = append(args, q.Chat)
	}
	if q.Before > 0 {
		where = append(where, "timestamp < ?")
		args = append(args, q.Before)
	}

	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	sb := strings.Builder{}
	sb.WriteString(`SELECT session, id, chat, sender, direction, from_me, is_group, type, body, timestamp FROM gw_messages`)
	if len(where) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(where, " AND "))
	}
	sb.WriteString(" ORDER BY timestamp DESC LIMIT ?")
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]StoredMessage, 0, limit)
	for rows.Next() {
		var (
			m             StoredMessage
			fromMe, isGrp int
		)
		if err := rows.Scan(&m.Session, &m.ID, &m.Chat, &m.Sender, &m.Direction,
			&fromMe, &isGrp, &m.Type, &m.Body, &m.Timestamp); err != nil {
			return nil, err
		}
		m.FromMe = fromMe != 0
		m.IsGroup = isGrp != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// startRetention purges old messages on startup and daily, when retentionDays > 0.
func (s *messageStore) startRetention(retentionDays int) {
	if !s.enabled || retentionDays <= 0 {
		return
	}
	s.purge(retentionDays)
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-s.quit:
				return
			case <-ticker.C:
				s.purge(retentionDays)
			}
		}
	}()
}

func (s *messageStore) purge(retentionDays int) {
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	res, err := s.db.Exec(`DELETE FROM gw_messages WHERE timestamp < ?`, cutoff)
	if err != nil {
		s.log.Errorf("purge old messages: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.log.Infof("purged %d messages older than %d days", n, retentionDays)
	}
}

func (s *messageStore) stop() {
	s.once.Do(func() { close(s.quit) })
}

// deleteSession removes all stored messages for a session.
func (s *messageStore) deleteSession(ctx context.Context, name string) {
	if !s.enabled {
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gw_messages WHERE session = ?`, name); err != nil {
		s.log.Errorf("delete messages for session %s: %v", name, err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
