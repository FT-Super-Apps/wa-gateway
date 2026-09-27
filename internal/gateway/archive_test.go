package gateway

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"wa-gateway/internal/config"
)

func TestReceiptParticipant(t *testing.T) {
	pn := types.NewJID("628111", types.DefaultUserServer)
	lid := types.NewJID("99887766", types.HiddenUserServer)
	group := types.NewJID("1203630@g.us", types.GroupServer)

	cases := []struct {
		name string
		src  types.MessageSource
		res  func(string) string
		want string
	}{
		{"phone sender", types.MessageSource{Chat: group, Sender: pn}, nil, "628111"},
		{"lid with alt", types.MessageSource{Chat: group, Sender: lid, SenderAlt: pn}, nil, "628111"},
		{"lid resolved", types.MessageSource{Chat: group, Sender: lid}, func(s string) string { return pn.String() }, "628111"},
		{"lid unresolved", types.MessageSource{Chat: group, Sender: lid}, func(string) string { return "" }, lid.String()},
		{"ad device stripped", types.MessageSource{Chat: pn, Sender: types.NewADJID("628111", 0, 3)}, nil, "628111"},
	}
	for _, c := range cases {
		if got := receiptParticipant(c.src, c.res); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestChatFilterArchiveOptIn(t *testing.T) {
	arch := &chatArchive{set: map[string]bool{"default|g1@g.us": true}}

	// STORE_MESSAGES off: only opted-in chats pass.
	f := newChatFilter(&config.Config{StoreMessages: false}, arch)
	if !f.allowChat("default", "g1@g.us") {
		t.Error("opted-in chat must be stored even with STORE_MESSAGES=false")
	}
	if f.allowChat("default", "g2@g.us") || f.allowChat("other", "g1@g.us") {
		t.Error("non-opted chats must not be stored with STORE_MESSAGES=false")
	}

	// STORE_MESSAGES on with a denylist: opt-in overrides the denylist.
	f = newChatFilter(&config.Config{StoreMessages: true, StoreChatsExclude: []string{"g1@g.us"}}, arch)
	if !f.allowChat("default", "g1@g.us") {
		t.Error("opt-in must override denylist")
	}
	if !f.allowChat("default", "628111@s.whatsapp.net") {
		t.Error("unlisted chat must be stored with STORE_MESSAGES=true and no allowlist")
	}
}

func TestSaveReceiptsUpgradeOnly(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`TRUNCATE gw_message_receipts`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	saveOutgoing(s, "default", "m1", "g1@g.us")

	at := func(sec int64, typ types.ReceiptType) *events.Receipt {
		return &events.Receipt{MessageIDs: []types.MessageID{"m1"}, Timestamp: time.Unix(sec, 0), Type: typ}
	}
	s.saveReceipts("default", "g1@g.us", "628111", at(10, types.ReceiptTypeRead), "read")
	s.saveReceipts("default", "g1@g.us", "628111", at(11, types.ReceiptTypeDelivered), "delivered") // late, ignored
	s.saveReceipts("default", "g1@g.us", "628222", at(12, types.ReceiptTypeDelivered), "delivered")

	got, err := s.receiptsFor(ctx, "default", []string{"m1"})
	if err != nil {
		t.Fatalf("receiptsFor: %v", err)
	}
	rc := got["m1"]
	if len(rc) != 2 {
		t.Fatalf("want 2 receipts, got %+v", rc)
	}
	byP := map[string]Receipt{}
	for _, r := range rc {
		byP[r.Participant] = r
	}
	if byP["628111"].Type != "read" || byP["628111"].Timestamp != 10 {
		t.Errorf("628111 must stay read@10, got %+v", byP["628111"])
	}
	if byP["628222"].Type != "delivered" {
		t.Errorf("628222 want delivered, got %+v", byP["628222"])
	}
}
