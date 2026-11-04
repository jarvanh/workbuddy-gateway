package main

// usagecache.go —— 缓存命中 usage 别名归一化（跨客户端协议兼容层）
//
// 背景（2026-11-04 实测）：上游 www.workbuddy.cn 的 hy4free 命中前缀缓存时，把真实
// 命中数写进 prompt_cache_hit_tokens 与 prompt_tokens_details.cached_tokens，却把
// OpenAI / Anthropic 常用别名 cached_tokens / cache_read_input_tokens 恒置为 0。
// 下游 OmniRoute 用 `??` 链读取命中数：
//
//	cacheRead ?? cached_tokens ?? cache_read_input_tokens
//	  ?? prompt_tokens_details.cached_tokens ?? input_tokens_details.cached_tokens ?? 0
//
// JS 的 `??` 只在 null/undefined 时继续，0 会被当成「有效值」立即返回 —— 于是恒读到
// 第一个别名 0，真实命中数永不被采纳：call_logs.tokens_cache_read 全 0、统计口径
// 显示「零缓存」，而上游其实一直在命中（同前缀第二次 prompt_cache_hit_tokens=30208）。
// 缓存写入侧 cache_creation_input_tokens 被同样的 0 别名挡住，问题对称。
//
// 修法：在网关把 usage 转发/聚合给客户端前，只在「别名为 0 或缺失、而权威字段 > 0」时，
// 把权威值回填到别名。别名已有正值、或权威值本身为 0（真 miss）时一律不动 —— 不影响
// 其他上游，也不伪造命中。权威字段本身保留不删，网关自身统计与其他客户端仍可照常读取。

// usageAuthoritativeCache 取一组缓存字段里第一个 > 0 的值作为权威值。
// primary 是顶层键，nested 是「嵌套对象键 + 内部键」，两者按给定顺序扫描。
func usageAuthoritativeCache(usage map[string]any, primary []string, nested [][2]string) int64 {
	for _, key := range primary {
		if v, ok := usageNumber(usage, key); ok && v > 0 {
			return v
		}
	}
	for _, pair := range nested {
		m, ok := usage[pair[0]].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := usageNumber(m, pair[1]); ok && v > 0 {
			return v
		}
	}
	return 0
}

// backfillUsageAlias 仅在别名缺失或为 0（含非数值脏值）时写入权威值。
// 返回是否真的改写，供调用方决定要不要记诊断日志。
func backfillUsageAlias(usage map[string]any, key string, real int64) bool {
	if cur, ok := usageNumber(usage, key); ok && cur > 0 {
		return false
	}
	usage[key] = real
	return true
}

// normalizeUsageCacheAliases 就地归一化 usage 的缓存读/写别名。
// 幂等：重复调用不会二次改写；对不含缓存字段的 usage 是零副作用 no-op。
func normalizeUsageCacheAliases(usage map[string]any) bool {
	if usage == nil {
		return false
	}
	changed := false

	// 读侧：权威值优先取腾讯系 prompt_cache_hit_tokens，其次标准别名与嵌套 details。
	if real := usageAuthoritativeCache(usage,
		[]string{"prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens", "cacheRead"},
		[][2]string{
			{"prompt_tokens_details", "cached_tokens"},
			{"input_tokens_details", "cached_tokens"},
		}); real > 0 {
		for _, key := range []string{"cached_tokens", "cache_read_input_tokens"} {
			if backfillUsageAlias(usage, key, real) {
				changed = true
			}
		}
	}

	// 写侧：与读侧对称处理缓存创建 token。hy4free 恒为 0（上游不建块），
	// 但 deepseek 等模型会给出真实写入值，避免它们的命中数同样被 0 别名吞掉。
	if real := usageAuthoritativeCache(usage,
		[]string{"prompt_cache_write_tokens", "cache_creation_input_tokens", "cacheCreation"},
		[][2]string{
			{"prompt_tokens_details", "cache_creation_tokens"},
			{"input_tokens_details", "cache_creation_tokens"},
		}); real > 0 {
		if backfillUsageAlias(usage, "cache_creation_input_tokens", real) {
			changed = true
		}
	}
	return changed
}
