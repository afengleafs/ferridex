package provider

import (
	"strings"
	"testing"
)

func TestParseCodexProfilesAndValidate(t *testing.T) {
	src := `# suppliers
[openrouter]
OPENAI_BASE_URL="https://openrouter.ai/api/v1"
export OPENAI_API_KEY='sk-or-test'
OPENAI_DEFAULT_MODEL=openai/gpt-5.6

[local]
OPENAI_BASE_URL=http://127.0.0.1:8080/v1
OPENAI_API_KEY=local-key
OPENAI_DEFAULT_MODEL=test-model
`
	parsed, err := ParseCodexProfiles([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Warnings) != 0 || len(parsed.Profiles) != 2 {
		t.Fatalf("parsed = %#v", parsed)
	}
	cfg, err := CodexProfileFromValues(parsed.Profiles[0])
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Name != "openrouter" || cfg.BaseURL != "https://openrouter.ai/api/v1" || cfg.APIKey != "sk-or-test" || cfg.DefaultModel != "openai/gpt-5.6" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestParseCodexProfilesWarningsAndLastDuplicateWins(t *testing.T) {
	src := `OPENAI_BASE_URL=https://before.example/v1
[empty]
[dup]
UNKNOWN=x
OPENAI_BASE_URL=https://first.example/v1
[dup]
bad-line
OPENAI_BASE_URL=https://second.example/v1
OPENAI_API_KEY=secret
OPENAI_DEFAULT_MODEL=model
`
	parsed, err := ParseCodexProfiles([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	joined := strings.Join(parsed.Warnings, "\n")
	for _, want := range []string{"[供应商名]", "empty", "重复定义"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings missing %q: %v", want, parsed.Warnings)
		}
	}
	if len(parsed.Profiles) != 1 || parsed.Profiles[0].Name != "dup" {
		t.Fatalf("profiles = %#v", parsed.Profiles)
	}
	if got := parsed.Profiles[0].Values["OPENAI_BASE_URL"]; got != "https://second.example/v1" {
		t.Fatalf("base URL = %q", got)
	}
	if len(parsed.Profiles[0].Warnings) == 0 {
		t.Fatal("missing in-section warning")
	}
}

func TestParseCodexProfilesWarningsDoNotEchoMalformedCredential(t *testing.T) {
	const secret = "sk-secret-must-not-leak"
	parsed, err := ParseCodexProfiles([]byte("[supplier]\nOPENAI_API_KEY " + secret + "\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	allWarnings := strings.Join(parsed.Warnings, "\n")
	for _, entry := range parsed.Profiles {
		allWarnings += strings.Join(entry.Warnings, "\n")
	}
	if strings.Contains(allWarnings, secret) {
		t.Fatalf("warning leaked malformed credential: %q", allWarnings)
	}
}

func TestCodexProfileValidationAndTemplate(t *testing.T) {
	parsed, err := ParseCodexProfiles([]byte(DefaultCodexProfilesTemplate()))
	if err != nil || len(parsed.Profiles) != 0 || len(parsed.Warnings) != 0 {
		t.Fatalf("template parse = %#v, err = %v", parsed, err)
	}

	entry := CodexProfileEntry{Name: "broken", Values: map[string]string{
		"OPENAI_BASE_URL":      "https://example.com/v1/responses",
		"OPENAI_API_KEY":       "key",
		"OPENAI_DEFAULT_MODEL": "model",
	}}
	if _, err := CodexProfileFromValues(entry); err == nil {
		t.Fatal("expected /responses validation error")
	}

	big := make([]byte, codexProfilesMaxSize+1)
	if _, err := ParseCodexProfiles(big); err == nil {
		t.Fatal("expected size error")
	}
}
