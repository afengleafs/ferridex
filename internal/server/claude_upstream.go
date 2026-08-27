package server

// Dashboard management for Claude upstream profiles: a hand-edited
// claude_provider.env in the working directory defines switchable
// Anthropic-compatible upstreams; this manager loads it, hot-applies the
// persisted choice at startup, and serves /api/claude-upstream* so the panel
// can list, activate and reload profiles. Mirrors responsesUpstreamManager.

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ferridex/internal/provider"
)

const (
	claudeProfilesFileName       = "claude_provider.env"
	legacyClaudeProfilesFileName = "ferridex-profiles.env"
)

// claudeUpstreamManager serializes dashboard mutations so the persisted
// selection and the live routing decision move together. Handlers must hold mu.
type claudeUpstreamManager struct {
	mu            sync.Mutex
	runtime       *provider.ClaudeUpstreamProvider
	subscription  provider.Provider
	persist       func(func(*config) error) error
	readPersisted func() (config, error) // nil -> readConfigFile(configPath())
	profilesPath  string
	legacyPath    string
	entries       []provider.ClaudeProfileEntry
	fileError     string
	warnings      []string
	staleProfile  string // persisted selection currently absent from the file
}

func configureClaudeProviders(providers ...provider.Provider) ([]provider.Provider, *claudeUpstreamManager) {
	cfg, err := readConfigFile(configPath())
	if err != nil {
		log.Printf("无法读取 Claude 上游配置,Claude 路由将使用本机订阅: %v", err)
		cfg = config{}
	}
	providers, manager := configureClaudeProvidersWithConfig(cfg, updateConfig, claudeProfilesPath(), legacyClaudeProfilesPath(), providers...)
	if manager != nil {
		manager.readPersisted = func() (config, error) { return readConfigFile(configPath()) }
	}
	return providers, manager
}

func configureClaudeProvidersWithConfig(cfg config, persist func(func(*config) error) error, profilesPath, legacyPath string, providers ...provider.Provider) ([]provider.Provider, *claudeUpstreamManager) {
	configured := append([]provider.Provider(nil), providers...)
	for i, current := range configured {
		if current.Name() != "claude" {
			continue
		}
		if runtime, ok := current.(*provider.ClaudeUpstreamProvider); ok {
			return configured, &claudeUpstreamManager{runtime: runtime, persist: persist, profilesPath: profilesPath}
		}

		runtime := provider.NewClaudeUpstreamProvider(current)
		manager := &claudeUpstreamManager{
			runtime:      runtime,
			subscription: current,
			persist:      persist,
			profilesPath: profilesPath,
			legacyPath:   legacyPath,
		}
		manager.loadProfiles()
		manager.applyPersisted(cfg.ClaudeUpstream)

		configured[i] = runtime
		return configured, manager
	}
	return configured, nil
}

func claudeProfilesPath() string {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, claudeProfilesFileName)
}

func legacyClaudeProfilesPath() string {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, legacyClaudeProfilesFileName)
}

// ensureClaudeProfilesFile writes the commented template on first use. It never
// overwrites an existing file, and creates it user-only because enabled
// profiles keep real upstream credentials there.
func ensureClaudeProfilesFile(path, legacyPath string) (bool, error) {
	var migration []byte
	if legacyPath != "" {
		legacy, err := os.ReadFile(legacyPath)
		if err == nil && len(legacy) > 0 {
			migration = legacy
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return ensurePrivateProviderFile(path, migration, provider.DefaultClaudeProfilesTemplate())
}

// loadProfiles (re)reads the env file and loads every valid profile into the
// runtime switcher. Parse problems degrade to warnings surfaced via the API —
// one bad line never invalidates the rest of the file.
func (m *claudeUpstreamManager) loadProfiles() {
	if migrated, err := ensureClaudeProfilesFile(m.profilesPath, m.legacyPath); err != nil {
		m.fileError = err.Error()
	} else if migrated {
		log.Printf("已把 %s 迁移到 %s;旧文件保留用于回退", legacyClaudeProfilesFileName, claudeProfilesFileName)
	}
	src, err := os.ReadFile(m.profilesPath)
	if err != nil {
		m.fileError = err.Error()
		m.entries, m.warnings = nil, nil
		return
	}
	parsed, err := provider.ParseClaudeProfiles(src)
	if err != nil {
		m.fileError = err.Error()
		m.entries, m.warnings = nil, nil
		return
	}
	m.fileError = ""
	m.entries = parsed.Profiles
	m.warnings = parsed.Warnings
	for _, entry := range parsed.Profiles {
		if cfg, err := provider.ClaudeProfileFromValues(entry); err == nil {
			if err := m.runtime.SetProfile(cfg); err != nil {
				log.Printf("忽略无效的 Claude 上游供应商 %q: %v", entry.Name, err)
			}
		}
	}
}

// applyPersisted re-resolves the persisted profile name against the loaded
// entries. An unknown name falls back to the subscription at runtime while the
// persisted value is kept intact, so re-adding the profile to the file and
// hitting reload restores it without a restart.
func (m *claudeUpstreamManager) applyPersisted(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		_ = m.runtime.RestoreActive("")
		m.staleProfile = ""
		return
	}
	entry, ok := m.entryByName(name)
	if !ok {
		_ = m.runtime.RestoreActive("")
		m.staleProfile = name
		log.Printf("已保存的 Claude 上游供应商 %q 不在 %s 中,已回退到本机 Anthropic 订阅;把供应商加回文件后点击「重新加载」即可恢复", name, claudeProfilesFileName)
		return
	}
	cfg, err := provider.ClaudeProfileFromValues(entry)
	if err != nil {
		_ = m.runtime.RestoreActive("")
		m.staleProfile = name
		log.Printf("Claude 上游供应商 %q 无效,已回退到本机 Anthropic 订阅: %v", name, err)
		return
	}
	if err := m.runtime.SetProfile(cfg); err != nil {
		_ = m.runtime.RestoreActive("")
		m.staleProfile = name
		log.Printf("Claude 上游供应商 %q 无法应用,已回退到本机 Anthropic 订阅: %v", name, err)
		return
	}
	if err := m.runtime.RestoreActive(name); err != nil {
		log.Printf("恢复 Claude 上游供应商 %q 失败: %v", name, err)
		return
	}
	m.staleProfile = ""
}

func (m *claudeUpstreamManager) entryByName(name string) (provider.ClaudeProfileEntry, bool) {
	for _, entry := range m.entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return provider.ClaudeProfileEntry{}, false
}

type claudeProfileView struct {
	Name          string   `json:"name"`
	BaseURL       string   `json:"base_url,omitempty"`
	AuthMode      string   `json:"auth_mode,omitempty"` // "bearer" | "api_key"
	HasAuthToken  bool     `json:"has_auth_token"`
	HasAPIKey     bool     `json:"has_api_key"`
	Valid         bool     `json:"valid"`
	OpusModel     string   `json:"opus_model,omitempty"`
	SonnetModel   string   `json:"sonnet_model,omitempty"`
	HaikuModel    string   `json:"haiku_model,omitempty"`
	SubagentModel string   `json:"subagent_model,omitempty"`
	Problems      []string `json:"problems,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type claudeSubscriptionView struct {
	Available bool   `json:"available"`
	LoggedIn  bool   `json:"logged_in"`
	Title     string `json:"title,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// claudeUpstreamView is the redacted management view: credentials appear only
// as booleans/auth_mode, never as values.
type claudeUpstreamView struct {
	Active       string                 `json:"active"` // "" = 本机 Anthropic 订阅
	StaleProfile string                 `json:"stale_profile,omitempty"`
	Profiles     []claudeProfileView    `json:"profiles"`
	Subscription claudeSubscriptionView `json:"subscription"`
	File         string                 `json:"file"`
	FileError    string                 `json:"file_error,omitempty"`
	Warnings     []string               `json:"warnings,omitempty"`
}

// view builds the redacted view. Callers must hold m.mu.
func (m *claudeUpstreamManager) view() claudeUpstreamView {
	snapshot := m.runtime.Snapshot()
	view := claudeUpstreamView{
		Profiles:  make([]claudeProfileView, 0, len(m.entries)),
		File:      m.profilesPath,
		FileError: m.fileError,
		Warnings:  m.warnings,
	}
	if snapshot.Active == provider.ClaudeUpstreamProfile {
		view.Active = snapshot.ProfileName
	}
	view.StaleProfile = m.staleProfile
	for _, entry := range m.entries {
		pv := claudeProfileView{Name: entry.Name, Warnings: entry.Warnings}
		if cfg, err := provider.ClaudeProfileFromValues(entry); err == nil {
			pv.Valid = true
			pv.BaseURL = cfg.BaseURL
			pv.HasAuthToken = cfg.AuthToken != ""
			pv.HasAPIKey = cfg.APIKey != ""
			if pv.HasAuthToken {
				pv.AuthMode = "bearer"
			} else {
				pv.AuthMode = "api_key"
			}
			pv.OpusModel = cfg.OpusModel
			pv.SonnetModel = cfg.SonnetModel
			pv.HaikuModel = cfg.HaikuModel
			pv.SubagentModel = cfg.SubagentModel
		} else {
			pv.Problems = []string{redactClaudeSecret(err.Error(), cfg)}
		}
		view.Profiles = append(view.Profiles, pv)
	}
	if m.subscription != nil {
		st := m.subscription.Status()
		view.Subscription = claudeSubscriptionView{
			Available: true,
			LoggedIn:  st.LoggedIn,
			Title:     st.Title,
			Detail:    st.Detail,
		}
	}
	return view
}

func redactClaudeSecret(message string, cfg provider.ClaudeProfileConfig) string {
	for _, secret := range []string{cfg.AuthToken, cfg.APIKey} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

func registerClaudeUpstreamAPI(mux *http.ServeMux, manager *claudeUpstreamManager) {
	if manager == nil {
		return
	}
	mux.HandleFunc("GET /api/claude-upstream", manager.handleGet)
	mux.HandleFunc("POST /api/claude-upstream/activate", manager.handleActivate)
	mux.HandleFunc("POST /api/claude-upstream/reload", manager.handleReload)
}

func (m *claudeUpstreamManager) handleGet(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *claudeUpstreamManager) handleActivate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeResponsesUpstreamBody(w, r, &body); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	name := strings.TrimSpace(body.Name)
	before := m.runtime.Snapshot()

	if name == "" {
		if err := m.persist(func(cfg *config) error {
			cfg.ClaudeUpstream = ""
			return nil
		}); err != nil {
			writeResponsesUpstreamError(w, http.StatusInternalServerError, "保存切换状态失败: "+err.Error())
			return
		}
		if err := m.runtime.ActivateSubscription(); err != nil {
			_ = m.persist(func(cfg *config) error {
				cfg.ClaudeUpstream = persistedClaudeName(before)
				return nil
			})
			writeResponsesUpstreamError(w, http.StatusConflict, err.Error())
			return
		}
		m.staleProfile = ""
		writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
		return
	}

	entry, ok := m.entryByName(name)
	if !ok {
		writeResponsesUpstreamError(w, http.StatusNotFound, fmt.Sprintf("未知供应商 %q;请确认 %s 中存在该 [供应商名] 段,或先点击「重新加载」", name, claudeProfilesFileName))
		return
	}
	candidate, err := provider.ClaudeProfileFromValues(entry)
	if err != nil {
		writeResponsesUpstreamError(w, http.StatusConflict, redactClaudeSecret("供应商无效: "+err.Error(), candidate))
		return
	}
	if err := m.persist(func(cfg *config) error {
		cfg.ClaudeUpstream = name
		return nil
	}); err != nil {
		writeResponsesUpstreamError(w, http.StatusInternalServerError, "保存切换状态失败: "+err.Error())
		return
	}
	if err := m.runtime.SetProfile(candidate); err != nil {
		_ = m.persist(func(cfg *config) error {
			cfg.ClaudeUpstream = persistedClaudeName(before)
			return nil
		})
		m.restoreRuntime(before)
		writeResponsesUpstreamError(w, http.StatusInternalServerError, redactClaudeSecret("应用供应商失败: "+err.Error(), candidate))
		return
	}
	if err := m.runtime.ActivateProfile(name); err != nil {
		_ = m.persist(func(cfg *config) error {
			cfg.ClaudeUpstream = persistedClaudeName(before)
			return nil
		})
		m.restoreRuntime(before)
		writeResponsesUpstreamError(w, http.StatusConflict, err.Error())
		return
	}
	m.staleProfile = ""
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func (m *claudeUpstreamManager) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := decodeResponsesUpstreamBody(w, r, &struct{}{}); err != nil {
		writeResponsesUpstreamError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadProfiles()
	readPersisted := m.readPersisted
	if readPersisted == nil {
		readPersisted = func() (config, error) { return readConfigFile(configPath()) }
	}
	persisted := ""
	if cfg, err := readPersisted(); err != nil {
		log.Printf("重新加载 Claude 供应商时读取配置失败: %v", err)
	} else {
		persisted = cfg.ClaudeUpstream
	}
	m.applyPersisted(persisted)
	writeResponsesUpstreamJSON(w, http.StatusOK, m.view())
}

func persistedClaudeName(snapshot provider.ClaudeUpstreamSnapshot) string {
	if snapshot.Active == provider.ClaudeUpstreamProfile {
		return snapshot.ProfileName
	}
	return ""
}

// restoreRuntime puts the switcher back into a previous snapshot after a
// failed activation.
func (m *claudeUpstreamManager) restoreRuntime(snapshot provider.ClaudeUpstreamSnapshot) {
	if snapshot.Configured {
		_ = m.runtime.SetProfile(snapshot.Profile)
	}
	_ = m.runtime.RestoreActive(persistedClaudeName(snapshot))
}
