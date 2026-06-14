package tavern

import "testing"

func TestScopedTopic(t *testing.T) {
	tests := []struct {
		name  string
		base  string
		parts []string
		want  string
	}{
		{"zero parts", "sales-goals", nil, "sales-goals"},
		{"single key", "sales-goals", []string{"subdivision"}, "sales-goals:subdivision"},
		{"key value", "sales-goals", []string{"subdivision", "12"}, "sales-goals:subdivision:12"},
		{"nested key value", "sales-goals", []string{"subdivision", "12", "agent", "348"}, "sales-goals:subdivision:12:agent:348"},
		{"uuid scope", "theme", []string{"session", "2de6db8d-79d3-49fe-8449-78d8ad8ddc8d"}, "theme:session:2de6db8d-79d3-49fe-8449-78d8ad8ddc8d"},

		// Empty parts are preserved as empty segments, not dropped, so a
		// missing scope cannot silently collapse onto a shorter real topic.
		{"empty trailing part", "theme", []string{"session", ""}, "theme:session:"},
		{"empty middle part", "sales-goals", []string{"", "12"}, "sales-goals::12"},
		{"empty base", "", []string{"session", "abc"}, ":session:abc"},
		{"empty base no parts", "", nil, ""},

		// Parts containing the separator are passed through verbatim; callers
		// own canonicalization.
		{"part contains separator", "theme", []string{"session", "a:b"}, "theme:session:a:b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScopedTopic(tt.base, tt.parts...); got != tt.want {
				t.Errorf("ScopedTopic(%q, %q) = %q, want %q", tt.base, tt.parts, got, tt.want)
			}
		})
	}
}
