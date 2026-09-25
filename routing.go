package main

// routing.go —— v6 价格驱动路由核心（commit ①）
//
// 职责边界（重要）：
//   - 本文件只提供「价格解析 + 站点五态判定 + 账号排序 + 保底/预算记账」的纯逻辑，
//     不改动任何现有调度函数，也不接管请求入口；与现有调度函数的接线见 commit ②。
//
// 设计要点：
//  1. 价格来源优先级：catalog（官方倍率，促销有效）> ledger（真实大样本）> probe（小样本兜底）。
//     原因：probe 用最小请求（max_tokens:300），credit/tokens 会把单价高估约 14 倍——
//     ja 事故实测 302 tokens/0.01 credit vs 真实 4.66M tokens/10.74 credit。
//  2. 冲突优先级：minBalanceGuard（能不能花的底线）> fallbackBudget（能花多少的上限）。
//     被保底拦截时请求根本没发出，零扣费，故不消耗 fallbackBudget。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 配置
// -----------------------------------------------------------------------------

type routingWindow struct {
	Start string `json:"start"` // HH:MM
	End   string `json:"end"`   // HH:MM
}

type routingActive struct {
	From  string `json:"from"`  // YYYY-MM-DD，含当日
	Until string `json:"until"` // YYYY-MM-DD，排他：该日 00:00 起失效
}

type routingBudget struct {
	Credits float64 `json:"credits"` // 每日窗口外破例的 credit 上限
}

type routingSiteRule struct {
	Site           string         `json:"site"`
	Window         *routingWindow `json:"window"`
	Outside        string         `json:"outside"`
	FallbackBudget *routingBudget `json:"fallbackBudget"`
	Active         *routingActive `json:"active"`
	OnExpire       string         `json:"onExpire"`
}

type routingRule struct {
	Models []string          `json:"models"`
	Sites  []routingSiteRule `json:"sites"`
}

type routingAnchor struct {
	Model      string  `json:"model"`
	Site       string  `json:"site"`
	Multiplier float64 `json:"multiplier"`
}

type routingConfig struct {
	TZ              string         `json:"tz"`
	DefaultPolicy   string         `json:"defaultPolicy"`
	AccountOrder    string         `json:"accountOrder"`
	MinBalanceGuard float64        `json:"minBalanceGuard"`
	MaxPrice        float64        `json:"maxPrice"`
	PriceAnchor     *routingAnchor `json:"priceAnchor"`
	Rules           []routingRule  `json:"rules"`
}

var (
	routingMu sync.RWMutex
	// routingCurrent 为生效配置；未显式配置时使用默认值。
	routingCurrent = defaultRoutingConfig()
	// routingSpend 记录预算消耗：key（站点|模型|日期）-> 已消耗 credit。仅内存态，重启清零。
	routingSpend = map[string]float64{}
)

func defaultRoutingConfig() routingConfig {
	return routingConfig{
		TZ:              "Asia/Shanghai",
		DefaultPolicy:   "allow",
		AccountOrder:    "expiringFirst",
		MinBalanceGuard: 10,
		MaxPrice:        0.06,
		PriceAnchor:     &routingAnchor{Model: "glm-5.3-flash", Site: "cn", Multiplier: 0.06},
	}
}

// setRouting 装载配置，缺失字段回落默认值。
func setRouting(cfg routingConfig) {
	routingMu.Lock()
	defer routingMu.Unlock()
	if cfg.TZ == "" {
		cfg.TZ = defaultRoutingConfig().TZ
	}
	if cfg.DefaultPolicy == "" {
		cfg.DefaultPolicy = "allow"
	}
	if cfg.AccountOrder == "" {
		cfg.AccountOrder = "expiringFirst"
	}
	if cfg.MinBalanceGuard <= 0 {
		cfg.MinBalanceGuard = 10
	}
	if cfg.MaxPrice <= 0 {
		cfg.MaxPrice = 0.06
	}
	if cfg.PriceAnchor == nil {
		cfg.PriceAnchor = &routingAnchor{Model: "glm-5.3-flash", Site: "cn", Multiplier: 0.06}
	}
	routingCurrent = cfg
}

func routingSnapshot() routingConfig {
	routingMu.RLock()
	defer routingMu.RUnlock()
	return routingCurrent
}

// routingLocation 返回配置时区；无 tzdata 环境回落 UTC+8（与 displayLoc 一致取舍）。
func routingLocation(cfg routingConfig) *time.Location {
	if cfg.TZ != "" {
		if loc, err := time.LoadLocation(cfg.TZ); err == nil && loc != nil {
			return loc
		}
	}
	return time.FixedZone("UTC+8", 8*60*60)
}

// -----------------------------------------------------------------------------
// 价格解析：catalog > ledger > probe，并经 priceAnchor 校准
// -----------------------------------------------------------------------------

// measuredRate 返回实测「每 token 消耗 credit」。ledger 真实大样本优先，probe 小样本兜底。
func measuredRate(site, model string) (float64, bool) {
	if r, ok := modelCreditRate(model); ok {
		return r, true
	}
	if p, ok := modelProbes[probeKey(site, model)]; ok && p.Tokens > 0 {
		return p.Credit / float64(p.Tokens), true
	}
	return 0, false
}

// effectiveMultiplier 返回该站点该模型的「有效倍率」，用于价格闸与 cheapest-first。
// 返回值 0 表示免费或未知（不因猜测而拒绝；保底与预算另行兜底）。
func effectiveMultiplier(site, model string) float64 {
	cfg := routingSnapshot()
	// 1) 官方倍率：仅在促销有效时可信
	if e, ok := modelEntry(site, model); ok && e.HasMultiplier && !e.PromoExpired {
		return e.Multiplier
	}
	// 2) 实测校准
	rate, ok := measuredRate(site, model)
	if !ok || rate <= 0 {
		return 0
	}
	anchor := cfg.PriceAnchor
	if anchor == nil || anchor.Multiplier <= 0 {
		return 0
	}
	anchorRate, ok := measuredRate(anchor.Site, anchor.Model)
	if !ok || anchorRate <= 0 {
		return 0
	}
	return anchor.Multiplier * (rate / anchorRate)
}

// sortSitesByPrice 按有效倍率升序（cheapest-first）。
func sortSitesByPrice(sites []string, model string) []string {
	out := append([]string(nil), sites...)
	sort.SliceStable(out, func(i, j int) bool {
		return effectiveMultiplier(out[i], model) < effectiveMultiplier(out[j], model)
	})
	return out
}

// -----------------------------------------------------------------------------
// 站点五态机
// -----------------------------------------------------------------------------

type siteState string

const (
	siteAllowed   siteState = "allowed"   // 全天免费/无窗口限制
	siteInWindow  siteState = "in_window" // 处于免费时段内
	siteBudgeted  siteState = "budgeted"  // 窗口外但当日预算未耗尽（破例，按实际 credit 累计）
	siteForbidden siteState = "forbidden" // 禁止：超价/区间外拒绝/预算耗尽
	siteExpired   siteState = "expired"   // 不在生效区间（由 onExpire 决定最终态）
)

// isFreeNow 判断该态是否属于「当前可免费使用」集合（FREE_NOW）。
func (s siteState) isFreeNow() bool {
	return s == siteAllowed || s == siteInWindow
}

// parseClock 解析 HH:MM 为当日分钟数。
func parseClock(v string) (int, bool) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// budgetKey 生成预算记账键：站点|模型|日期（按配置时区的自然日）。
func budgetKey(site, model string, now time.Time, loc *time.Location) string {
	return site + "|" + normalizeModelName(model) + "|" + now.In(loc).Format("2006-01-02")
}

func budgetUsed(site, model string, now time.Time, loc *time.Location) float64 {
	routingMu.RLock()
	defer routingMu.RUnlock()
	return routingSpend[budgetKey(site, model, now, loc)]
}

// consumeBudget 累加实际消耗（仅真扣费才计；免费模型 credit=0 累加无影响）。
func consumeBudget(site, model string, credit float64, now time.Time) {
	cfg := routingSnapshot()
	loc := routingLocation(cfg)
	routingMu.Lock()
	defer routingMu.Unlock()
	routingSpend[budgetKey(site, model, now, loc)] += credit
}

func resetRoutingSpend() {
	routingMu.Lock()
	defer routingMu.Unlock()
	routingSpend = map[string]float64{}
}

// evaluateSite 判定单个站点在给定时刻的状态。
func evaluateSite(r routingSiteRule, model string, now time.Time, loc *time.Location, cfg routingConfig) siteState {
	// 1) 价格闸：任何状态下，超过价格上限即禁止
	if mult := effectiveMultiplier(r.Site, model); mult > cfg.MaxPrice {
		return siteForbidden
	}
	// 2) 生效区间
	if r.Active != nil {
		var from, until time.Time
		if r.Active.From != "" {
			if t, err := time.ParseInLocation("2006-01-02", r.Active.From, loc); err == nil {
				from = t
			}
		}
		if r.Active.Until != "" {
			if t, err := time.ParseInLocation("2006-01-02", r.Active.Until, loc); err == nil {
				until = t
			}
		}
		lt := now.In(loc)
		if (!from.IsZero() && lt.Before(from)) || (!until.IsZero() && !lt.Before(until)) {
			switch r.OnExpire {
			case "reject":
				return siteForbidden
			case "warn", "allow", "":
				return siteAllowed
			}
			return siteExpired
		}
	}
	// 3) 时段窗口
	if r.Window == nil {
		return siteAllowed
	}
	startMin, ok1 := parseClock(r.Window.Start)
	endMin, ok2 := parseClock(r.Window.End)
	if !ok1 || !ok2 {
		return siteAllowed
	}
	lt := now.In(loc)
	cur := lt.Hour()*60 + lt.Minute()
	inWindow := false
	if startMin > endMin {
		// 跨午夜，如 23:00-08:00
		inWindow = cur >= startMin || cur < endMin
	} else {
		inWindow = cur >= startMin && cur < endMin
	}
	if inWindow {
		return siteInWindow
	}
	// 4) 窗口外：走 fallbackBudget
	if r.FallbackBudget == nil || r.FallbackBudget.Credits <= 0 {
		// 未配预算 → 默认拒绝（决策 3）
		return siteForbidden
	}
	if budgetUsed(r.Site, model, now, loc) < r.FallbackBudget.Credits {
		return siteBudgeted
	}
	return siteForbidden
}

// routingRejection 对模型做站点级判定。
// 返回 blocked=true 仅当「所有站点均禁止」或 defaultPolicy=reject 且无匹配规则；
// 否则返回各站点状态供调度层过滤（FORBIDDEN 需穿透所有 pick 轮，见 commit ②）。
func routingRejection(model string, now time.Time) (bool, map[string]siteState) {
	cfg := routingSnapshot()
	loc := routingLocation(cfg)
	key := normalizeModelName(model)

	var rule *routingRule
	for i := range cfg.Rules {
		for _, m := range cfg.Rules[i].Models {
			if normalizeModelName(m) == key {
				rule = &cfg.Rules[i]
				break
			}
		}
		if rule != nil {
			break
		}
	}
	if rule == nil {
		// 未匹配任何规则：由 defaultPolicy 决定
		if cfg.DefaultPolicy == "reject" {
			return true, nil
		}
		return false, nil
	}

	states := make(map[string]siteState, len(rule.Sites))
	freeExists := false
	for _, s := range rule.Sites {
		st := evaluateSite(s, model, now, loc, cfg)
		states[s.Site] = st
		if st.isFreeNow() {
			freeExists = true
		}
	}
	// 全站 FORBIDDEN 才在入口拒绝；BUDGETED 视为可用但受预算约束
	for _, st := range states {
		if st != siteForbidden {
			return false, states
		}
	}
	_ = freeExists
	return true, states
}

// routingRejectionWithReason 返回 (blocked, 人类可读原因)，供入口 403 使用。
func routingRejectionWithReason(model string, now time.Time) (bool, string) {
	blocked, states := routingRejection(model, now)
	if !blocked {
		return false, ""
	}
	if states == nil {
		return true, "模型未匹配任何 routing 规则，且 defaultPolicy=reject"
	}
	parts := make([]string, 0, len(states))
	for site, st := range states {
		parts = append(parts, fmt.Sprintf("%s=%s", site, st))
	}
	sort.Strings(parts)
	return true, "模型在所有允许站点均不可调度: " + strings.Join(parts, ", ")
}

// -----------------------------------------------------------------------------
// 账号维度：快过期优先 + 保底余额
// -----------------------------------------------------------------------------

// authExpiresAt 返回账号授权到期时间戳；未知/失效排到最后。
func authExpiresAt(acc *Account) int64 {
	const far = int64(1 << 62)
	if acc == nil || acc.Auth == nil {
		return far
	}
	if acc.Auth.Auth.ExpiresAt <= 0 {
		return far
	}
	return acc.Auth.Auth.ExpiresAt
}

// sortedAccounts 按 accountOrder 返回账号顺序。expiringFirst：授权快到期的优先消耗。
func sortedAccounts(in []*Account) []*Account {
	cfg := routingSnapshot()
	out := append([]*Account(nil), in...)
	if cfg.AccountOrder == "expiringFirst" {
		sort.SliceStable(out, func(i, j int) bool {
			return authExpiresAt(out[i]) < authExpiresAt(out[j])
		})
	}
	return out
}

// accountSite 返回账号所属站点 key，避免依赖外部锁。
func accountSite(acc *Account) string {
	if acc == nil || acc.Auth == nil {
		return ""
	}
	if p := profileForEdition(acc.Auth.Edition); p != nil {
		return p.Key
	}
	return ""
}

// guardMinBalance 判断账号是否可调度该模型。
// 保底余额（minBalanceGuard）优先于 fallbackBudget：余额低于阈值时不跑付费模型；
// 免费模型（有效倍率为 0）不受限制——这正是「拦截即零消耗、不消耗预算」的原因。
func guardMinBalance(acc *Account, model string) bool {
	cfg := routingSnapshot()
	if cfg.MinBalanceGuard <= 0 || acc == nil {
		return true
	}
	if effectiveMultiplier(accountSite(acc), model) <= 0 {
		return true
	}
	if !acc.QuotaKnown {
		return true // 额度未知时不做判断
	}
	return acc.QuotaRemaining >= cfg.MinBalanceGuard
}
