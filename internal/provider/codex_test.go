package provider

import "testing"

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
