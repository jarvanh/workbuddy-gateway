package main

// routing_budget_test.go —— 2026-10-15 事故修复的回归测试。
// 事故链：hy4-preview-f 目录无条目 + 探针无结论 + 校准断供（锚点无台账样本）
// → 价格结论停在「付费但无法定标」（pricePaidUnk）→ effectiveMultiplier 返回 0
// → 价格闸放行 728 笔、烧掉 5576 credit。
// 修复语义（预算兜底）：有扣费证据就裁决，不再因定标失败而全放行——
//   1. paidUnk 用最近已知扣费（台账 lastCredit 优先、探针其次）比对 maxPrice；
//   2. 窗口让位扣费证据：窗口期内被实测扣费后，窗口不得再充当价格闸旁路；
//   3. 动态锚点：配置锚没有台账样本时，从目录里挑「倍率可信且有样本」的模型校准；
//   4. 有扣费证据的模型一律受保底余额约束（与可校准付费模型同权）。

import (
	"os"
	"testing"
	"time"
)

// setupBudgetTest 清理全局价格状态并把账本指到临时目录，避免用例互相污染。
func setupBudgetTest(t *testing.T) {
	t.Helper()
	savedFile, savedMirror := priceLedgerFile, priceLedgerMirrorDir
	priceLedgerFile = t.TempDir() + "/wb-price-ledger.json"
	priceLedgerMirrorDir = "" // 测试不写 Dropbox
	resetSitePriceSamples()
	t.Cleanup(func() {
		priceLedgerFile, priceLedgerMirrorDir = savedFile, savedMirror
		resetSitePriceSamples()
	})
}

// TestBudgetFallbackBlocksBilledUnknown 预算兜底主链路：
// 纯未知放行（保留学习闭环）→ 第一笔扣费落账 → 立即按预算裁决。
func TestBudgetFallbackBlocksBilledUnknown(t *testing.T) {
	setupBudgetTest(t)
	catalogModels = map[string][]catalogModel{}
	t.Cleanup(func() { catalogModels = map[string][]catalogModel{} })

	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	rule := routingSiteRule{Site: "intl"}
	if st := evaluateSite(rule, "m-ghost-f", time.Now(), loc, cfg); st != siteAllowed {
		t.Fatalf("纯未知模型应放行以学习价格，实际=%v", st)
	}

	// 2026-10-15 实测口径：hy4-preview-f 单笔 16.85 credit，远超 0.06
	recordSitePriceSample("intl", "m-ghost-f", 385, 16.85)
	if st := evaluateSite(rule, "m-ghost-f", time.Now(), loc, cfg); st != siteForbidden {
		t.Fatalf("有扣费证据且超预算应立即封禁，实际=%v", st)
	}
}

// TestBudgetFallbackUsesProbeCredit 探针判定收费且记录了单笔扣费时，
// 无台账样本也应能用探针 Credit 兜底比对预算。
func TestBudgetFallbackUsesProbeCredit(t *testing.T) {
	setupBudgetTest(t)
	catalogModels = map[string][]catalogModel{}
	t.Cleanup(func() { catalogModels = map[string][]catalogModel{} })

	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	modelProbes[probeKey("intl", "m-ghost-p")] = modelPriceProbe{Verdict: "paid", Credit: 5.28, Tokens: 385, LastProbeAt: time.Now().Unix()}
	defer delete(modelProbes, probeKey("intl", "m-ghost-p"))

	if st := evaluateSite(routingSiteRule{Site: "intl"}, "m-ghost-p", time.Now(), loc, cfg); st != siteForbidden {
		t.Fatalf("探针扣费 5.28 超预算 0.06 应封禁，实际=%v", st)
	}

	// 对照：探针扣费在预算内（0.05）仍放行——预算兜底不扩大打击面
	modelProbes[probeKey("intl", "m-ghost-p")] = modelPriceProbe{Verdict: "paid", Credit: 0.05, Tokens: 385, LastProbeAt: time.Now().Unix()}
	if st := evaluateSite(routingSiteRule{Site: "intl"}, "m-ghost-p", time.Now(), loc, cfg); st != siteAllowed {
		t.Fatalf("探针扣费 0.05 未超预算应放行，实际=%v", st)
	}

	// 对照：探针结论超过 12h 视为无当前价位证据 → 放行重学（价格可能已变）
	modelProbes[probeKey("intl", "m-ghost-p")] = modelPriceProbe{Verdict: "paid", Credit: 5.28, Tokens: 385, LastProbeAt: time.Now().Add(-13 * time.Hour).Unix()}
	if st := evaluateSite(routingSiteRule{Site: "intl"}, "m-ghost-p", time.Now(), loc, cfg); st != siteAllowed {
		t.Fatalf("过期探针证据不得裁决现价，应放行重学，实际=%v", st)
	}
}

// TestBudgetFallbackDisabledWhenMaxPriceZero maxPrice=0 语义是「只触发免费」，
// 预算兜底不参与（0 不是合法预算），避免兜底分支越权封禁。
func TestBudgetFallbackDisabledWhenMaxPriceZero(t *testing.T) {
	setupBudgetTest(t)
	catalogModels = map[string][]catalogModel{}
	t.Cleanup(func() { catalogModels = map[string][]catalogModel{} })

	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)

	recordSitePriceSample("intl", "m-billed", 385, 16.85)
	if st := evaluateSite(routingSiteRule{Site: "intl"}, "m-billed", time.Now(), loc, cfg); st != siteAllowed {
		t.Fatalf("maxPrice=0 时预算兜底应停用，实际=%v", st)
	}
}

// setSiteSample 测试辅助：直接写入指定时间戳的台账样本。
func setSiteSample(site, model string, s sitePriceSample) {
	sitePriceMu.Lock()
	sitePriceSamples[site+"|"+normalizeModelName(model)] = &s
	sitePriceMu.Unlock()
}

// TestBudgetEvidenceExpiresAfterFreshnessWindow 台账 lastCredit 只代表
// 「12h 内的价位」：过期证据不得封禁（放行重学），新鲜证据恢复裁决。
// 背景：被 403 封禁的模型拿不到新样本，旧价会被永久冻结——
// 主人 2026-10-16 指出「万一现在价格又有了变动呢」，本用例钉住该语义。
func TestBudgetEvidenceExpiresAfterFreshnessWindow(t *testing.T) {
	setupBudgetTest(t)
	catalogModels = map[string][]catalogModel{}
	t.Cleanup(func() { catalogModels = map[string][]catalogModel{} })

	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)
	rule := routingSiteRule{Site: "intl"}

	old := time.Now().Add(-13 * time.Hour).Unix()
	setSiteSample("intl", "m-stale", sitePriceSample{Credit: 900, Tokens: 62500, LastCredit: 16.85, UpdatedAt: old})
	if st := evaluateSite(rule, "m-stale", time.Now(), loc, cfg); st != siteAllowed {
		t.Fatalf("过期台账证据应放行一笔重新学习，实际=%v", st)
	}

	setSiteSample("intl", "m-stale", sitePriceSample{Credit: 916.85, Tokens: 62885, LastCredit: 16.85, UpdatedAt: time.Now().Unix()})
	if st := evaluateSite(rule, "m-stale", time.Now(), loc, cfg); st != siteForbidden {
		t.Fatalf("新鲜台账证据应恢复预算裁决，实际=%v", st)
	}
}

// TestLedgerSchema1SamplesLoadExpired 升级兼容：schema 1 台账（无时间戳）
// 加载后扣费证据保留（billedEvidence / 校准可用），但不代表现价（预算兜底不用）。
func TestLedgerSchema1SamplesLoadExpired(t *testing.T) {
	setupBudgetTest(t)
	doc := `{"schema":1,"updatedAt":1792030000,"samples":{"intl|m-old":{"credit":4186.3,"tokens":188565436,"lastCredit":16.85}}}`
	if err := os.WriteFile(priceLedgerFile, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	loadPriceLedger()
	t.Cleanup(resetSitePriceSamples)

	if last := lastCreditFor("intl", "m-old"); last != 16.85 {
		t.Fatalf("schema1 样本应保留 lastCredit 供证据与校准，实际=%v", last)
	}
	if ts := lastCreditFreshAt("intl", "m-old"); ts != 0 {
		t.Fatalf("schema1 样本无时间戳应按已过期处理，实际 ts=%d", ts)
	}
	if c := lastKnownCreditFresh("intl", "m-old", time.Now()); c != 0 {
		t.Fatalf("过期证据不得作为现价参与预算兜底，实际=%v", c)
	}
}

// TestWindowYieldsToBillingEvidence 窗口让位扣费证据：
// 上游在「政策免费窗口」内偷偷计费后，窗口不得继续绕过价格闸。
func TestWindowYieldsToBillingEvidence(t *testing.T) {
	setupBudgetTest(t)
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-night", HasMultiplier: true, Multiplier: 0.29, FromLive: true}},
	}
	t.Cleanup(func() { catalogModels = map[string][]catalogModel{} })

	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	loc := routingLocation(cfg)
	rule := routingSiteRule{Site: "cn", Window: &routingWindow{Start: "23:00", End: "08:00"}}
	inWindow := time.Date(2026, 9, 25, 23, 30, 0, 0, loc)

	if st := evaluateSite(rule, "m-night", inWindow, loc, cfg); st != siteInWindow {
		t.Fatalf("无扣费证据时窗口内应按政策放行，实际=%v", st)
	}
	recordSitePriceSample("cn", "m-night", 385, 16.85)
	if st := evaluateSite(rule, "m-night", inWindow, loc, cfg); st != siteForbidden {
		t.Fatalf("有扣费证据后窗口不得再当价格闸旁路（0.29>0.06 应封死），实际=%v", st)
	}
}

// TestDynamicAnchorRestoresCalibration 动态锚点：
// 配置锚（cn glm-5.3-flash）没有台账样本时，从目录挑有样本的模型校准，
// 校准断供不再导致价格闸失明。
func TestDynamicAnchorRestoresCalibration(t *testing.T) {
	setupBudgetTest(t)
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-calib", HasMultiplier: true, Multiplier: 0.12, FromLive: true}},
	}
	t.Cleanup(func() { catalogModels = map[string][]catalogModel{} })

	cfg := defaultRoutingConfig() // 配置锚: glm-5.3-flash@cn = 0.06，无样本
	setTestRouting(t, cfg)

	recordSitePriceSample("cn", "m-calib", 1000000, 6)     // 动态锚：6e-6 credit/token
	recordSitePriceSample("intl", "m-target", 1000000, 60) // 目标：60e-6 credit/token

	// 校准 = 0.12 × (60/6) = 1.2
	if got := effectiveMultiplier("intl", "m-target"); got < 1.2-1e-9 || got > 1.2+1e-9 {
		t.Fatalf("动态锚校准应得 1.2，实际=%v", got)
	}
	// 兜底联动：校准恢复后 1.2 > 0.06，规则路径直接封禁
	if st := evaluateSite(routingSiteRule{Site: "intl"}, "m-target", time.Now(), routingLocation(cfg), cfg); st != siteForbidden {
		t.Fatalf("校准恢复后超价模型应被封禁，实际=%v", st)
	}
}

// TestGuardMinBalanceEnforcedForBilledUncalibratable 有扣费证据但无法定标的模型：
// 保底余额与可校准付费模型同权生效（事故中 5 个账号被从 800+ 花到 25 以下的同型场景）。
func TestGuardMinBalanceEnforcedForBilledUncalibratable(t *testing.T) {
	setupBudgetTest(t)
	catalogModels = map[string][]catalogModel{}
	t.Cleanup(func() { catalogModels = map[string][]catalogModel{} })

	cfg := defaultRoutingConfig() // minBalanceGuard=10
	setTestRouting(t, cfg)

	recordSitePriceSample("intl", "m-billed", 385, 16.85) // 无锚样本 → paidUnk

	low := &Account{Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 8}
	if guardMinBalance(low, "m-billed") {
		t.Fatal("有扣费证据但无法定标的模型应受保底余额约束（8 < 10）")
	}
	rich := &Account{Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 50}
	if !guardMinBalance(rich, "m-billed") {
		t.Fatal("余额高于保底应放行")
	}
}
