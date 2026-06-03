package gateway

import "testing"

func TestRenderTemplate(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		vars map[string]string
		want string
	}{
		{
			name: "basic",
			tmpl: "Halo {{name}}, nilai kamu {{nilai}}.",
			vars: map[string]string{"name": "Budi", "nilai": "90"},
			want: "Halo Budi, nilai kamu 90.",
		},
		{
			name: "spaces inside braces",
			tmpl: "Kelas mulai {{ waktu }} ya {{ name }}",
			vars: map[string]string{"waktu": "08:00", "name": "Sari"},
			want: "Kelas mulai 08:00 ya Sari",
		},
		{
			name: "unknown placeholder kept",
			tmpl: "Hai {{name}}, kode {{otp}}",
			vars: map[string]string{"name": "Budi"},
			want: "Hai Budi, kode {{otp}}",
		},
		{
			name: "no vars returns template unchanged",
			tmpl: "Hai {{name}}",
			vars: nil,
			want: "Hai {{name}}",
		},
		{
			name: "repeated placeholder",
			tmpl: "{{name}} {{name}}",
			vars: map[string]string{"name": "X"},
			want: "X X",
		},
		{
			name: "no placeholders",
			tmpl: "pesan biasa",
			vars: map[string]string{"name": "Budi"},
			want: "pesan biasa",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderTemplate(c.tmpl, c.vars); got != c.want {
				t.Errorf("renderTemplate(%q, %v) = %q, want %q", c.tmpl, c.vars, got, c.want)
			}
		})
	}
}
