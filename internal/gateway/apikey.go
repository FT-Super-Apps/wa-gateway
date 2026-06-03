package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"

	"wa-gateway/internal/config"
)

// Error API key.
var (
	ErrKeyNotFound = errors.New("api key not found")
	ErrKeyInvalid  = errors.New("invalid api key")
	ErrKeyDisabled = errors.New("api key disabled")
	ErrKeyExpired  = errors.New("api key expired")
)

// Scope yang dikenal. "*" memberi akses semua.
const (
	ScopeAll      = "*"
	ScopeSend     = "send"     // kirim pesan, normalize, check
	ScopeRead     = "read"     // status, qr, groups, messages
	ScopeSessions = "sessions" // create/delete session, pair, logout
	ScopeAdmin    = "admin"    // kelola api key
)

// APIKey adalah representasi key yang aman untuk diserialisasi (tanpa hash).
type APIKey struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Prefix        string   `json:"prefix"`
	Scopes        []string `json:"scopes"`
	RateLimit     int      `json:"rateLimit"`     // request per window; 0 = unlimited
	RateWindowSec int      `json:"rateWindowSec"` // panjang window (detik)
	MaxSessions   int      `json:"maxSessions"`   // 0 = unlimited
	Enabled       bool     `json:"enabled"`
	ExpiresAt     int64    `json:"expiresAt,omitempty"`
	CreatedAt     int64    `json:"createdAt"`
	LastUsedAt    int64    `json:"lastUsedAt,omitempty"`
	RequestCount  int64    `json:"requestCount"`

	// Secret hanya diisi saat create/rotate, tidak pernah disimpan plaintext.
	Secret string `json:"secret,omitempty"`

	// Master menandai key master dari env API_KEY (tidak ada di DB).
	Master bool `json:"master,omitempty"`
}

// HasScope melaporkan apakah key memiliki scope tertentu (atau "*").
func (k *APIKey) HasScope(scope string) bool {
	if scope == "" {
		return true
	}
	for _, s := range k.Scopes {
		if s == ScopeAll || s == scope {
			return true
		}
	}
	return false
}

// IsExpired melaporkan apakah key sudah kedaluwarsa.
func (k *APIKey) IsExpired() bool {
	return k.ExpiresAt > 0 && time.Now().Unix() >= k.ExpiresAt
}

// KeyCreateOptions adalah parameter pembuatan key.
type KeyCreateOptions struct {
	Name          string
	Scopes        []string
	RateLimit     int
	RateWindowSec int
	MaxSessions   int
	ExpiresAt     int64
}

// KeyUpdateOptions berisi field opsional untuk update (nil = tidak diubah).
type KeyUpdateOptions struct {
	Name          *string
	Scopes        *[]string
	RateLimit     *int
	RateWindowSec *int
	MaxSessions   *int
	Enabled       *bool
	ExpiresAt     *int64
}

// keyState menyimpan key beserta state rate-limit dan delta penggunaan di memori.
type keyState struct {
	key APIKey

	winStart int64 // unix detik awal window saat ini
	winCount int

	usageDelta int64 // request belum di-flush ke DB
	lastUsed   int64
}

// apiKeyStore mengelola managed API key (CRUD, auth, rate limit) dengan cache
// in-memory untuk menghindari hit DB tiap request.
type apiKeyStore struct {
	db        *sql.DB
	log       waLog.Logger
	masterKey string

	defRateLimit  int
	defRateWindow int
	defMaxSession int

	mu    sync.Mutex
	cache map[string]*keyState // key: hash hex

	quit chan struct{}
	once sync.Once
}

func newAPIKeyStore(db *sql.DB, cfg *config.Config) *apiKeyStore {
	return &apiKeyStore{
		db:            db,
		log:           waLog.Stdout("APIKeys", cfg.LogLevel, true),
		masterKey:     cfg.APIKey,
		defRateLimit:  cfg.DefaultRateLimit,
		defRateWindow: cfg.DefaultRateWindowSec,
		defMaxSession: cfg.DefaultMaxSessions,
		cache:         make(map[string]*keyState),
		quit:          make(chan struct{}),
	}
}

func (s *apiKeyStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS gw_api_keys (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL DEFAULT '',
			key_hash        TEXT NOT NULL UNIQUE,
			prefix          TEXT NOT NULL DEFAULT '',
			scopes          TEXT NOT NULL DEFAULT '*',
			rate_limit      INTEGER NOT NULL DEFAULT 0,
			rate_window_sec INTEGER NOT NULL DEFAULT 60,
			max_sessions    INTEGER NOT NULL DEFAULT 0,
			enabled         INTEGER NOT NULL DEFAULT 1,
			expires_at      INTEGER NOT NULL DEFAULT 0,
			created_at      INTEGER NOT NULL DEFAULT 0,
			last_used_at    INTEGER NOT NULL DEFAULT 0,
			request_count   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gw_api_keys_hash ON gw_api_keys(key_hash)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create gw_api_keys schema: %w", err)
		}
	}
	return s.reload(ctx)
}

// reload memuat ulang seluruh key dari DB ke cache, mempertahankan state window.
func (s *apiKeyStore) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, key_hash, prefix, scopes,
		rate_limit, rate_window_sec, max_sessions, enabled, expires_at,
		created_at, last_used_at, request_count FROM gw_api_keys`)
	if err != nil {
		return fmt.Errorf("load api keys: %w", err)
	}
	defer rows.Close()

	next := make(map[string]*keyState)
	for rows.Next() {
		var (
			k       APIKey
			hash    string
			scopes  string
			enabled int
		)
		if err := rows.Scan(&k.ID, &k.Name, &hash, &k.Prefix, &scopes,
			&k.RateLimit, &k.RateWindowSec, &k.MaxSessions, &enabled, &k.ExpiresAt,
			&k.CreatedAt, &k.LastUsedAt, &k.RequestCount); err != nil {
			return err
		}
		k.Enabled = enabled != 0
		k.Scopes = splitScopes(scopes)

		s.mu.Lock()
		prev := s.cache[hash]
		s.mu.Unlock()

		st := &keyState{key: k}
		if prev != nil {
			st.winStart = prev.winStart
			st.winCount = prev.winCount
			st.usageDelta = prev.usageDelta
			st.lastUsed = prev.lastUsed
		}
		next[hash] = st
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.cache = next
	s.mu.Unlock()
	return nil
}

// hasKeys melaporkan apakah ada managed key terdaftar.
func (s *apiKeyStore) hasKeys() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cache) > 0
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func generateSecret() (secret, hash, prefix string, err error) {
	buf := make([]byte, 20)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	secret = "wag_" + hex.EncodeToString(buf)
	hash = hashSecret(secret)
	prefix = secret[:12] // "wag_" + 8 hex
	return secret, hash, prefix, nil
}

func splitScopes(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{ScopeAll}
	}
	return out
}

func joinScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ScopeAll
	}
	return strings.Join(scopes, ",")
}

// Create membuat key baru dan mengembalikan plaintext secret sekali saja.
func (s *apiKeyStore) Create(ctx context.Context, opts KeyCreateOptions) (*APIKey, error) {
	secret, hash, prefix, err := generateSecret()
	if err != nil {
		return nil, fmt.Errorf("generate secret: %w", err)
	}

	scopes := opts.Scopes
	if len(scopes) == 0 {
		scopes = []string{ScopeAll}
	}
	rateWindow := opts.RateWindowSec
	if rateWindow <= 0 {
		rateWindow = s.defRateWindow
	}
	rateLimit := opts.RateLimit
	if rateLimit == 0 {
		rateLimit = s.defRateLimit
	}
	maxSessions := opts.MaxSessions
	if maxSessions == 0 {
		maxSessions = s.defMaxSession
	}

	now := time.Now().Unix()
	id := "key_" + hex.EncodeToString(mustRand(8))

	_, err = s.db.ExecContext(ctx, `INSERT INTO gw_api_keys
		(id, name, key_hash, prefix, scopes, rate_limit, rate_window_sec,
		 max_sessions, enabled, expires_at, created_at, last_used_at, request_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 0, 0)`,
		id, opts.Name, hash, prefix, joinScopes(scopes),
		rateLimit, rateWindow, maxSessions, opts.ExpiresAt, now)
	if err != nil {
		return nil, fmt.Errorf("insert api key: %w", err)
	}

	if err := s.reload(ctx); err != nil {
		return nil, err
	}

	k := &APIKey{
		ID: id, Name: opts.Name, Prefix: prefix, Scopes: scopes,
		RateLimit: rateLimit, RateWindowSec: rateWindow, MaxSessions: maxSessions,
		Enabled: true, ExpiresAt: opts.ExpiresAt, CreatedAt: now,
		Secret: secret,
	}
	return k, nil
}

// Rotate mengganti secret key dan mengembalikan plaintext baru sekali saja.
func (s *apiKeyStore) Rotate(ctx context.Context, id string) (*APIKey, error) {
	secret, hash, prefix, err := generateSecret()
	if err != nil {
		return nil, fmt.Errorf("generate secret: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE gw_api_keys SET key_hash = ?, prefix = ? WHERE id = ?`,
		hash, prefix, id)
	if err != nil {
		return nil, fmt.Errorf("rotate api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrKeyNotFound
	}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	k, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	k.Secret = secret
	return k, nil
}

// Update mengubah atribut key. Field nil tidak diubah.
func (s *apiKeyStore) Update(ctx context.Context, id string, patch KeyUpdateOptions) (*APIKey, error) {
	var (
		sets []string
		args []any
	)
	if patch.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *patch.Name)
	}
	if patch.Scopes != nil {
		sets = append(sets, "scopes = ?")
		args = append(args, joinScopes(*patch.Scopes))
	}
	if patch.RateLimit != nil {
		sets = append(sets, "rate_limit = ?")
		args = append(args, *patch.RateLimit)
	}
	if patch.RateWindowSec != nil {
		sets = append(sets, "rate_window_sec = ?")
		args = append(args, *patch.RateWindowSec)
	}
	if patch.MaxSessions != nil {
		sets = append(sets, "max_sessions = ?")
		args = append(args, *patch.MaxSessions)
	}
	if patch.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*patch.Enabled))
	}
	if patch.ExpiresAt != nil {
		sets = append(sets, "expires_at = ?")
		args = append(args, *patch.ExpiresAt)
	}
	if len(sets) == 0 {
		return s.Get(ctx, id)
	}

	args = append(args, id)
	res, err := s.db.ExecContext(ctx,
		"UPDATE gw_api_keys SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return nil, fmt.Errorf("update api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrKeyNotFound
	}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Delete menghapus key.
func (s *apiKeyStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM gw_api_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return s.reload(ctx)
}

// Get mengembalikan satu key (tanpa secret) dengan penggunaan terkini dari cache.
func (s *apiKeyStore) Get(ctx context.Context, id string) (*APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.cache {
		if st.key.ID == id {
			k := st.key
			k.RequestCount += st.usageDelta
			if st.lastUsed > k.LastUsedAt {
				k.LastUsedAt = st.lastUsed
			}
			return &k, nil
		}
	}
	return nil, ErrKeyNotFound
}

// List mengembalikan semua key (tanpa secret), terurut waktu pembuatan.
func (s *apiKeyStore) List(ctx context.Context) ([]APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]APIKey, 0, len(s.cache))
	for _, st := range s.cache {
		k := st.key
		k.RequestCount += st.usageDelta
		if st.lastUsed > k.LastUsedAt {
			k.LastUsedAt = st.lastUsed
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// RateResult adalah hasil pemeriksaan rate limit.
type RateResult struct {
	Limit     int
	Remaining int
	ResetAt   int64
	Allowed   bool
}

// Authenticate memvalidasi secret mentah, mencatat penggunaan, dan menerapkan
// rate limit. Mengembalikan salinan key beserta hasil rate limit.
func (s *apiKeyStore) Authenticate(rawSecret string) (*APIKey, RateResult, error) {
	hash := hashSecret(rawSecret)

	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.cache[hash]
	if !ok {
		return nil, RateResult{}, ErrKeyInvalid
	}
	if !st.key.Enabled {
		return nil, RateResult{}, ErrKeyDisabled
	}
	if st.key.IsExpired() {
		return nil, RateResult{}, ErrKeyExpired
	}

	now := time.Now().Unix()
	rr := RateResult{Allowed: true, Limit: st.key.RateLimit}

	if st.key.RateLimit > 0 {
		win := int64(st.key.RateWindowSec)
		if win <= 0 {
			win = 60
		}
		if st.winStart == 0 || now-st.winStart >= win {
			st.winStart = now
			st.winCount = 0
		}
		rr.ResetAt = st.winStart + win
		if st.winCount >= st.key.RateLimit {
			rr.Allowed = false
			rr.Remaining = 0
			return nil, rr, nil
		}
		st.winCount++
		rr.Remaining = st.key.RateLimit - st.winCount
	}

	st.usageDelta++
	st.lastUsed = now

	k := st.key
	return &k, rr, nil
}

// MasterKey mengembalikan representasi key master dari env (akses penuh).
func (s *apiKeyStore) MasterKey() *APIKey {
	return &APIKey{
		ID:      "master",
		Name:    "master (env API_KEY)",
		Scopes:  []string{ScopeAll},
		Enabled: true,
		Master:  true,
	}
}

// ConstantTimeMatchMaster membandingkan secret dengan master key secara aman.
func (s *apiKeyStore) ConstantTimeMatchMaster(rawSecret string) bool {
	if s.masterKey == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(rawSecret), []byte(s.masterKey)) == 1
}

// startFlusher menulis akumulasi penggunaan ke DB secara berkala.
func (s *apiKeyStore) startFlusher() {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.quit:
				s.flush()
				return
			case <-ticker.C:
				s.flush()
			}
		}
	}()
}

func (s *apiKeyStore) flush() {
	type upd struct {
		id       string
		delta    int64
		lastUsed int64
	}
	var updates []upd

	s.mu.Lock()
	for _, st := range s.cache {
		if st.usageDelta > 0 {
			updates = append(updates, upd{st.key.ID, st.usageDelta, st.lastUsed})
			st.key.RequestCount += st.usageDelta
			if st.lastUsed > st.key.LastUsedAt {
				st.key.LastUsedAt = st.lastUsed
			}
			st.usageDelta = 0
		}
	}
	s.mu.Unlock()

	for _, u := range updates {
		_, err := s.db.Exec(
			`UPDATE gw_api_keys SET request_count = request_count + ?, last_used_at = ? WHERE id = ?`,
			u.delta, u.lastUsed, u.id)
		if err != nil {
			s.log.Errorf("flush usage for %s: %v", u.id, err)
		}
	}
}

func (s *apiKeyStore) stop() {
	s.once.Do(func() { close(s.quit) })
}

func mustRand(n int) []byte {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return buf
}
