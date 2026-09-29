package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
