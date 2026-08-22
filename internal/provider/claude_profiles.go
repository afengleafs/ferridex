package provider

// Parsing for ferridex-profiles.env: a hand-edited, INI-like env file whose
// [name] sections each describe one switchable Claude upstream profile using
// the same ANTHROPIC_* variable names Claude Code itself accepts. The parser
// is pure (no I/O, no logging): problems are surfaced as warnings so one bad
// line never invalidates the rest of the file.

import (
	"fmt"
	"strings"
)

// claudeProfilesMaxSize bounds a profiles file; it holds credentials and is
// hand-edited, so anything larger is almost certainly not a valid profile set.
const claudeProfilesMaxSize = 256 << 10

// claudeProfileKeys is the exact set of variables recognized inside a section.
// CLAUDE_CODE_SUBAGENT_MODEL is accepted (so users can paste their launch env
// verbatim) but deliberately unmapped: Claude Code consumes it locally before
// the request ever reaches ferridex.
var claudeProfileKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":             true,
	"ANTHROPIC_AUTH_TOKEN":           true,
	"ANTHROPIC_API_KEY":              true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL":   true,
	"ANTHROPIC_DEFAULT_SONNET_MODEL": true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":  true,
	"CLAUDE_CODE_SUBAGENT_MODEL":     true,
}

// ClaudeProfileEntry is one raw [name] section: recognized keys only, plus any
// warnings accumulated while parsing that section.
type ClaudeProfileEntry struct {
	Name     string
	Values   map[string]string
	Warnings []string
	Line     int // 1-based line of the [name] header
}

// ClaudeProfilesFile is the parse result for a whole file.
type ClaudeProfilesFile struct {
	Profiles []ClaudeProfileEntry
	Warnings []string
}

// ParseClaudeProfiles parses the profiles file grammar:
//
//	# full-line comment
//	[profile-name]
//	export KEY="VALUE"   (single quotes literal, double quotes stripped, bare trimmed)
//
// Duplicate section names: the last block wins. Keys before any section,
// unknown keys and malformed lines produce warnings and are skipped.
func ParseClaudeProfiles(src []byte) (ClaudeProfilesFile, error) {
	if len(src) > claudeProfilesMaxSize {
		return ClaudeProfilesFile{}, fmt.Errorf("profiles 文件超过 %d 字节上限", claudeProfilesMaxSize)
	}
	text := strings.TrimPrefix(string(src), "\ufeff")
	out := ClaudeProfilesFile{}
	index := map[string]int{}
	var current *ClaudeProfileEntry

	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			name := ""
			if end := strings.IndexByte(line, ']'); end >= 0 {
				name = strings.TrimSpace(line[1:end])
			}
			if name == "" {
				out.Warnings = append(out.Warnings, warnAt(lineNo, "空的档案名称,该段已忽略"))
				current = nil
				continue
			}
			if idx, dup := index[name]; dup {
				out.Warnings = append(out.Warnings, warnAt(lineNo, fmt.Sprintf("档案 %q 重复定义,以最后一次为准", name)))
				// Last definition wins: start the section over, keep old warnings.
				out.Profiles[idx].Values = map[string]string{}
				out.Profiles[idx].Line = lineNo
				current = &out.Profiles[idx]
				continue
			}
			out.Profiles = append(out.Profiles, ClaudeProfileEntry{
				Name:   name,
				Values: map[string]string{},
				Line:   lineNo,
			})
			index[name] = len(out.Profiles) - 1
			current = &out.Profiles[len(out.Profiles)-1]
			continue
		}

		body := line
		if rest, ok := strings.CutPrefix(body, "export "); ok {
			body = strings.TrimSpace(rest)
		}
		key, value, found := strings.Cut(body, "=")
		key = strings.TrimSpace(key)
		if !found || !isEnvKey(key) {
			warn := warnAt(lineNo, fmt.Sprintf("无法识别的行 %q,已忽略", truncateForWarning(line)))
			appendProfileWarning(&out, &current, warn)
			continue
		}
		key = strings.ToUpper(key)
		if _, known := claudeProfileKeys[key]; !known {
			warn := warnAt(lineNo, fmt.Sprintf("未知的配置项 %q,已忽略", key))
			appendProfileWarning(&out, &current, warn)
			continue
		}
		if current == nil {
			out.Warnings = append(out.Warnings, warnAt(lineNo, fmt.Sprintf("%s 出现在任何 [档案名] 之前,已忽略", key)))
			continue
		}
		current.Values[key] = unquoteEnvValue(strings.TrimSpace(value))
	}

	kept := out.Profiles[:0]
	for _, entry := range out.Profiles {
		if len(entry.Values) == 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("档案 %q(第 %d 行)没有可识别的配置项,已忽略", entry.Name, entry.Line))
			delete(index, entry.Name)
			continue
		}
		kept = append(kept, entry)
	}
	out.Profiles = kept
	return out, nil
}

// ClaudeProfileFromValues maps a parsed section onto a validated profile
// config. CLAUDE_CODE_SUBAGENT_MODEL is carried along purely so the dashboard
// can echo it back into generated client configs; the proxy itself never
// consumes it (Claude Code resolves it locally before sending requests).
func ClaudeProfileFromValues(e ClaudeProfileEntry) (ClaudeProfileConfig, error) {
	return NormalizeClaudeProfile(ClaudeProfileConfig{
		Name:          e.Name,
		BaseURL:       e.Values["ANTHROPIC_BASE_URL"],
		AuthToken:     e.Values["ANTHROPIC_AUTH_TOKEN"],
		APIKey:        e.Values["ANTHROPIC_API_KEY"],
		OpusModel:     e.Values["ANTHROPIC_DEFAULT_OPUS_MODEL"],
		SonnetModel:   e.Values["ANTHROPIC_DEFAULT_SONNET_MODEL"],
		HaikuModel:    e.Values["ANTHROPIC_DEFAULT_HAIKU_MODEL"],
		SubagentModel: e.Values["CLAUDE_CODE_SUBAGENT_MODEL"],
	})
}

// DefaultClaudeProfilesTemplate is written to ferridex-profiles.env on first
// start. Every line is a comment so parsing the untouched template yields zero
// profiles and zero warnings.
func DefaultClaudeProfilesTemplate() string {
	return `# ferridex Claude 上游档案
#
# 本文件定义网页「Claude 上游」面板中可一键切换的自定义 Anthropic 上游。
# 语法:[档案名] 分段,段内每行一个 KEY=VALUE,支持 export 前缀与单/双引号;
# 以 # 开头的行为注释。编辑后点击面板中的「重新加载档案」即可生效。
#
# 每个档案可用的字段:
#   ANTHROPIC_BASE_URL              上游 API 根地址(必填,如 https://openrouter.ai/api)
#   ANTHROPIC_AUTH_TOKEN            Bearer 凭据(请求带 Authorization: Bearer <值>)
#   ANTHROPIC_API_KEY               x-api-key 凭据(与 AUTH_TOKEN 同时设置时 Bearer 优先)
#   ANTHROPIC_DEFAULT_OPUS_MODEL    模型映射:请求模型名含 opus 时改写为该值
#   ANTHROPIC_DEFAULT_SONNET_MODEL  模型映射:请求模型名含 sonnet 时改写为该值
#   ANTHROPIC_DEFAULT_HAIKU_MODEL   模型映射:请求模型名含 haiku 时改写为该值
#   CLAUDE_CODE_SUBAGENT_MODEL      仅客户端本地变量,ferridex 不消费,粘贴无副作用
#
# 示例(去掉各行行首的 "# " 即可启用):
# [openrouter]
# ANTHROPIC_BASE_URL="https://openrouter.ai/api"
# ANTHROPIC_AUTH_TOKEN="sk-or-v1-xxxxxxxxxxxxxxxx"
# ANTHROPIC_API_KEY=""
# ANTHROPIC_DEFAULT_SONNET_MODEL="stealth/ox-alpha"
# ANTHROPIC_DEFAULT_OPUS_MODEL="stealth/ox-alpha"
# ANTHROPIC_DEFAULT_HAIKU_MODEL="stealth/ox-alpha"
# CLAUDE_CODE_SUBAGENT_MODEL="stealth/ox-alpha"
#
# 安全提示:启用档案后本文件会保存真实凭据。它已被 .gitignore 忽略,
# 请勿提交到仓库或分享给他人。
`
}

func warnAt(line int, message string) string {
	return fmt.Sprintf("第 %d 行:%s", line, message)
}

func appendProfileWarning(out *ClaudeProfilesFile, current **ClaudeProfileEntry, warning string) {
	if *current != nil {
		(*current).Warnings = append((*current).Warnings, warning)
		return
	}
	out.Warnings = append(out.Warnings, warning)
}

// unquoteEnvValue strips one layer of surrounding quotes without escape
// processing (env-file semantics), so 'a"b' and "a\"b" stay readable.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

func isEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func truncateForWarning(s string) string {
	runes := []rune(s)
	if len(runes) <= 40 {
		return s
	}
	return string(runes[:40]) + "…"
}
