package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"wa-gateway/internal/config"
)

func newTestKeyStore(t *testing.T) *apiKeyStore {
	t.Helper()
	db := testDB(t)

	cfg := &config.Config{LogLevel: "ERROR", DefaultRateWindowSec: 60}
	s := newAPIKeyStore(db, cfg)
	if err := s.ensureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := db.Exec(`TRUNCATE gw_api_keys`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return s
}

func TestGenerateSecretFormat(t *testing.T) {
	secret, hash, prefix, err := generateSecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(secret, "wag_") {
		t.Errorf("secret prefix = %q, want wag_", secret)
	}
	if len(secret) != 44 { // "wag_" (4) + 40 hex
		t.Errorf("secret len = %d, want 44", len(secret))
	}
	if prefix != secret[:12] {
		t.Errorf("prefix = %q, want %q", prefix, secret[:12])
	}
	if hash != hashSecret(secret) {
		t.Errorf("hash mismatch")
	}
}

func TestHasScope(t *testing.T) {
	cases := []struct {
		scopes []string
		want   string
		ok     bool
	}{
		{[]string{ScopeAll}, ScopeSend, true},
		{[]string{ScopeSend}, ScopeSend, true},
		{[]string{ScopeRead}, ScopeSend, false},
		{[]string{ScopeRead, ScopeSessions}, ScopeSessions, true},
		{[]string{ScopeSend}, "", true},
	}
	for _, c := range cases {
		k := &APIKey{Scopes: c.scopes}
		if got := k.HasScope(c.want); got != c.ok {
			t.Errorf("HasScope(%v,%q) = %v, want %v", c.scopes, c.want, got, c.ok)
		}
	}
}

func TestSplitJoinScopes(t *testing.T) {
	if got := splitScopes(""); len(got) != 1 || got[0] != ScopeAll {
		t.Errorf("splitScopes(empty) = %v, want [*]", got)
	}
	got := splitScopes(" send , read ,, sessions ")
	want := []string{ScopeSend, ScopeRead, ScopeSessions}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("splitScopes = %v, want %v", got, want)
	}
	if joinScopes(nil) != ScopeAll {
		t.Errorf("joinScopes(nil) = %q, want *", joinScopes(nil))
	}
}

func TestCreateAndAuthenticate(t *testing.T) {
	s := newTestKeyStore(t)
	ctx := context.Background()

	key, err := s.Create(ctx, KeyCreateOptions{Name: "test", Scopes: []string{ScopeSend}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.Secret == "" {
		t.Fatal("expected secret on create")
	}

	got, rr, err := s.Authenticate(key.Secret)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !rr.Allowed {
		t.Error("expected allowed")
	}
	if got.ID != key.ID {
		t.Errorf("id = %q, want %q", got.ID, key.ID)
	}

	if _, _, err := s.Authenticate("wag_wrongsecret"); err != ErrKeyInvalid {
		t.Errorf("wrong secret err = %v, want ErrKeyInvalid", err)
	}
}

func TestRateLimit(t *testing.T) {
	s := newTestKeyStore(t)
	ctx := context.Background()

	key, err := s.Create(ctx, KeyCreateOptions{
		Name: "limited", RateLimit: 2, RateWindowSec: 60,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 1; i <= 2; i++ {
		_, rr, err := s.Authenticate(key.Secret)
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		if !rr.Allowed {
			t.Fatalf("req %d should be allowed", i)
		}
		if rr.Limit != 2 {
			t.Errorf("limit = %d, want 2", rr.Limit)
		}
	}

	_, rr, err := s.Authenticate(key.Secret)
	if err != nil {
		t.Fatalf("req 3: %v", err)
	}
	if rr.Allowed {
		t.Error("req 3 should be denied")
	}
	if rr.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", rr.Remaining)
	}
}

func TestRateLimitWindowReset(t *testing.T) {
	s := newTestKeyStore(t)
	ctx := context.Background()

	key, _ := s.Create(ctx, KeyCreateOptions{RateLimit: 1, RateWindowSec: 1})

	if _, rr, _ := s.Authenticate(key.Secret); !rr.Allowed {
		t.Fatal("first should be allowed")
	}
	if _, rr, _ := s.Authenticate(key.Secret); rr.Allowed {
		t.Fatal("second should be denied")
	}

	// Mundurkan window start agar window dianggap kedaluwarsa.
	hash := hashSecret(key.Secret)
	s.mu.Lock()
	s.cache[hash].winStart = time.Now().Unix() - 5
	s.mu.Unlock()

	if _, rr, _ := s.Authenticate(key.Secret); !rr.Allowed {
		t.Error("after window reset should be allowed")
	}
}

func TestDisabledAndExpired(t *testing.T) {
	s := newTestKeyStore(t)
	ctx := context.Background()

	key, _ := s.Create(ctx, KeyCreateOptions{Name: "d"})
	disabled := false
	if _, err := s.Update(ctx, key.ID, KeyUpdateOptions{Enabled: &disabled}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, _, err := s.Authenticate(key.Secret); err != ErrKeyDisabled {
		t.Errorf("err = %v, want ErrKeyDisabled", err)
	}

	expKey, _ := s.Create(ctx, KeyCreateOptions{Name: "e", ExpiresAt: time.Now().Unix() - 10})
	if _, _, err := s.Authenticate(expKey.Secret); err != ErrKeyExpired {
		t.Errorf("err = %v, want ErrKeyExpired", err)
	}
}

func TestRotateInvalidatesOldSecret(t *testing.T) {
	s := newTestKeyStore(t)
	ctx := context.Background()

	key, _ := s.Create(ctx, KeyCreateOptions{Name: "r"})
	old := key.Secret

	rotated, err := s.Rotate(ctx, key.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Secret == old {
		t.Error("rotated secret should differ")
	}
	if _, _, err := s.Authenticate(old); err != ErrKeyInvalid {
		t.Errorf("old secret err = %v, want ErrKeyInvalid", err)
	}
	if _, _, err := s.Authenticate(rotated.Secret); err != nil {
		t.Errorf("new secret err = %v, want nil", err)
	}
}

func TestDeleteAndList(t *testing.T) {
	s := newTestKeyStore(t)
	ctx := context.Background()

	k1, _ := s.Create(ctx, KeyCreateOptions{Name: "a"})
	_, _ = s.Create(ctx, KeyCreateOptions{Name: "b"})

	list, _ := s.List(ctx)
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	for _, k := range list {
		if k.Secret != "" {
			t.Error("list should not expose secret")
		}
	}

	if err := s.Delete(ctx, k1.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, k1.ID); err != ErrKeyNotFound {
		t.Errorf("get deleted err = %v, want ErrKeyNotFound", err)
	}
	if err := s.Delete(ctx, "key_nonexistent"); err != ErrKeyNotFound {
		t.Errorf("delete missing err = %v, want ErrKeyNotFound", err)
	}
}

func TestMasterKeyMatch(t *testing.T) {
	cfg := &config.Config{LogLevel: "ERROR", APIKey: "super-secret-master"}
	s := &apiKeyStore{masterKey: cfg.APIKey, cache: map[string]*keyState{}}

	if !s.ConstantTimeMatchMaster("super-secret-master") {
		t.Error("master should match")
	}
	if s.ConstantTimeMatchMaster("wrong") {
		t.Error("wrong should not match master")
	}
	if m := s.MasterKey(); !m.Master || !m.HasScope(ScopeAdmin) {
		t.Error("master key should have admin scope and Master=true")
	}
}
