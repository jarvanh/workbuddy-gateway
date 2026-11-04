package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeUsage(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("解析 JSON 失败: %v\n%s", err, raw)
	}
	return m
}

// 复现 2026-11-04 线上事故：上游把真实命中数写在 prompt_cache_hit_tokens 与
// prompt_tokens_details.cached_tokens，顶层别名却是 0。下游 `??` 链读到第一个
// 0 就短路，命中数被记成 0。归一化后别名必须回填为真实值，原字段保持不变。
func TestNormalizeUsageCacheAliasesHy4freeHit(t *testing.T) {
	usage := decodeUsage(t, `{
		"prompt_cache_hit_tokens": 30208,
		"prompt_cache_miss_tokens": 82,
		"prompt_cache_write_tokens": 0,
		"cached_tokens": 0,
		"cache_read_input_tokens": 0,
		"cache_creation_input_tokens": 0,
		"prompt_tokens": 30290,
		"completion_tokens": 16,
		"prompt_tokens_details": {"cached_tokens": 30208}
	}`)
	if !normalizeUsageCacheAliases(usage) {
		t.Fatal("命中样本必须发生别名回填")
	}
	for _, key := range []string{"cached_tokens", "cache_read_input_tokens"} {
		if v, ok := usageNumber(usage, key); !ok || v != 30208 {
			t.Errorf("别名 %s 应回填为 30208，实际=%v", key, usage[key])
		}
	}
	// 权威字段不删不改，网关自身统计与其他客户端仍可读。
	if v, _ := usageNumber(usage, "prompt_cache_hit_tokens"); v != 30208 {
		t.Errorf("prompt_cache_hit_tokens 不应被改动，实际=%d", v)
	}
	// 真 miss（写侧 0）不能凭空造出缓存创建。
	if v, _ := usageNumber(usage, "cache_creation_input_tokens"); v != 0 {
		t.Errorf("cache_creation_input_tokens 应保持 0，实际=%d", v)
	}
}

// 只有嵌套 details 给出命中数时也要回填。
func TestNormalizeUsageCacheAliasesNestedOnly(t *testing.T) {
	usage := decodeUsage(t, `{"prompt_tokens_details":{"cached_tokens":8192},"cache_read_input_tokens":0}`)
	if !normalizeUsageCacheAliases(usage) {
		t.Fatal("嵌套字段应触发回填")
	}
	if v, _ := usageNumber(usage, "cached_tokens"); v != 8192 {
		t.Errorf("cached_tokens=%v want 8192", usage["cached_tokens"])
	}
}

// 别名已有正值时不覆盖；全 0 的真 miss 是 no-op。
func TestNormalizeUsageCacheAliasesNoOpCases(t *testing.T) {
	t.Run("alias already positive", func(t *testing.T) {
		usage := decodeUsage(t, `{"cached_tokens":9000,"cache_read_input_tokens":9000,"prompt_cache_hit_tokens":9001}`)
		if normalizeUsageCacheAliases(usage) {
			t.Fatal("别名已有正值不应改写")
		}
		if v, _ := usageNumber(usage, "cached_tokens"); v != 9000 {
			t.Errorf("cached_tokens 被覆盖为 %d", v)
		}
	})
	t.Run("real miss", func(t *testing.T) {
		usage := decodeUsage(t, `{"prompt_cache_hit_tokens":0,"cached_tokens":0,"cache_read_input_tokens":0,"prompt_tokens":19563}`)
		if normalizeUsageCacheAliases(usage) {
			t.Fatal("真 miss 不应改写")
		}
	})
	t.Run("nil", func(t *testing.T) {
		if normalizeUsageCacheAliases(nil) {
			t.Fatal("nil usage 不应改写")
		}
	})
}

// 写侧：缓存创建 token 也要避免被 0 别名吞掉，且与读侧互不串台。
func TestNormalizeUsageCacheAliasesCreation(t *testing.T) {
	usage := decodeUsage(t, `{"prompt_cache_write_tokens":4096,"cache_creation_input_tokens":0,"cached_tokens":0}`)
	if !normalizeUsageCacheAliases(usage) {
		t.Fatal("写侧应触发回填")
	}
	if v, _ := usageNumber(usage, "cache_creation_input_tokens"); v != 4096 {
		t.Errorf("cache_creation_input_tokens=%v want 4096", usage["cache_creation_input_tokens"])
	}
	if v, _ := usageNumber(usage, "cached_tokens"); v != 0 {
		t.Errorf("读侧不应被写侧带出命中，实际=%d", v)
	}
}

// 幂等：重复调用不产生二次改写。
func TestNormalizeUsageCacheAliasesIdempotent(t *testing.T) {
	usage := decodeUsage(t, `{"prompt_cache_hit_tokens":1234,"cached_tokens":0}`)
	if !normalizeUsageCacheAliases(usage) {
		t.Fatal("首次应回填")
	}
	if normalizeUsageCacheAliases(usage) {
		t.Fatal("第二次调用应是 no-op")
	}
}

// 流式分片出口：cleanChunkJSON 必须把归一化后的 usage 一起序列化回去。
func TestCleanChunkJSONBackfillsCacheAliases(t *testing.T) {
	chunk := `{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_cache_hit_tokens":30208,"cached_tokens":0,"cache_read_input_tokens":0,"prompt_tokens_details":{"cached_tokens":30208}}}`
	cleaned := cleanChunkJSON(chunk)
	if !strings.Contains(cleaned, `"cached_tokens":30208`) || !strings.Contains(cleaned, `"cache_read_input_tokens":30208`) {
		t.Fatalf("流式分片未回填缓存别名: %s", cleaned)
	}
	if !strings.Contains(cleaned, `"prompt_cache_hit_tokens":30208`) {
		t.Fatalf("权威字段被意外删除: %s", cleaned)
	}
}

// 端到端语义回归：模拟 OmniRoute 实际使用的 `??` 读取链
//
//	cacheRead ?? cached_tokens ?? cache_read_input_tokens
//	  ?? prompt_tokens_details.cached_tokens ?? 0
//
// JS 的 `??` 只在 null/undefined 时继续，0 是有效值会立即返回。
// 归一化前恒得 0（线上事故），归一化后必须得真实命中数。
func TestNormalizeUsageCacheAliasesDownstreamNullishChain(t *testing.T) {
	// 从干净的上游 usage 出发，走完整链路：解析 → 归一化 → 流式序列化 → 下游读取。
	chunk := `{"choices":[],"usage":{"prompt_cache_hit_tokens":30208,"cached_tokens":0,"cache_read_input_tokens":0,"prompt_tokens_details":{"cached_tokens":30208}}}`
	raw := decodeUsage(t, chunk)["usage"].(map[string]any)

	nullish := func(u map[string]any) int64 {
		for _, key := range []string{"cacheRead", "cached_tokens", "cache_read_input_tokens"} {
			if v, ok := usageNumber(u, key); ok {
				return v // `??` 拿到非 null 就停，0 也算有效值
			}
		}
		if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
			if v, ok := usageNumber(d, "cached_tokens"); ok {
				return v
			}
		}
		return 0
	}

	if got := nullish(raw); got != 0 {
		t.Fatalf("归一化前下游应读到 0（复现事故），实际=%d", got)
	}
	normalizeUsageCacheAliases(raw)
	if got := nullish(raw); got != 30208 {
		t.Fatalf("归一化后下游应读到 30208，实际=%d", got)
	}
}
