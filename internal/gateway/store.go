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
	ID         string `json:"id"`
	Session    string `json:"session"`
	Chat       string `json:"chat"`
	Sender     string `json:"sender,omitempty"`
	Direction  string `json:"direction"` // "in" or "out"
	FromMe     bool   `json:"fromMe"`
	IsGroup    bool   `json:"isGroup"`
	Type       string `json:"type"`
	Body       string `json:"body,omitempty"`
	Mimetype   string `json:"mimetype,omitempty"`
	Filename   string `json:"filename,omitempty"`
	FileLength int64  `json:"fileLength,omitempty"`
	MediaKey   string `json:"-"`                  // internal storage object key/path
	MediaURL   string `json:"mediaUrl,omitempty"` // derived by the API layer
	Timestamp  int64  `json:"timestamp"`
	Status     string `json:"status,omitempty"`   // outgoing: sent|delivered|read|played
	StatusAt   int64  `json:"statusAt,omitempty"` // unix seconds of the last status change
}

// MessageQuery describes filters for listing stored messages.
type MessageQuery struct {
	Session string
	Chat    string
	Limit   int
	Before  int64  // only messages with timestamp < Before (unix seconds); 0 = no bound
	After   int64  // only messages with timestamp >= After (unix seconds); 0 = no bound
	Order   string // "asc" for oldest-first (catch-up); default newest-first
}

// messageStore persists messages to the shared database when enabled.
type messageStore struct {
	db      *pgDB
	enabled bool
	media   MediaStore
	log     waLog.Logger

	quit chan struct{}
	once sync.Once
}

func newMessageStore(db *pgDB, cfg *config.Config, media MediaStore) *messageStore {
	return &messageStore{
		db:      db,
		enabled: cfg.StoreMessages,
		media:   media,
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
			session     TEXT NOT NULL,
			id          TEXT NOT NULL,
			chat        TEXT NOT NULL,
			sender      TEXT,
			direction   TEXT NOT NULL,
			from_me     INTEGER NOT NULL DEFAULT 0,
			is_group    INTEGER NOT NULL DEFAULT 0,
			type        TEXT,
			body        TEXT,
			mimetype    TEXT NOT NULL DEFAULT '',
			filename    TEXT NOT NULL DEFAULT '',
			file_length BIGINT NOT NULL DEFAULT 0,
			media_path  TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT '',
			status_ts   BIGINT NOT NULL DEFAULT 0,
			timestamp   BIGINT NOT NULL,
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
	// Migrasi DB lama: tambah kolom media bila belum ada.
	for _, col := range []string{
		`ALTER TABLE gw_messages ADD COLUMN IF NOT EXISTS mimetype TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE gw_messages ADD COLUMN IF NOT EXISTS filename TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE gw_messages ADD COLUMN IF NOT EXISTS file_length BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE gw_messages ADD COLUMN IF NOT EXISTS media_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE gw_messages ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE gw_messages ADD COLUMN IF NOT EXISTS status_ts BIGINT NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.ExecContext(ctx, col); err != nil {
			s.log.Debugf("alter gw_messages: %v", err)
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
		`INSERT INTO gw_messages
			(session, id, chat, sender, direction, from_me, is_group, type, body,
			 mimetype, filename, file_length, media_path, timestamp, status, status_ts)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (session, id) DO NOTHING`,
		rec.Session, rec.ID, rec.Chat, rec.Sender, rec.Direction,
		boolToInt(rec.FromMe), boolToInt(rec.IsGroup), rec.Type, rec.Body,
		rec.Mimetype, rec.Filename, rec.FileLength, rec.MediaKey, rec.Timestamp,
		rec.Status, rec.StatusAt,
	)
	if err != nil {
		s.log.Errorf("save message %s: %v", rec.ID, err)
	}
}

// updateMedia sets the media columns for an already-saved message. Used by the
// asynchronous incoming-media download path (metadata is saved first, then the
// file is downloaded and this fills in the storage key).
func (s *messageStore) updateMedia(session, id, mediaKey, mimetype, filename string, size int64) {
	if !s.enabled {
		return
	}
	_, err := s.db.Exec(
		`UPDATE gw_messages SET media_path=?, mimetype=?, filename=?, file_length=?
			WHERE session=? AND id=?`,
		mediaKey, mimetype, filename, size, session, id)
	if err != nil {
		s.log.Errorf("update media %s: %v", id, err)
	}
}

// statusRank ranks delivery statuses so updateStatus only ever moves forward
// (sent → delivered → read → played). Unknown status returns -1.
func statusRank(status string) int {
	switch status {
	case "sent":
		return 0
	case "delivered":
		return 1
	case "read":
		return 2
	case "played":
		return 3
	default:
		return -1
	}
}

// updateStatus advances the delivery status of outgoing messages identified by
// ids within a session. It only upgrades (never downgrades) so out-of-order
// receipts are safe. No-op when storage is disabled or ids is empty.
func (s *messageStore) updateStatus(session string, ids []string, status string, ts int64) {
	if !s.enabled || len(ids) == 0 {
		return
	}
	rank := statusRank(status)
	if rank < 0 {
		return
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+4)
	args = append(args, status, ts, session)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, rank)
	q := `UPDATE gw_messages SET status=?, status_ts=?
		WHERE session=? AND from_me=1 AND id IN (` + strings.Join(placeholders, ",") + `)
		AND (CASE status WHEN 'played' THEN 3 WHEN 'read' THEN 2 WHEN 'delivered' THEN 1 WHEN 'sent' THEN 0 ELSE -1 END) < ?`
	if _, err := s.db.Exec(q, args...); err != nil {
		s.log.Errorf("update status %v: %v", ids, err)
	}
}

// messageByID fetches a single stored message (with media columns) by id. When
// session is empty the first match across sessions is returned.
func (s *messageStore) messageByID(ctx context.Context, session, id string) (StoredMessage, bool, error) {
	if !s.enabled {
		return StoredMessage{}, false, fmt.Errorf("message storage is disabled (set STORE_MESSAGES=true)")
	}
	query := `SELECT session, id, chat, sender, direction, from_me, is_group, type, body, mimetype, filename, file_length, media_path, timestamp, status, status_ts FROM gw_messages WHERE id = ?`
	args := []any{id}
	if session != "" {
		query += ` AND session = ?`
		args = append(args, session)
	}
	query += ` LIMIT 1`

	var (
		m             StoredMessage
		fromMe, isGrp int
	)
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&m.Session, &m.ID, &m.Chat,
		&m.Sender, &m.Direction, &fromMe, &isGrp, &m.Type, &m.Body, &m.Mimetype,
		&m.Filename, &m.FileLength, &m.MediaKey, &m.Timestamp, &m.Status, &m.StatusAt)
	if err == sql.ErrNoRows {
		return StoredMessage{}, false, nil
	}
	if err != nil {
		return StoredMessage{}, false, err
	}
	m.FromMe = fromMe != 0
	m.IsGroup = isGrp != 0
	return m, true, nil
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
	if q.After > 0 {
		where = append(where, "timestamp >= ?")
		args = append(args, q.After)
	}

	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	sb := strings.Builder{}
	sb.WriteString(`SELECT session, id, chat, sender, direction, from_me, is_group, type, body, mimetype, filename, file_length, media_path, timestamp, status, status_ts FROM gw_messages`)
	if len(where) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(where, " AND "))
	}
	if strings.EqualFold(q.Order, "asc") {
		sb.WriteString(" ORDER BY timestamp ASC")
	} else {
		sb.WriteString(" ORDER BY timestamp DESC")
	}
	sb.WriteString(" LIMIT ?")
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
			&fromMe, &isGrp, &m.Type, &m.Body, &m.Mimetype, &m.Filename,
			&m.FileLength, &m.MediaKey, &m.Timestamp, &m.Status, &m.StatusAt); err != nil {
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
	s.purgeMediaFiles(`SELECT media_path FROM gw_messages WHERE timestamp < ? AND media_path <> ''`, cutoff)
	res, err := s.db.Exec(`DELETE FROM gw_messages WHERE timestamp < ?`, cutoff)
	if err != nil {
		s.log.Errorf("purge old messages: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.log.Infof("purged %d messages older than %d days", n, retentionDays)
	}
}

// purgeMediaFiles deletes stored media objects for the rows matched by query.
func (s *messageStore) purgeMediaFiles(query string, args ...any) {
	if s.media == nil {
		return
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		s.log.Errorf("select media for purge: %v", err)
		return
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err == nil && key != "" {
			keys = append(keys, key)
		}
	}
	rows.Close()
	for _, k := range keys {
		if err := s.media.Delete(context.Background(), k); err != nil {
			s.log.Debugf("delete media %s: %v", k, err)
		}
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
	s.purgeMediaFiles(`SELECT media_path FROM gw_messages WHERE session = ? AND media_path <> ''`, name)
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
