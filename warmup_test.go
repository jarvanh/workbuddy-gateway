package main

// warmup_test.go —— 5 小时窗口主动触发（warmup.go）单元测试。
//
// 覆盖：
//  1. 配置装载与默认值（默认开启 / 04:00 / 上限 0.06 / 跟随 warmup）；
//  2. 价格筛选语义（上限 0 = 只触发免费模型；未知价格一律不触发）；
//  3. 显式指定模型优先于价格筛选；
//  4. 触发结果落盘与状态流转（pending → ok / failed / skipped）；
//  5. 失败后保留 pending 供补触发；授权失效判定为结构性失败不再重试；
//  6. 管理接口的回环限制。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// resetWarmupStateT 清理 warmup 相关全局状态，避免用例互相污染。
func resetWarmupStateT(t *testing.T) {
	t.Helper()
	setWarmup(warmupConfig{})
	setProbeSchedule(probeScheduleConfig{})
	setRouting(defaultRoutingConfig())
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{}
	modelProbes = map[string]modelPriceProbe{}
	dynamicSource = ""
	modelsMu.Unlock()
	resetRoutingSpend()
	warmupMu.Lock()
	probeScheduleDate = ""
	warmupMu.Unlock()
}

func TestWarmupDefaults(t *testing.T) {
	resetWarmupStateT(t)
	rt := warmupSnapshot()
	if !rt.Enabled {
		t.Fatal("warmup 默认应开启")
	}
	if rt.Time != "04:00" {
		t.Fatalf("默认触发时刻应为 04:00，实际=%s", rt.Time)
	}
	if rt.MaxPrice != 0.06 {
		t.Fatalf("默认价格上限应为 0.06，实际=%v", rt.MaxPrice)
	}
	if rt.CatchUpHours != 4 || rt.RetryMinutes != 10 {
		t.Fatalf("补触发默认值不符: catchUp=%d retry=%d", rt.CatchUpHours, rt.RetryMinutes)
	}
	if rt.Notify {
		t.Fatal("结果通知默认应关闭")
	}
	pr := probeScheduleSnapshot()
	if !pr.Enabled || !pr.FollowWarmup || pr.Time != "06:00" {
		t.Fatalf("主动探测默认值不符: %+v", pr)
	}
}

func TestWarmupConfigOverride(t *testing.T) {
	resetWarmupStateT(t)
	enabled := false
	price := 0.0
	hours := 2
	minutes := 5
	notify := true
	setWarmup(warmupConfig{
		Enabled: &enabled, Time: "05:30", MaxPrice: &price,
		Models: []string{"hy4-preview-f", " glm-5.3-flash "},
		CatchUpHours: &hours, RetryMinutes: &minutes, Notify: &notify,
	})
	rt := warmupSnapshot()
	if rt.Enabled {
		t.Fatal("显式关闭应生效")
	}
	if rt.Time != "05:30" {
		t.Fatalf("时刻未生效: %s", rt.Time)
	}
	if rt.MaxPrice != 0 {
		t.Fatalf("上限 0 应被保留（而非回落默认），实际=%v", rt.MaxPrice)
	}
	if len(rt.Models) != 2 || rt.Models[1] != "glm-5.3-flash" {
		t.Fatalf("模型列表应去空白且保留顺序: %v", rt.Models)
	}
	if rt.CatchUpHours != 2 || rt.RetryMinutes != 5 || !rt.Notify {
		t.Fatalf("补触发/通知未生效: %+v", rt)
	}
}

// 价格上限 0 = 只触发已确认免费的模型；价格未知的绝不能触发（避免真实计费）。
func TestWarmupSelectModelsPriceZeroOnlyFree(t *testing.T) {
	resetWarmupStateT(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {
			{ID: "m-free", HasMultiplier: true, Multiplier: 0, FromLive: true},
			{ID: "m-cheap", HasMultiplier: true, Multiplier: 0.03, FromLive: true},
			{ID: "m-pricey", HasMultiplier: true, Multiplier: 0.29, FromLive: true},
			{ID: "m-unknown", HasMultiplier: true, Multiplier: 0, PromoExpired: true, FromLive: true},
		},
	}
	modelsMu.Unlock()

	rt := warmupSnapshot()
	rt.MaxPrice = 0
	got := warmupSelectModels(rt, time.Now())
	if len(got) != 1 || got[0] != "m-free" {
		t.Fatalf("上限 0 应只选中已确认免费的模型，实际=%v", got)
	}

	// 上限 0.06：便宜的入选，贵的与未知的不入选。
	rt.MaxPrice = 0.06
	got = warmupSelectModels(rt, time.Now())
	if len(got) != 2 || got[0] != "m-free" || got[1] != "m-cheap" {
		t.Fatalf("上限 0.06 应选中 m-free 与 m-cheap，实际=%v", got)
	}
}

// 显式指定模型优先于价格筛选：即便价格未知也按用户点名触发。
func TestWarmupExplicitModelsIgnorePrice(t *testing.T) {
	resetWarmupStateT(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-unknown", HasMultiplier: true, Multiplier: 0, PromoExpired: true, FromLive: true}},
	}
	modelsMu.Unlock()

	rt := warmupSnapshot()
	rt.MaxPrice = 0.06
	rt.Models = []string{"m-unknown"}
	got := warmupSelectModels(rt, time.Now())
	if len(got) != 1 || got[0] != "m-unknown" {
		t.Fatalf("显式指定模型应优先于价格筛选，实际=%v", got)
	}
	sites := warmupCandidateSites("m-unknown", rt.MaxPrice, true, time.Now())
	if len(sites) == 0 {
		t.Fatal("显式指定模型应忽略价格上限，允许触发")
	}
}

// 端到端：上游正常返回 → 状态记为 ok，并落盘可查。
func TestWarmupRunOnceRecordsSuccess(t *testing.T) {
	resetWarmupStateT(t)
	chdirTemp(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-free", HasMultiplier: true, Multiplier: 0, FromLive: true}},
	}
	modelsMu.Unlock()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseWithUsage(`{"credit":0,"total_tokens":500}`)))
	}))
	defer server.Close()

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
	ok, failed, skipped, pending, _ := runWarmupOnce(rt, time.Now())
	if ok != 1 || failed != 0 || skipped != 0 || pending != 0 {
		t.Fatalf("应成功触发 1 个模型，实际 ok=%d failed=%d skipped=%d pending=%d", ok, failed, skipped, pending)
	}

	st := loadWarmupState()
	if st.Current == nil || st.Current.Models["m-free"] == nil {
		t.Fatal("触发结果未落盘")
	}
	if st.Current.Models["m-free"].Status != "ok" {
		t.Fatalf("状态应为 ok，实际=%s（%s）", st.Current.Models["m-free"].Status, st.Current.Models["m-free"].Detail)
	}
}

// 上游返回 6004 模型限流 → 保留 pending，等待补触发重试。
func TestWarmupKeepsPendingOnRateLimit(t *testing.T) {
	resetWarmupStateT(t)
	chdirTemp(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-free", HasMultiplier: true, Multiplier: 0, FromLive: true}},
	}
	modelsMu.Unlock()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"data":{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-30 09:00:00 UTC+8 重置，您也可以切换其他模型继续使用。"}}}`))
	}))
	defer server.Close()

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
	_, _, _, pending, _ := runWarmupOnce(rt, time.Now())
	if pending != 1 {
		t.Fatalf("限流后应保留 1 个待补触发，实际=%d", pending)
	}
	st := loadWarmupState()
	if st.Current.Models["m-free"].Status != "pending" {
		t.Fatalf("限流后状态应为 pending，实际=%s", st.Current.Models["m-free"].Status)
	}
}

// 授权失效属于结构性失败：不重试，直接标记 failed。
func TestWarmupAuthFailureIsTerminal(t *testing.T) {
	resetWarmupStateT(t)
	chdirTemp(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-free", HasMultiplier: true, Multiplier: 0, FromLive: true}},
	}
	modelsMu.Unlock()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":14001,"msg":"登录已失效"}`))
	}))
	defer server.Close()

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
	_, failed, _, pending, _ := runWarmupOnce(rt, time.Now())
	if failed != 1 || pending != 0 {
		t.Fatalf("授权失效应判为结构性失败，实际 failed=%d pending=%d", failed, pending)
	}
}

// 无可用账号：不算失败，保留 pending —— 账号可能稍后热加载进来或冷却结束。
func TestWarmupKeepsPendingWhenNoAccount(t *testing.T) {
	resetWarmupStateT(t)
	chdirTemp(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-free", HasMultiplier: true, Multiplier: 0, FromLive: true}},
	}
	modelsMu.Unlock()

	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accounts, rrIndex = nil, 0
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	rt := warmupSnapshot()
	_, failed, _, pending, _ := runWarmupOnce(rt, time.Now())
	if pending != 1 || failed != 0 {
		t.Fatalf("无可用账号应保留待补触发，实际 pending=%d failed=%d", pending, failed)
	}
}

// 无合规站点（routing 全 FORBIDDEN）→ 不入选，也不触发。
func TestWarmupSkipsWhenNoAllowedSite(t *testing.T) {
	resetWarmupStateT(t)
	chdirTemp(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-blocked", HasMultiplier: true, Multiplier: 0, FromLive: true}},
	}
	modelsMu.Unlock()

	// cn 站被空窗口规则拒绝，intl 未声明即 FORBIDDEN → 全站不可调度。
	setRouting(routingConfig{
		TZ:            "Asia/Shanghai",
		DefaultPolicy: "allow",
		MaxPrice:      0.06,
		Rules: []routingRule{{
			Models: []string{"m-blocked"},
			Sites:  []routingSiteRule{{Site: "cn", Window: &routingWindow{Start: "00:00", End: "00:00"}, Outside: "reject"}},
		}},
	})

	if got := warmupCandidateSites("m-blocked", 0.06, false, time.Now()); len(got) != 0 {
		t.Fatalf("全站 FORBIDDEN 不应有候选站点，实际=%v", got)
	}
	// 也不应进入本轮触发列表，避免对不可调度模型白打请求。
	rt := warmupSnapshot()
	if got := warmupSelectModels(rt, time.Now()); len(got) != 0 {
		t.Fatalf("全站 FORBIDDEN 的模型不应入选，实际=%v", got)
	}
}

// 时刻计算：同一自然日内算出计划时刻；跨天时取次日。
func TestWarmupScheduleTime(t *testing.T) {
	resetWarmupStateT(t)
	loc := warmupLocation()
	day := time.Date(2026, 9, 30, 1, 0, 0, 0, loc)
	got := scheduledTimeOn(day, "04:00", 4, 0)
	if got.Hour() != 4 || got.Minute() != 0 || got.Day() != 30 {
		t.Fatalf("计划时刻计算错误: %v", got)
	}
	next := nextScheduledAfter(time.Date(2026, 9, 30, 5, 0, 0, 0, loc), "04:00", 4, 0)
	if next.Day() != 1 || next.Month() != 10 {
		t.Fatalf("已过当日触发点应取次日，实际=%v", next)
	}
	// 非法时刻回落默认 04:00
	fallback := scheduledTimeOn(day, "bad", 4, 0)
	if fallback.Hour() != 4 {
		t.Fatalf("非法时刻应回落默认，实际=%v", fallback)
	}
}

// 管理接口只接受回环来源。
func TestHandleAdminWarmupRejectsNonLoopback(t *testing.T) {
	resetWarmupStateT(t)
	chdirTemp(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/warmup", nil)
	req.RemoteAddr = "203.0.113.9:12345"
	rec := httptest.NewRecorder()
	handleAdminWarmup(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非回环来源应被拒绝，实际=%d", rec.Code)
	}
}

// 管理接口 GET 返回可解析的状态（含 probe 生效时刻）。
func TestHandleAdminWarmupStatusRoundTrip(t *testing.T) {
	resetWarmupStateT(t)
	chdirTemp(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/warmup", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	rec := httptest.NewRecorder()
	handleAdminWarmup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("回环 GET 应成功，实际=%d", rec.Code)
	}
	var out warmupStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if !out.Enabled || out.Time != "04:00" {
		t.Fatalf("状态不符: %+v", out)
	}
	if out.ProbeEffectiveAt == "" {
		t.Fatal("应返回主动探测生效时刻")
	}
}

// warmup 关闭时，主动探测应回落到独立时刻（默认 06:00）。
func TestProbeFallsBackToOwnTimeWhenWarmupOff(t *testing.T) {
	resetWarmupStateT(t)
	off := false
	setWarmup(warmupConfig{Enabled: &off})
	st := warmupStatusNow()
	if st.Enabled {
		t.Fatal("warmup 应已关闭")
	}
	if !strings.Contains(st.ProbeEffectiveAt, "06:00") {
		t.Fatalf("warmup 关闭时探测应回落 06:00，实际=%s", st.ProbeEffectiveAt)
	}
}
