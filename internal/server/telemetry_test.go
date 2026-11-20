package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTelemetryAnthropicSSE(t *testing.T) {
	start := time.Now()
	rec := httptest.NewRecorder()
	tw := newTelemetryResponseWriter(rec, start, "claude-sonnet-4-5", true)
	tw.Header().Set("Content-Type", "text/event-stream")
	tw.WriteHeader(http.StatusOK)

	_, _ = tw.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5-20250929\",\"usage\":{\"input_tokens\":1200,\"cache_read_input_tokens\":800,\"cache_creation_input_tokens\":200,\"output_tokens\":1}}}\n\n"))
	time.Sleep(15 * time.Millisecond)
	_, _ = tw.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n"))
	time.Sleep(60 * time.Millisecond)
	_, _ = tw.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n"))
	_, _ = tw.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":120}}\n\n"))

	got := tw.finish(time.Now())
	if got.Model != "claude-sonnet-4-5-20250929" {
		t.Fatalf("model = %q, want claude-sonnet-4-5-20250929", got.Model)
	}
	if got.InputTokens != 1200 || got.OutputTokens != 120 || got.CacheReadTokens != 800 || got.CacheWriteTokens != 200 {
		t.Fatalf("unexpected tokens: %+v", got)
	}
	if got.TotalTokens != 2320 {
		t.Fatalf("total_tokens = %d, want 2320", got.TotalTokens)
	}
	if got.TTFTMs < 10 {
		t.Fatalf("ttft_ms = %d, want >= 10", got.TTFTMs)
	}
	if got.DecodeMs < 50 {
		t.Fatalf("decode_ms = %d, want >= 50", got.DecodeMs)
	}
	if got.TokensPerSec <= 0 {
		t.Fatalf("tokens_per_sec = %v, want > 0", got.TokensPerSec)
	}
}

func TestTelemetryResponsesAndGeminiAndGrok(t *testing.T) {
	t.Run("openai responses sse", func(t *testing.T) {
		tw := newTelemetryResponseWriter(httptest.NewRecorder(), time.Now().Add(-100*time.Millisecond), "gpt-5", true)
		tw.Header().Set("Content-Type", "text/event-stream")
		_, _ = tw.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
		_, _ = tw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.2\",\"usage\":{\"input_tokens\":500,\"output_tokens\":80,\"input_tokens_details\":{\"cached_tokens\":300}}}}\n\n"))
		got := tw.finish(time.Now())
		if got.Model != "gpt-5.2" || got.InputTokens != 500 || got.OutputTokens != 80 || got.CacheReadTokens != 300 {
			t.Fatalf("unexpected responses record: %+v", got)
		}
	})

	t.Run("gemini agy json", func(t *testing.T) {
		tw := newTelemetryResponseWriter(httptest.NewRecorder(), time.Now().Add(-80*time.Millisecond), "gemini-2.5-pro", false)
		tw.Header().Set("Content-Type", "application/json")
		_, _ = tw.Write([]byte(`{"modelVersion":"gemini-2.5-pro-exp","candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":400,"candidatesTokenCount":60,"thoughtsTokenCount":40,"cachedContentTokenCount":100}}`))
		got := tw.finish(time.Now())
		if got.Model != "gemini-2.5-pro-exp" || got.InputTokens != 400 || got.OutputTokens != 100 || got.CacheReadTokens != 100 {
			t.Fatalf("unexpected gemini record: %+v", got)
		}
	})

	t.Run("grok chat completions sse", func(t *testing.T) {
		tw := newTelemetryResponseWriter(httptest.NewRecorder(), time.Now().Add(-90*time.Millisecond), "grok-3", true)
		tw.Header().Set("Content-Type", "text/event-stream")
		_, _ = tw.Write([]byte("data: {\"model\":\"grok-3-beta\",\"choices\":[{\"delta\":{\"content\":\"yo\"}}]}\n\n"))
		_, _ = tw.Write([]byte("data: {\"model\":\"grok-3-beta\",\"choices\":[],\"usage\":{\"prompt_tokens\":210,\"completion_tokens\":45,\"prompt_tokens_details\":{\"cached_tokens\":50}}}\n\n"))
		_, _ = tw.Write([]byte("data: [DONE]\n\n"))
		got := tw.finish(time.Now())
		if got.Model != "grok-3-beta" || got.InputTokens != 210 || got.OutputTokens != 45 || got.CacheReadTokens != 50 {
			t.Fatalf("unexpected grok record: %+v", got)
		}
	})
}

func TestTelemetryStorePersistenceAndSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	store := newTelemetryStore(path)
	now := time.Now()
	store.add(TelemetryRecord{
		Timestamp:       now.Add(-10 * time.Minute).UnixMilli(),
		Time:            "12:00:00",
		Provider:        "claude",
		Upstream:        "本机订阅",
		Model:           "claude-sonnet-4-5",
		Status:          200,
		Stream:          true,
		InputTokens:     1000,
		OutputTokens:    200,
		CacheReadTokens: 1000,
		TTFTMs:          600,
		DecodeMs:        4000,
		TotalMs:         4600,
		TokensPerSec:    50.0,
	})
	store.add(TelemetryRecord{
		Timestamp:    now.Add(-2 * time.Minute).UnixMilli(),
		Time:         "12:08:00",
		Provider:     "codex",
		Upstream:     "openrouter",
		Model:        "gpt-5.2",
		Status:       200,
		Stream:       true,
		InputTokens:  500,
		OutputTokens: 100,
		TTFTMs:       400,
		DecodeMs:     1000,
		TotalMs:      1400,
		TokensPerSec: 100.0,
	})

	// Reload from disk to verify persistence.
	reloaded := newTelemetryStore(path)
	snap := reloaded.snapshot("24h", now)
	if snap.Summary.Requests != 2 {
		t.Fatalf("requests = %d, want 2", snap.Summary.Requests)
	}
	if snap.Summary.TotalTokens != 2800 {
		t.Fatalf("total_tokens = %d, want 2800", snap.Summary.TotalTokens)
	}
	if snap.Summary.AvgTPS != 75.0 {
		t.Fatalf("avg_tps = %v, want 75.0", snap.Summary.AvgTPS)
	}
	if snap.Summary.MedianTTFTMs != 500 {
		t.Fatalf("median_ttft_ms = %v, want 500", snap.Summary.MedianTTFTMs)
	}
	if len(snap.ByProvider) != 2 || len(snap.ByModel) != 2 || len(snap.Hourly) != 24 || len(snap.Recent) != 2 {
		t.Fatalf("unexpected snapshot lengths: prov=%d model=%d hourly=%d recent=%d", len(snap.ByProvider), len(snap.ByModel), len(snap.Hourly), len(snap.Recent))
	}
	if snap.Recent[0].Provider != "codex" {
		t.Fatalf("recent[0] should be newest (codex), got %q", snap.Recent[0].Provider)
	}

	if err := reloaded.clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := reloaded.snapshot("all", now).Summary.Requests; got != 0 {
		t.Fatalf("after clear requests = %d, want 0", got)
	}
}

func TestPeekRequestMeta(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	model, stream := peekRequestMeta(req)
	if model != "claude-opus-4-8" || !stream {
		t.Fatalf("peekRequestMeta = (%q, %v), want (claude-opus-4-8, true)", model, stream)
	}
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatalf("body not restored: %v", err)
	}

	agyReq := httptest.NewRequest(http.MethodPost, "/agy/v1beta/models/gemini-2.5-pro:streamGenerateContent", strings.NewReader(`{}`))
	agyModel, agyStream := peekRequestMeta(agyReq)
	if agyModel != "gemini-2.5-pro" || !agyStream {
		t.Fatalf("agy peekRequestMeta = (%q, %v), want (gemini-2.5-pro, true)", agyModel, agyStream)
	}
}
