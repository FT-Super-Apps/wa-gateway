package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "modernc.org/sqlite"

	"wa-gateway/internal/config"
)

// ErrSessionNotFound is returned when a named session does not exist.
var ErrSessionNotFound = errors.New("session not found")

// ErrSessionExists is returned when creating a session that already exists.
var ErrSessionExists = errors.New("session already exists")

// Manager owns the shared session store and manages multiple WhatsApp sessions.
type Manager struct {
	cfg       *config.Config
	db        *sql.DB
	container *sqlstore.Container
	log       waLog.Logger
	notifier  *webhookNotifier
	store     *messageStore
	bulk      *bulkRunner

	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewManager opens the on-disk store and prepares the manager.
func NewManager(cfg *config.Config) (*Manager, error) {
	if err := os.MkdirAll(cfg.StoreDir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}

	dbPath := filepath.Join(cfg.StoreDir, "store.db")
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", dbPath)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	dbLog := waLog.Stdout("Database", cfg.LogLevel, true)
	container := sqlstore.NewWithDB(db, "sqlite3", dbLog)
	if err := container.Upgrade(context.Background()); err != nil {
		return nil, fmt.Errorf("upgrade database: %w", err)
	}

	m := &Manager{
		cfg:       cfg,
		db:        db,
		container: container,
		log:       waLog.Stdout("Manager", cfg.LogLevel, true),
		notifier:  newWebhookNotifier(cfg),
		store:     newMessageStore(db, cfg),
		sessions:  make(map[string]*Session),
	}

	if err := m.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	if err := m.store.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	m.bulk = newBulkRunner(m)
	return m, nil
}

func (m *Manager) ensureSchema(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS gw_sessions (
		name TEXT PRIMARY KEY,
		jid  TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return fmt.Errorf("create gw_sessions table: %w", err)
	}
	return nil
}

// Start loads persisted sessions and connects them. If none exist, a "default"
// session is created so the gateway is immediately usable.
func (m *Manager) Start(ctx context.Context) error {
	m.notifier.start()
	m.store.startRetention(m.cfg.MessageRetentionDays)

	rows, err := m.db.QueryContext(ctx, `SELECT name, jid FROM gw_sessions`)
	if err != nil {
		return fmt.Errorf("load sessions: %w", err)
	}
	defer rows.Close()

	type record struct{ name, jid string }
	var records []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.name, &r.jid); err != nil {
			return err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(records) == 0 {
		if _, err := m.Create(ctx, "default"); err != nil {
			return err
		}
		return nil
	}

	devices, err := m.container.GetAllDevices(ctx)
	if err != nil {
		return fmt.Errorf("get devices: %w", err)
	}

	for _, r := range records {
		var dev = m.findDevice(devices, r.jid)
		if dev == nil {
			dev = m.container.NewDevice()
		}
		sess := newSession(m, r.name, whatsmeow.NewClient(dev, waLog.Stdout("Session/"+r.name, m.cfg.LogLevel, true)))
		m.mu.Lock()
		m.sessions[r.name] = sess
		m.mu.Unlock()
		if err := sess.start(ctx); err != nil {
			m.log.Errorf("failed to start session %s: %v", r.name, err)
		}
	}
	return nil
}

func (m *Manager) findDevice(devices []*store.Device, jid string) *store.Device {
	if jid == "" {
		return nil
	}
	for _, d := range devices {
		if d.ID != nil && d.ID.String() == jid {
			return d
		}
	}
	return nil
}

// Create registers a new session and begins the QR pairing flow.
func (m *Manager) Create(ctx context.Context, name string) (*Session, error) {
	if name == "" {
		return nil, errors.New("session name is required")
	}

	m.mu.Lock()
	if _, ok := m.sessions[name]; ok {
		m.mu.Unlock()
		return nil, ErrSessionExists
	}
	m.mu.Unlock()

	if _, err := m.db.ExecContext(ctx, `INSERT INTO gw_sessions (name, jid) VALUES (?, '')`, name); err != nil {
		return nil, fmt.Errorf("persist session: %w", err)
	}

	dev := m.container.NewDevice()
	sess := newSession(m, name, whatsmeow.NewClient(dev, waLog.Stdout("Session/"+name, m.cfg.LogLevel, true)))

	m.mu.Lock()
	m.sessions[name] = sess
	m.mu.Unlock()

	if err := sess.start(ctx); err != nil {
		return nil, err
	}
	return sess, nil
}

// Get returns a session by name.
func (m *Manager) Get(name string) (*Session, error) {
	if name == "" {
		name = "default"
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[name]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return sess, nil
}

// List returns the status of all sessions, sorted by name.
func (m *Manager) List() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Remove logs out (if logged in) and deletes a session entirely.
func (m *Manager) Remove(ctx context.Context, name string) error {
	m.mu.Lock()
	sess, ok := m.sessions[name]
	if !ok {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	delete(m.sessions, name)
	m.mu.Unlock()

	sess.stop()
	if sess.wa.Store != nil && sess.wa.Store.ID != nil {
		if err := sess.wa.Logout(ctx); err != nil {
			m.log.Warnf("logout during remove %s: %v", name, err)
			_ = m.container.DeleteDevice(ctx, sess.wa.Store)
		}
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM gw_sessions WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete session row: %w", err)
	}
	m.store.deleteSession(ctx, name)
	return nil
}

// Messages returns stored messages matching the query (storage must be enabled).
func (m *Manager) Messages(ctx context.Context, q MessageQuery) ([]StoredMessage, error) {
	return m.store.query(ctx, q)
}

// StorageEnabled reports whether message persistence is active.
func (m *Manager) StorageEnabled() bool {
	return m.store.enabled
}

// SubmitBulk validates and starts a bulk send job, returning its initial state.
func (m *Manager) SubmitBulk(req BulkRequest) (BulkJob, error) {
	return m.bulk.submit(req)
}

// BulkJob returns the current state of a bulk job by ID.
func (m *Manager) BulkJob(id string) (BulkJob, bool) {
	return m.bulk.job(id)
}

// BulkJobs returns all known bulk jobs, newest first.
func (m *Manager) BulkJobs() []BulkJob {
	return m.bulk.list()
}

// bindJID stores the JID once a session finishes pairing.
func (m *Manager) bindJID(name string, jid types.JID) {
	if _, err := m.db.Exec(`UPDATE gw_sessions SET jid = ? WHERE name = ?`, jid.String(), name); err != nil {
		m.log.Errorf("bind jid for %s: %v", name, err)
	}
}

// Stop disconnects all sessions and stops the webhook queue.
func (m *Manager) Stop() {
	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.RUnlock()

	for _, s := range sessions {
		s.stop()
	}
	m.notifier.stop()
	m.store.stop()
	if m.bulk != nil {
		m.bulk.stop()
	}
}
