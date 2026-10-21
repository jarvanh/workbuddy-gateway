package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isolatePriceLedger 把价格账本重定向到临时目录并清空内存样本。
//
// 必需性：fork 的 observeModelCredit 会把真实请求的 (tokens, credit) 写入全局
// sitePriceSamples 并按锚点校准倍率。全链路测试（如企业额度 HTTP 流）会真的发请求，
// 其样本会泄漏到后续用例，改变锚点校准结果（实测 0.6 → 0.51）。
// 凡会触发真实上游流量的测试都要在开头调用本函数。
func isolatePriceLedger(t *testing.T) {
	t.Helper()
	savedFile, savedDir := priceLedgerFile, priceLedgerMirrorDir
	priceLedgerFile = filepath.Join(t.TempDir(), "wb-price-ledger.json")
	priceLedgerMirrorDir = "" // 测试不写 Dropbox
	sitePriceMu.Lock()
	savedSamples := sitePriceSamples
	sitePriceSamples = map[string]*sitePriceSample{}
	sitePriceMu.Unlock()
	t.Cleanup(func() {
		priceLedgerFile, priceLedgerMirrorDir = savedFile, savedDir
		sitePriceMu.Lock()
		sitePriceSamples = savedSamples
		sitePriceMu.Unlock()
	})
}

// TestRuleSiteWhitelist 站点白名单语义（2026-09-29 事故修复）：
// 规则命中的模型，未声明的站点一律 FORBIDDEN。
// 事故中 hy4-preview 只声明 cn，intl 未声明却被当作无限制参与代偿调度，
// 结果 cn 站 429 冷却期间 84 笔请求涌向 intl 烧光 4 个账号。
func TestRuleSiteWhitelist(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MaxPrice = 0.06
	cfg.Rules = []routingRule{{
		Models: []string{"m-cn-only"},
		Sites:  []routingSiteRule{{Site: "cn"}},
	}}
	setTestRouting(t, cfg)

	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "m-cn-only", HasMultiplier: true, Multiplier: 0.05, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	blocked, states := routingRejection("m-cn-only", time.Now())
	if blocked {
		t.Fatal("cn 可用时不应整体拒绝")
	}
	if states["cn"] == siteForbidden {
		t.Fatal("已声明的 cn 站不应被禁")
	}
	if states["intl"] != siteForbidden {
		t.Fatalf("未声明的 intl 站应 FORBIDDEN（站点白名单语义），实际=%v", states["intl"])
	}
}

// TestPriceLedgerPersistRestore 价格账本落盘与恢复：
// 重启不得让价格记忆清零——事故中重启导致价格退化成「未知」而放行烧钱。
func TestPriceLedgerPersistRestore(t *testing.T) {
	dir := t.TempDir()
	savedFile, savedDir := priceLedgerFile, priceLedgerMirrorDir
	priceLedgerFile = filepath.Join(dir, "wb-price-ledger.json")
	priceLedgerMirrorDir = "" // 测试不写 Dropbox
	defer func() {
		priceLedgerFile, priceLedgerMirrorDir = savedFile, savedDir
		sitePriceMu.Lock()
		sitePriceSamples = map[string]*sitePriceSample{}
		sitePriceMu.Unlock()
	}()

	cfg := defaultRoutingConfig() // anchor: glm-5.3-flash@cn = 0.06
	setTestRouting(t, cfg)

	// cn 锚点：6 credit / 1M tokens；intl 未知模型：60 credit / 1M tokens → 校准为 0.6
	recordSitePriceSample("cn", "glm-5.3-flash", 1000000, 6)
	recordSitePriceSample("intl", "m-unknown", 1000000, 60)

	if _, err := os.Stat(priceLedgerFile); err != nil {
		t.Fatalf("有样本后应落盘价格账本，实际未生成: %v", err)
	}
	before := effectiveMultiplier("intl", "m-unknown")
	if before < 0.6-1e-9 || before > 0.6+1e-9 {
		t.Fatalf("校准倍率应为 0.6，实际=%v", before)
	}

	// 模拟重启：内存清零后从磁盘恢复
	sitePriceMu.Lock()
	sitePriceSamples = map[string]*sitePriceSample{}
	sitePriceMu.Unlock()
	if got := effectiveMultiplier("intl", "m-unknown"); got != 0 {
		t.Fatalf("清零后未恢复时倍率应为 0，实际=%v", got)
	}
	loadPriceLedger()
	if got := effectiveMultiplier("intl", "m-unknown"); got < 0.6-1e-9 || got > 0.6+1e-9 {
		t.Fatalf("重启恢复后倍率应仍为 0.6，实际=%v", got)
	}
}

// TestInFlightReserveBlocksConcurrentBurn 在途预留（并发烧穿护栏）：
// 静态保底只在选号瞬间看余额，额度却要等请求完成后才回写；
// 事故中 84 笔并发请求每次选号余额都够，请求过程中才被烧穿。
func TestInFlightReserveBlocksConcurrentBurn(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MinBalanceGuard = 10
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	catalogModels = map[string][]catalogModel{}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	acc := &Account{Path: "small", Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 150}

	// 价格未知 → 预留 10×guard=100：150 的额度最多两笔在途
	if !reserveInFlightLocked(acc, 1, "m-unknown") {
		t.Fatal("首笔未知价请求应允许（150-0-100 ≥ 0）")
	}
	if reserveInFlightLocked(acc, 2, "m-unknown") {
		t.Fatal("第二笔应被挡住：150-100-100 < 0（未知价按 10×guard 预留）")
	}
	if reserveInFlightLocked(acc, 3, "m-unknown") {
		t.Fatal("第三笔应被在途预留挡住，防并发烧穿")
	}
	// 释放一笔后额度重新可用
	releaseInFlight(acc, 1)
	if !reserveInFlightLocked(acc, 4, "m-unknown") {
		t.Fatal("释放后应重新允许预留")
	}
	// 同一 reqID 重复释放安全
	releaseInFlight(acc, 1)
}

// TestInFlightReserveFreeModelUnlimited 免费模型不受在途预留限制：
// 零余额账号仍应服务免费模型，不得被预留机制误伤。
func TestInFlightReserveFreeModelUnlimited(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MinBalanceGuard = 10
	setTestRouting(t, cfg)
	catalogModels = map[string][]catalogModel{
		"intl": {{ID: "m-free", HasMultiplier: true, Multiplier: 0, FromLive: true}},
	}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	acc := &Account{Path: "exhausted", Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 0, QuotaExhausted: true}
	for i := uint64(1); i <= 20; i++ {
		if !reserveInFlightLocked(acc, i, "m-free") {
			t.Fatalf("免费模型第 %d 笔不应被预留限制", i)
		}
	}
	releaseInFlight(acc, 1)
}

// TestInFlightReserveSiteWindowExempt 站点免费时段豁免在途预留（2026-10-21 修复）：
// 回归 intl deepseek-v4.1-flash 限免模型被永拒 503 的死锁。
// 链路：目录价 0.11 > maxPrice 0.06 → 价格闸 forbidden；intl 无目录条目 →
// priceUnknown → 预留 10×guard；账号余额 195 < 250 → 拒绝请求；
// 拿不到 usage.credit → ModelStates.CostClass 永远学不到 free → 永久 503。
// 站点被声明为免费时段（siteInWindow）时必须免预留，放开学习闭环。
func TestInFlightReserveSiteWindowExempt(t *testing.T) {
	cfg := defaultRoutingConfig()
	cfg.MinBalanceGuard = 25
	cfg.MaxPrice = 0.06
	setTestRouting(t, cfg)
	// intl 目录里没有 deepseek-v4.1-flash（限免促销模型不进目录）→ priceUnknown
	catalogModels = map[string][]catalogModel{}
	defer func() { catalogModels = map[string][]catalogModel{} }()

	acc := &Account{Path: "intl-acc", Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 195.6}

	// 1) 不传窗口豁免（旧口径）：应被 10×guard=250 挡住 —— 复现死锁
	if reserveInFlightLocked(acc, 1, "deepseek-v4.1-flash") {
		t.Fatal("旧口径应挡住：195.6 - 250 < 0（未知价 10×guard 预留）")
	}

	// 2) 传窗口豁免=true：应放行，且不占预留额
	if !reserveInFlightLocked(acc, 2, "deepseek-v4.1-flash", true) {
		t.Fatal("站点免费时段应免预留（窗口豁免）")
	}
	// 豁免不该产生在途记录（否则会白占额度且需释放）
	if len(acc.inFlight) != 0 {
		t.Fatalf("窗口豁免不应占预留额，实际在途记录=%d", len(acc.inFlight))
	}
	// 释放不存在的 reqID 也必须安全
	releaseInFlight(acc, 2)

	// 3) 窗口外（false）恢复旧口径：仍应挡住
	if reserveInFlightLocked(acc, 3, "deepseek-v4.1-flash", false) {
		t.Fatal("窗口外应回到旧口径并挡住")
	}

	// 4) 无规则约束的站点（states 里查不到）视为可用 → siteIsFreeNowLocked=true
	states := map[string]siteState{"intl": siteInWindow, "cn": siteForbidden}
	if !siteIsFreeNowLocked("intl", states) {
		t.Fatal("intl 处于窗口内应判为免费可用")
	}
	if siteIsFreeNowLocked("cn", states) {
		t.Fatal("cn 被 forbidden 不应判为免费可用")
	}
	if !siteIsFreeNowLocked("jp", states) {
		t.Fatal("未声明站点应视为可用（与 freeNow 过滤器同语义）")
	}
}
