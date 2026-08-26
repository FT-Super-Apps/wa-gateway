package gateway

import (
	"context"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"

	"wa-gateway/internal/config"
)

func newTestMessageStore(t *testing.T) *messageStore {
	t.Helper()
	db := testDB(t)

	s := newMessageStore(db, &config.Config{StoreMessages: true}, nil)
	if err := s.ensureSchema(context.Background()); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	if _, err := db.Exec(`TRUNCATE gw_messages`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func saveOutgoing(s *messageStore, session, id, chat string) {
	s.save(StoredMessage{
		ID: id, Session: session, Chat: chat, Direction: "out",
		FromMe: true, Type: "text", Body: "hi", Timestamp: 1000,
		Status: "sent", StatusAt: 1000,
	})
}

func TestStatusByIDs(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()

	saveOutgoing(s, "default", "m1", "628001@s.whatsapp.net")
	saveOutgoing(s, "default", "m2", "628002@s.whatsapp.net")
	saveOutgoing(s, "default", "m3", "628003@s.whatsapp.net")

	s.updateStatus("default", []string{"m1"}, "read", 2000)
	s.updateStatus("default", []string{"m2"}, "delivered", 1500)

	// "gone" was never stored (e.g. purged by retention): it must come back
	// explicitly not-found, not silently dropped, or the caller would misread
	// it as "still waiting for a receipt".
	got, err := s.statusByIDs(ctx, "default", []string{"m1", "gone", "m2", "m3"})
	if err != nil {
		t.Fatalf("statusByIDs: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d results, want 4 (request order preserved)", len(got))
	}
	want := []struct {
		id     string
		status string
		found  bool
	}{
		{"m1", "read", true},
		{"gone", "", false},
		{"m2", "delivered", true},
		{"m3", "sent", true},
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Status != w.status || got[i].Found != w.found {
			t.Errorf("result %d = %+v, want id=%q status=%q found=%v",
				i, got[i], w.id, w.status, w.found)
		}
	}
	if got[0].StatusAt != 2000 {
		t.Errorf("statusAt = %d, want 2000", got[0].StatusAt)
	}
}

func TestStatusByIDsScopedToSession(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()

	saveOutgoing(s, "default", "m1", "628001@s.whatsapp.net")
	saveOutgoing(s, "otp", "m2", "628002@s.whatsapp.net")

	got, err := s.statusByIDs(ctx, "otp", []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("statusByIDs: %v", err)
	}
	if got[0].Found {
		t.Error("m1 belongs to another session and must not be returned")
	}
	if !got[1].Found || got[1].Session != "otp" {
		t.Errorf("m2 = %+v, want found in session otp", got[1])
	}
}

func TestStatusByIDsIgnoresIncoming(t *testing.T) {
	// Incoming messages carry no delivery status of ours; returning them would
	// let a caller mistake a received message for a delivered one.
	s := newTestMessageStore(t)
	s.save(StoredMessage{
		ID: "in1", Session: "default", Chat: "628001@s.whatsapp.net",
		Direction: "in", FromMe: false, Type: "text", Timestamp: 1000,
	})

	got, err := s.statusByIDs(context.Background(), "default", []string{"in1"})
	if err != nil {
		t.Fatalf("statusByIDs: %v", err)
	}
	if got[0].Found {
		t.Errorf("incoming message returned: %+v", got[0])
	}
}

func TestStatusByIDsDisabled(t *testing.T) {
	s := &messageStore{enabled: false, log: waLog.Noop}
	if _, err := s.statusByIDs(context.Background(), "", []string{"m1"}); err == nil {
		t.Error("want an error when message storage is disabled")
	}
}

func TestStatusByIDsEmpty(t *testing.T) {
	s := &messageStore{enabled: true, log: waLog.Noop}
	got, err := s.statusByIDs(context.Background(), "", nil)
	if err != nil || got != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", got, err)
	}
}
