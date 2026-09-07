package provider

import (
	"net/http"
	"testing"
)

func TestCodexClientVersion(t *testing.T) {
	for _, tc := range []struct{ name, ua, version, want string }{
		{"interactive", "codex_cli_rs/0.153.4 (Linux)", "", "0.153.4"},
		{"tui", "codex-tui/0.153.4 (Ubuntu 22.4.0; x86_64) xterm-256color (codex-tui; 0.153.4)", "", "0.153.4"},
		{"exec", "codex_exec/0.153.4 (Ubuntu 22.4.0; x86_64)", "", "0.153.4"},
		{"explicit wins", "codex_exec/0.153.4", "0.154.0", "0.154.0"},
		{"prerelease", "codex_cli_rs/0.154.0-alpha.1 (Linux)", "", "0.154.0-alpha.1"},
		{"unknown client", "curl/8.0.1", "", codexFallbackVersion},
		{"missing identity", "", "", codexFallbackVersion},
		{"invalid version", "codex_exec/invalid", "", codexFallbackVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, out := make(http.Header), make(http.Header)
			in.Set("User-Agent", tc.ua)
			in.Set("version", tc.version)
			setCodexClientHeaders(in, out)
			if got := out.Get("version"); got != tc.want {
				t.Fatalf("version = %q, want %q", got, tc.want)
			}
			if out.Get("User-Agent") != tc.ua {
				t.Fatal("User-Agent changed")
			}
		})
	}
}

func TestParseCodexUsage(t *testing.T) {
	// Shape captured live from https://chatgpt.com/backend-api/wham/usage.
	body := []byte(`{
		"plan_type": "plus",
		"rate_limit": {
			"primary_window":   {"used_percent": 50,  "limit_window_seconds": 18000,  "reset_at": 1781764550},
			"secondary_window": {"used_percent": 150, "limit_window_seconds": 604800, "reset_at": 1782351350}
		}
	}`)

	got, ok := parseCodexUsage(body)
	if !ok || len(got) != 2 {
		t.Fatalf("ok=%v len=%d", ok, len(got))
	}
	if got[0].Label != "5 小时" || got[0].Utilization != 0.5 || got[0].ResetsAt != 1781764550 {
		t.Fatalf("primary = %+v", got[0])
	}
	if got[1].Label != "每周" || got[1].Utilization != 1 { // 150% clamps to 1
		t.Fatalf("secondary = %+v", got[1])
	}

	t.Run("milliseconds reset normalized", func(t *testing.T) {
		body := []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"reset_at":1781764550000}}}`)
		got, ok := parseCodexUsage(body)
		if !ok || len(got) != 1 || got[0].ResetsAt != 1781764550 {
			t.Fatalf("ok=%v got=%+v", ok, got)
		}
	})

	t.Run("unrecognized shape", func(t *testing.T) {
		for _, b := range []string{`not json`, `{}`, `{"rate_limit":{}}`} {
			if _, ok := parseCodexUsage([]byte(b)); ok {
				t.Fatalf("body %q unexpectedly parsed", b)
			}
		}
	})
}
