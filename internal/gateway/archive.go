package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Per-chat archive opt-in and per-participant receipts.
//
// STORE_CHATS is static configuration; an integrator that links WhatsApp groups
// at runtime (e.g. one group per class) needs to switch history on for a chat
// without a restart. chatArchive keeps that set in gw_chat_archive and in
// memory. A chat that is opted in is stored — messages AND media — even when
// STORE_MESSAGES / STORE_MEDIA are off globally.
//
// gw_message_receipts keeps WhatsApp receipts per participant, so a group
// broadcast can be reported as "read by 24 of 30" instead of the single
// aggregate status on gw_messages (which flips to "read" at the first reader).

// chatArchive is the runtime per-chat opt-in set.
type chatArchive struct {
	db  *pgDB
	log waLog.Logger

	mu  sync.RWMutex
	set map[string]bool // session + "|" + chat
}

func newChatArchive(db *pgDB, level string) *chatArchive {
	return &chatArchive{db: db, log: waLog.Stdout("Archive", level, true), set: map[string]bool{}}
}

func (a *chatArchive) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS gw_chat_archive (
			session    TEXT NOT NULL,
			chat       TEXT NOT NULL,
			updated_at BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (session, chat)
		)`,
		`CREATE TABLE IF NOT EXISTS gw_message_receipts (
			session     TEXT NOT NULL,
			message_id  TEXT NOT NULL,
			chat        TEXT NOT NULL,
			participant TEXT NOT NULL,
			type        TEXT NOT NULL,
			ts          BIGINT NOT NULL,
			PRIMARY KEY (session, message_id, participant)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gw_message_receipts_chat ON gw_message_receipts(session, chat, ts)`,
	}
	for _, q := range stmts {
		if _, err := a.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create archive schema: %w", err)
		}
	}
	return a.load(ctx)
}

func (a *chatArchive) load(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT session, chat FROM gw_chat_archive`)
	if err != nil {
		return fmt.Errorf("load chat archive: %w", err)
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var s, c string
		if err := rows.Scan(&s, &c); err != nil {
			return err
		}
		set[s+"|"+c] = true
	}
	a.mu.Lock()
	a.set = set
	a.mu.Unlock()
	return rows.Err()
}

// enabled reports whether the chat is opted in for the session.
func (a *chatArchive) enabled(session, chat string) bool {
	if a == nil {
		return false
	}
	if session == "" {
		session = "default"
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.set[session+"|"+chat]
}

// any reports whether at least one chat is opted in (storage must be active).
func (a *chatArchive) any() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.set) > 0
}

// setEnabled persists and applies the opt-in for one chat.
func (a *chatArchive) setEnabled(ctx context.Context, session, chat string, on bool, now int64) error {
	if session == "" {
		session = "default"
	}
	chat = strings.TrimSpace(chat)
	if chat == "" {
		return fmt.Errorf("chat is required")
	}
	var err error
	if on {
		_, err = a.db.ExecContext(ctx, `INSERT INTO gw_chat_archive (session, chat, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (session, chat) DO UPDATE SET updated_at = EXCLUDED.updated_at`, session, chat, now)
	} else {
		_, err = a.db.ExecContext(ctx, `DELETE FROM gw_chat_archive WHERE session = ? AND chat = ?`, session, chat)
	}
	if err != nil {
		return fmt.Errorf("update chat archive: %w", err)
	}
	a.mu.Lock()
	if on {
		a.set[session+"|"+chat] = true
	} else {
		delete(a.set, session+"|"+chat)
	}
	a.mu.Unlock()
	return nil
}

// Receipt is one participant's receipt for an outgoing message.
type Receipt struct {
	Participant string `json:"participant"` // phone digits, or full JID when unresolvable
	Type        string `json:"type"`        // delivered|read|played
	Timestamp   int64  `json:"timestamp"`
}

// receiptParticipant picks the phone-number identity of the receipt sender:
// SenderAlt when Sender is a privacy alias (@lid), else Sender; lidResolve
// (optional) maps aliases the event did not resolve.
func receiptParticipant(src types.MessageSource, lidResolve func(string) string) string {
	j := src.Sender
	if j.Server == types.HiddenUserServer && !src.SenderAlt.IsEmpty() {
		j = src.SenderAlt
	}
	if j.Server == types.HiddenUserServer && lidResolve != nil {
		if pn := lidResolve(j.String()); pn != "" {
			if parsed, err := types.ParseJID(pn); err == nil {
				j = parsed
			}
		}
	}
	if j.Server == types.DefaultUserServer {
		return j.User
	}
	return j.ToNonAD().String()
}

// saveReceipts upserts one receipt per message id for the participant,
// upgrading only (delivered → read → played).
func (s *messageStore) saveReceipts(session, chat, participant string, v *events.Receipt, status string) {
	if participant == "" || len(v.MessageIDs) == 0 {
		return
	}
	rank := statusRank(status)
	if rank <= 0 { // "sent" is not a receipt
		return
	}
	ts := v.Timestamp.Unix()
	for _, id := range v.MessageIDs {
		_, err := s.db.Exec(`INSERT INTO gw_message_receipts (session, message_id, chat, participant, type, ts)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (session, message_id, participant) DO UPDATE SET type = EXCLUDED.type, ts = EXCLUDED.ts
			WHERE (CASE gw_message_receipts.type WHEN 'played' THEN 3 WHEN 'read' THEN 2 WHEN 'delivered' THEN 1 ELSE 0 END) < ?`,
			session, string(id), chat, participant, status, ts, rank)
		if err != nil {
			s.log.Errorf("save receipt %s/%s: %v", id, participant, err)
		}
	}
}

// receiptsFor returns receipts grouped by message id.
func (s *messageStore) receiptsFor(ctx context.Context, session string, ids []string) (map[string][]Receipt, error) {
	out := make(map[string][]Receipt, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, session)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, participant, type, ts FROM gw_message_receipts
		WHERE session = ? AND message_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY ts`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r Receipt
		if err := rows.Scan(&id, &r.Participant, &r.Type, &r.Timestamp); err != nil {
			return nil, err
		}
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}
