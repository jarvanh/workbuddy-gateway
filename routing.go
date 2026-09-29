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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// MaxPrice 为该规则的模型级价格上限；nil 表示沿用全局 maxPrice。
	// 用指针以区分「省略（用全局）」与「显式 0（该规则禁止一切付费模型）」，
	// 与 debug.go 中 NetworkRetries *int 的既有约定一致。
	MaxPrice *float64 `json:"maxPrice"`
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
	// routingSpend 记录预算消耗：key（站点|模型|日期）-> 已消耗 credit。
	// 已持久化到 routingSpendFile，重启后自动恢复（不再清零）。
	routingSpend = map[string]float64{}
)

const (
	// routingSpendFile 预算消耗落盘文件（相对工作目录，与 status/models cache 同目录）。
	routingSpendFile   = "wb-routing-spend.json"
	routingSpendSchema = 1
	// routingSpendKeepDays 保留天数：预算按自然日计量，过期记录只让文件无意义膨胀。
	routingSpendKeepDays = 3
)

// routingSpendFileData 落盘格式。
type routingSpendFileData struct {
	Schema    int                `json:"schema"`
	UpdatedAt int64              `json:"updatedAt"`
	Spend     map[string]float64 `json:"spend"`
}

// pruneRoutingSpendLocked 淘汰过期日期的记录（调用方持有 routingMu 写锁）。
func pruneRoutingSpendLocked() {
	cfg := routingCurrent
	loc := routingLocation(cfg)
	keep := map[string]bool{}
	for i := 0; i < routingSpendKeepDays; i++ {
		keep[time.Now().In(loc).AddDate(0, 0, -i).Format("2006-01-02")] = true
	}
	out := make(map[string]float64, len(routingSpend))
	for k, v := range routingSpend {
		parts := strings.Split(k, "|")
		if len(parts) == 3 && keep[parts[2]] {
			out[k] = v
		}
	}
	routingSpend = out
}

// persistRoutingSpendLocked 把预算消耗原子落盘（调用方持有 routingMu 写锁）。
// 注意：锁内必须直接读 routingCurrent，不可调用 routingSnapshot()——
// Go 的 sync.RWMutex 不可重入，持写锁再加读锁会自锁。
func persistRoutingSpendLocked() {
	pruneRoutingSpendLocked()
	data, err := json.MarshalIndent(routingSpendFileData{
		Schema:    routingSpendSchema,
		UpdatedAt: time.Now().Unix(),
		Spend:     routingSpend,
	}, "", "  ")
	if err != nil {
		return
	}
	tmp := routingSpendFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, routingSpendFile)
}

// loadRoutingSpend 启动时恢复预算消耗；文件缺失/损坏则忽略（按空预算起步）。
func loadRoutingSpend() {
	data, err := os.ReadFile(routingSpendFile)
	if err != nil {
		return
	}
	var d routingSpendFileData
	if err := json.Unmarshal(data, &d); err != nil || d.Schema != routingSpendSchema {
		return
	}
	routingMu.Lock()
	defer routingMu.Unlock()
	if routingSpend == nil {
		routingSpend = map[string]float64{}
	}
	for k, v := range d.Spend {
		routingSpend[k] = v
	}
	pruneRoutingSpendLocked()
}

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

// -----------------------------------------------------------------------------
// 站点级价格样本：按 站点×模型 累计真实消耗（ledger 大样本）
// -----------------------------------------------------------------------------

type sitePriceSample struct {
	Credit float64 `json:"credit"`
	Tokens int64   `json:"tokens"`
	// LastCredit 记录最近一次真实请求的扣费。
	// 免费/收费判定必须看「最近一次」而非累积：限时免费模型在免费期内
	// 累积 Credit 仍可能 >0，用累积会把当期免费的模型误判为收费。
	// 累积 Credit/Tokens 只用于数值定价（大样本校准），不做 free/paid 判定。
	LastCredit float64 `json:"lastCredit"`
}

var (
	sitePriceMu      sync.Mutex
	sitePriceSamples = map[string]*sitePriceSample{} // key: site|model
)

// -----------------------------------------------------------------------------
// 站点价格账本持久化（2026-09-29 事故修复）
// -----------------------------------------------------------------------------
//
// 事故复盘：网关连续重启 6 次，内存中的站点价格样本全部清零；intl 站
// hy4-preview 的目录条目促销已过期，价格退化成「未知」，价格闸按设计
// 「不因猜测拒绝」放行，84 笔请求烧穿 4 个账号共 670.91 credits。
// 落盘后重启不再失明；镜像到 Dropbox 后，runner 重置（/tmp 清空）也能恢复价格记忆。

const (
	priceLedgerSchema      = 1
	priceLedgerMirrorEvery = 30 * time.Second // Dropbox 镜像节流间隔
)

var (
	// priceLedgerFile 价格账本落盘路径（相对工作目录，与 status/models cache 同目录）。
	priceLedgerFile = "wb-price-ledger.json"
	// priceLedgerMirrorDir Dropbox 镜像目录；空串表示禁用镜像（测试用）。
	priceLedgerMirrorDir = "/dropbox/self-hosted/workbuddy-gateway"
	lastLedgerMirror     int64
)

type priceLedgerFileData struct {
	Schema    int                         `json:"schema"`
	UpdatedAt int64                       `json:"updatedAt"`
	Samples   map[string]*sitePriceSample `json:"samples"`
}

// persistPriceLedgerLocked 原子落盘价格账本（调用方持有 sitePriceMu）。
// Dropbox 镜像走 goroutine + 节流：fuse 挂载偶发卡顿不得阻塞计费路径。
func persistPriceLedgerLocked() {
	data, err := json.MarshalIndent(priceLedgerFileData{
		Schema:    priceLedgerSchema,
		UpdatedAt: time.Now().Unix(),
		Samples:   sitePriceSamples,
	}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(priceLedgerFile+".tmp", data, 0600); err == nil {
		_ = os.Rename(priceLedgerFile+".tmp", priceLedgerFile)
	}
	if priceLedgerMirrorDir == "" {
		return
	}
	now := time.Now().Unix()
	if now-atomic.LoadInt64(&lastLedgerMirror) < int64(priceLedgerMirrorEvery/time.Second) {
		return
	}
	atomic.StoreInt64(&lastLedgerMirror, now)
	dir, payload := priceLedgerMirrorDir, data
	go func() {
		tmp := dir + "/" + priceLedgerFile + ".tmp"
		if err := os.WriteFile(tmp, payload, 0600); err != nil {
			return
		}
		_ = os.Rename(tmp, dir+"/"+priceLedgerFile)
	}()
}

// loadPriceLedger 启动时恢复价格账本：本地文件优先，缺失再取 Dropbox 镜像。
// 只填补缺失项：进程内已有的样本（更实时）不被旧文件覆盖。
func loadPriceLedger() {
	candidates := []string{priceLedgerFile}
	if priceLedgerMirrorDir != "" {
		candidates = append(candidates, priceLedgerMirrorDir+"/"+priceLedgerFile)
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var d priceLedgerFileData
		if err := json.Unmarshal(data, &d); err != nil || d.Schema != priceLedgerSchema {
			continue
		}
		sitePriceMu.Lock()
		for k, v := range d.Samples {
			if v == nil || sitePriceSamples[k] != nil {
				continue
			}
			sitePriceSamples[k] = v
		}
		sitePriceMu.Unlock()
		log.Printf("[PriceLedger] 已恢复 %d 条站点价格样本（来源=%s）", len(d.Samples), p)
		return
	}
}

// recordSitePriceSample 累加一次真实请求的 (tokens, credit) 样本。
// 仅由 observeModelCredit（真实业务流量）调用；探测小样本不进入此处。
// credit=0 的免费样本同样入账——这正是「免费模型」结论的数据来源。
func recordSitePriceSample(site, model string, tokens int64, credit float64) {
	if site == "" || tokens <= 0 {
		return
	}
	key := site + "|" + normalizeModelName(model)
	sitePriceMu.Lock()
	s := sitePriceSamples[key]
	if s == nil {
		s = &sitePriceSample{}
		sitePriceSamples[key] = s
	}
	s.Tokens += tokens
	s.Credit += credit
	s.LastCredit = credit
	// 落盘：重启不得让价格记忆清零（2026-09-29 事故根因之一）
	persistPriceLedgerLocked()
	sitePriceMu.Unlock()
}

// siteCreditRate 返回该站点该模型的实测「每 token 消耗 credit」。
func siteCreditRate(site, model string) (float64, bool) {
	sitePriceMu.Lock()
	s := sitePriceSamples[site+"|"+normalizeModelName(model)]
	sitePriceMu.Unlock()
	if s == nil || s.Tokens <= 0 {
		return 0, false
	}
	return s.Credit / float64(s.Tokens), true
}

// priceConfidence 价格结论可信度。
type priceConfidence int

const (
	priceUnknown priceConfidence = iota // 无任何数据
	priceFree                           // 确认免费
	pricePaidNum                        // 付费且数值可信（catalog / 同源 ledger 校准）
	pricePaidUnk                        // 付费但数值不可信（仅 probe 存在性判断）
)

// lastCreditFor 返回该站点该模型最近一次真实请求的扣费（无样本返回 -1）。
func lastCreditFor(site, model string) float64 {
	sitePriceMu.Lock()
	s := sitePriceSamples[site+"|"+normalizeModelName(model)]
	sitePriceMu.Unlock()
	if s == nil {
		return -1
	}
	return s.LastCredit
}

// classifyPrice 返回价格结论与可信度。
// 口径（决策 17）：catalog（促销有效）> 站点级 ledger 校准 > probe 存在性。
// 校准要求锚点与目标同源（同为站点级 ledger）；混源比率会被小样本偏差污染（约 14 倍）。
func classifyPrice(site, model string) (float64, priceConfidence) {
	// 1) 官方倍率：仅在促销有效时可信
	if e, ok := modelEntry(site, model); ok && e.HasMultiplier && !e.PromoExpired {
		if e.Multiplier <= 0 {
			return 0, priceFree
		}
		return e.Multiplier, pricePaidNum
	}
	// 2) 站点级 ledger
	if rate, ok := siteCreditRate(site, model); ok {
		if lastCreditFor(site, model) <= 0 {
			return 0, priceFree // 最近一次免费 → 当前按免费处理
		}
		anchor := routingSnapshot().PriceAnchor
		if anchor != nil && anchor.Multiplier > 0 {
			if anchorRate, ok := siteCreditRate(anchor.Site, anchor.Model); ok && anchorRate > 0 {
				return anchor.Multiplier * (rate / anchorRate), pricePaidNum
			}
		}
		return 0, pricePaidUnk // 付费但无法定标
	}
	// 3) probe 存在性判断（不参与数值）
	switch modelProbeVerdict(site, model) {
	case "free":
		return 0, priceFree
	case "paid":
		return 0, pricePaidUnk
	}
	return 0, priceUnknown
}

// effectiveMultiplier 返回该站点该模型的「有效倍率」，用于价格闸与 cheapest-first。
// 仅 pricePaidNum 返回数值；免费/未知/付费未知一律返回 0（不因猜测而拒绝）。
func effectiveMultiplier(site, model string) float64 {
	mult, conf := classifyPrice(site, model)
	if conf == pricePaidNum {
		return mult
	}
	return 0
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
	// 仅真扣费才落盘：免费模型 credit=0 累加无意义，不该产生磁盘 IO。
	if credit > 0 {
		persistRoutingSpendLocked()
	}
}

func resetRoutingSpend() {
	routingMu.Lock()
	defer routingMu.Unlock()
	routingSpend = map[string]float64{}
}

// evaluateSite 判定单个站点在给定时刻的状态。
// evaluateSite 判定单个站点在给定时刻的状态，价格上限取全局。
func evaluateSite(r routingSiteRule, model string, now time.Time, loc *time.Location, cfg routingConfig) siteState {
	return evaluateSiteWithLimit(r, model, now, loc, cfg, cfg.MaxPrice)
}

// evaluateSiteWithLimit 同 evaluateSite，但允许覆盖价格上限（按规则覆盖时用）。
func evaluateSiteWithLimit(r routingSiteRule, model string, now time.Time, loc *time.Location, cfg routingConfig, maxPrice float64) siteState {
	// 1) 生效区间
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
	// 2) 时段窗口：窗口内视为该站免费期（政策表达），直接放行、不受价格闸影响。
	//    窗口必须先于价格闸：如 hy4-preview cn 夜间免费而目录价仍标 0.29x，
	//    价格闸先行会把免费窗口一并封死（以方案 §九 运行推演为准）。
	inWindow := false
	hasWindow := false
	if r.Window != nil {
		startMin, ok1 := parseClock(r.Window.Start)
		endMin, ok2 := parseClock(r.Window.End)
		if !ok1 || !ok2 {
			// 窗口配置不合法：视为无限制
			return siteAllowed
		}
		hasWindow = true
		lt := now.In(loc)
		cur := lt.Hour()*60 + lt.Minute()
		if startMin > endMin {
			// 跨午夜，如 23:00-08:00
			inWindow = cur >= startMin || cur < endMin
		} else {
			inWindow = cur >= startMin && cur < endMin
		}
		if inWindow {
			return siteInWindow
		}
	}
	// 3) 价格闸（无窗口或窗口外）：超过上限即禁止
	if mult := effectiveMultiplier(r.Site, model); mult > maxPrice {
		return siteForbidden
	}
	if !hasWindow {
		return siteAllowed
	}
	// 4) 窗口外：outside 策略 + fallbackBudget
	if strings.EqualFold(strings.TrimSpace(r.Outside), "prefer") {
		// 放行但降权：cheapest-first 按倍率排序，天然靠后
		return siteAllowed
	}
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
		// 未匹配任何规则：先看 defaultPolicy，再过「幽灵模型护栏」
		if cfg.DefaultPolicy == "reject" {
			return true, nil
		}
		if ghostPaidBlocked(model, cfg) {
			return true, nil
		}
		return false, nil
	}

	limit := cfg.MaxPrice
	if rule.MaxPrice != nil {
		limit = *rule.MaxPrice
	}
	states := make(map[string]siteState, len(rule.Sites))
	freeExists := false
	for _, s := range rule.Sites {
		st := evaluateSiteWithLimit(s, model, now, loc, cfg, limit)
		states[s.Site] = st
		if st.isFreeNow() {
			freeExists = true
		}
	}
	// 站点白名单语义（2026-09-29 事故根因）：规则命中的模型，未声明的站点一律 FORBIDDEN。
	// 事故中 hy4-preview 只声明 cn，intl 未声明却被当作「无限制」参与代偿调度，
	// cn 站 429 冷却期间 84 笔请求全部涌向 intl，烧光 4 个小额度账号。
	for _, site := range allRoutingSites() {
		if _, ok := states[site]; !ok {
			states[site] = siteForbidden
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

// ghostPaidBlocked 判断「未匹配任何 routing 规则」的模型是否应被价格闸拒绝。
// 这是 ja 事故同源风险的护栏：当年 glm-5.3-flash 正是目录外幽灵模型、
// 无规则、裸奔被真实计费烧光。探测是事后的（先有请求才探），挡不住第一次，
// 因此只能靠「价格明确超上限就拒绝」来堵。
//
// 关键约束（保守，且绝不能阻断学习闭环）：
//   - 只在「价格明确已知(pricePaidNum)且超上限」时拒绝；
//   - 价格未知(priceUnknown)、或付费但无法定标(pricePaidUnk)一律放行。
//     理由：拒绝会阻断请求 → 再也拿不到新的 credit 证据 → 模型被永久
//     锁死在 paid，连「恢复免费」都无法再观测（死锁式误杀）。
//   - 任一站点确认免费、或任一站点价格可接受 → 放行。
func ghostPaidBlocked(model string, cfg routingConfig) bool {
	anyFreeOrOK := false
	anyOver := false
	for _, site := range []string{"cn", "intl"} {
		mult, conf := classifyPrice(site, model)
		switch conf {
		case priceFree:
			anyFreeOrOK = true
		case pricePaidNum:
			if mult <= cfg.MaxPrice {
				anyFreeOrOK = true
			} else {
				anyOver = true
			}
		}
	}
	if anyFreeOrOK {
		return false
	}
	return anyOver
}

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
	_, conf := classifyPrice(accountSite(acc), model)
	if conf == priceFree || conf == priceUnknown {
		return true
	}
	if !acc.QuotaKnown {
		return true // 额度未知时不做判断
	}
	return acc.QuotaRemaining >= cfg.MinBalanceGuard
}

// -----------------------------------------------------------------------------
// 在途预留：并发烧穿护栏（2026-09-29 事故修复）
// -----------------------------------------------------------------------------
//
// guardMinBalance 只在选号瞬间看静态余额，而额度要等请求完成后的额度扫描才回写。
// 事故中 84 笔请求并发在途，每次选号时余额都还 ≥10（保护通过），
// 额度却在请求过程中被烧穿，事后才发现归零。
// 在途预留把「已选中但尚未结算」的请求按预留额记账，选号时以
// 「余额 - 在途预留」判定，并发请求被挡在门外而不是一起冲进去烧穿。
//
// 预留额口径：
//   - 确认免费（priceFree）：0 —— 免费模型不受限，零余额账号仍可服务；
//   - 价格已知/可校准（pricePaidNum、pricePaidUnk）：minBalanceGuard；
//   - 价格未知（priceUnknown）：minBalanceGuard × 10 —— 未知价可能极贵
//     （事故中 intl hy4-preview 目录标 x0.00，实际等效 x66），按最坏情况预留。

const (
	inFlightReserveTTL    = 30 * time.Minute // 超时未释放的兜底回收阈值
	inFlightUnknownFactor = 10              // 未知价模型的预留放大倍数
)

// inFlightReserve 单笔在途请求的预留记录。
type inFlightReserve struct {
	Credit float64   // 预留 credit
	At     time.Time // 预留时刻：超时兜底回收依据
}

// reserveInFlightLocked 检查并提交一笔在途预留（调用方持有 accountMu）。
func reserveInFlightLocked(acc *Account, reqID uint64, model string) bool {
	cfg := routingSnapshot()
	guard := cfg.MinBalanceGuard
	if acc == nil || guard <= 0 {
		return true // 保底关闭时整体停用（沿用原语义）
	}
	now := time.Now()
	// 兜底回收：流式异常中断而未走到释放路径的残留
	for id, r := range acc.inFlight {
		if now.Sub(r.At) > inFlightReserveTTL {
			delete(acc.inFlight, id)
		}
	}
	// 账号已实测该模型为免费：不预留。零余额账号正是靠这一条服务免费模型。
	if st := acc.ModelStates[normalizeModelName(model)]; st != nil && st.CostClass == modelCostFree {
		return true
	}
	_, conf := classifyPrice(accountSite(acc), model)
	if conf == priceFree {
		return true
	}
	// 额度未知，或额度已归零：没有额度可保护，放行。
	// 这正是「受控探测」（selectionProbeExhausted）与「免费耗尽账号」的路径：
	// 单笔且有 5 分钟频控，护栏不得掐断价格学习闭环。
	if !acc.QuotaKnown || acc.QuotaRemaining <= 0 {
		return true
	}
	avail := acc.QuotaRemaining - inFlightReservedLocked(acc)
	reserve := guard
	if conf == priceUnknown {
		reserve = guard * inFlightUnknownFactor
	}
	if avail-reserve < 0 {
		log.Printf("[InFlight] 账号 %s 模型 %s 在途预留超限：剩余=%.2f 在途已占=%.2f 本笔预留=%.2f，跳过该账号",
			acc.Path, model, acc.QuotaRemaining, acc.QuotaRemaining-avail, reserve)
		return false
	}
	if reqID == 0 {
		return true // 内部探测：只判定不占额
	}
	if acc.inFlight == nil {
		acc.inFlight = map[uint64]inFlightReserve{}
	}
	acc.inFlight[reqID] = inFlightReserve{Credit: reserve, At: now}
	return true
}

// releaseInFlight 释放一笔请求的在途预留（请求结束 / 换号 / 失败时调用）。
func releaseInFlight(acc *Account, reqID uint64) {
	if acc == nil || reqID == 0 {
		return
	}
	accountMu.Lock()
	delete(acc.inFlight, reqID)
	accountMu.Unlock()
}

// releasePendingInFlight 释放换号/失败路径占用的预留（nil 安全）。
func releasePendingInFlight(acc *Account, reqID uint64) {
	if acc != nil {
		releaseInFlight(acc, reqID)
	}
}

// inFlightReservedLocked 汇总该账号在途预留总额（调用方持有 accountMu）。
func inFlightReservedLocked(acc *Account) float64 {
	var sum float64
	for _, r := range acc.inFlight {
		sum += r.Credit
	}
	return sum
}
