package main

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// runtimePromptCompatibilityConfig controls neutral rewrites applied to
// system/developer/assistant text before it is sent upstream. It is separate
// from identity replacements: identity controls visible assistant names, while
// this list exists for upstream channel-verification phrases that may change
// without a gateway release.
type runtimePromptCompatibilityConfig struct {
	Enabled      *bool             `json:"enabled,omitempty"`
	Replacements map[string]string `json:"replacements,omitempty"`
}

type promptCompatibilityRule struct {
	Old string
	New string
}

type promptCompatibilitySettings struct {
	enabled bool
	rules   []promptCompatibilityRule
}

// Optional wording rules. They require explicit opt-in and do not guarantee
// acceptance by any upstream policy. Default rewrites belong to identity.
var defaultPromptCompatibilityReplacements = map[string]string{
	"workspace root": "workspace base",
}

var (
	promptCompatibilityMu      sync.RWMutex
	currentPromptCompatibility = defaultPromptCompatibilitySettings()
)

func defaultPromptCompatibilitySettings() promptCompatibilitySettings {
	return buildPromptCompatibilitySettings(runtimePromptCompatibilityConfig{})
}

func defaultPromptCompatibilityConfig() runtimePromptCompatibilityConfig {
	enabled := false
	replacements := make(map[string]string, len(defaultPromptCompatibilityReplacements))
	for old, newValue := range defaultPromptCompatibilityReplacements {
		replacements[old] = newValue
	}
	return runtimePromptCompatibilityConfig{Enabled: &enabled, Replacements: replacements}
}

func buildPromptCompatibilitySettings(c runtimePromptCompatibilityConfig) promptCompatibilitySettings {
	enabled := false
	if c.Enabled != nil {
		enabled = *c.Enabled
	}
	replacements := c.Replacements
	if len(replacements) == 0 {
		replacements = defaultPromptCompatibilityReplacements
	}
	rules := make([]promptCompatibilityRule, 0, len(replacements))
	for old, newValue := range replacements {
		if strings.TrimSpace(old) == "" || old == newValue {
			continue
		}
		rules = append(rules, promptCompatibilityRule{Old: old, New: newValue})
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if len(rules[i].Old) != len(rules[j].Old) {
			return len(rules[i].Old) > len(rules[j].Old)
		}
		return rules[i].Old < rules[j].Old
	})
	return promptCompatibilitySettings{enabled: enabled, rules: rules}
}

func setPromptCompatibilityConfig(c runtimePromptCompatibilityConfig) {
	settings := buildPromptCompatibilitySettings(c)
	promptCompatibilityMu.Lock()
	currentPromptCompatibility = settings
	promptCompatibilityMu.Unlock()
}

func promptCompatibilitySnapshot() promptCompatibilitySettings {
	promptCompatibilityMu.RLock()
	snapshot := currentPromptCompatibility
	snapshot.rules = append([]promptCompatibilityRule(nil), currentPromptCompatibility.rules...)
	promptCompatibilityMu.RUnlock()
	return snapshot
}

func promptCompatibilitySettingsSummary() (bool, int) {
	settings := promptCompatibilitySnapshot()
	return settings.enabled, len(settings.rules)
}

func normalizePromptCompatibilityText(text string) (string, int) {
	settings := promptCompatibilitySnapshot()
	if !settings.enabled {
		return text, 0
	}
	changed := 0
	for _, rule := range settings.rules {
		if !strings.Contains(text, rule.Old) {
			continue
		}
		changed += strings.Count(text, rule.Old)
		text = strings.ReplaceAll(text, rule.Old, rule.New)
	}
	return text, changed
}

// normalizePromptCompatibilityMessages applies the configured neutral rewrites
// to system/developer/assistant text only. User text, tool descriptions, tool
// arguments and reasoning fields intentionally remain byte-for-byte unchanged.
func normalizePromptCompatibilityMessages(obj map[string]any) int {
	messages, _ := obj["messages"].([]any)
	modified := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch roleOfMessage(message) {
		case "system", "developer", "assistant":
		default:
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if rewritten, changed := normalizePromptCompatibilityText(content); changed > 0 {
				message["content"] = rewritten
				modified++
			}
		case []any:
			messageChanged := false
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if part["type"] != "text" {
					continue
				}
				text, ok := part["text"].(string)
				if !ok {
					continue
				}
				if rewritten, changed := normalizePromptCompatibilityText(text); changed > 0 {
					part["text"] = rewritten
					messageChanged = true
				}
			}
			if messageChanged {
				modified++
			}
		}
	}
	return modified
}

func applyPromptCompatibility(obj map[string]any, r *http.Request, requestID uint64) int {
	modified := normalizePromptCompatibilityMessages(obj)
	if modified <= 0 {
		return 0
	}
	enabled, ruleCount := promptCompatibilitySettingsSummary()
	trace := debugTraceID(r)
	if trace == "" {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[提示词兼容替换] traceId=%s requestId=%d 启用=%t 规则数=%d 修改消息数=%d 结果=已等义替换已知渠道校验词 说明=仅处理 system/developer/assistant 文本，不记录提示词正文",
		trace, requestID, enabled, ruleCount, modified)
	debugEvent(r, "info", "prompt_compatibility_sanitized", map[string]any{
		"rule_count": ruleCount, "modified_messages": modified,
		"result": "rewritten", "prompt_body_logged": false,
		"business_impact": "避免上游新增渠道校验词导致 11128；不修改 user/tool 内容",
	})
	return modified
}
