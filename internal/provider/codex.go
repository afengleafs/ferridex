package provider

// CodexProvider relays to the ChatGPT/Codex backend used by the Codex CLI,
// authenticating with the local ChatGPT subscription login (~/.codex/auth.json).
// The relay logic is ported from David-Factor/codex-responses-proxy.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ferridex/internal/util"
)

const (
	codexBaseURL      = "https://chatgpt.com/backend-api/codex"
	codexResponsesURL = codexBaseURL + "/responses"
	codexUsageURL     = "https://chatgpt.com/backend-api/wham/usage"
	codexRefreshURL   = "https://auth.openai.com/oauth/token"
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexRefreshSkew  = 30 * time.Second

	// codexFallbackOriginator/Version impersonate a recent Codex CLI for
	// downstream clients that don't identify themselves. The backend picks the
	// available model table from originator + client version; without them it
	// serves a legacy table and gpt-5.6-* returns 404 "Model not found".
	codexFallbackOriginator = "codex_cli_rs"
	codexFallbackVersion    = "0.144.1"

)

var codexAuthMu sync.Mutex

type CodexProvider struct {
	instructions string
	renameMap    map[string]string
	client       *http.Client

	// on-demand subscription usage query (dashboard button); seam for tests.
	usageFetcher func(context.Context) ([]UsageWindow, error)
}

func NewCodexProvider() *CodexProvider {
	c := &CodexProvider{
		instructions: "You are a helpful coding assistant.",
		renameMap:    map[string]string{"web_search_preview": "web_search"},
		client:       &http.Client{Timeout: 10 * time.Minute},
	}
	c.usageFetcher = c.fetchUsage
	return c
}

func (c *CodexProvider) Name() string { return "codex" }

func (c *CodexProvider) Status() ProviderStatus {
	st := ProviderStatus{Name: "codex", Title: "ChatGPT · Codex", Endpoint: "/v1/responses", Models: []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5"}}
	path, err := codexAuthPath()
	if err != nil {
		st.Detail = "未找到 ~/.codex/auth.json(请先 codex login)"
		return st
	}
	doc, err := readAuthDoc(path)
	if err != nil {
		st.Detail = "读取 auth.json 失败"
		return st
	}
	if util.GetString(doc, "auth_mode") != "chatgpt" {
		st.Detail = "auth_mode 非 chatgpt(需订阅登录)"
		return st
	}
	tokens, _ := util.GetMap(doc, "tokens")
	if util.GetString(tokens, "access_token") == "" {
		st.Detail = "无 access_token"
		return st
	}
	acct := util.GetString(tokens, "account_id")
	if acct == "" {
		acct = extractAccountID(util.GetString(tokens, "id_token"), util.GetString(tokens, "access_token"))
	}
	st.LoggedIn = true
	st.Account = acct
	st.Detail = "ChatGPT 订阅"
	return st
}

func (c *CodexProvider) Relay(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	downstreamWantsStream := requestWantsStream(rawBody)
	patchedBody, fallbackModel := patchPayloadForCodex(rawBody, c.instructions, c.renameMap)

	token, accountID, err := borrowCodexKey(r.Context())
	if err != nil {
		http.Error(w, "failed to borrow Codex auth: "+err.Error(), http.StatusUnauthorized)
		return
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, codexResponsesURL, bytes.NewReader(patchedBody))
	if err != nil {
		http.Error(w, "failed to create upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream")
	upstreamReq.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		upstreamReq.Header.Set("ChatGPT-Account-ID", accountID)
	}
	setCodexClientHeaders(r.Header, upstreamReq.Header)

	upstreamResp, err := c.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstreamResp.Body.Close()

	contentType := upstreamResp.Header.Get("Content-Type")
	if upstreamResp.StatusCode < 200 || upstreamResp.StatusCode >= 300 {
		body, _ := io.ReadAll(upstreamResp.Body)
		log.Printf("codex upstream HTTP %d: %s", upstreamResp.StatusCode, util.Truncate(string(body), 2000))
		w.Header().Set("Content-Type", util.FirstNonEmpty(contentType, "application/json"))
		w.WriteHeader(upstreamResp.StatusCode)
		_, _ = w.Write(body)
		return
	}

	// Codex normally requires stream=true and returns SSE; if it ever returns
	// completed JSON, pass it through.
	if strings.Contains(strings.ToLower(contentType), "application/json") {
		body, _ := io.ReadAll(upstreamResp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	if downstreamWantsStream {
		if _, ok := w.(http.Flusher); !ok {
			http.Error(w, "streaming response requires http.Flusher", http.StatusInternalServerError)
			return
		}
		if err := streamSSEToResponses(w, upstreamResp.Body, fallbackModel); err != nil {
			log.Printf("codex stream error: %v", err)
		}
		return
	}

	responseJSON, err := convertSSEToResponsesJSON(upstreamResp.Body, fallbackModel)
	if err != nil {
		http.Error(w, "failed to convert Codex stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseJSON)
}

// --- request patching ---

func requestWantsStream(raw []byte) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	return payload.Stream
}

// setCodexClientHeaders forwards the downstream client's identity headers
// upstream. The Codex backend needs originator plus a client version (the
// `version` header or a codex_cli_rs User-Agent) to serve the current model
// table; bare clients get impersonated as a recent Codex CLI so gpt-5.6-*
// doesn't 404 with "Model not found".
func setCodexClientHeaders(in, out http.Header) {
	for _, k := range []string{"originator", "version", "session_id", "User-Agent", "OpenAI-Beta"} {
		if v := in.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	if out.Get("originator") == "" {
		out.Set("originator", codexFallbackOriginator)
	}
	if out.Get("version") == "" {
		out.Set("version", codexFallbackVersion)
	}
}

func patchPayloadForCodex(raw []byte, defaultInstructions string, renameMap map[string]string) ([]byte, string) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return raw, ""
	}
	model, _ := payload["model"].(string)

	payload["stream"] = true // Codex backend requires streaming
	if _, ok := payload["store"]; !ok {
		payload["store"] = false
	}
	if _, ok := payload["instructions"]; !ok {
		payload["instructions"] = defaultInstructions
	}
	delete(payload, "max_output_tokens")
	delete(payload, "max_completion_tokens")
	renameTools(payload, renameMap)

	next, err := json.Marshal(payload)
	if err != nil {
		return raw, model
	}
	return next, model
}

func renameTools(payload map[string]any, renameMap map[string]string) {
	if len(renameMap) == 0 {
		return
	}
	tools, ok := payload["tools"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(tools))
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			filtered = append(filtered, t)
			continue
		}
		typ, _ := tool["type"].(string)
		newType, found := renameMap[typ]
		if !found {
			filtered = append(filtered, t)
			continue
		}
		if newType == "" {
			continue
		}
		tool["type"] = newType
		filtered = append(filtered, t)
	}
	if len(filtered) == 0 {
		delete(payload, "tools")
	} else {
		payload["tools"] = filtered
	}
}

// --- SSE → Responses translation ---

type codexStreamEvent struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
	Item     json.RawMessage `json:"item,omitempty"`
}

type responseProbe struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Status    string            `json:"status"`
	Model     string            `json:"model"`
	Output    []json.RawMessage `json:"output"`
	Usage     json.RawMessage   `json:"usage"`
	Error     json.RawMessage   `json:"error"`
}

type syntheticResponsesResponse struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Status    string            `json:"status"`
	Model     string            `json:"model"`
	Output    []json.RawMessage `json:"output"`
	Usage     json.RawMessage   `json:"usage"`
	Error     any               `json:"error"`
}

type syntheticMessageItem struct {
	ID      string                    `json:"id"`
	Type    string                    `json:"type"`
	Role    string                    `json:"role"`
	Status  string                    `json:"status"`
	Content []syntheticMessageContent `json:"content"`
}

type syntheticMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type streamingResponseState struct {
	fallbackModel  string
	textBuilder    strings.Builder
	outputItems    []json.RawMessage
	sawMessageItem bool
}

func parseCodexStreamEvent(ev util.SSEEvent) (codexStreamEvent, string, bool) {
	var streamEvent codexStreamEvent
	data := strings.TrimSpace(ev.Data)
	if err := json.Unmarshal([]byte(data), &streamEvent); err != nil {
		return streamEvent, ev.EventType, false
	}
	return streamEvent, util.FirstNonEmpty(streamEvent.Type, ev.EventType), true
}

func (s *streamingResponseState) rememberOutputItem(item json.RawMessage) {
	if len(item) == 0 {
		return
	}
	s.outputItems = append(s.outputItems, append(json.RawMessage(nil), item...))
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(item, &probe); err == nil && probe.Type == "message" {
		s.sawMessageItem = true
	}
}

func (s *streamingResponseState) emitTerminalEvent(emit func(string, string) error, eventType string, response json.RawMessage) error {
	if responseHasOutput(response) {
		return emit(eventType, util.MarshalJSONLine(map[string]any{"type": eventType, "response": json.RawMessage(response)}))
	}
	finalOutput := make([]json.RawMessage, 0, len(s.outputItems)+1)
	finalOutput = append(finalOutput, s.outputItems...)
	if s.textBuilder.Len() > 0 && !s.sawMessageItem {
		messageItem := util.MustMarshalRaw(syntheticMessageItem{
			ID: "msg_proxy_0", Type: "message", Role: "assistant", Status: "completed",
			Content: []syntheticMessageContent{{Type: "output_text", Text: s.textBuilder.String()}},
		})
		finalOutput = append(finalOutput, messageItem)
		if err := emit("response.output_item.done", util.MarshalJSONLine(map[string]any{
			"type": "response.output_item.done", "output_index": len(finalOutput) - 1, "item": messageItem,
		})); err != nil {
			return err
		}
	}
	if len(finalOutput) == 0 {
		return emit(eventType, util.MarshalJSONLine(map[string]any{"type": eventType, "response": json.RawMessage(response)}))
	}
	patched := ensureResponseOutput(response, finalOutput, s.fallbackModel)
	return emit(eventType, util.MarshalJSONLine(map[string]any{"type": eventType, "response": json.RawMessage(patched)}))
}

func ensureResponseOutput(raw json.RawMessage, output []json.RawMessage, fallbackModel string) json.RawMessage {
	var response map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &response) != nil {
		response = make(map[string]any)
	}
	now := time.Now().Unix()
	if util.GetString(response, "id") == "" {
		response["id"] = fmt.Sprintf("resp_proxy_%d", now)
	}
	if util.GetString(response, "object") == "" {
		response["object"] = "response"
	}
	if _, ok := response["created_at"]; !ok {
		response["created_at"] = now
	}
	if util.GetString(response, "status") == "" {
		response["status"] = "completed"
	}
	if util.GetString(response, "model") == "" {
		response["model"] = fallbackModel
	}
	if v, ok := response["usage"]; !ok || v == nil {
		response["usage"] = map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	if _, ok := response["error"]; !ok {
		response["error"] = nil
	}
	response["output"] = output
	return util.MustMarshalRaw(response)
}

func streamSSEToResponses(w http.ResponseWriter, body io.Reader, fallbackModel string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	state := &streamingResponseState{fallbackModel: fallbackModel}

	emit := func(eventType string, data string) error {
		if eventType != "" {
			if _, err := fmt.Fprintf(w, "event: %s\n", eventType); err != nil {
				return err
			}
		}
		for _, line := range strings.Split(data, "\n") {
			if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	err := util.IterSSEEvents(body, func(ev util.SSEEvent) error {
		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			return nil
		}
		streamEvent, eventType, parsed := parseCodexStreamEvent(ev)
		if parsed {
			switch eventType {
			case "response.output_text.delta":
				state.textBuilder.WriteString(streamEvent.Delta)
			case "response.output_item.done":
				state.rememberOutputItem(streamEvent.Item)
			case "response.completed", "response.incomplete":
				return state.emitTerminalEvent(emit, eventType, streamEvent.Response)
			case "response.failed", "error":
				return emit(eventType, data)
			}
		}
		return emit(util.FirstNonEmpty(eventType, ev.EventType), data)
	})
	if err != nil {
		_ = emit("error", util.MarshalJSONLine(map[string]any{"type": "error", "message": "failed to read upstream stream: " + err.Error()}))
		return err
	}
	return nil
}

func convertSSEToResponsesJSON(body io.Reader, fallbackModel string) ([]byte, error) {
	var textBuilder strings.Builder
	var completedRaw json.RawMessage
	var completed responseProbe
	var upstreamError json.RawMessage
	var nonMessageItems []json.RawMessage
	var messageItem json.RawMessage

	err := util.IterSSEEvents(body, func(ev util.SSEEvent) error {
		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			return nil
		}
		var streamEvent codexStreamEvent
		if err := json.Unmarshal([]byte(data), &streamEvent); err != nil {
			return nil
		}
		switch streamEvent.Type {
		case "response.output_text.delta":
			textBuilder.WriteString(streamEvent.Delta)
		case "response.output_item.done":
			if len(streamEvent.Item) == 0 {
				return nil
			}
			var probe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(streamEvent.Item, &probe); err != nil {
				return nil
			}
			switch probe.Type {
			case "message":
				messageItem = append(json.RawMessage(nil), streamEvent.Item...)
			default:
				nonMessageItems = append(nonMessageItems, append(json.RawMessage(nil), streamEvent.Item...))
			}
		case "response.completed":
			if len(streamEvent.Response) > 0 {
				completedRaw = append(json.RawMessage(nil), streamEvent.Response...)
				_ = json.Unmarshal(streamEvent.Response, &completed)
			}
		case "response.failed", "response.incomplete":
			if len(streamEvent.Response) > 0 {
				completedRaw = append(json.RawMessage(nil), streamEvent.Response...)
				_ = json.Unmarshal(streamEvent.Response, &completed)
				if len(completed.Error) > 0 && string(completed.Error) != "null" {
					upstreamError = append(json.RawMessage(nil), completed.Error...)
				}
			}
		case "error":
			upstreamError = append(json.RawMessage(nil), []byte(data)...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Best case: completed already has full output — return unchanged.
	if responseHasOutput(completedRaw) {
		return completedRaw, nil
	}

	now := time.Now().Unix()
	if completed.ID == "" {
		completed.ID = fmt.Sprintf("resp_proxy_%d", now)
	}
	if completed.Object == "" {
		completed.Object = "response"
	}
	if completed.CreatedAt == 0 {
		completed.CreatedAt = now
	}
	if completed.Status == "" {
		completed.Status = "completed"
	}
	if completed.Model == "" {
		completed.Model = fallbackModel
	}
	if len(completed.Usage) == 0 || string(completed.Usage) == "null" {
		completed.Usage = json.RawMessage(`{"input_tokens":0,"output_tokens":0,"total_tokens":0}`)
	}

	var output []json.RawMessage
	if textBuilder.Len() > 0 {
		output = append(output, util.MustMarshalRaw(syntheticMessageItem{
			ID: "msg_proxy_0", Type: "message", Role: "assistant", Status: "completed",
			Content: []syntheticMessageContent{{Type: "output_text", Text: textBuilder.String()}},
		}))
	} else if len(messageItem) > 0 {
		output = append(output, messageItem)
	}
	output = append(output, nonMessageItems...)

	if len(upstreamError) > 0 {
		return nil, fmt.Errorf("Codex stream ended with error: %s", util.Truncate(string(upstreamError), 1000))
	}
	if completed.Status == "failed" || completed.Status == "incomplete" || completed.Status == "cancelled" {
		return nil, fmt.Errorf("Codex stream ended with status %q and no output", completed.Status)
	}
	if textBuilder.Len() == 0 && len(messageItem) == 0 && len(nonMessageItems) == 0 {
		return nil, errors.New("Codex stream completed without output")
	}

	return json.Marshal(syntheticResponsesResponse{
		ID: completed.ID, Object: completed.Object, CreatedAt: completed.CreatedAt,
		Status: completed.Status, Model: completed.Model, Output: output, Usage: completed.Usage, Error: nil,
	})
}

func responseHasOutput(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var probe struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return len(probe.Output) > 0
}

// --- auth: read ~/.codex/auth.json, refresh access token as needed ---

type codexTokenRefresh struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

func borrowCodexKey(ctx context.Context) (accessToken string, accountID string, err error) {
	codexAuthMu.Lock()
	defer codexAuthMu.Unlock()

	authPath, err := codexAuthPath()
	if err != nil {
		return "", "", err
	}
	auth, err := readAuthDoc(authPath)
	if err != nil {
		return "", "", err
	}
	if util.GetString(auth, "auth_mode") != "chatgpt" {
		return "", "", fmt.Errorf("expected auth_mode 'chatgpt', got %q; run `codex login`", util.GetString(auth, "auth_mode"))
	}
	tokens, ok := util.GetMap(auth, "tokens")
	if !ok {
		return "", "", errors.New("no tokens object found in auth.json; run `codex login`")
	}

	accessToken = util.GetString(tokens, "access_token")
	refreshToken := util.GetString(tokens, "refresh_token")
	idToken := util.GetString(tokens, "id_token")
	accountID = util.GetString(tokens, "account_id")
	if accountID == "" {
		accountID = extractAccountID(idToken, accessToken)
		if accountID != "" {
			tokens["account_id"] = accountID
		}
	}
	if accessToken == "" {
		return "", "", errors.New("no access_token found; run `codex login`")
	}

	if expiry, ok := util.JWTExpiry(accessToken); ok && time.Now().Before(expiry.Add(-codexRefreshSkew)) {
		if accountID != "" {
			auth["tokens"] = tokens
			_ = writeAuthDoc(authPath, auth)
		}
		return accessToken, accountID, nil
	}

	if refreshToken == "" {
		return "", "", errors.New("access token expired and no refresh_token; run `codex login`")
	}
	refreshed, err := refreshCodexToken(ctx, refreshToken)
	if err != nil {
		return "", "", err
	}
	if refreshed.AccessToken != "" {
		tokens["access_token"] = refreshed.AccessToken
		accessToken = refreshed.AccessToken
	}
	if refreshed.RefreshToken != "" {
		tokens["refresh_token"] = refreshed.RefreshToken
	}
	if refreshed.IDToken != "" {
		tokens["id_token"] = refreshed.IDToken
		idToken = refreshed.IDToken
	}
	if accountID == "" {
		accountID = extractAccountID(idToken, accessToken)
		if accountID != "" {
			tokens["account_id"] = accountID
		}
	}
	auth["tokens"] = tokens
	auth["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeAuthDoc(authPath, auth); err != nil {
		return "", "", err
	}
	return accessToken, accountID, nil
}

func codexAuthPath() (string, error) {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		codexHome = filepath.Join(home, ".codex")
	}
	path := filepath.Join(codexHome, "auth.json")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("Codex auth file not found at %s", path)
	}
	return path, nil
}

func readAuthDoc(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func writeAuthDoc(path string, doc map[string]any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func refreshCodexToken(ctx context.Context, refreshToken string) (*codexTokenRefresh, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     codexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexRefreshURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh network error: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token refresh failed: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var refreshed codexTokenRefresh
	if err := json.Unmarshal(respBody, &refreshed); err != nil {
		return nil, fmt.Errorf("failed to parse token refresh response: %w", err)
	}
	if refreshed.AccessToken == "" {
		return nil, errors.New("token refresh response did not include access_token")
	}
	return &refreshed, nil
}

func extractAccountID(idToken, accessToken string) string {
	for _, token := range []string{idToken, accessToken} {
		if token == "" {
			continue
		}
		claims, ok := util.ParseJWTClaims(token)
		if !ok {
			continue
		}
		if v, ok := claims["chatgpt_account_id"].(string); ok && v != "" {
			return v
		}
		if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			if v, ok := nested["chatgpt_account_id"].(string); ok && v != "" {
				return v
			}
		}
		if orgs, ok := claims["organizations"].([]any); ok && len(orgs) > 0 {
			if first, ok := orgs[0].(map[string]any); ok {
				if v, ok := first["id"].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// --- subscription usage (ChatGPT/Codex usage endpoint, mirrors the Claude path) ---
//
// The ChatGPT backend exposes the account's rate-limit windows at /backend-api/
// wham/usage (primary ≈ 5h, secondary ≈ weekly), the same data the Codex CLI shows.

// QueryUsage fetches the usage windows on demand — only when the dashboard's
// 查询用量 button is clicked for this provider; nothing is cached or polled.
func (c *CodexProvider) QueryUsage(ctx context.Context) ([]UsageWindow, error) {
	return c.usageFetcher(ctx)
}

func (c *CodexProvider) fetchUsage(ctx context.Context) ([]UsageWindow, error) {
	token, accountID, err := codexUsageCreds()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("usage HTTP %d: %s", resp.StatusCode, util.Truncate(string(body), 300))
	}
	windows, ok := parseCodexUsage(body)
	if !ok {
		return nil, errors.New("usage: unrecognized response shape")
	}
	return windows, nil
}

// codexUsageCreds reads the access token + account id from auth.json without
// mutating it (refresh is left to the relay path; an expired token just 401s and
// the dashboard degrades to no usage bars until the next real request).
func codexUsageCreds() (token, accountID string, err error) {
	authPath, err := codexAuthPath()
	if err != nil {
		return "", "", err
	}
	auth, err := readAuthDoc(authPath)
	if err != nil {
		return "", "", err
	}
	tokens, ok := util.GetMap(auth, "tokens")
	if !ok {
		return "", "", errors.New("no tokens object in auth.json")
	}
	token = util.GetString(tokens, "access_token")
	if token == "" {
		return "", "", errors.New("no access_token in auth.json")
	}
	accountID = util.GetString(tokens, "account_id")
	if accountID == "" {
		accountID = extractAccountID(util.GetString(tokens, "id_token"), token)
	}
	return token, accountID, nil
}

type codexUsageWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetAt     int64   `json:"reset_at"`
}

// parseCodexUsage maps the wham/usage response into display windows. used_percent
// is a 0..100 percentage; reset_at is a unix timestamp (seconds, sometimes ms).
func parseCodexUsage(body []byte) ([]UsageWindow, bool) {
	var doc struct {
		RateLimit struct {
			Primary   *codexUsageWindow `json:"primary_window"`
			Secondary *codexUsageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil, false
	}
	out := make([]UsageWindow, 0, 2)
	if w := doc.RateLimit.Primary; w != nil {
		out = append(out, w.usageWindow("5 小时"))
	}
	if w := doc.RateLimit.Secondary; w != nil {
		out = append(out, w.usageWindow("每周"))
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func (w codexUsageWindow) usageWindow(label string) UsageWindow {
	frac := w.UsedPercent / 100
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	reset := w.ResetAt
	if reset > 1e11 { // milliseconds
		reset /= 1000
	}
	if reset < 0 {
		reset = 0
	}
	return UsageWindow{Label: label, Utilization: frac, ResetsAt: reset}
}
