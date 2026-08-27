package server

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"ferridex/internal/provider"
)

const testResponsesAPIKey = "test-upstream-key-do-not-use"

func TestWriteConfigFileIsAtomicAndRestrictsPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", ".ferridex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "config.json")

	first := fullTestConfig("first")
	second := fullTestConfig("second")
	if err := writeConfigFile(path, first); err != nil {
		t.Fatalf("initial writeConfigFile: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			got, err := readConfigFile(path)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			if !reflect.DeepEqual(got, first) && !reflect.DeepEqual(got, second) {
				select {
				case errCh <- &unexpectedConfigError{got: got}:
				default:
				}
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		cfg := first
		if i%2 == 1 {
			cfg = second
		}
		if err := writeConfigFile(path, cfg); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("writeConfigFile iteration %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("reader observed a partial config: %v", err)
	default:
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat config directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("config directory permissions = %04o, want 0700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("config file permissions = %04o, want 0600", got)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".config-*.tmp"))
	if err != nil {
		t.Fatalf("glob temporary configs: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("atomic write left temporary files behind: %v", matches)
	}
}

func TestUpdateConfigFilePreservesUnrelatedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ferridex", "config.json")
	want := fullTestConfig("preserved")
	if err := writeConfigFile(path, want); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}

	if err := updateConfigFile(path, func(cfg *config) error {
		cfg.ResponsesSource = provider.ResponsesSourceCustom
		cfg.CustomResponses.Name = "updated upstream"
		return nil
	}); err != nil {
		t.Fatalf("updateConfigFile: %v", err)
	}

	want.ResponsesSource = provider.ResponsesSourceCustom
	want.CustomResponses.Name = "updated upstream"
	got, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config fields were not preserved\n got: %#v\nwant: %#v", got, want)
	}
}

func fullTestConfig(suffix string) config {
	return config{
		DownstreamKey:   "downstream-" + suffix,
		LANKey:          "lan-" + suffix,
		TunnelRemote:    "user@relay.example:" + suffix,
		TunnelKey:       "tunnel-" + suffix,
		ResponsesSource: provider.ResponsesSourceSubscription,
		CustomResponses: provider.CustomResponsesConfig{
			Name:         "upstream-" + suffix,
			BaseURL:      "https://api.example.com/v1/" + suffix,
			APIKey:       testResponsesAPIKey + "-" + suffix,
			DefaultModel: "model-" + suffix,
		},
		ClaudeUpstream: "claude-provider-" + suffix,
		CodexUpstream:  "codex-provider-" + suffix,
	}
}

type unexpectedConfigError struct {
	got config
}

func (e *unexpectedConfigError) Error() string {
	return "unexpected complete config observed"
}
