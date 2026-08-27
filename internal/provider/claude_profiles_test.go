package provider

import (
	"strings"
	"testing"
)

func TestParseClaudeProfilesHappyPath(t *testing.T) {
	src := "\ufeff" + "# comment\r\n" +
		"\r\n" +
		"[openrouter]\r\n" +
		"ANTHROPIC_BASE_URL=\"https://openrouter.ai/api\"\r\n" +
		"export ANTHROPIC_AUTH_TOKEN='sk-or-v1 abc'\r\n" +
		"ANTHROPIC_API_KEY=\"\"\r\n" +
		"  ANTHROPIC_DEFAULT_SONNET_MODEL = stealth/ox-alpha \r\n" +
		"anthropic_default_opus_model=\"Stealth/OX-Alpha\"\r\n" +
		"CLAUDE_CODE_SUBAGENT_MODEL=\"stealth/ox-alpha\"\r\n" +
		"\r\n" +
		"[second]\n" +
		"ANTHROPIC_BASE_URL=http://127.0.0.1:8082\n"

	parsed, err := ParseClaudeProfiles([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", parsed.Warnings)
	}
	if len(parsed.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(parsed.Profiles))
	}
	first := parsed.Profiles[0]
	if first.Name != "openrouter" || first.Line != 3 {
		t.Fatalf("unexpected first entry: %#v", first)
	}
	want := map[string]string{
		"ANTHROPIC_BASE_URL":             "https://openrouter.ai/api",
		"ANTHROPIC_AUTH_TOKEN":           "sk-or-v1 abc",
		"ANTHROPIC_API_KEY":              "",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "stealth/ox-alpha",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "Stealth/OX-Alpha",
		"CLAUDE_CODE_SUBAGENT_MODEL":     "stealth/ox-alpha",
	}
	for key, value := range want {
		if first.Values[key] != value {
			t.Fatalf("%s = %q, want %q", key, first.Values[key], value)
		}
	}
	if got := parsed.Profiles[1].Values["ANTHROPIC_BASE_URL"]; got != "http://127.0.0.1:8082" {
		t.Fatalf("bare value = %q", got)
	}
}

func TestParseClaudeProfilesWarnings(t *testing.T) {
	src := `# 头部说明
UNKNOWN_BEFORE_SECTION="x"
[bad section name
[empty]
[only-unknown]
SOME_OTHER_KEY="y"
[dup]
ANTHROPIC_BASE_URL="https://a.example"
[dup]
noequalsline
ANTHROPIC_BASE_URL="https://b.example"
`
	parsed, err := ParseClaudeProfiles([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	joined := strings.Join(parsed.Warnings, "\n")
	for _, want := range []string{"第 2 行", "第 3 行", "第 4 行", "重复定义"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings missing %q: %v", want, parsed.Warnings)
		}
	}
	if len(parsed.Profiles) != 1 || parsed.Profiles[0].Name != "dup" {
		t.Fatalf("profiles = %#v, want only dup", parsed.Profiles)
	}
	// Last [dup] block wins; the malformed line warned inside the section.
	if got := parsed.Profiles[0].Values["ANTHROPIC_BASE_URL"]; got != "https://b.example" {
		t.Fatalf("base_url = %q, want last block to win", got)
	}
	if len(parsed.Profiles[0].Warnings) == 0 {
		t.Fatal("expected in-section warning for the malformed line")
	}
}

func TestParseClaudeProfilesWarningsDoNotEchoMalformedCredential(t *testing.T) {
	const secret = "sk-secret-must-not-leak"
	parsed, err := ParseClaudeProfiles([]byte("[supplier]\nANTHROPIC_AUTH_TOKEN " + secret + "\n"))
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

func TestParseClaudeProfilesSkipsKeylessSections(t *testing.T) {
	src := "[ghost]\n# nothing here\n[real]\nANTHROPIC_AUTH_TOKEN=\"sk\"\n"
	parsed, err := ParseClaudeProfiles([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Profiles) != 1 || parsed.Profiles[0].Name != "real" {
		t.Fatalf("profiles = %#v, want only real", parsed.Profiles)
	}
	if len(parsed.Warnings) != 1 || !strings.Contains(parsed.Warnings[0], "ghost") {
		t.Fatalf("warnings = %v, want one ghost warning", parsed.Warnings)
	}
}

func TestParseClaudeProfilesTooLarge(t *testing.T) {
	big := make([]byte, claudeProfilesMaxSize+1)
	if _, err := ParseClaudeProfiles(big); err == nil {
		t.Fatal("expected size error")
	}
}

func TestDefaultClaudeProfilesTemplateParsesClean(t *testing.T) {
	parsed, err := ParseClaudeProfiles([]byte(DefaultClaudeProfilesTemplate()))
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}
	if len(parsed.Profiles) != 0 {
		t.Fatalf("template profiles = %#v, want none", parsed.Profiles)
	}
	if len(parsed.Warnings) != 0 {
		t.Fatalf("template warnings = %v, want none", parsed.Warnings)
	}
}

func TestClaudeProfileFromValues(t *testing.T) {
	entry := ClaudeProfileEntry{
		Name:   "openrouter",
		Line:   1,
		Values: map[string]string{},
	}
	if _, err := ClaudeProfileFromValues(entry); err == nil {
		t.Fatal("expected missing credential error")
	}

	entry.Values = map[string]string{
		"ANTHROPIC_BASE_URL":             "https://openrouter.ai/api/v1",
		"ANTHROPIC_AUTH_TOKEN":           "sk-or-v1-x",
		"ANTHROPIC_API_KEY":              "",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "stealth/ox-alpha",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "stealth/ox-alpha",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "stealth/ox-alpha",
		"CLAUDE_CODE_SUBAGENT_MODEL":     "subagent-display-only",
	}
	cfg, err := ClaudeProfileFromValues(entry)
	if err != nil {
		t.Fatalf("from values: %v", err)
	}
	// Trailing /v1 is stripped so the relay target becomes /v1/messages.
	if cfg.BaseURL != "https://openrouter.ai/api" {
		t.Fatalf("base_url = %q, want versioned root stripped", cfg.BaseURL)
	}
	if cfg.AuthToken != "sk-or-v1-x" || cfg.APIKey != "" {
		t.Fatalf("credentials = %q/%q", cfg.AuthToken, cfg.APIKey)
	}
	if cfg.OpusModel != "stealth/ox-alpha" || cfg.SonnetModel != "stealth/ox-alpha" || cfg.HaikuModel != "stealth/ox-alpha" {
		t.Fatalf("model mapping lost: %#v", cfg)
	}
	if cfg.SubagentModel != "subagent-display-only" {
		t.Fatalf("subagent model = %q, want echoed for client configs", cfg.SubagentModel)
	}
}
