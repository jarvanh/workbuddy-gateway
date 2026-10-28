package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptCompatibilityOptInRewritesSystemAndAssistantText(t *testing.T) {
	t.Cleanup(func() { setPromptCompatibilityConfig(runtimePromptCompatibilityConfig{}) })
	enabled := true
	setPromptCompatibilityConfig(runtimePromptCompatibilityConfig{Enabled: &enabled})
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "Relative paths resolve from the workspace root; keep the rest."},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "Earlier the workspace root was mentioned."},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "workspace root"}},
			}},
			map[string]any{"role": "user", "content": "workspace root must stay in user text"},
			map[string]any{"role": "tool", "content": "workspace root must stay in tool output"},
		},
	}
	if modified := normalizePromptCompatibilityMessages(obj); modified != 2 {
		t.Fatalf("modified messages=%d, want 2", modified)
	}
	messages := obj["messages"].([]any)
	if got := messages[0].(map[string]any)["content"]; got != "Relative paths resolve from the workspace base; keep the rest." {
		t.Fatalf("system content=%q", got)
	}
	blocks := messages[1].(map[string]any)["content"].([]any)
	if got := blocks[0].(map[string]any)["text"]; got != "Earlier the workspace base was mentioned." {
		t.Fatalf("assistant text=%q", got)
	}
	if got := blocks[1].(map[string]any)["image_url"].(map[string]any)["url"]; got != "workspace root" {
		t.Fatalf("non-text block changed: %q", got)
	}
	if got := messages[2].(map[string]any)["content"]; got != "workspace root must stay in user text" {
		t.Fatalf("user content changed: %q", got)
	}
	if got := messages[3].(map[string]any)["content"]; got != "workspace root must stay in tool output" {
		t.Fatalf("tool content changed: %q", got)
	}
}

func TestPromptCompatibilityConfigSupportsArbitraryWordsAndDisable(t *testing.T) {
	t.Cleanup(func() { setPromptCompatibilityConfig(runtimePromptCompatibilityConfig{}) })
	enabled := true
	setPromptCompatibilityConfig(runtimePromptCompatibilityConfig{
		Enabled: &enabled,
		Replacements: map[string]string{
			"secret channel phrase": "neutral phrase",
			"secret":                "neutral",
		},
	})
	got, changed := normalizePromptCompatibilityText("secret channel phrase and secret")
	if got != "neutral phrase and neutral" || changed != 2 {
		t.Fatalf("got=%q changed=%d", got, changed)
	}
	if again, changed := normalizePromptCompatibilityText(got); again != got || changed != 0 {
		t.Fatalf("not idempotent: %q changed=%d", again, changed)
	}
	disabled := false
	setPromptCompatibilityConfig(runtimePromptCompatibilityConfig{Enabled: &disabled})
	if got, changed := normalizePromptCompatibilityText("workspace root"); got != "workspace root" || changed != 0 {
		t.Fatalf("disabled rule changed text: %q %d", got, changed)
	}
}

func TestEnsureRuntimeConfigFileDoesNotAddOptionalPromptCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"gateway":{"apiPort":18317,"webPort":18316},"identity":{"enabled":true,"target":"Keep"},"systemPrompt":{"force":"keep"}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["promptCompatibility"]; ok {
		t.Fatal("optional compatibility must not be injected into an upgraded config")
	}
	c, err := parseRuntimeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptCompatibility.Enabled != nil {
		t.Fatal("absent optional compatibility setting was changed")
	}
	if c.SystemPrompt.Force != "keep" || c.Identity.Target != "Keep" {
		t.Fatal("existing config was overwritten")
	}
}

func TestPromptCompatibilityDefaultsOffAndRespectsIdentityControl(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() {
		setIdentityConfig(runtimeIdentityConfig{})
		setPromptCompatibilityConfig(runtimePromptCompatibilityConfig{})
	})
	for _, tc := range []struct {
		name, config, want string
	}{
		{"identity_disabled", `{"identity":{"enabled":false}}`, "workspace root; are paths the user"},
		{"custom_identity_rule", `{"identity":{"replacements":{"workspace root":"custom base"}}}`, "custom base; are paths the user"},
		{"default_identity", `{}`, "workspace base; are paths that the user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseRuntimeConfig([]byte(tc.config))
			if err != nil {
				t.Fatal(err)
			}
			if err := applyRuntimeConfig(c, false, "identity-control-regression"); err != nil {
				t.Fatal(err)
			}
			obj := map[string]any{"model": "synthetic-model", "max_tokens": 16,
				"messages": []any{map[string]any{"role": "system", "content": "workspace root; are paths the user"}}}
			request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			prepareUpstreamChatRequest(obj, request, 1, "synthetic-model")
			got := obj["messages"].([]any)[0].(map[string]any)["content"]
			if got != tc.want {
				t.Fatalf("effective text=%q want=%q", got, tc.want)
			}
			if enabled, _ := promptCompatibilitySettingsSummary(); enabled {
				t.Fatal("independent compatibility unexpectedly enabled")
			}
		})
	}
}

func TestPromptCompatibilityMigrationPreservesExplicitOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"promptCompatibility":{"enabled":true,"replacements":{"custom phrase":"custom result"}}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.PromptCompatibility.Enabled == nil || !*c.PromptCompatibility.Enabled ||
		c.PromptCompatibility.Replacements["custom phrase"] != "custom result" {
		t.Fatal("explicit optional configuration changed during migration")
	}
}

func TestPromptCompatibilityLeavesLongerRulesFirst(t *testing.T) {
	t.Cleanup(func() { setPromptCompatibilityConfig(runtimePromptCompatibilityConfig{}) })
	enabled := true
	setPromptCompatibilityConfig(runtimePromptCompatibilityConfig{
		Enabled: &enabled,
		Replacements: map[string]string{
			"workspace":      "ws",
			"workspace root": "workspace base",
		},
	})
	got, _ := normalizePromptCompatibilityText("workspace root")
	if got != "ws base" {
		t.Fatalf("longest-first replacement got=%q", got)
	}
	if strings.Contains(got, "workspace root") {
		t.Fatal("longest rule did not run first")
	}
}
