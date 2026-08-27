package server

import (
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

	"ferridex/internal/provider"
)

const (
	responsesUpstreamBodyLimit = 64 << 10
	codexProfilesFileName      = "codex_provider.env"
)

// responsesUpstreamManager serializes dashboard mutations so the persisted
// configuration and the live routing decision move together.
type responsesUpstreamManager struct {
	mu            sync.Mutex
	runtime       *provider.ResponsesProvider
	subscription  provider.Provider
	persist       func(func(*config) error) error
	readPersisted func() (config, error)
	profilesPath  string
	entries       []provider.CodexProfileEntry
	fileError     string
	warnings      []string
	staleProfile  string
}

func configureResponsesProviders(providers ...provider.Provider) ([]provider.Provider, *responsesUpstreamManager) {
	cfg, err := readConfigFile(configPath())
	if err != nil {
		log.Printf("无法读取 Responses 上游配置,Codex 路由将保持关闭以避免误用本机订阅: %v", err)
		cfg = config{ResponsesSource: provider.ResponsesSourceCustom}
	}
	configured, manager := configureResponsesProvidersWithConfig(cfg, updateConfig, codexProfilesPath(), providers...)
	if manager != nil {
		manager.readPersisted = func() (config, error) { return readConfigFile(configPath()) }
	}
	return configured, manager
}

func configureResponsesProvidersWithConfig(cfg config, persist func(func(*config) error) error, profilesPath string, providers ...provider.Provider) ([]provider.Provider, *responsesUpstreamManager) {
	configured := append([]provider.Provider(nil), providers...)

	var manager *responsesUpstreamManager
	for i, current := range configured {
		if current.Name() != "codex" {
			continue
		}
		if runtime, ok := current.(*provider.ResponsesProvider); ok {
			manager = &responsesUpstreamManager{runtime: runtime, persist: persist, profilesPath: profilesPath}
			manager.loadProfiles(cfg.CustomResponses)
			manager.applyPersisted(cfg.CodexUpstream, cfg.CodexUpstream != "" || cfg.ResponsesSource == provider.ResponsesSourceCustom)
			break
		}

		runtime := provider.NewResponsesProvider(current)
		configured[i] = runtime
		manager = &responsesUpstreamManager{
			runtime:      runtime,
			subscription: current,
			persist:      persist,
			profilesPath: profilesPath,
		}
		manager.loadProfiles(cfg.CustomResponses)
		activeName := strings.TrimSpace(cfg.CodexUpstream)
		if activeName == "" && cfg.ResponsesSource == provider.ResponsesSourceCustom && customResponsesConfigPresent(cfg.CustomResponses) {
			activeName = legacyCodexSupplierName(cfg.CustomResponses.Name)
			if _, ok := manager.entryByName(activeName); ok {
				if err := persist(func(current *config) error {
					current.CodexUpstream = activeName
					return nil
				}); err != nil {
					log.Printf("保存迁移后的 Codex 供应商选择失败: %v", err)
				}
			}
		}
		requestedCustom := activeName != "" || cfg.ResponsesSource == provider.ResponsesSourceCustom
		if cfg.ResponsesSource != "" && cfg.ResponsesSource != provider.ResponsesSourceSubscription && cfg.ResponsesSource != provider.ResponsesSourceCustom {
			requestedCustom = true
			log.Printf("无效的已保存 Responses 上游选择 %q;Codex 路由将保持关闭", cfg.ResponsesSource)
		}
		manager.applyPersisted(activeName, requestedCustom)
		break
	}
	return configured, manager
}

func codexProfilesPath() string {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, codexProfilesFileName)
}

func ensureCodexProfilesFile(path string, legacy provider.CustomResponsesConfig) (string, bool, error) {
	var migration []byte
	migrationName := ""
	if customResponsesConfigPresent(legacy) {
		if normalized, err := provider.CodexProfileFromValues(provider.CodexProfileEntry{
			Name: legacyCodexSupplierName(legacy.Name),
			Values: map[string]string{
				"OPENAI_BASE_URL":      legacy.BaseURL,
				"OPENAI_API_KEY":       legacy.APIKey,
				"OPENAI_DEFAULT_MODEL": legacy.DefaultModel,
			},
		}); err == nil {
			migrationName = normalized.Name
			migration = []byte(fmt.Sprintf("# 从 ~/.ferridex/config.json 自动迁移\n[%s]\nOPENAI_BASE_URL=%s\nOPENAI_API_KEY=%s\nOPENAI_DEFAULT_MODEL=%s\n",
				migrationName, normalized.BaseURL, normalized.APIKey, normalized.DefaultModel))
		}
	}
	migrated, err := ensurePrivateProviderFile(path, migration, provider.DefaultCodexProfilesTemplate())
	if !migrated {
		migrationName = ""
	}
	return migrationName, migrated, err
}

func legacyCodexSupplierName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.NewReplacer("[", "-", "]", "-").Replace(name)
	if name == "" {
		return "custom"
	}
	return name
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

type responsesProfileView struct {
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url,omitempty"`
	DefaultModel string   `json:"default_model,omitempty"`
	HasAPIKey    bool     `json:"has_api_key"`
	Valid        bool     `json:"valid"`
	Problems     []string `json:"problems,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

type responsesUpstreamView struct {
	ActiveSource provider.ResponsesSource  `json:"active_source"`
	Active       string                    `json:"active"`
	StaleProfile string                    `json:"stale_profile,omitempty"`
	Profiles     []responsesProfileView    `json:"profiles"`
	Subscription responsesSubscriptionView `json:"subscription"`
	Custom       customResponsesView       `json:"custom"`
	File         string                    `json:"file"`
	FileError    string                    `json:"file_error,omitempty"`
	Warnings     []string                  `json:"warnings,omitempty"`
}

func (m *responsesUpstreamManager) view() responsesUpstreamView {
	snapshot := m.runtime.Snapshot()
	view := responsesUpstreamView{
		ActiveSource: snapshot.ActiveSource,
		Profiles:     make([]responsesProfileView, 0, len(m.entries)),
		File:         m.profilesPath,
		FileError:    m.fileError,
		Warnings:     m.warnings,
		Custom: customResponsesView{
			Configured:   snapshot.Configured,
			Name:         snapshot.Custom.Name,
			BaseURL:      snapshot.Custom.BaseURL,
			DefaultModel: snapshot.Custom.DefaultModel,
			HasAPIKey:    snapshot.HasAPIKey,
		},
	}
	if snapshot.ActiveSource == provider.ResponsesSourceCustom && snapshot.Configured {
		view.Active = snapshot.Custom.Name
	}
	view.StaleProfile = m.staleProfile
	for _, entry := range m.entries {
		profileView := responsesProfileView{Name: entry.Name, Warnings: entry.Warnings}
		if cfg, err := provider.CodexProfileFromValues(entry); err == nil {
			profileView.Valid = true
			profileView.BaseURL = cfg.BaseURL
			profileView.DefaultModel = cfg.DefaultModel
			profileView.HasAPIKey = cfg.APIKey != ""
		} else {
			profileView.Problems = []string{redactResponsesSecret(err.Error(), entry.Values["OPENAI_API_KEY"])}
		}
		view.Profiles = append(view.Profiles, profileView)
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

func (m *responsesUpstreamManager) loadProfiles(legacy provider.CustomResponsesConfig) {
	if _, migrated, err := ensureCodexProfilesFile(m.profilesPath, legacy); err != nil {
		m.fileError = err.Error()
	} else if migrated {
		log.Printf("已把旧 Codex 自定义上游迁移到 %s;config.json 旧字段保留用于回退", codexProfilesFileName)
	}
	src, err := os.ReadFile(m.profilesPath)
	if err != nil {
		m.fileError = err.Error()
		m.entries, m.warnings = nil, nil
		return
	}
	parsed, err := provider.ParseCodexProfiles(src)
	if err != nil {
		m.fileError = err.Error()
		m.entries, m.warnings = nil, nil
		return
	}
	m.fileError = ""
	m.entries = parsed.Profiles
	m.warnings = parsed.Warnings
}

func (m *responsesUpstreamManager) entryByName(name string) (provider.CodexProfileEntry, bool) {
	for _, entry := range m.entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return provider.CodexProfileEntry{}, false
}

func (m *responsesUpstreamManager) applyPersisted(name string, requestedCustom bool) {
	name = strings.TrimSpace(name)
	if name == "" && !requestedCustom {
		_ = m.runtime.RestoreSource(provider.ResponsesSourceSubscription)
		m.staleProfile = ""
		return
	}
	entry, ok := m.entryByName(name)
	if !ok {
		m.runtime.ClearCustom()
		_ = m.runtime.RestoreSource(provider.ResponsesSourceCustom)
		m.staleProfile = name
		log.Printf("已保存的 Codex 供应商 %q 不在 %s 中;路由保持关闭,不会回退本机订阅", name, codexProfilesFileName)
		return
	}
	candidate, err := provider.CodexProfileFromValues(entry)
	if err == nil {
		err = m.runtime.SaveCustom(candidate)
	}
	if err != nil {
		m.runtime.ClearCustom()
		_ = m.runtime.RestoreSource(provider.ResponsesSourceCustom)
		m.staleProfile = name
		log.Printf("Codex 供应商 %q 无效;路由保持关闭,不会回退本机订阅: %s", name, redactResponsesSecret(err.Error(), entry.Values["OPENAI_API_KEY"]))
		return
	}
	if err := m.runtime.RestoreSource(provider.ResponsesSourceCustom); err != nil {
		log.Printf("恢复 Codex 供应商 %q 失败: %v", name, err)
	}
	m.staleProfile = ""
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
	mux.HandleFunc("POST /api/responses-upstream/reload", manager.handleReload)
}

func (m *responsesUpstreamManager) handleGet(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *responsesUpstreamManager) handleSave(w http.ResponseWriter, r *http.Request) {
	writeResponsesUpstreamError(w, http.StatusConflict, "Codex 供应商由 codex_provider.env 管理;请编辑文件后点击「重新加载」")
}

func (m *responsesUpstreamManager) handleActivate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   *string                  `json:"name"`
		Source provider.ResponsesSource `json:"source,omitempty"`
	}
	if err := decodeResponsesUpstreamBody(w, r, &body); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	name := ""
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
	} else {
		switch body.Source {
		case provider.ResponsesSourceSubscription:
		case provider.ResponsesSourceCustom:
			snapshot := m.runtime.Snapshot()
			if snapshot.Configured {
				name = snapshot.Custom.Name
			} else if len(m.entries) == 1 {
				name = m.entries[0].Name
			} else {
				writeResponsesUpstreamError(w, http.StatusConflict, "请指定要启用的 Codex 供应商名称")
				return
			}
		default:
			writeResponsesUpstreamError(w, http.StatusBadRequest, "请提供供应商名称;空名称表示本机订阅")
			return
		}
	}
	before := m.runtime.Snapshot()
	beforeStale := m.staleProfile
	if name == "" && m.subscription == nil {
		writeResponsesUpstreamError(w, http.StatusConflict, provider.ErrSubscriptionResponsesNotConfigured.Error())
		return
	}
	if name != "" {
		entry, ok := m.entryByName(name)
		if !ok {
			writeResponsesUpstreamError(w, http.StatusNotFound, fmt.Sprintf("未知供应商 %q;请确认 %s 中存在对应分段,或先点击「重新加载」", name, codexProfilesFileName))
			return
		}
		candidate, err := provider.CodexProfileFromValues(entry)
		if err != nil {
			writeResponsesUpstreamError(w, http.StatusConflict, redactResponsesSecret("供应商无效: "+err.Error(), entry.Values["OPENAI_API_KEY"]))
			return
		}
		if err := m.runtime.SaveCustom(candidate); err != nil {
			writeResponsesUpstreamError(w, http.StatusConflict, redactResponsesSecret(err.Error(), candidate.APIKey))
			return
		}
		if err := m.runtime.Activate(provider.ResponsesSourceCustom); err != nil {
			m.restoreRuntime(before)
			writeResponsesUpstreamError(w, http.StatusConflict, err.Error())
			return
		}
	} else if err := m.runtime.Activate(provider.ResponsesSourceSubscription); err != nil {
		writeResponsesUpstreamError(w, http.StatusConflict, err.Error())
		return
	}
	if err := m.persist(func(cfg *config) error {
		cfg.CodexUpstream = name
		if name == "" {
			cfg.ResponsesSource = provider.ResponsesSourceSubscription
		} else {
			cfg.ResponsesSource = provider.ResponsesSourceCustom
		}
		return nil
	}); err != nil {
		m.restoreRuntime(before)
		m.staleProfile = beforeStale
		writeResponsesUpstreamError(w, http.StatusInternalServerError, "保存切换状态失败: "+err.Error())
		return
	}
	m.staleProfile = ""
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *responsesUpstreamManager) handleTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name,omitempty"`
	}
	if err := decodeResponsesUpstreamBody(w, r, &body); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.mu.Lock()
	name := strings.TrimSpace(body.Name)
	if name == "" {
		snapshot := m.runtime.Snapshot()
		if snapshot.Configured {
			name = snapshot.Custom.Name
		} else if len(m.entries) == 1 {
			name = m.entries[0].Name
		}
	}
	entry, ok := m.entryByName(name)
	if !ok {
		m.mu.Unlock()
		writeResponsesUpstreamError(w, http.StatusConflict, "请指定要测试的 Codex 供应商")
		return
	}
	candidate, err := provider.CodexProfileFromValues(entry)
	m.mu.Unlock()
	if err != nil {
		writeResponsesUpstreamError(w, http.StatusConflict, redactResponsesSecret(err.Error(), entry.Values["OPENAI_API_KEY"]))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := provider.TestCustomResponses(ctx, candidate)
	if err != nil {
		message := redactResponsesSecret(err.Error(), candidate.APIKey)
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
	writeResponsesUpstreamError(w, http.StatusConflict, "Codex 供应商由 codex_provider.env 管理;请编辑文件后点击「重新加载」")
}

func (m *responsesUpstreamManager) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := decodeResponsesUpstreamBody(w, r, &struct{}{}); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadProfiles(provider.CustomResponsesConfig{})
	readPersisted := m.readPersisted
	if readPersisted == nil {
		readPersisted = func() (config, error) { return readConfigFile(configPath()) }
	}
	cfg, err := readPersisted()
	if err != nil {
		writeResponsesUpstreamError(w, http.StatusInternalServerError, "读取切换状态失败: "+err.Error())
		return
	}
	m.applyPersisted(cfg.CodexUpstream, cfg.CodexUpstream != "" || cfg.ResponsesSource == provider.ResponsesSourceCustom)
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *responsesUpstreamManager) restoreRuntime(snapshot provider.ResponsesSnapshot) {
	if snapshot.Configured {
		_ = m.runtime.SaveCustom(snapshot.Custom)
	} else {
		m.runtime.ClearCustom()
	}
	_ = m.runtime.RestoreSource(snapshot.ActiveSource)
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
