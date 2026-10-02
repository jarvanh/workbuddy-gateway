package main

// warmup 连续限流熔断的测试。
//
// 背景：6004 这类模型级限流往往整个补触发窗口都不恢复。旧实现没有重试上限，
// 一个卡住的模型会刷满整个窗口（实测 hunyuan-2.0-instruct 连续 13 轮全部无效），
// 白白消耗真实的上游请求。现在达到 MaxRateLimitRetries 即放弃本周期。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// warmupRateLimitServer 起一个恒定返回 6004 限流的假上游。
func warmupRateLimitServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"data":{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-30 09:00:00 UTC+8 重置，您也可以切换其他模型继续使用。"}}}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// warmupRateLimitFixture 装配：假上游 + 单账号 + 免费目录 + 指定熔断阈值。
func warmupRateLimitFixture(t *testing.T, maxRetries int) warmupRuntime {
	t.Helper()
	resetWarmupStateT(t)
	chdirTemp(t)

	server := warmupRateLimitServer(t)

	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-free", HasMultiplier: true, Multiplier: 0, FromLive: true}},
	}
	modelsMu.Unlock()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accounts, rrIndex = []*Account{{
		Path: "cn.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
		QuotaKnown: true, QuotaRemaining: 100,
	}}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()

	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})

	rt := warmupSnapshot()
	rt.MaxRateLimitRetries = maxRetries
	return rt
}

// 连续被限流达到阈值后应放弃本周期（failed），而不是一直 pending 刷到窗口结束。
func TestWarmupGivesUpAfterRepeatedRateLimit(t *testing.T) {
	rt := warmupRateLimitFixture(t, 3)
	now := time.Now()

	// 第 1、2 次：未达阈值，保留 pending 让补触发窗口内再试。
	for i := 1; i <= 2; i++ {
		_, _, _, pending, _ := runWarmupOnce(rt, now)
		if pending != 1 {
			t.Fatalf("第 %d 次限流后应保留 1 个待补触发，实际=%d", i, pending)
		}
		st := loadWarmupState()
		if got := st.Current.Models["m-free"].Status; got != "pending" {
			t.Fatalf("第 %d 次限流后状态应为 pending，实际=%s", i, got)
		}
		if got := st.Current.Models["m-free"].RateLimitHits; got != i {
			t.Fatalf("第 %d 次限流后 RateLimitHits 应为 %d，实际=%d", i, i, got)
		}
	}

	// 第 3 次：达到阈值，判本周期失败。
	_, failed, _, pending, _ := runWarmupOnce(rt, now)
	if pending != 0 {
		t.Fatalf("达阈值后不应再有待补触发，实际=%d", pending)
	}
	if failed != 1 {
		t.Fatalf("达阈值后应记 1 个失败，实际=%d", failed)
	}
	st := loadWarmupState()
	ms := st.Current.Models["m-free"]
	if ms.Status != "failed" {
		t.Fatalf("达阈值后状态应为 failed，实际=%s", ms.Status)
	}
	if ms.RateLimitHits < 3 {
		t.Errorf("RateLimitHits 应累计到 3，实际=%d", ms.RateLimitHits)
	}
}

// 阈值 <=0 表示不启用熔断：连续限流也一直保留待补触发（旧行为）。
func TestWarmupRateLimitCircuitDisabledWhenZero(t *testing.T) {
	rt := warmupRateLimitFixture(t, 0)
	now := time.Now()

	for i := 1; i <= 4; i++ {
		_, _, _, pending, _ := runWarmupOnce(rt, now)
		if pending != 1 {
			t.Fatalf("熔断关闭时第 %d 次限流后仍应保留 1 个待补触发，实际=%d", i, pending)
		}
	}
	st := loadWarmupState()
	if got := st.Current.Models["m-free"].Status; got != "pending" {
		t.Fatalf("熔断关闭时应始终 pending，实际=%s", got)
	}
}

// 默认值应启用熔断，阈值为 3。
func TestWarmupDefaultMaxRateLimitRetries(t *testing.T) {
	resetWarmupStateT(t)
	if warmupDefaultMaxRateLimitRetries != 3 {
		t.Fatalf("默认熔断阈值应为 3，实际=%d", warmupDefaultMaxRateLimitRetries)
	}
	// 未配置时应装载到生效 runtime。
	setWarmup(warmupConfig{})
	rt := warmupSnapshot()
	if rt.MaxRateLimitRetries != warmupDefaultMaxRateLimitRetries {
		t.Fatalf("未配置时应回落默认阈值 %d，实际=%d", warmupDefaultMaxRateLimitRetries, rt.MaxRateLimitRetries)
	}

	// 显式配置应生效，含「显式 0 = 关闭熔断」。
	zero := 0
	setWarmup(warmupConfig{MaxRateLimitRetries: &zero})
	if rt := warmupSnapshot(); rt.MaxRateLimitRetries != 0 {
		t.Fatalf("显式 0 应关闭熔断，实际=%d", rt.MaxRateLimitRetries)
	}
	five := 5
	setWarmup(warmupConfig{MaxRateLimitRetries: &five})
	if rt := warmupSnapshot(); rt.MaxRateLimitRetries != 5 {
		t.Fatalf("显式 5 应生效，实际=%d", rt.MaxRateLimitRetries)
	}
	// 还原默认，避免污染其他用例。
	setWarmup(warmupConfig{})
}

// 成功触发后应清零限流计数：只统计「连续」被限流。
func TestWarmupRateLimitHitsResetOnSuccess(t *testing.T) {
	resetWarmupStateT(t)
	chdirTemp(t)

	// 先限流一次，再成功：计数应归零。
	var limited bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if !limited {
			limited = true
			_, _ = w.Write([]byte("data: {\"error\":{\"data\":{\"code\":6004}}}\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"total_tokens\":500,\"credit\":0}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-free", HasMultiplier: true, Multiplier: 0, FromLive: true}},
	}
	modelsMu.Unlock()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accounts, rrIndex = []*Account{{
		Path: "cn.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
		QuotaKnown: true, QuotaRemaining: 100,
	}}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	rt := warmupSnapshot()
	rt.MaxRateLimitRetries = 3
	now := time.Now()
	runWarmupOnce(rt, now)
	runWarmupOnce(rt, now) // 第二次走成功分支

	st := loadWarmupState()
	ms := st.Current.Models["m-free"]
	if ms == nil {
		t.Fatal("缺少模型状态")
	}
	if ms.Status == "ok" && ms.RateLimitHits != 0 {
		t.Errorf("成功后应清零限流计数，实际=%d", ms.RateLimitHits)
	}
}
