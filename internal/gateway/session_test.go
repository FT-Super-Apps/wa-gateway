package gateway

import "testing"

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		cc      string
		want    string
		wantErr bool
	}{
		// Format internasional murni
		{"plain international", "628114100444", "", "628114100444", false},
		{"with plus", "+628114100444", "", "628114100444", false},
		{"with plus and dashes", "+62-811-4100-444", "", "628114100444", false},
		{"with plus and spaces", "+62 811 4100 444", "", "628114100444", false},

		// Separator campuran
		{"dashes only", "0812-345-678", "62", "62812345678", false},
		{"equal signs", "0812=345=678", "62", "62812345678", false},
		{"dots", "0812.345.678", "62", "62812345678", false},
		{"parentheses and space", "(0812) 345 678", "62", "62812345678", false},
		{"slash", "0812/345/678", "62", "62812345678", false},
		{"mixed separators", "0812=345-678", "62", "62812345678", false},
		{"all separators", "+6281-234.5 678", "", "62812345678", false},

		// Nomor lokal dengan country code
		{"local with cc", "08114100444", "62", "628114100444", false},
		{"local with dashes and cc", "0812-345-678", "62", "62812345678", false},

		// Format internasional dengan separators
		{"international with dashes", "628114100444", "", "628114100444", false},
		{"plus international dashes", "+6281-234-5678", "", "62812345678", false},

		// Error cases
		{"empty", "", "", "", true},
		{"only separators", "---  ---", "", "", true},
		{"local no cc", "08114100444", "", "", true},
		{"too short", "628", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizePhone(tt.raw, tt.cc)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("NormalizePhone(%q, %q) = %q, want %q", tt.raw, tt.cc, got, tt.want)
			}
		})
	}
}

func TestParseJID(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		cc        string
		wantUser  string
		wantError bool
	}{
		// Format dasar
		{"international plain", "628114100444", "", "628114100444", false},
		{"with plus", "+628114100444", "", "628114100444", false},
		{"legacy c.us suffix", "628114100444@c.us", "", "628114100444", false},
		{"whatsapp.net passthrough", "628114100444@s.whatsapp.net", "", "628114100444", false},
		{"group jid passthrough", "1234567890123456789@g.us", "", "1234567890123456789", false},

		// Nomor lokal
		{"local zero with cc", "08114100444", "62", "628114100444", false},
		{"local zero without cc rejected", "08114100444", "", "", true},

		// Dengan separator
		{"dashes with cc", "0812-345-678", "62", "62812345678", false},
		{"equal and dashes with cc", "0812=345-678", "62", "62812345678", false},
		{"plus with separators", "+6281-234-5678", "", "62812345678", false},
		{"spaces", "(628) 114 100 444", "", "628114100444", false},

		// Error
		{"empty", "", "", "", true},
		{"only letters", "abcdef", "", "", true},
		{"too short", "628", "", "", true},
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
