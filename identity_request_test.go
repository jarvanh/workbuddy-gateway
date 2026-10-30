package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIdentityIsOnlyConfigurableReplacementInOutboundRequests(t *testing.T) {
	const original = "Codex; workspace root; are paths the user; legacy phrase"
	for _, tc := range []struct {
		name, config, want string
	}{
		{"defaults", `{}`, "WorkBuddy; workspace base; are paths that the user; legacy phrase"},
		{"disabled", `{"identity":{"enabled":false}}`, original},
		{"custom", `{"identity":{"replacements":{"workspace root":"custom base","legacy phrase":"identity result"}}}`,
			"Codex; custom base; are paths the user; identity result"},
		{"retired_enabled_cannot_override_disabled", `{"identity":{"enabled":false},"promptCompatibility":{"enabled":true,"replacements":{"workspace root":"legacy result","legacy phrase":"legacy result"}}}`, original},
		{"retired_enabled_cannot_override_custom", `{"identity":{"replacements":{"workspace root":"custom base"}},"promptCompatibility":{"enabled":true,"replacements":{"workspace root":"legacy result","custom base":"legacy result","legacy phrase":"legacy result"}}}`,
			"Codex; custom base; are paths the user; legacy phrase"},
		{"retired_enabled_does_not_add_rules", `{"promptCompatibility":{"enabled":true,"replacements":{"legacy phrase":"legacy result"}}}`,
			"WorkBuddy; workspace base; are paths that the user; legacy phrase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedGatewayConfig(t)
			t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
			c, err := parseRuntimeConfig([]byte(tc.config))
			if err != nil {
				t.Fatal(err)
			}
			if err := applyRuntimeConfig(c, false, "identity-only-test"); err != nil {
				t.Fatal(err)
			}
			obj := map[string]any{
				"model": "synthetic-model", "max_tokens": 16,
				"messages": []any{
					map[string]any{"role": "developer", "content": original},
					map[string]any{"role": "user", "content": original},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "text", "text": original},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": original}},
					}, "reasoning_content": original, "tool_calls": []any{
						map[string]any{"id": "call_identity_test", "type": "function", "function": map[string]any{
							"name": "test_tool", "arguments": `{"text":"` + original + `"}`,
						}},
					}},
					map[string]any{"role": "tool", "tool_call_id": "call_identity_test", "content": original},
				},
				"tools": []any{map[string]any{"type": "function", "function": map[string]any{
					"name": "test_tool", "description": original, "parameters": map[string]any{
						"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string", "description": original}},
					},
				}}},
			}
			protected, _ := json.Marshal(obj["tools"])
			request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			request.Header.Set("X-Trace-ID", "identity-only-test")
			prepareUpstreamChatRequest(obj, request, 1, "synthetic-model")
			messages := obj["messages"].([]any)
			if got := messages[0].(map[string]any)["content"]; got != tc.want {
				t.Fatalf("system text=%q want %q", got, tc.want)
			}
			assistant := messages[2].(map[string]any)
			blocks := assistant["content"].([]any)
			if got := blocks[0].(map[string]any)["text"]; got != tc.want {
				t.Fatalf("assistant text=%q want %q", got, tc.want)
			}
			if got := blocks[1].(map[string]any)["image_url"].(map[string]any)["url"]; got != original {
				t.Fatal("image block was rewritten")
			}
			if messages[1].(map[string]any)["content"] != original || messages[3].(map[string]any)["content"] != original {
				t.Fatal("user text or tool result was rewritten")
			}
			if assistant["reasoning_content"] != original {
				t.Fatal("reasoning text was rewritten")
			}
			call := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
			if call["arguments"] != `{"text":"`+original+`"}` {
				t.Fatal("tool arguments were rewritten")
			}
			after, _ := json.Marshal(obj["tools"])
			if !bytes.Equal(protected, after) {
				t.Fatal("tool definitions were rewritten")
			}
		})
	}
}

func TestRetiredCompatibilityConfigDoesNotBlockStartupOrMigration(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := map[string]any{"enabled": true, "replacements": map[string]any{"legacy phrase": "legacy result"}}
	raw, _ := json.Marshal(map[string]any{"promptCompatibility": legacy, "systemPrompt": map[string]any{"force": "keep"}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeObject(t, migrated)
	if !reflect.DeepEqual(doc["promptCompatibility"], legacy) {
		t.Fatal("startup must not delete or silently move old custom rules")
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatalf("old field must not prevent startup: %v", err)
	}
	if text, _ := normalizeAssistantIdentityText("legacy phrase"); text != "legacy phrase" {
		t.Fatal("old rules must not be silently merged into identity")
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(migrated, again) {
		t.Fatal("complete migrated config must not be rewritten repeatedly")
	}
	if _, err := parseRuntimeConfig([]byte(`{"promptCompatibility":{},"unknownSetting":true}`)); err == nil {
		t.Fatal("retired-field compatibility must not disable validation of other unknown fields")
	}
}

func TestNewAndUpgradedConfigsDoNotAddRetiredCompatibility(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "upgrade"}[existing], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if existing {
				if err := os.WriteFile(path, []byte(`{"identity":{"target":"Keep"},"systemPrompt":{"force":"keep"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ensureRuntimeConfigFile(path); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			doc := decodeObject(t, raw)
			if _, exists := doc["promptCompatibility"]; exists {
				t.Fatal("removed field must not be generated")
			}
			identity := doc["identity"].(map[string]any)
			if len(identity["replacements"].(map[string]any)) != 7 {
				t.Fatal("all seven identity defaults must still be generated")
			}
			if existing && (identity["target"] != "Keep" || doc["systemPrompt"].(map[string]any)["force"] != "keep") {
				t.Fatal("existing settings were overwritten")
			}
		})
	}
}

func TestRetiredCompatibilityWarningAndIdentityHotReload(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	oldHash, oldHasHash := runtimeAppliedHash, runtimeHasAppliedHash
	t.Cleanup(func() { runtimeAppliedHash, runtimeHasAppliedHash = oldHash, oldHasHash })
	var output bytes.Buffer
	writer := log.Writer()
	log.SetOutput(io.MultiWriter(writer, &output))
	t.Cleanup(func() { log.SetOutput(writer) })
	const privateRule = "synthetic-private-phrase-never-log"
	watcher := runtimeConfigWatcher{}
	for i, tc := range []struct {
		config, want string
		warn         bool
	}{
		{`{"identity":{"enabled":false},"promptCompatibility":{"enabled":true,"replacements":{"` + privateRule + `":"custom"}}}`, "workspace root", true},
		{`{"identity":{"replacements":{"workspace root":"custom base"}},"promptCompatibility":null}`, "custom base", true},
		{`{"identity":{}}`, "workspace base", false},
	} {
		output.Reset()
		if err := os.WriteFile(runtimeConfigFile, []byte(tc.config), 0600); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatal(err)
			}
		} else if !watcher.reload() || watcher.lastError != nil {
			t.Fatalf("valid update must hot reload: %v", watcher.lastError)
		}
		// Startup repeats this after initializing file logging; hot reload uses
		// the same function with the request trace from applyRuntimeConfig.
		logIdentityRuntimeConfig("identity-reload-test")
		if text, _ := normalizeAssistantIdentityText("workspace root"); text != tc.want {
			t.Fatalf("reloaded text=%q want %q", text, tc.want)
		}
		hasWarning := strings.Contains(output.String(), "[废弃配置] traceId=identity-reload-test")
		if hasWarning != tc.warn {
			t.Fatalf("retired-field warning=%t want %t", hasWarning, tc.warn)
		}
		if strings.Contains(output.String(), privateRule) {
			t.Fatal("retired custom rule leaked into normal logs")
		}
	}
	output.Reset()
	if err := os.WriteFile(runtimeConfigFile, []byte(`{"identity":{"enabled":false},"unknownSetting":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if watcher.reload() || watcher.lastError == nil {
		t.Fatal("unknown configuration must still reject hot reload")
	}
	if text, _ := normalizeAssistantIdentityText("workspace root"); text != "workspace base" {
		t.Fatal("rejected reload must preserve the last working identity settings")
	}
	if !strings.Contains(output.String(), "结果=拒绝") {
		t.Fatal("rejected reload must have an audit entry")
	}
}
