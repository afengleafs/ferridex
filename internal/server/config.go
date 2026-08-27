package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"ferridex/internal/provider"
)

// config is ferridex's single local source of truth. Secrets are deliberately
// kept out of dashboard responses; the file itself is restricted to the user.
type config struct {
	DownstreamKey   string                         `json:"downstream_key"`
	LANKey          string                         `json:"lan_key,omitempty"`
	TunnelRemote    string                         `json:"tunnel_remote,omitempty"`
	TunnelKey       string                         `json:"tunnel_key,omitempty"`
	ResponsesSource provider.ResponsesSource       `json:"responses_source,omitempty"`
	CustomResponses provider.CustomResponsesConfig `json:"custom_responses,omitempty"`
	// ClaudeUpstream is the active Claude profile name from
	// claude_provider.env; empty means the built-in Anthropic subscription.
	ClaudeUpstream string `json:"claude_upstream,omitempty"`
	// CodexUpstream is the active supplier name from codex_provider.env; empty
	// means the built-in ChatGPT subscription. Legacy ResponsesSource and
	// CustomResponses fields are retained for one-way migration and rollback.
	CodexUpstream string `json:"codex_upstream,omitempty"`
}

var configMu sync.Mutex

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ferridex", "config.json")
}

func loadConfig() config {
	configMu.Lock()
	defer configMu.Unlock()
	c, _ := readConfigFile(configPath())
	return c
}

func readConfigFile(path string) (config, error) {
	var c config
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	return c, nil
}

func saveConfig(c config) error {
	configMu.Lock()
	defer configMu.Unlock()
	return writeConfigFile(configPath(), c)
}

// updateConfig makes read-modify-write operations atomic within the process,
// preventing tunnel, LAN-key and upstream edits from overwriting each other.
func updateConfig(update func(*config) error) error {
	configMu.Lock()
	defer configMu.Unlock()
	return updateConfigFile(configPath(), update)
}

func updateConfigFile(path string, update func(*config) error) error {
	c, err := readConfigFile(path)
	if err != nil {
		return err
	}
	if err := update(&c); err != nil {
		return err
	}
	return writeConfigFile(path, c)
}

func writeConfigFile(path string, c config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
