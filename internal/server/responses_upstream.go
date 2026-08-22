package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ferridex/internal/provider"
)

const responsesUpstreamBodyLimit = 64 << 10

// responsesUpstreamManager serializes dashboard mutations so the persisted
// configuration and the live routing decision move together.
type responsesUpstreamManager struct {
	mu           sync.Mutex
	runtime      *provider.ResponsesProvider
	subscription provider.Provider
	persist      func(func(*config) error) error
}

func configureResponsesProviders(providers ...provider.Provider) ([]provider.Provider, *responsesUpstreamManager) {
	cfg, err := readConfigFile(configPath())
	if err != nil {
		log.Printf("无法读取 Responses 上游配置,Codex 路由将保持关闭以避免误用本机订阅: %v", err)
		cfg = config{ResponsesSource: provider.ResponsesSourceCustom}
	}
	return configureResponsesProvidersWithConfig(cfg, updateConfig, providers...)
}

func configureResponsesProvidersWithConfig(cfg config, persist func(func(*config) error) error, providers ...provider.Provider) ([]provider.Provider, *responsesUpstreamManager) {
	configured := append([]provider.Provider(nil), providers...)

	var manager *responsesUpstreamManager
	for i, current := range configured {
		if current.Name() != "codex" {
			continue
		}
		if runtime, ok := current.(*provider.ResponsesProvider); ok {
			manager = &responsesUpstreamManager{runtime: runtime, persist: persist}
			break
		}

		runtime := provider.NewResponsesProvider(current)
		source := cfg.ResponsesSource
		loadCustom := true
		switch source {
		case "":
			source = provider.ResponsesSourceSubscription
		case provider.ResponsesSourceSubscription, provider.ResponsesSourceCustom:
		default:
			log.Printf("无效的已保存 Responses 上游选择 %q;Codex 路由将保持关闭", source)
			source = provider.ResponsesSourceCustom
			loadCustom = false
		}
		if loadCustom && customResponsesConfigPresent(cfg.CustomResponses) {
			if err := runtime.SaveCustom(cfg.CustomResponses); err != nil {
				log.Printf("忽略无效的自定义 Responses 上游配置: %v", err)
			}
		}
		if err := runtime.RestoreSource(source); err != nil {
			log.Printf("已保存的 Responses 上游 %q 当前不可用;保留该选择并拒绝请求,不会回退本机订阅: %v", source, err)
		}

		configured[i] = runtime
		manager = &responsesUpstreamManager{
			runtime:      runtime,
			subscription: current,
			persist:      persist,
		}
		break
	}
	return configured, manager
}

func customResponsesConfigPresent(cfg provider.CustomResponsesConfig) bool {
	return cfg.Name != "" || cfg.BaseURL != "" || cfg.APIKey != "" || cfg.DefaultModel != ""
}

type responsesSubscriptionView struct {
	Available    bool   `json:"available"`
	LoggedIn     bool   `json:"logged_in"`
	Title        string `json:"title,omitempty"`
	Detail       string `json:"detail,omitempty"`
	DefaultModel string `json:"default_model,omitempty"`
}

type customResponsesView struct {
	Configured   bool   `json:"configured"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	DefaultModel string `json:"default_model"`
	HasAPIKey    bool   `json:"has_api_key"`
}

type responsesUpstreamView struct {
	ActiveSource provider.ResponsesSource  `json:"active_source"`
	Subscription responsesSubscriptionView `json:"subscription"`
	Custom       customResponsesView       `json:"custom"`
}

func (m *responsesUpstreamManager) view() responsesUpstreamView {
	snapshot := m.runtime.Snapshot()
	view := responsesUpstreamView{
		ActiveSource: snapshot.ActiveSource,
		Custom: customResponsesView{
			Configured:   snapshot.Configured,
			Name:         snapshot.Custom.Name,
			BaseURL:      snapshot.Custom.BaseURL,
			DefaultModel: snapshot.Custom.DefaultModel,
			HasAPIKey:    snapshot.HasAPIKey,
		},
	}
	if m.subscription != nil {
		status := m.subscription.Status()
		view.Subscription = responsesSubscriptionView{
			Available: true,
			LoggedIn:  status.LoggedIn,
			Title:     status.Title,
			Detail:    status.Detail,
		}
		if len(status.Models) > 0 {
			view.Subscription.DefaultModel = status.Models[0]
		}
	}
	return view
}

func registerResponsesUpstreamAPI(mux *http.ServeMux, manager *responsesUpstreamManager) {
	if manager == nil {
		return
	}
	mux.HandleFunc("GET /api/responses-upstream", manager.handleGet)
	mux.HandleFunc("POST /api/responses-upstream/save", manager.handleSave)
	mux.HandleFunc("POST /api/responses-upstream/activate", manager.handleActivate)
	mux.HandleFunc("POST /api/responses-upstream/test", manager.handleTest)
	mux.HandleFunc("POST /api/responses-upstream/clear", manager.handleClear)
}

func (m *responsesUpstreamManager) handleGet(w http.ResponseWriter, _ *http.Request) {
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *responsesUpstreamManager) handleSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string  `json:"name"`
		BaseURL      string  `json:"base_url"`
		APIKey       *string `json:"api_key"`
		DefaultModel string  `json:"default_model"`
	}
	if err := decodeResponsesUpstreamBody(w, r, &body); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.runtime.Snapshot()
	candidate := provider.CustomResponsesConfig{
		Name:         strings.TrimSpace(body.Name),
		BaseURL:      strings.TrimSpace(body.BaseURL),
		DefaultModel: strings.TrimSpace(body.DefaultModel),
	}
	if body.APIKey == nil {
		candidate.APIKey = before.Custom.APIKey
	} else {
		candidate.APIKey = strings.TrimSpace(*body.APIKey)
	}
	if err := provider.ValidateCustomResponsesConfig(candidate); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := m.persist(func(cfg *config) error {
		cfg.CustomResponses = candidate
		cfg.ResponsesSource = before.ActiveSource
		return nil
	}); err != nil {
		writeResponsesUpstreamError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	if err := m.runtime.SaveCustom(candidate); err != nil {
		_ = m.restore(before)
		writeResponsesUpstreamError(w, http.StatusInternalServerError, "应用配置失败: "+err.Error())
		return
	}
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *responsesUpstreamManager) handleActivate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source provider.ResponsesSource `json:"source"`
	}
	if err := decodeResponsesUpstreamBody(w, r, &body); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Source != provider.ResponsesSourceSubscription && body.Source != provider.ResponsesSourceCustom {
		writeResponsesUpstreamError(w, http.StatusBadRequest, fmt.Sprintf("无效的 Responses 上游 %q", body.Source))
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.runtime.Snapshot()
	if body.Source == provider.ResponsesSourceCustom && !before.Configured {
		writeResponsesUpstreamError(w, http.StatusConflict, provider.ErrCustomResponsesNotConfigured.Error())
		return
	}
	if body.Source == provider.ResponsesSourceSubscription && m.subscription == nil {
		writeResponsesUpstreamError(w, http.StatusConflict, provider.ErrSubscriptionResponsesNotConfigured.Error())
		return
	}
	if err := m.persist(func(cfg *config) error {
		cfg.ResponsesSource = body.Source
		return nil
	}); err != nil {
		writeResponsesUpstreamError(w, http.StatusInternalServerError, "保存切换状态失败: "+err.Error())
		return
	}
	if err := m.runtime.Activate(body.Source); err != nil {
		_ = m.persist(func(cfg *config) error {
			cfg.ResponsesSource = before.ActiveSource
			return nil
		})
		writeResponsesUpstreamError(w, http.StatusConflict, err.Error())
		return
	}
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *responsesUpstreamManager) handleTest(w http.ResponseWriter, r *http.Request) {
	if err := decodeResponsesUpstreamBody(w, r, &struct{}{}); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !m.runtime.Snapshot().Configured {
		writeResponsesUpstreamError(w, http.StatusConflict, provider.ErrCustomResponsesNotConfigured.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := m.runtime.TestCustom(ctx)
	if err != nil {
		message := redactResponsesSecret(err.Error(), m.runtime.Snapshot().Custom.APIKey)
		writeResponsesUpstreamError(w, http.StatusBadGateway, message)
		return
	}
	if !result.OK {
		writeResponsesUpstreamJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"status":     result.StatusCode,
			"latency_ms": result.DurationMS,
			"error":      result.Message,
		})
		return
	}
	writeResponsesUpstreamJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"status":     result.StatusCode,
		"latency_ms": result.DurationMS,
		"message":    "连接成功",
	})
}

func (m *responsesUpstreamManager) handleClear(w http.ResponseWriter, r *http.Request) {
	if err := decodeResponsesUpstreamBody(w, r, &struct{}{}); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.persist(func(cfg *config) error {
		cfg.CustomResponses = provider.CustomResponsesConfig{}
		cfg.ResponsesSource = provider.ResponsesSourceSubscription
		return nil
	}); err != nil {
		writeResponsesUpstreamError(w, http.StatusInternalServerError, "清除配置失败: "+err.Error())
		return
	}
	m.runtime.ClearCustom()
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *responsesUpstreamManager) restore(snapshot provider.ResponsesSnapshot) error {
	if err := m.persist(func(cfg *config) error {
		cfg.ResponsesSource = snapshot.ActiveSource
		cfg.CustomResponses = snapshot.Custom
		return nil
	}); err != nil {
		return err
	}
	if snapshot.Configured {
		if err := m.runtime.SaveCustom(snapshot.Custom); err != nil {
			return err
		}
	} else {
		m.runtime.ClearCustom()
	}
	return m.runtime.RestoreSource(snapshot.ActiveSource)
}

func decodeResponsesUpstreamBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, responsesUpstreamBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("无效的 JSON 请求: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("请求只能包含一个 JSON 对象")
		}
		return fmt.Errorf("无效的 JSON 请求: %w", err)
	}
	return nil
}

func redactResponsesSecret(message, secret string) string {
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[REDACTED]")
}

func writeResponsesUpstreamError(w http.ResponseWriter, status int, message string) {
	writeResponsesUpstreamJSON(w, status, map[string]any{"ok": false, "error": message})
}

func writeResponsesUpstreamJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
