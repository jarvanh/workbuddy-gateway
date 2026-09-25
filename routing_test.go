package main

// routing_test.go —— v6 价格核心（routing.go）单元测试。
// 覆盖：价格闸（maxPrice）、保底余额（minBalanceGuard）、按费用预算（fallbackBudget credits）、
// 站点五态机（含跨午夜窗口与生效区间）、账号快过期优先、锚点校准、样本优先级、cheapest-first。

import (
	"testing"
	"time"
)

// setTestRouting 装载测试配置并清理预算，避免用例互相污染。
func setTestRouting(t *testing.T, cfg routingConfig) {
	t.Helper()
	setRouting(cfg)
	resetRoutingSpend()
}

func TestEffectiveMultiplierUsesCatalogWhenPromoValid(t *testing.T) {
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-catalog", HasMultiplier: true, Multiplier: 0.29, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	if got := effectiveMultiplier("cn", "m-catalog"); got != 0.29 {
		t.Fatalf("目录倍率应直接采用 0.29，实际=%v", got)
	}
}

func TestEffectiveMultiplierIgnoresExpiredPromo(t *testing.T) {
	// 促销过期时倍率不可信：应回落实测校准，而非继续采信 0
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-expired", HasMultiplier: true, Multiplier: 0, PromoExpired: true, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	// 无任何实测样本 → 返回 0（未知），不因猜测而拒绝
	if got := effectiveMultiplier("cn", "m-expired"); got != 0 {
		t.Fatalf("促销过期且无实测样本时应返回 0，实际=%v", got)
	}
}

func TestMaxPriceBlocksExpensiveModel(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)

	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-expensive", HasMultiplier: true, Multiplier: 0.29, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	now := time.Now()
	rule := routingSiteRule{Site: "cn"}
	st := evaluateSite(rule, "m-expensive", now, routingLocation(cfg), cfg)
	if st != siteForbidden {
		t.Fatalf("0.29x 超过 maxPrice 0.06 应被封死，实际=%v", st)
	}
}

func TestMaxPriceAllowsCheapModel(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)

	catalogModels = map[string][]catalogModel{
		"intl": {{ID: "m-cheap", HasMultiplier: true, Multiplier: 0.01, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	rule := routingSiteRule{Site: "intl"}
	st := evaluateSite(rule, "m-cheap", time.Now(), routingLocation(cfg), cfg)
	if st != siteAllowed {
		t.Fatalf("0.01x 未超 maxPrice 应放行，实际=%v", st)
	}
}

func TestWindowCrossesMidnight(t *testing.T) {
	cfg := defaultRoutingConfig()
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	rule := routingSiteRule{Site: "cn", Window: &routingWindow{Start: "23:00", End: "08:00"}}

	cases := []struct {
		hour, min int
		want      siteState
	}{
		{23, 30, siteInWindow},
		{0, 30, siteInWindow},
		{7, 59, siteInWindow},
		{8, 0, siteForbidden}, // 窗口外且无预算 → 默认拒绝
		{16, 0, siteForbidden},
	}
	for _, c := range cases {
		now := time.Date(2026, 9, 25, c.hour, c.min, 0, 0, loc)
		if got := evaluateSite(rule, "m-win", now, loc, cfg); got != c.want {
			t.Fatalf("%02d:%02d 期望=%v 实际=%v", c.hour, c.min, c.want, got)
		}
	}
}

func TestActiveWindowBoundaries(t *testing.T) {
	cfg := defaultRoutingConfig()
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	// until 为排他日期：2026-10-11 00:00 起失效
	rule := routingSiteRule{Site: "cn", Active: &routingActive{From: "2026-09-11", Until: "2026-10-11"}, OnExpire: "reject"}

	if got := evaluateSite(rule, "m-act", time.Date(2026, 10, 10, 23, 59, 0, 0, loc), loc, cfg); got != siteAllowed {
		t.Fatalf("10-10 23:59 仍在区间内应放行，实际=%v", got)
	}
	if got := evaluateSite(rule, "m-act", time.Date(2026, 10, 11, 0, 0, 0, 0, loc), loc, cfg); got != siteForbidden {
		t.Fatalf("10-11 00:00 已过 until 应拒绝（onExpire=reject），实际=%v", got)
	}
	if got := evaluateSite(rule, "m-act", time.Date(2026, 9, 10, 12, 0, 0, 0, loc), loc, cfg); got != siteForbidden {
		t.Fatalf("早于 from 应拒绝，实际=%v", got)
	}
}

func TestFallbackBudgetByCredits(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	// 站点无窗口限制时不会走到预算分支；此处构造窗口外 + 便宜模型
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-budget", HasMultiplier: true, Multiplier: 0.05, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	rule := routingSiteRule{
		Site:           "cn",
		Window:         &routingWindow{Start: "23:00", End: "08:00"},
		FallbackBudget: &routingBudget{Credits: 1},
	}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, loc) // 窗口外

	if got := evaluateSite(rule, "m-budget", now, loc, cfg); got != siteBudgeted {
		t.Fatalf("窗口外且预算未耗时应为 BUDGETED，实际=%v", got)
	}
	// 消耗 1.0 credit 后应转为 FORBIDDEN
	consumeBudget("cn", "m-budget", 1.0, now)
	if got := evaluateSite(rule, "m-budget", now, loc, cfg); got != siteForbidden {
		t.Fatalf("预算耗尽后应为 FORBIDDEN，实际=%v", got)
	}
}

func TestFreeModelsDoNotConsumeBudget(t *testing.T) {
	cfg := defaultRoutingConfig()
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, loc)

	consumeBudget("cn", "m-free", 0, now) // 免费请求 credit=0
	if got := budgetUsed("cn", "m-free", now, loc); got != 0 {
		t.Fatalf("免费模型不应消耗预算，实际=%v", got)
	}
}

func TestMinBalanceGuardOverridesBudget(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MinBalanceGuard = 10
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)

	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-paid", HasMultiplier: true, Multiplier: 0.05, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	paid := &Account{Auth: &StoredAuth{Edition: "cn"}, QuotaKnown: true, QuotaRemaining: 8}
	if guardMinBalance(paid, "m-paid") {
		t.Fatal("余额 8 < 10 时不该允许调度付费模型")
	}
	rich := &Account{Auth: &StoredAuth{Edition: "cn"}, QuotaKnown: true, QuotaRemaining: 50}
	if !guardMinBalance(rich, "m-paid") {
		t.Fatal("余额 50 ≥ 10 应允许调度付费模型")
	}
	// 免费模型（有效倍率 0）不受保底限制
	poor := &Account{Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 1}
	if !guardMinBalance(poor, "m-unknown-free") {
		t.Fatal("免费/未知价格模型不应受保底余额限制")
	}
}

func TestAccountOrderExpiringFirst(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.AccountOrder = "expiringFirst"
	setTestRouting(t, cfg)

	now := time.Now().Unix()
	far := &Account{Path: "far", Auth: &StoredAuth{Auth: StoredTokens{ExpiresAt: now + 86400*400}}}
	near := &Account{Path: "near", Auth: &StoredAuth{Auth: StoredTokens{ExpiresAt: now + 86400}}}
	unknown := &Account{Path: "unknown"} // 无凭据 → 排到最后

	got := sortedAccounts([]*Account{far, unknown, near})
	if len(got) != 3 || got[0].Path != "near" || got[2].Path != "unknown" {
		t.Fatalf("快过期应优先、无凭据排最后，实际顺序=%v", []string{got[0].Path, got[1].Path, got[2].Path})
	}
}

func TestRoutingRejectionBlocksWhenAllSitesForbidden(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	cfg.Rules = []routingRule{{
		Models: []string{"m-allbad"},
		Sites:  []routingSiteRule{{Site: "cn"}, {Site: "intl"}},
	}}
	setTestRouting(t, cfg)

	catalogModels = map[string][]catalogModel{
		"cn":   {{ID: "m-allbad", HasMultiplier: true, Multiplier: 0.29, FromLive: true}},
		"intl": {{ID: "m-allbad", HasMultiplier: true, Multiplier: 0.29, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	blocked, _ := routingRejection("m-allbad", time.Now())
	if !blocked {
		t.Fatal("全站均超价时应拒绝")
	}
}

func TestRoutingRejectionUnmatchedModelUsesDefaultPolicy(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.Rules = nil
	setTestRouting(t, cfg)

	if blocked, _ := routingRejection("m-unmatched", time.Now()); blocked {
		t.Fatal("defaultPolicy=allow 时未匹配模型应放行")
	}

	cfg.DefaultPolicy = "reject"
	setTestRouting(t, cfg)
	if blocked, _ := routingRejection("m-unmatched", time.Now()); !blocked {
		t.Fatal("defaultPolicy=reject 时未匹配模型应拒绝")
	}
}

func TestSortSitesByPricePrefersCheapest(t *testing.T) {
	cfg := defaultRoutingConfig()
	setTestRouting(t, cfg)

	catalogModels = map[string][]catalogModel{
		"cn":   {{ID: "m-cmp", HasMultiplier: true, Multiplier: 0.06, FromLive: true}},
		"intl": {{ID: "m-cmp", HasMultiplier: true, Multiplier: 0.01, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	got := sortSitesByPrice([]string{"cn", "intl"}, "m-cmp")
	if len(got) != 2 || got[0] != "intl" {
		t.Fatalf("cheapest-first 应把 intl(0.01) 排前面，实际=%v", got)
	}
}

func TestSiteLedgerCalibration(t *testing.T) {
	// 站点级 ledger + 同源锚点校准：cn 锚点 0.06，intl 实测为 cn 的 1/6 → intl ≈ 0.01
	catalogModels = map[string][]catalogModel{}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	cfg := defaultRoutingConfig() // anchor: glm-5.3-flash@cn = 0.06
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	recordSitePriceSample("cn", "glm-5.3-flash", 1000000, 6)   // 6e-6 credit/token
	recordSitePriceSample("intl", "glm-5.3-flash", 1000000, 1) // 1e-6 credit/token

	if got := effectiveMultiplier("intl", "glm-5.3-flash"); got < 0.01-1e-9 || got > 0.01+1e-9 {
		t.Fatalf("intl 校准应得 0.01，实际=%v", got)
	}
	if got := effectiveMultiplier("cn", "glm-5.3-flash"); got < 0.06-1e-9 || got > 0.06+1e-9 {
		t.Fatalf("cn 自校准应得 0.06，实际=%v", got)
	}
	// probe-only 付费模型：不参与数值定价（决策 17），价格闸不得封禁
	modelProbes[probeKey("intl", "hy4-preview")] = modelPriceProbe{Verdict: "paid", Credit: 0.05, Tokens: 385}
	defer delete(modelProbes, probeKey("intl", "hy4-preview"))
	if got := effectiveMultiplier("intl", "hy4-preview"); got != 0 {
		t.Fatalf("probe-only 付费模型数值应为 0（未知），实际=%v", got)
	}
	rule := routingSiteRule{Site: "intl"}
	if got := evaluateSite(rule, "hy4-preview", time.Now(), loc, cfg); got != siteAllowed {
		t.Fatalf("probe-only 付费模型不应被价格闸封禁，实际=%v", got)
	}
}

func TestProbePaidUnknownGuarded(t *testing.T) {
	catalogModels = map[string][]catalogModel{}
	defer func() { catalogModels = map[string][]catalogModel{} }()
	cfg := defaultRoutingConfig()
	setTestRouting(t, cfg)

	modelProbes[probeKey("intl", "m-ghost")] = modelPriceProbe{Verdict: "paid", Credit: 0.05, Tokens: 385}
	defer delete(modelProbes, probeKey("intl", "m-ghost"))

	// 付费未知：数值不封禁，但受保底余额约束（防 ja 式烧穿）
	low := &Account{Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 8}
	if guardMinBalance(low, "m-ghost") {
		t.Fatal("probe-paid 未知价模型应受保底余额约束")
	}
	rich := &Account{Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 50}
	if !guardMinBalance(rich, "m-ghost") {
		t.Fatal("余额充足应放行")
	}
	// 完全未知（无任何数据）：放行，避免误杀潜在免费模型
	if !guardMinBalance(low, "m-never-seen") {
		t.Fatal("完全未知价格应放行")
	}
}

func TestInWindowBypassesPriceGate(t *testing.T) {
	// 修订场景：目录价 0.29x 的模型，窗口期是政策免费时段，应放行而非被价格闸封死
	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-night", HasMultiplier: true, Multiplier: 0.29, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	rule := routingSiteRule{Site: "cn", Window: &routingWindow{Start: "23:00", End: "08:00"}}
	if got := evaluateSite(rule, "m-night", time.Date(2026, 9, 25, 23, 30, 0, 0, loc), loc, cfg); got != siteInWindow {
		t.Fatalf("窗口内应视为免费期放行（不受 0.29x 价格闸影响），实际=%v", got)
	}
	if got := evaluateSite(rule, "m-night", time.Date(2026, 9, 25, 12, 0, 0, 0, loc), loc, cfg); got != siteForbidden {
		t.Fatalf("窗口外 0.29x 应被价格闸封死，实际=%v", got)
	}
}

func TestOutsidePreferAllowsAfterPriceGate(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-pref", HasMultiplier: true, Multiplier: 0.05, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	rule := routingSiteRule{Site: "cn", Window: &routingWindow{Start: "23:00", End: "08:00"}, Outside: "prefer"}
	if got := evaluateSite(rule, "m-pref", time.Date(2026, 9, 25, 12, 0, 0, 0, loc), loc, cfg); got != siteAllowed {
		t.Fatalf("outside=prefer 且价格达标应放行，实际=%v", got)
	}
}
