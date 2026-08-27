package provider

// Parsing for codex_provider.env: an INI-like env file whose [supplier]
// sections define switchable OpenAI Responses-compatible upstreams.

import (
	"fmt"
	"strings"
)

const codexProfilesMaxSize = 256 << 10

var codexProfileKeys = map[string]bool{
	"OPENAI_BASE_URL":      true,
	"OPENAI_API_KEY":       true,
	"OPENAI_DEFAULT_MODEL": true,
}

type CodexProfileEntry struct {
	Name     string
	Values   map[string]string
	Warnings []string
	Line     int
}

type CodexProfilesFile struct {
	Profiles []CodexProfileEntry
	Warnings []string
}

// ParseCodexProfiles accepts comments, optional export prefixes, quoted or
// bare values, and multiple [supplier] sections. Duplicate suppliers are
// replaced by their last definition; malformed content degrades to warnings.
func ParseCodexProfiles(src []byte) (CodexProfilesFile, error) {
	if len(src) > codexProfilesMaxSize {
		return CodexProfilesFile{}, fmt.Errorf("供应商文件超过 %d 字节上限", codexProfilesMaxSize)
	}
	text := strings.TrimPrefix(string(src), "\ufeff")
	out := CodexProfilesFile{}
	index := map[string]int{}
	var current *CodexProfileEntry

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
				out.Warnings = append(out.Warnings, warnAt(lineNo, "空的供应商名称,该段已忽略"))
				current = nil
				continue
			}
			if idx, duplicate := index[name]; duplicate {
				out.Warnings = append(out.Warnings, warnAt(lineNo, fmt.Sprintf("供应商 %q 重复定义,以最后一次为准", name)))
				out.Profiles[idx].Values = map[string]string{}
				out.Profiles[idx].Line = lineNo
				current = &out.Profiles[idx]
				continue
			}
			out.Profiles = append(out.Profiles, CodexProfileEntry{
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
			// Never include malformed source text in API-visible warnings: it
			// may contain a credential pasted without the equals sign.
			warning := warnAt(lineNo, "无法识别的配置行,已忽略")
			appendCodexProfileWarning(&out, &current, warning)
			continue
		}
		key = strings.ToUpper(key)
		if !codexProfileKeys[key] {
			warning := warnAt(lineNo, fmt.Sprintf("未知的配置项 %q,已忽略", key))
			appendCodexProfileWarning(&out, &current, warning)
			continue
		}
		if current == nil {
			out.Warnings = append(out.Warnings, warnAt(lineNo, fmt.Sprintf("%s 出现在任何 [供应商名] 之前,已忽略", key)))
			continue
		}
		current.Values[key] = unquoteEnvValue(strings.TrimSpace(value))
	}

	kept := out.Profiles[:0]
	for _, entry := range out.Profiles {
		if len(entry.Values) == 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("供应商 %q(第 %d 行)没有可识别的配置项,已忽略", entry.Name, entry.Line))
			continue
		}
		kept = append(kept, entry)
	}
	out.Profiles = kept
	return out, nil
}

func CodexProfileFromValues(entry CodexProfileEntry) (CustomResponsesConfig, error) {
	return normalizeCustomResponsesConfig(CustomResponsesConfig{
		Name:         entry.Name,
		BaseURL:      entry.Values["OPENAI_BASE_URL"],
		APIKey:       entry.Values["OPENAI_API_KEY"],
		DefaultModel: entry.Values["OPENAI_DEFAULT_MODEL"],
	})
}

func DefaultCodexProfilesTemplate() string {
	return `# ferridex Codex 上游供应商
#
# 本文件定义网页「Codex 上游」面板中可一键切换的 OpenAI Responses 兼容上游。
# 语法:[供应商名] 分段,段内每行一个 KEY=VALUE,支持 export 前缀与单/双引号;
# 以 # 开头的行为注释。编辑后点击面板中的「重新加载」即可生效。
#
# 每个供应商需要以下字段:
#   OPENAI_BASE_URL       API 根地址(必填,通常以 /v1 结尾,不要包含 /responses)
#   OPENAI_API_KEY        上游 API Key(必填)
#   OPENAI_DEFAULT_MODEL  默认模型(必填)
#
# 示例(去掉各行行首的 "# " 即可启用):
# [openrouter]
# OPENAI_BASE_URL="https://openrouter.ai/api/v1"
# OPENAI_API_KEY="sk-or-v1-xxxxxxxxxxxxxxxx"
# OPENAI_DEFAULT_MODEL="openai/gpt-5.6"
#
# 安全提示:启用供应商后本文件会保存真实凭据。它已被 .gitignore 忽略,
# 请勿提交到仓库或分享给他人。
`
}

func appendCodexProfileWarning(out *CodexProfilesFile, current **CodexProfileEntry, warning string) {
	if *current != nil {
		(*current).Warnings = append((*current).Warnings, warning)
		return
	}
	out.Warnings = append(out.Warnings, warning)
}
