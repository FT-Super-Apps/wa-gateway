package gateway

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestReceiptStatus(t *testing.T) {
	cases := []struct {
		in     types.ReceiptType
		want   string
		wantOk bool
	}{
		{types.ReceiptTypeDelivered, "delivered", true},
		{types.ReceiptTypeRead, "read", true},
		{types.ReceiptTypePlayed, "played", true},
		{types.ReceiptTypeReadSelf, "read-self", true},
		{types.ReceiptTypePlayedSelf, "played-self", true},
		{types.ReceiptTypeSender, "", false},
		{types.ReceiptTypeRetry, "", false},
		{types.ReceiptTypeInactive, "", false},
	}
	for _, c := range cases {
		got, ok := receiptStatus(c.in)
		if got != c.want || ok != c.wantOk {
			t.Errorf("receiptStatus(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOk)
		}
	}
}

func TestStatusRank(t *testing.T) {
	// Ranks must be strictly increasing along the delivery lifecycle so that
	// updateStatus only ever moves a message forward.
	order := []string{"sent", "delivered", "read", "played"}
	for i := 1; i < len(order); i++ {
		if statusRank(order[i]) <= statusRank(order[i-1]) {
			t.Errorf("statusRank(%q)=%d not greater than statusRank(%q)=%d",
				order[i], statusRank(order[i]), order[i-1], statusRank(order[i-1]))
		}
	}
	for _, s := range []string{"", "read-self", "played-self", "bogus"} {
		if statusRank(s) != -1 {
			t.Errorf("statusRank(%q) = %d, want -1", s, statusRank(s))
		}
	}
}
