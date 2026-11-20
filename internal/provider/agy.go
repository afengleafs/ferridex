package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ferridex/internal/util"
)

const agyModels = "/agy/v1beta/models"

type agyLogin struct {
	Account string `json:"id_token_email"`
	Project string `json:"project_id"`
	Method  string `json:"auth_method"`
	Token   struct {
		Access string    `json:"access_token"`
		Expiry time.Time `json:"expiry"`
	} `json:"token"`
}

type AgyProvider struct {
	path       string
	client     *http.Client
	refresh    func(context.Context) error
	listModels func(context.Context) ([]agyCLIModel, error)
	mu         sync.Mutex
}

type agyCLIModel struct {
	ID          string
	DisplayName string
}

func NewAgyProvider() *AgyProvider {
	home, _ := os.UserHomeDir()
	return &AgyProvider{
		path: filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		client: &http.Client{
			Timeout:       10 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		refresh:    refreshAgy,
		listModels: fetchAgyCLIModels,
	}
}

func (a *AgyProvider) Name() string { return "agy" }

func (a *AgyProvider) login() (agyLogin, error) {
	var login agyLogin
	data, err := os.ReadFile(a.path)
	if err == nil {
		err = json.Unmarshal(data, &login)
	}
	if err != nil || login.Token.Access == "" || login.Token.Expiry.IsZero() {
		return agyLogin{}, errors.New("请先在服务端运行 agy 登录")
	}
	return login, nil
}

func (a *AgyProvider) Status() ProviderStatus {
	s := ProviderStatus{Name: "agy", Title: "Antigravity · CLI", Endpoint: agyModels}
	login, err := a.login()
	if err != nil {
		s.Detail = err.Error()
		return s
	}
	s.Account = login.Account
	s.LoggedIn = login.Token.Expiry.After(time.Now())
	s.Detail = "Antigravity 订阅"
	if !s.LoggedIn {
		s.Detail = "凭据已过期，将在请求时尝试刷新"
	}
	return s
}

// ACP initialization lets the installed CLI refresh its own OAuth credentials.
func refreshAgy(ctx context.Context) error {
	workspace, err := os.MkdirTemp("", "ferridex-agy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	cmd := exec.CommandContext(ctx, "agy", "acp", "--stdio")
	cmd.Dir = workspace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	defer func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() }()
	_, err = io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"ferridex","version":"1"},"capabilities":{},"clientCapabilities":{}}}`+"\n")
	if err != nil {
		return err
	}
	dec := json.NewDecoder(stdout)
	for {
		var msg struct {
			ID     json.RawMessage
			Result json.RawMessage
		}
		if err = dec.Decode(&msg); err != nil {
			return err
		}
		if string(msg.ID) == "1" || string(msg.ID) == "2" {
			if len(msg.Result) == 0 {
				return errors.New("ACP session setup failed")
			}
			if string(msg.ID) == "2" {
				return nil
			}
			// A new session triggers credential recovery without sending a prompt.
			message := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": workspace, "mcpServers": []any{}}}
			if err := json.NewEncoder(stdin).Encode(message); err != nil {
				return err
			}
		}
	}
}

func (a *AgyProvider) credentials(ctx context.Context) (agyLogin, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, e := a.login()
	if e != nil || time.Until(l.Token.Expiry) > 30*time.Second {
		return l, e
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if a.refresh(ctx) == nil {
		l, e = a.login()
		if e == nil && l.Token.Expiry.After(time.Now()) {
			return l, nil
		}
	}
	return agyLogin{}, errors.New("Antigravity 凭据刷新失败，请在服务端运行 agy 重新登录")
}

func IsAgyPath(path string) bool { return path == agyModels || strings.HasPrefix(path, agyModels+"/") }

func agyError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func (a *AgyProvider) call(ctx context.Context, l agyLogin, method string, payload any) (*http.Response, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	base := "https://daily-cloudcode-pa.googleapis.com"
	if l.Method == "oauth-business" || l.Method == "enterprise" {
		base = "https://cloudcode-pa.googleapis.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1internal:"+method, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.Token.Access)
	req.Header.Set("User-Agent", "antigravity")
	return a.client.Do(req)
}

type agyModel struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Methods     []string `json:"supportedGenerationMethods"`
}

func (a *AgyProvider) Relay(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !IsAgyPath(path) {
		http.NotFound(w, r)
		return
	}
	id := ""
	if path != agyModels {
		id = strings.TrimPrefix(path, agyModels+"/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
	}
	model, op, action := strings.Cut(id, ":")
	if action && (model == "" || (op != "generateContent" && op != "streamGenerateContent")) {
		http.NotFound(w, r)
		return
	}
	if (action && r.Method != "POST") || (!action && r.Method != "GET") {
		agyError(w, 405, "Unsupported method")
		return
	}
	login, err := a.credentials(r.Context())
	if err != nil {
		agyError(w, 401, err.Error())
		return
	}
	payload := map[string]any{}
	if login.Project != "" {
		payload["project"] = login.Project
	}
	if action {
		var body map[string]json.RawMessage
		if err = json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
			agyError(w, 400, "Expected a Gemini request object")
			return
		}
		if login.Project == "" {
			resp, e := a.call(r.Context(), login, "loadCodeAssist", map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY", "pluginType": "GEMINI"}})
			if e != nil {
				agyError(w, 502, "Antigravity project lookup failed")
				return
			}
			var info struct {
				Project json.RawMessage `json:"cloudaicompanionProject"`
			}
			e = json.NewDecoder(resp.Body).Decode(&info)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				agyError(w, resp.StatusCode, "Antigravity project lookup failed")
				return
			}
			var project string
			if json.Unmarshal(info.Project, &project) != nil {
				var p struct{ ID string }
				_ = json.Unmarshal(info.Project, &p)
				project = p.ID
			}
			if e != nil || project == "" {
				agyError(w, 502, "Antigravity project unavailable")
				return
			}
			payload["project"] = project
		}
		payload["model"] = model
		payload["request"] = body
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			agyError(w, 500, "Could not create request ID")
			return
		}
		payload["requestId"] = fmt.Sprintf("agent-%x", id)
		payload["userAgent"] = "antigravity"
		payload["requestType"] = "agent"
	} else {
		op = "fetchAvailableModels"
	}
	stream := op == "streamGenerateContent"
	if stream {
		op += "?alt=sse"
	}
	resp, err := a.call(r.Context(), login, op, payload)
	if err != nil {
		agyError(w, 502, "Antigravity request failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		agyError(w, resp.StatusCode, "Antigravity: "+http.StatusText(resp.StatusCode))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !action {
		writeAgyModels(w, resp.Body, model)
		return
	}
	if stream {
		writeAgyStream(w, resp.Body)
		return
	}
	var frame struct{ Response json.RawMessage }
	if json.NewDecoder(resp.Body).Decode(&frame) != nil || len(frame.Response) == 0 {
		agyError(w, 502, "Invalid Antigravity response")
		return
	}
	w.Write(frame.Response)
}

func writeAgyModels(w http.ResponseWriter, body io.Reader, model string) {
	var result struct {
		Models map[string]struct{ DisplayName string }
	}
	if json.NewDecoder(body).Decode(&result) != nil || result.Models == nil {
		agyError(w, 502, "Invalid Antigravity model list")
		return
	}
	models := make([]agyModel, 0, len(result.Models))
	for name, meta := range result.Models {
		m := agyModel{"models/" + name, meta.DisplayName, []string{"generateContent", "streamGenerateContent"}}
		if model == name {
			json.NewEncoder(w).Encode(m)
			return
		}
		models = append(models, m)
	}
	if model != "" {
		agyError(w, 404, "Unknown model")
		return
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	json.NewEncoder(w).Encode(map[string]any{"models": models})
	return
}

func writeAgyStream(w http.ResponseWriter, body io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	started := false
	err := util.IterSSEEvents(body, func(event util.SSEEvent) error {
		var frame struct{ Response json.RawMessage }
		if json.Unmarshal([]byte(event.Data), &frame) != nil || len(frame.Response) == 0 {
			return errors.New("invalid stream event")
		}
		compact, err := json.Marshal(frame.Response)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", compact); err != nil {
			return err
		}
		started = true
		return http.NewResponseController(w).Flush()
	})
	if err != nil || !started {
		if !started {
			agyError(w, 502, "Invalid Antigravity stream")
		} else {
			fmt.Fprint(w, "data: {\"error\":{\"code\":502,\"message\":\"Antigravity stream interrupted\"}}\n\n")
		}
	}
	return
}

// QueryUsage fetches Antigravity subscription quota pools on demand from
// v1internal:fetchAvailableModels. Usage is never polled automatically.
func (a *AgyProvider) QueryUsage(ctx context.Context) ([]UsageWindow, error) {
	rep, err := a.QueryUsageReport(ctx)
	if err != nil {
		return nil, err
	}
	return rep.Windows, nil
}

// QueryUsageReport fetches Antigravity quota pools plus subscription tier and
// per-pool model lists.
func (a *AgyProvider) QueryUsageReport(ctx context.Context) (UsageReport, error) {
	login, err := a.credentials(ctx)
	if err != nil {
		return UsageReport{}, err
	}
	payload := map[string]any{}
	if login.Project != "" {
		payload["project"] = login.Project
	}

	var (
		tierPlan    string
		tierProject string
		cliModels   []agyCLIModel
		wg          sync.WaitGroup
	)
	// Best-effort lookup of plan tier (e.g. Google AI Pro) in parallel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		resp, err := a.call(tctx, login, "loadCodeAssist", map[string]any{
			"metadata": map[string]string{"ideType": "ANTIGRAVITY", "pluginType": "GEMINI"},
		})
		if err != nil || resp == nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return
		}
		var info struct {
			CurrentTier *struct {
				Name string `json:"name"`
			} `json:"currentTier"`
			PaidTier *struct {
				Name string `json:"name"`
			} `json:"paidTier"`
			Project json.RawMessage `json:"cloudaicompanionProject"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 512<<10)).Decode(&info) != nil {
			return
		}
		if info.PaidTier != nil && info.PaidTier.Name != "" {
			tierPlan = info.PaidTier.Name
		} else if info.CurrentTier != nil && info.CurrentTier.Name != "" {
			tierPlan = info.CurrentTier.Name
		}
		var proj string
		if json.Unmarshal(info.Project, &proj) != nil {
			var p struct{ ID string }
			_ = json.Unmarshal(info.Project, &p)
			proj = p.ID
		}
		tierProject = proj
	}()

	// Best-effort lookup of the exact model catalog shown by `agy models`.
	if a.listModels != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			if list, err := a.listModels(mctx); err == nil && len(list) > 0 {
				cliModels = list
			}
		}()
	}

	resp, err := a.call(ctx, login, "fetchAvailableModels", payload)
	wg.Wait()
	if err != nil {
		return UsageReport{}, fmt.Errorf("Antigravity 用量查询失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UsageReport{}, fmt.Errorf("usage HTTP %d: %s", resp.StatusCode, util.Truncate(string(body), 300))
	}
	windows, totalModels, ok := parseAgyUsageDetailed(body)
	if !ok {
		return UsageReport{}, errors.New("usage: 未在 Antigravity 响应中找到配额信息")
	}
	if len(cliModels) > 0 {
		applyAgyCLIModels(windows, cliModels)
		totalModels = len(cliModels)
	}

	status := "正常可用"
	exhausted := 0
	for _, w := range windows {
		if w.Utilization >= 1 {
			exhausted++
		}
	}
	if exhausted == len(windows) {
		status = "已达限额"
	} else if exhausted > 0 {
		status = "部分达限"
	}

	var extras []UsageExtra
	if totalModels > 0 {
		extras = append(extras, UsageExtra{Label: "可用模型", Value: fmt.Sprintf("%d 个", totalModels), Tone: "ok"})
	}
	proj := login.Project
	if proj == "" {
		proj = tierProject
	}
	if proj != "" {
		extras = append(extras, UsageExtra{Label: "项目", Value: proj})
	}
	if login.Method != "" {
		extras = append(extras, UsageExtra{Label: "认证", Value: login.Method})
	}

	return UsageReport{
		Plan:    tierPlan,
		Account: login.Account,
		Status:  status,
		Extras:  extras,
		Windows: windows,
	}, nil
}

type agyQuotaInfo struct {
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

type agyModelMeta struct {
	DisplayName string        `json:"displayName"`
	Recommended bool          `json:"recommended"`
	IsInternal  bool          `json:"isInternal"`
	QuotaInfo   *agyQuotaInfo `json:"quotaInfo"`
}

func parseAgyUsage(body []byte) ([]UsageWindow, bool) {
	w, _, ok := parseAgyUsageDetailed(body)
	return w, ok
}

func parseAgyUsageDetailed(body []byte) ([]UsageWindow, int, bool) {
	var raw struct {
		Models map[string]agyModelMeta `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.Models) == 0 {
		return nil, 0, false
	}

	type poolBucket struct {
		key         string
		utilization float64
		resetsAt    int64
		families    map[string]bool
		models      map[string]bool
	}
	buckets := map[string]*poolBucket{}
	visibleCount := 0

	for name, meta := range raw.Models {
		if meta.IsInternal || strings.HasPrefix(name, "tab_") {
			continue
		}
		if meta.DisplayName == "" && !meta.Recommended {
			continue
		}
		if meta.QuotaInfo == nil {
			continue
		}
		// Protobuf JSON omits remainingFraction when it is 0.0 (fully exhausted).
		var rem float64
		if meta.QuotaInfo.RemainingFraction != nil {
			rem = *meta.QuotaInfo.RemainingFraction
		} else if meta.QuotaInfo.ResetTime == "" {
			continue
		}
		visibleCount++
		rem = math.Max(0, math.Min(1, rem))
		utilFrac := math.Max(0, math.Min(1, 1-rem))

		var resetsAt int64
		if meta.QuotaInfo.ResetTime != "" {
			if t, err := time.Parse(time.RFC3339, meta.QuotaInfo.ResetTime); err == nil {
				resetsAt = t.Unix()
			}
		}

		// Group models sharing the same reset time and remaining fraction into one quota pool.
		key := fmt.Sprintf("%d:%.4f", resetsAt, rem)
		b := buckets[key]
		if b == nil {
			b = &poolBucket{
				key:         key,
				utilization: utilFrac,
				resetsAt:    resetsAt,
				families:    map[string]bool{},
				models:      map[string]bool{},
			}
			buckets[key] = b
		}
		b.families[agyModelFamily(name, meta.DisplayName)] = true
		b.models[agyModelDisplayLabel(name, meta.DisplayName)] = true
	}

	if len(buckets) == 0 {
		return nil, 0, false
	}

	windows := make([]UsageWindow, 0, len(buckets))
	for _, b := range buckets {
		fams := make([]string, 0, len(b.families))
		for f := range b.families {
			fams = append(fams, f)
		}
		sort.Strings(fams)
		// Collapse "Gemini Flash" + "Gemini Pro" into "Gemini" when they share one pool.
		fams = compactAgyFamilies(fams)
		models := make([]string, 0, len(b.models))
		for m := range b.models {
			models = append(models, m)
		}
		sort.Strings(models)
		label := strings.Join(fams, " / ") + " 配额"
		windows = append(windows, UsageWindow{
			Label:       label,
			Utilization: b.utilization,
			ResetsAt:    b.resetsAt,
			Group:       "共享池",
			Detail:      fmt.Sprintf("%d 个模型共享", len(models)),
			Models:      models,
		})
	}
	sort.Slice(windows, func(i, j int) bool {
		return windows[i].Label < windows[j].Label
	})
	return windows, visibleCount, true
}

// agyModelDisplayLabel collapses thinking-effort suffixes like "(High)/(Medium)/(Low)/(XHigh)"
// so the pool model chips stay crisp and readable.
func agyModelDisplayLabel(id, display string) string {
	name := strings.TrimSpace(display)
	if name == "" {
		return id
	}
	for _, suf := range []string{" (XHigh)", " (High)", " (Medium)", " (Low)"} {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf)
		}
	}
	return name
}

func fetchAgyCLIModels(ctx context.Context) ([]agyCLIModel, error) {
	out, err := exec.CommandContext(ctx, "agy", "models").Output()
	if err != nil {
		return nil, err
	}
	return parseAgyCLIModels(string(out)), nil
}

func parseAgyCLIModels(output string) []agyCLIModel {
	lines := strings.Split(output, "\n")
	models := make([]agyCLIModel, 0, len(lines))
	for _, raw := range lines {
		// Strip spinner carriage-return prefixes if any.
		if idx := strings.LastIndexByte(raw, '\r'); idx >= 0 {
			raw = raw[idx+1:]
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "Fetching ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		id := fields[0]
		display := strings.TrimSpace(strings.TrimPrefix(line, id))
		if id == "" || display == "" {
			continue
		}
		models = append(models, agyCLIModel{ID: id, DisplayName: display})
	}
	return models
}

// applyAgyCLIModels replaces raw backend model names inside each quota pool
// with the exact deduplicated model names reported by `agy models`.
func applyAgyCLIModels(windows []UsageWindow, cliModels []agyCLIModel) {
	if len(windows) == 0 || len(cliModels) == 0 {
		return
	}
	for i := range windows {
		lowLabel := strings.ToLower(windows[i].Label)
		wantsGemini := strings.Contains(lowLabel, "gemini")
		wantsGeminiFlash := strings.Contains(lowLabel, "gemini flash")
		wantsGeminiPro := strings.Contains(lowLabel, "gemini pro")
		wantsClaude := strings.Contains(lowLabel, "claude")
		wantsGPT := strings.Contains(lowLabel, "gpt")

		seen := map[string]bool{}
		matched := make([]string, 0, len(cliModels))
		for _, m := range cliModels {
			low := strings.ToLower(m.ID + " " + m.DisplayName)
			hit := false
			switch {
			case strings.Contains(low, "claude"):
				hit = wantsClaude
			case strings.Contains(low, "gpt"):
				hit = wantsGPT
			case strings.Contains(low, "gemini") || strings.Contains(low, "orchestrator"):
				if wantsGeminiFlash {
					hit = strings.Contains(low, "flash")
				} else if wantsGeminiPro {
					hit = !strings.Contains(low, "flash")
				} else {
					hit = wantsGemini
				}
			}
			if !hit {
				continue
			}
			label := agyModelDisplayLabel(m.ID, m.DisplayName)
			if !seen[label] {
				seen[label] = true
				matched = append(matched, label)
			}
		}
		if len(matched) > 0 {
			windows[i].Models = matched
			windows[i].Detail = fmt.Sprintf("%d 个模型共享", len(matched))
		}
	}
}

func agyModelFamily(id, display string) string {
	low := strings.ToLower(id + " " + display)
	switch {
	case strings.Contains(low, "claude"):
		return "Claude"
	case strings.Contains(low, "gpt"):
		return "GPT"
	case strings.Contains(low, "gemini"):
		if strings.Contains(low, "pro") {
			return "Gemini Pro"
		}
		if strings.Contains(low, "flash") {
			return "Gemini Flash"
		}
		return "Gemini"
	default:
		if display != "" {
			return display
		}
		return id
	}
}

func compactAgyFamilies(fams []string) []string {
	hasGemini := false
	geminiCount := 0
	for _, f := range fams {
		if f == "Gemini" || strings.HasPrefix(f, "Gemini ") {
			hasGemini = true
			geminiCount++
		}
	}
	if !hasGemini || geminiCount <= 1 {
		return fams
	}
	out := make([]string, 0, len(fams))
	addedGemini := false
	for _, f := range fams {
		if f == "Gemini" || strings.HasPrefix(f, "Gemini ") {
			if !addedGemini {
				out = append(out, "Gemini")
				addedGemini = true
			}
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
