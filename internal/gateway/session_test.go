package gateway

import "testing"

func TestParseJID(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		cc        string
		wantUser  string
		wantError bool
	}{
		{"international plain", "628114100444", "", "628114100444", false},
		{"with plus", "+628114100444", "", "628114100444", false},
		{"legacy c.us suffix", "628114100444@c.us", "", "628114100444", false},
		{"local zero with cc", "08114100444", "62", "628114100444", false},
		{"local zero without cc rejected", "08114100444", "", "", true},
		{"empty", "", "", "", true},
		{"non numeric", "62-811", "", "", true},
		{"whatsapp.net passthrough", "628114100444@s.whatsapp.net", "", "628114100444", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jid, err := parseJID(tt.raw, tt.cc)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got jid=%s", jid.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if jid.User != tt.wantUser {
				t.Errorf("user = %q, want %q", jid.User, tt.wantUser)
			}
		})
	}
}
