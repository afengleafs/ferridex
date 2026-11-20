package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ferridex/internal/provider"
)

const (
	maxTelemetryRecords = 500
	maxJSONCaptureBytes = 2 << 20 // 2 MiB cap for non-streaming JSON usage parsing
	maxReqPeekBytes     = 1 << 20 // 1 MiB cap for request model peeking
)

// TelemetryRecord captures token usage and latency/speed metrics for one
// proxied conversation/inference request.
type TelemetryRecord struct {
	ID               int64   `json:"id"`
	Timestamp        int64   `json:"timestamp"` // unix milliseconds
	Time             string  `json:"time"`      // HH:MM:SS local
	Provider         string  `json:"provider"`
	Upstream         string  `json:"upstream,omitempty"`
	Model            string  `json:"model,omitempty"`
	Method           string  `json:"method"`
	Path             string  `json:"path"`
	Status           int     `json:"status"`
	Stream           bool    `json:"stream"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	TTFTMs           int64   `json:"ttft_ms"`
	DecodeMs         int64   `json:"decode_ms"`
	TotalMs          int64   `json:"total_ms"`
	TokensPerSec     float64 `json:"tokens_per_sec"`
}

type telemetryStore struct {
	mu      sync.Mutex
	path    string
	nextID  int64
	records []TelemetryRecord
}

func defaultTelemetryPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ferridex", "telemetry.jsonl")
}

func newTelemetryStore(path string) *telemetryStore {
	s := &telemetryStore{path: path}
	if path != "" {
		s.load()
	}
	return s
}

func (s *telemetryStore) load() {
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	var loaded []TelemetryRecord
	var maxID int64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec TelemetryRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.ID > maxID {
			maxID = rec.ID
		}
		loaded = append(loaded, rec)
	}
	if len(loaded) > maxTelemetryRecords {
		loaded = loaded[len(loaded)-maxTelemetryRecords:]
	}
	s.records = loaded
	s.nextID = maxID
}

func (s *telemetryStore) add(rec TelemetryRecord) TelemetryRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	rec.ID = s.nextID
	if rec.TotalTokens == 0 {
		rec.TotalTokens = rec.InputTokens + rec.OutputTokens + rec.CacheReadTokens + rec.CacheWriteTokens
	}
	s.records = append(s.records, rec)
	if len(s.records) > maxTelemetryRecords {
		s.records = s.records[len(s.records)-maxTelemetryRecords:]
	}
	if s.path != "" {
		s.appendFileLocked(rec)
	}
	return rec
}

func (s *telemetryStore) appendFileLocked(rec TelemetryRecord) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

func (s *telemetryStore) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
	if s.path != "" {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

type metricsSummary struct {
	Requests         int     `json:"requests"`
	SuccessRequests  int     `json:"success_requests"`
	SuccessRate      float64 `json:"success_rate"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	AvgTTFTMs        float64 `json:"avg_ttft_ms"`
	MedianTTFTMs     float64 `json:"median_ttft_ms"`
	P95TTFTMs        float64 `json:"p95_ttft_ms"`
	AvgTPS           float64 `json:"avg_tps"`
	MaxTPS           float64 `json:"max_tps"`
	MinTPS           float64 `json:"min_tps"`
	SpeedSamples     int     `json:"speed_samples"`
	ActiveModels     int     `json:"active_models"`
	ActiveProviders  int     `json:"active_providers"`
}

type providerMetrics struct {
	Provider         string  `json:"provider"`
	Requests         int     `json:"requests"`
	SuccessRate      float64 `json:"success_rate"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	AvgTTFTMs        float64 `json:"avg_ttft_ms"`
	MedianTTFTMs     float64 `json:"median_ttft_ms"`
	AvgTPS           float64 `json:"avg_tps"`
}

type modelMetrics struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	Requests         int     `json:"requests"`
	SuccessRate      float64 `json:"success_rate"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	AvgTTFTMs        float64 `json:"avg_ttft_ms"`
	MedianTTFTMs     float64 `json:"median_ttft_ms"`
	AvgTPS           float64 `json:"avg_tps"`
	MaxTPS           float64 `json:"max_tps"`
}

type hourlyBucket struct {
	Hour         string `json:"hour"`
	Requests     int    `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
}

type metricsSnapshot struct {
	Window     string            `json:"window"`
	Summary    metricsSummary    `json:"summary"`
	ByProvider []providerMetrics `json:"by_provider"`
	ByModel    []modelMetrics    `json:"by_model"`
	Hourly     []hourlyBucket    `json:"hourly"`
	Recent     []TelemetryRecord `json:"recent"`
}

func (s *telemetryStore) snapshot(window string, now time.Time) metricsSnapshot {
	s.mu.Lock()
	all := append([]TelemetryRecord(nil), s.records...)
	s.mu.Unlock()

	var cutoffMs int64
	switch window {
	case "today":
		y, m, d := now.Date()
		cutoffMs = time.Date(y, m, d, 0, 0, 0, 0, now.Location()).UnixMilli()
	case "24h":
		cutoffMs = now.Add(-24 * time.Hour).UnixMilli()
	case "7d":
		cutoffMs = now.Add(-7 * 24 * time.Hour).UnixMilli()
	default:
		window = "all"
		cutoffMs = 0
	}

	filtered := make([]TelemetryRecord, 0, len(all))
	for _, rec := range all {
		if cutoffMs == 0 || rec.Timestamp >= cutoffMs {
			filtered = append(filtered, rec)
		}
	}

	var sum metricsSummary
	sum.Requests = len(filtered)
	var ttfts []float64
	var tpsSum float64
	var tpsCount int
	var ttftSum float64

	type agg struct {
		provider   string
		model      string
		reqs       int
		okReqs     int
		inTok      int64
		outTok     int64
		cacheRead  int64
		cacheWrite int64
		totalTok   int64
		ttfts      []float64
		tpsSum     float64
		tpsCount   int
		maxTPS     float64
	}
	provMap := map[string]*agg{}
	modelMap := map[string]*agg{}

	for _, rec := range filtered {
		ok := rec.Status >= 200 && rec.Status < 300
		if ok {
			sum.SuccessRequests++
		}
		sum.InputTokens += rec.InputTokens
		sum.OutputTokens += rec.OutputTokens
		sum.CacheReadTokens += rec.CacheReadTokens
		sum.CacheWriteTokens += rec.CacheWriteTokens
		sum.TotalTokens += rec.TotalTokens

		if ok && rec.TTFTMs > 0 {
			ttfts = append(ttfts, float64(rec.TTFTMs))
			ttftSum += float64(rec.TTFTMs)
		}
		if ok && rec.TokensPerSec > 0 {
			tpsSum += rec.TokensPerSec
			tpsCount++
			if rec.TokensPerSec > sum.MaxTPS {
				sum.MaxTPS = rec.TokensPerSec
			}
			if sum.MinTPS == 0 || rec.TokensPerSec < sum.MinTPS {
				sum.MinTPS = rec.TokensPerSec
			}
		}

		pName := rec.Provider
		if pName == "" {
			pName = "unknown"
		}
		pa := provMap[pName]
		if pa == nil {
			pa = &agg{provider: pName}
			provMap[pName] = pa
		}
		pa.reqs++
		if ok {
			pa.okReqs++
		}
		pa.inTok += rec.InputTokens
		pa.outTok += rec.OutputTokens
		pa.cacheRead += rec.CacheReadTokens
		pa.cacheWrite += rec.CacheWriteTokens
		pa.totalTok += rec.TotalTokens
		if ok && rec.TTFTMs > 0 {
			pa.ttfts = append(pa.ttfts, float64(rec.TTFTMs))
		}
		if ok && rec.TokensPerSec > 0 {
			pa.tpsSum += rec.TokensPerSec
			pa.tpsCount++
			if rec.TokensPerSec > pa.maxTPS {
				pa.maxTPS = rec.TokensPerSec
			}
		}

		mName := rec.Model
		if mName == "" {
			mName = pName + " (未识别模型)"
		}
		mKey := pName + "\x00" + mName
		ma := modelMap[mKey]
		if ma == nil {
			ma = &agg{provider: pName, model: mName}
			modelMap[mKey] = ma
		}
		ma.reqs++
		if ok {
			ma.okReqs++
		}
		ma.inTok += rec.InputTokens
		ma.outTok += rec.OutputTokens
		ma.cacheRead += rec.CacheReadTokens
		ma.cacheWrite += rec.CacheWriteTokens
		ma.totalTok += rec.TotalTokens
		if ok && rec.TTFTMs > 0 {
			ma.ttfts = append(ma.ttfts, float64(rec.TTFTMs))
		}
		if ok && rec.TokensPerSec > 0 {
			ma.tpsSum += rec.TokensPerSec
			ma.tpsCount++
			if rec.TokensPerSec > ma.maxTPS {
				ma.maxTPS = rec.TokensPerSec
			}
		}
	}

	if sum.Requests > 0 {
		sum.SuccessRate = round1(float64(sum.SuccessRequests) * 100 / float64(sum.Requests))
	}
	if denom := sum.InputTokens + sum.CacheReadTokens + sum.CacheWriteTokens; denom > 0 {
		sum.CacheHitRate = round1(float64(sum.CacheReadTokens) * 100 / float64(denom))
	}
	if len(ttfts) > 0 {
		sum.AvgTTFTMs = math.Round(ttftSum / float64(len(ttfts)))
		sum.MedianTTFTMs = math.Round(percentile(ttfts, 0.50))
		sum.P95TTFTMs = math.Round(percentile(ttfts, 0.95))
	}
	if tpsCount > 0 {
		sum.AvgTPS = round1(tpsSum / float64(tpsCount))
		sum.MaxTPS = round1(sum.MaxTPS)
		sum.MinTPS = round1(sum.MinTPS)
		sum.SpeedSamples = tpsCount
	}
	sum.ActiveProviders = len(provMap)
	sum.ActiveModels = len(modelMap)

	byProvider := make([]providerMetrics, 0, len(provMap))
	for _, pa := range provMap {
		pm := providerMetrics{
			Provider:         pa.provider,
			Requests:         pa.reqs,
			SuccessRate:      round1(float64(pa.okReqs) * 100 / float64(pa.reqs)),
			InputTokens:      pa.inTok,
			OutputTokens:     pa.outTok,
			CacheReadTokens:  pa.cacheRead,
			CacheWriteTokens: pa.cacheWrite,
			TotalTokens:      pa.totalTok,
		}
		if len(pa.ttfts) > 0 {
			pm.AvgTTFTMs = math.Round(avgFloat(pa.ttfts))
			pm.MedianTTFTMs = math.Round(percentile(pa.ttfts, 0.50))
		}
		if pa.tpsCount > 0 {
			pm.AvgTPS = round1(pa.tpsSum / float64(pa.tpsCount))
		}
		byProvider = append(byProvider, pm)
	}
	sort.Slice(byProvider, func(i, j int) bool {
		if byProvider[i].TotalTokens != byProvider[j].TotalTokens {
			return byProvider[i].TotalTokens > byProvider[j].TotalTokens
		}
		return byProvider[i].Requests > byProvider[j].Requests
	})

	byModel := make([]modelMetrics, 0, len(modelMap))
	for _, ma := range modelMap {
		mm := modelMetrics{
			Provider:         ma.provider,
			Model:            ma.model,
			Requests:         ma.reqs,
			SuccessRate:      round1(float64(ma.okReqs) * 100 / float64(ma.reqs)),
			InputTokens:      ma.inTok,
			OutputTokens:     ma.outTok,
			CacheReadTokens:  ma.cacheRead,
			CacheWriteTokens: ma.cacheWrite,
			TotalTokens:      ma.totalTok,
		}
		if len(ma.ttfts) > 0 {
			mm.AvgTTFTMs = math.Round(avgFloat(ma.ttfts))
			mm.MedianTTFTMs = math.Round(percentile(ma.ttfts, 0.50))
		}
		if ma.tpsCount > 0 {
			mm.AvgTPS = round1(ma.tpsSum / float64(ma.tpsCount))
			mm.MaxTPS = round1(ma.maxTPS)
		}
		byModel = append(byModel, mm)
	}
	sort.Slice(byModel, func(i, j int) bool {
		if byModel[i].TotalTokens != byModel[j].TotalTokens {
			return byModel[i].TotalTokens > byModel[j].TotalTokens
		}
		return byModel[i].Requests > byModel[j].Requests
	})

	// Build 24 hourly buckets ending at the current hour.
	curHour := now.Truncate(time.Hour)
	hourly := make([]hourlyBucket, 24)
	hourIndex := make(map[int64]int, 24)
	for i := 0; i < 24; i++ {
		t := curHour.Add(time.Duration(i-23) * time.Hour)
		hourly[i] = hourlyBucket{Hour: t.Format("15:00")}
		hourIndex[t.Unix()] = i
	}
	for _, rec := range all {
		t := time.UnixMilli(rec.Timestamp).In(now.Location()).Truncate(time.Hour).Unix()
		if idx, ok := hourIndex[t]; ok {
			hourly[idx].Requests++
			hourly[idx].InputTokens += rec.InputTokens + rec.CacheReadTokens + rec.CacheWriteTokens
			hourly[idx].OutputTokens += rec.OutputTokens
			hourly[idx].TotalTokens += rec.TotalTokens
		}
	}

	// Recent records (newest first, up to 100).
	recentLimit := 100
	if len(filtered) < recentLimit {
		recentLimit = len(filtered)
	}
	recent := make([]TelemetryRecord, 0, recentLimit)
	for i := len(filtered) - 1; i >= len(filtered)-recentLimit; i-- {
		recent = append(recent, filtered[i])
	}

	return metricsSnapshot{
		Window:     window,
		Summary:    sum,
		ByProvider: byProvider,
		ByModel:    byModel,
		Hourly:     hourly,
		Recent:     recent,
	}
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func avgFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	if len(cp) == 1 {
		return cp[0]
	}
	pos := p * float64(len(cp)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return cp[lo]
	}
	frac := pos - float64(lo)
	return cp[lo]*(1-frac) + cp[hi]*frac
}

// isConversationRequest returns true for actual inference/chat endpoints
// (excluding metadata calls like GET /models).
func isConversationRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	path := r.URL.Path
	switch path {
	case "/v1/messages", "/messages", "/v1/responses", "/responses":
		return true
	}
	if provider.IsAgyPath(path) && strings.Contains(path, ":") {
		return true
	}
	if provider.IsGrokPath(path) && (strings.Contains(path, "/chat/") || strings.Contains(path, "/responses") || strings.Contains(path, "/messages")) {
		return true
	}
	return provider.IsCursorConnectPath(path)
}

func peekRequestMeta(r *http.Request) (model string, stream bool) {
	// Gemini / agy encodes model name in path: /agy/v1beta/models/<model>:streamGenerateContent
	if provider.IsAgyPath(r.URL.Path) {
		if idx := strings.Index(r.URL.Path, "/models/"); idx >= 0 {
			rest := r.URL.Path[idx+len("/models/"):]
			if colon := strings.Index(rest, ":"); colon > 0 {
				model = rest[:colon]
				if strings.Contains(strings.ToLower(rest[colon:]), "stream") {
					stream = true
				}
			}
		}
	}
	if r.Body == nil {
		return model, stream
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "json") {
		return model, stream
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReqPeekBytes+1))
	if err != nil {
		return model, stream
	}
	if len(body) > maxReqPeekBytes {
		// Restore stream without parsing oversized payload.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return model, stream
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(body, &req) == nil {
		if req.Model != "" {
			model = req.Model
		}
		if req.Stream {
			stream = true
		}
	}
	return model, stream
}

func activeUpstreamLabel(p provider.Provider) string {
	switch v := p.(type) {
	case *provider.ClaudeUpstreamProvider:
		snap := v.Snapshot()
		if snap.Active == provider.ClaudeUpstreamProfile && snap.ProfileName != "" {
			return snap.ProfileName
		}
		return "本机订阅"
	case *provider.ResponsesProvider:
		snap := v.Snapshot()
		if snap.ActiveSource == provider.ResponsesSourceCustom {
			if name := loadConfig().CodexUpstream; name != "" {
				return name
			}
			return "自定义上游"
		}
		return "本机订阅"
	default:
		return "本机订阅"
	}
}

// telemetryResponseWriter transparently observes streamed/non-streamed response
// chunks to compute TTFT, decode duration, TPS, and token usage.
type telemetryResponseWriter struct {
	http.ResponseWriter
	status       int
	start        time.Time
	firstByteAt  time.Time
	firstTokenAt time.Time
	lastTokenAt  time.Time
	isSSE        bool
	sseBuf       bytes.Buffer
	jsonBuf      bytes.Buffer
	jsonTooLarge bool

	model            string
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	contentChunks    int
}

func newTelemetryResponseWriter(w http.ResponseWriter, start time.Time, model string, stream bool) *telemetryResponseWriter {
	return &telemetryResponseWriter{
		ResponseWriter: w,
		start:          start,
		model:          model,
		isSSE:          stream,
	}
}

func (tw *telemetryResponseWriter) Unwrap() http.ResponseWriter {
	return tw.ResponseWriter
}

func (tw *telemetryResponseWriter) WriteHeader(code int) {
	tw.status = code
	ct := strings.ToLower(tw.Header().Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		tw.isSSE = true
	} else if strings.Contains(ct, "application/json") {
		tw.isSSE = false
	}
	tw.ResponseWriter.WriteHeader(code)
}

func (tw *telemetryResponseWriter) Write(b []byte) (int, error) {
	if tw.status == 0 {
		tw.WriteHeader(http.StatusOK)
	}
	now := time.Now()
	if len(b) > 0 && tw.firstByteAt.IsZero() {
		tw.firstByteAt = now
	}
	n, err := tw.ResponseWriter.Write(b)
	if n > 0 && tw.status >= 200 && tw.status < 300 {
		tw.observe(b[:n], now)
	}
	return n, err
}

func (tw *telemetryResponseWriter) Flush() {
	if f, ok := tw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (tw *telemetryResponseWriter) observe(chunk []byte, now time.Time) {
	if tw.isSSE {
		tw.sseBuf.Write(chunk)
		for {
			data := tw.sseBuf.Bytes()
			idx := bytes.IndexByte(data, '\n')
			if idx < 0 {
				break
			}
			line := string(bytes.TrimRight(data[:idx], "\r"))
			tw.sseBuf.Next(idx + 1)
			tw.consumeSSELine(line, now)
		}
		return
	}
	if !tw.jsonTooLarge {
		if tw.jsonBuf.Len()+len(chunk) > maxJSONCaptureBytes {
			tw.jsonTooLarge = true
			tw.jsonBuf.Reset()
		} else {
			tw.jsonBuf.Write(chunk)
		}
	}
}

func (tw *telemetryResponseWriter) consumeSSELine(line string, now time.Time) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	tw.parseJSONPayload([]byte(payload), now, true)
}

func (tw *telemetryResponseWriter) finish(end time.Time) TelemetryRecord {
	if tw.isSSE && tw.sseBuf.Len() > 0 {
		tw.consumeSSELine(strings.TrimRight(tw.sseBuf.String(), "\r\n"), end)
		tw.sseBuf.Reset()
	} else if !tw.isSSE && tw.jsonBuf.Len() > 0 {
		tw.parseJSONPayload(tw.jsonBuf.Bytes(), end, false)
		tw.jsonBuf.Reset()
	}

	status := tw.status
	if status == 0 {
		status = http.StatusOK
	}
	totalMs := end.Sub(tw.start).Milliseconds()
	if totalMs < 0 {
		totalMs = 0
	}

	firstAt := tw.firstTokenAt
	if firstAt.IsZero() {
		firstAt = tw.firstByteAt
	}
	var ttftMs int64
	if !firstAt.IsZero() {
		ttftMs = firstAt.Sub(tw.start).Milliseconds()
		if ttftMs < 0 {
			ttftMs = 0
		}
	}

	lastAt := tw.lastTokenAt
	if lastAt.IsZero() || lastAt.Before(firstAt) {
		lastAt = end
	}
	var decodeMs int64
	if !firstAt.IsZero() && lastAt.After(firstAt) {
		decodeMs = lastAt.Sub(firstAt).Milliseconds()
	}

	var tps float64
	if tw.outputTokens > 0 && status >= 200 && status < 300 {
		if tw.isSSE && decodeMs >= 50 {
			tps = float64(tw.outputTokens) / (float64(decodeMs) / 1000.0)
		} else if totalMs > 0 {
			tps = float64(tw.outputTokens) / (float64(totalMs) / 1000.0)
		}
		tps = round1(tps)
	}

	return TelemetryRecord{
		Timestamp:        tw.start.UnixMilli(),
		Time:             tw.start.Format("15:04:05"),
		Model:            tw.model,
		Status:           status,
		Stream:           tw.isSSE,
		InputTokens:      tw.inputTokens,
		OutputTokens:     tw.outputTokens,
		CacheReadTokens:  tw.cacheReadTokens,
		CacheWriteTokens: tw.cacheWriteTokens,
		TotalTokens:      tw.inputTokens + tw.outputTokens + tw.cacheReadTokens + tw.cacheWriteTokens,
		TTFTMs:           ttftMs,
		DecodeMs:         decodeMs,
		TotalMs:          totalMs,
		TokensPerSec:     tps,
	}
}

func (tw *telemetryResponseWriter) markToken(now time.Time) {
	if tw.firstTokenAt.IsZero() {
		tw.firstTokenAt = now
	}
	tw.lastTokenAt = now
	tw.contentChunks++
}

func (tw *telemetryResponseWriter) parseJSONPayload(raw []byte, now time.Time, streaming bool) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}

	if m := jsonString(obj, "model"); m != "" {
		tw.model = m
	}
	if m := jsonString(obj, "modelVersion"); m != "" {
		tw.model = m
	}

	evType := jsonString(obj, "type")
	switch evType {
	// Anthropic Messages SSE
	case "message_start":
		if msg, ok := obj["message"].(map[string]any); ok {
			if m := jsonString(msg, "model"); m != "" {
				tw.model = m
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				tw.applyUsageMap(u)
			}
		}
	case "content_block_delta":
		tw.markToken(now)
	case "message_delta":
		if u, ok := obj["usage"].(map[string]any); ok {
			tw.applyUsageMap(u)
		}
	// OpenAI Responses SSE
	case "response.created", "response.in_progress":
		if resp, ok := obj["response"].(map[string]any); ok {
			if m := jsonString(resp, "model"); m != "" {
				tw.model = m
			}
		}
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.refusal.delta":
		tw.markToken(now)
	case "response.completed", "response.done":
		if resp, ok := obj["response"].(map[string]any); ok {
			if m := jsonString(resp, "model"); m != "" {
				tw.model = m
			}
			if u, ok := resp["usage"].(map[string]any); ok {
				tw.applyUsageMap(u)
			}
		}
	}

	// OpenAI Chat Completions (Grok / standard)
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if ch0, ok := choices[0].(map[string]any); ok {
			if delta, ok := ch0["delta"].(map[string]any); ok {
				if jsonString(delta, "content") != "" || jsonString(delta, "reasoning_content") != "" || delta["tool_calls"] != nil {
					tw.markToken(now)
				}
			} else if !streaming && ch0["message"] != nil {
				tw.markToken(now)
			}
		}
	}

	// Gemini / Antigravity
	if candidates, ok := obj["candidates"].([]any); ok && len(candidates) > 0 {
		if c0, ok := candidates[0].(map[string]any); ok {
			if content, ok := c0["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
					tw.markToken(now)
				}
			}
		}
	}

	// Top-level usage (Anthropic non-streaming, OpenAI Chat/Responses non-streaming & final chunk)
	if u, ok := obj["usage"].(map[string]any); ok {
		tw.applyUsageMap(u)
	}
	// Gemini usageMetadata
	if u, ok := obj["usageMetadata"].(map[string]any); ok {
		tw.applyGeminiUsageMap(u)
	}
}

func (tw *telemetryResponseWriter) applyUsageMap(u map[string]any) {
	if v := jsonInt64(u, "input_tokens"); v > 0 {
		tw.inputTokens = v
	} else if v := jsonInt64(u, "prompt_tokens"); v > 0 {
		tw.inputTokens = v
	}
	if v := jsonInt64(u, "output_tokens"); v > 0 {
		tw.outputTokens = v
	} else if v := jsonInt64(u, "completion_tokens"); v > 0 {
		tw.outputTokens = v
	}
	if v := jsonInt64(u, "cache_read_input_tokens"); v > 0 {
		tw.cacheReadTokens = v
	}
	if v := jsonInt64(u, "cache_creation_input_tokens"); v > 0 {
		tw.cacheWriteTokens = v
	}
	if details, ok := u["input_tokens_details"].(map[string]any); ok {
		if v := jsonInt64(details, "cached_tokens"); v > 0 {
			tw.cacheReadTokens = v
		}
	}
	if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v := jsonInt64(details, "cached_tokens"); v > 0 {
			tw.cacheReadTokens = v
		}
	}
}

func (tw *telemetryResponseWriter) applyGeminiUsageMap(u map[string]any) {
	if v := jsonInt64(u, "promptTokenCount"); v > 0 {
		tw.inputTokens = v
	}
	out := jsonInt64(u, "candidatesTokenCount") + jsonInt64(u, "thoughtsTokenCount")
	if out > 0 {
		tw.outputTokens = out
	}
	if v := jsonInt64(u, "cachedContentTokenCount"); v > 0 {
		tw.cacheReadTokens = v
	}
}

func jsonString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func jsonInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
