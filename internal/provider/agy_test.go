package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeAgyTestLogin(t *testing.T, path string, expiry time.Time) {
	t.Helper()
	data := fmt.Sprintf(`{"project_id":"test-project","token":{"access_token":"upstream","expiry":%q}}`, expiry.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestAgyRelay(t *testing.T) {
	a := NewAgyProvider()
	a.path = filepath.Join(t.TempDir(), "login")
	writeAgyTestLogin(t, a.path, time.Now().Add(time.Hour))
	a.listModels = func(context.Context) ([]agyCLIModel, error) {
		return parseAgyCLIModels(
			"\r⠋ Fetching available models...\rgemini-4-neon-high              Gemini 4 Neon (High)\n" +
				"gemini-4-neon-xhigh             Gemini 4 Neon (XHigh)\n" +
				"gemini-4-argon-medium           Gemini 4 Argon (Medium)\n" +
				"gemini-3.8-flash-high           Gemini 3.8 Flash (High)\n" +
				"claude-opus-5-5-high            Claude Opus 5.5 (High)\n" +
				"gpt-oss-120b-medium             GPT-OSS 120B (Medium)\n",
		), nil
	}
	for _, tc := range []struct {
		path, method, upstream, body, want string
		status                             int
	}{
		{"", "GET", "fetchAvailableModels", `{"models":{"test":{"displayName":"Test"}}}`, "models/test", 200},
		{"/test", "GET", "fetchAvailableModels", `{"models":{"test":{}}}`, "models/test", 200},
		{"/other", "GET", "fetchAvailableModels", `{"models":{"test":{}}}`, "Unknown model", 404},
		{"/test:generateContent", "POST", "generateContent", `{"response":{"candidates":[]}}`, `{"candidates":[]}`, 200},
		{"/test:streamGenerateContent", "POST", "streamGenerateContent", "data: {\"response\":{\"candidates\":[]}}\n\n", "data: {\"candidates\":[]}\n\n", 200},
		{"/test:generateContent", "POST", "generateContent", "sensitive error", "Too Many Requests", 429},
	} {
		t.Run(tc.path+tc.want, func(t *testing.T) {
			a.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1internal:"+tc.upstream || r.Header.Get("Authorization") != "Bearer upstream" || r.Header.Get("x-goog-api-key") != "" {
					t.Fatal("incorrect upstream request")
				}
				if tc.method == "POST" {
					data, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(data), `"project":"test-project"`) || !strings.Contains(string(data), `"request":{"contents":[]}`) {
						t.Fatalf("incorrect envelope: %s", data)
					}
				}
				status := 200
				if tc.status == 429 {
					status = 429
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})
			req := httptest.NewRequest(tc.method, agyModels+tc.path, strings.NewReader(`{"contents":[]}`))
			req.Header.Set("x-goog-api-key", "downstream")
			w := httptest.NewRecorder()
			a.Relay(w, req)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) || strings.Contains(w.Body.String(), "sensitive error") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if strings.Contains(tc.path, "stream") && !w.Flushed {
				t.Fatal("stream was buffered")
			}
		})
	}
}
func TestAgyConcurrentRefresh(t *testing.T) {
	a := NewAgyProvider()
	a.path = filepath.Join(t.TempDir(), "login")
	writeAgyTestLogin(t, a.path, time.Now().Add(-time.Hour))
	calls := 0
	a.refresh = func(context.Context) error {
		calls++
		writeAgyTestLogin(t, a.path, time.Now().Add(time.Hour))
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.credentials(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("refresh called %d times", calls)
	}
}
func TestAgyCancellation(t *testing.T) {
	a := NewAgyProvider()
	a.path = filepath.Join(t.TempDir(), "login")
	writeAgyTestLogin(t, a.path, time.Now().Add(time.Hour))
	a.listModels = func(context.Context) ([]agyCLIModel, error) {
		return parseAgyCLIModels(
			"\r⠋ Fetching available models...\rgemini-4-neon-high              Gemini 4 Neon (High)\n" +
				"gemini-4-neon-xhigh             Gemini 4 Neon (XHigh)\n" +
				"gemini-4-argon-medium           Gemini 4 Argon (Medium)\n" +
				"gemini-3.8-flash-high           Gemini 3.8 Flash (High)\n" +
				"claude-opus-5-5-high            Claude Opus 5.5 (High)\n" +
				"gpt-oss-120b-medium             GPT-OSS 120B (Medium)\n",
		), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan struct{})
	a.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	go func() {
		defer close(done)
		a.Relay(httptest.NewRecorder(), httptest.NewRequest("GET", agyModels, nil).WithContext(ctx))
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("upstream still running")
	}
}

func TestAgyQueryUsage(t *testing.T) {
	a := NewAgyProvider()
	a.path = filepath.Join(t.TempDir(), "login")
	writeAgyTestLogin(t, a.path, time.Now().Add(time.Hour))
	a.listModels = func(context.Context) ([]agyCLIModel, error) {
		return parseAgyCLIModels(
			"\r⠋ Fetching available models...\rgemini-4-neon-high              Gemini 4 Neon (High)\n" +
				"gemini-4-neon-xhigh             Gemini 4 Neon (XHigh)\n" +
				"gemini-4-argon-medium           Gemini 4 Argon (Medium)\n" +
				"gemini-3.8-flash-high           Gemini 3.8 Flash (High)\n" +
				"claude-opus-5-5-high            Claude Opus 5.5 (High)\n" +
				"gpt-oss-120b-medium             GPT-OSS 120B (Medium)\n",
		), nil
	}

	a.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"paidTier":{"name":"Google AI Pro"},"cloudaicompanionProject":"aicode-consumers"}`)),
				Header:     http.Header{},
			}, nil
		}
		if r.URL.Path != "/v1internal:fetchAvailableModels" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		body := `{
			"models": {
				"claude-opus-5-5-high": {"displayName": "Claude Opus 5.5 (High)", "recommended": true, "quotaInfo": {"remainingFraction": 0.04, "resetTime": "2026-11-26T02:31:07Z"}},
				"gpt-oss-120b-medium": {"displayName": "GPT-OSS 120B (Medium)", "recommended": true, "quotaInfo": {"remainingFraction": 0.04, "resetTime": "2026-11-26T02:31:07Z"}},
				"gemini-2.5-pro": {"displayName": "Gemini 2.5 Pro", "recommended": true, "quotaInfo": {"remainingFraction": 0.65, "resetTime": "2026-11-20T06:18:54Z"}},
				"gemini-3-flash": {"displayName": "Gemini 3 Flash", "recommended": true, "quotaInfo": {"remainingFraction": 0.65, "resetTime": "2026-11-20T06:18:54Z"}},
				"tab_flash_lite_preview": {"quotaInfo": {"remainingFraction": 1}}
			}
		}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})

	rep, err := a.QueryUsageReport(context.Background())
	if err != nil {
		t.Fatalf("QueryUsageReport: %v", err)
	}
	if rep.Plan != "Google AI Pro" {
		t.Fatalf("plan = %q, want Google AI Pro", rep.Plan)
	}
	windows := rep.Windows
	if len(windows) != 2 {
		t.Fatalf("len(windows) = %d, want 2: %+v", len(windows), windows)
	}
	if windows[0].Label != "Claude / GPT 配额" || windows[0].Utilization < 0.95 || windows[0].Utilization > 0.97 || len(windows[0].Models) != 2 {
		t.Fatalf("window[0] = %+v", windows[0])
	}
	if windows[1].Label != "Gemini 配额" || windows[1].Utilization < 0.34 || windows[1].Utilization > 0.36 || len(windows[1].Models) != 3 || windows[1].Models[0] != "Gemini 4 Neon" {
		t.Fatalf("window[1] = %+v", windows[1])
	}

	// Exhausted pool where protobuf omits remainingFraction (0.0) but includes resetTime.
	exhausted, ok := parseAgyUsage([]byte(`{"models":{"gemini-2.5-pro":{"displayName":"Gemini 2.5 Pro","recommended":true,"quotaInfo":{"resetTime":"2026-11-20T06:18:54Z"}}}}`))
	if !ok || len(exhausted) != 1 || exhausted[0].Utilization != 1 {
		t.Fatalf("exhausted parse = (%+v, %v), want 1 window with utilization=1", exhausted, ok)
	}
}
