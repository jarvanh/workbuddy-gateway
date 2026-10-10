package main

// warmup.go —— 5 小时窗口主动触发
//
// 背景：部分上游模型按「首次调用后的 5 小时」计算窗口额度。若不在意某个时刻
// 主动打一次请求，窗口就不会开始计时，白天真正要用时反而只剩很短的可用时间。
//
// 功能：
//   - 每天在指定时刻（默认 04:00，配置时区）对筛选出的模型各发一次最小请求，
//     把 5 小时窗口「打开」；
//   - 模型可按价格上限筛选（默认 0.06）；上限设为 0 表示只触发已确认免费的模型；
//   - 也可直接指定模型列表，跳过价格筛选；
//   - 触发结果落盘（wb-warmup.json）。到点没起来 / 冷却中 / 网络失败的模型
//     会在补触发窗口内按间隔重试，直到成功或窗口过期；
//   - 主动探测（模型价格探测）可与本功能同刻执行：warmup 开启时跟随 warmup 时刻，
//     warmup 关闭时默认 06:00。
//
// 安全约束（与价格闸同源，绝不烧钱）：
//   - 只对「确认免费」或「价格明确已知且不超过上限」的模型下手；
//     价格未知、或「付费但无法定标」的模型一律跳过（不因猜测触发真实计费）。
//   - 只走 routing 允许且当前未被 FORBIDDEN 的站点。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	warmupStateFile    = "wb-warmup.json"
	warmupStateSchema  = 1
	warmupHistoryLimit = 14

	warmupDefaultTime         = "04:00"
	warmupDefaultPrice        = 0.06
	warmupDefaultCatchUpHours = 4
	warmupDefaultRetryMinutes = 10
	probeDefaultTime          = "06:00"

	// warmupDefaultMaxRateLimitRetries 是单个模型「连续被限流」的放弃阈值。
	// 补触发窗口内每 retryMinutes 重试一轮，而 6004 模型级限流往往整个窗口都不恢复：
	// 没有上限时一个卡住的模型会刷满整个窗口（实测 13 轮全部无效）。
	// 达到阈值即判本轮失败，不再重试 —— 省下的是真实的上游请求。
	warmupDefaultMaxRateLimitRetries = 3
	// warmupDefaultCooldownTriggerMinMinutes 冷却恢复后补触发的最小间隔（分钟）。
	warmupDefaultCooldownTriggerMinMinutes = 10
	// warmupCooldownWatchTick 冷却恢复监听的扫描间隔。
	warmupCooldownWatchTick = 30 * time.Second

	warmupTickInterval = 30 * time.Second
)

// warmupConfig 是 config.json 的 warmup 段。指针字段用于区分「未配置」（回落默认）
// 与「显式设为 0 / false」。
type warmupConfig struct {
	// Enabled 总开关，默认 true。
	Enabled *bool `json:"enabled"`
	// Time 每日触发时刻（HH:MM，配置时区），默认 04:00。
	Time string `json:"time"`
	// MaxPrice 价格上限；默认 0.06。设为 0 表示只触发已确认免费的模型。
	MaxPrice *float64 `json:"maxPrice"`
	// Models 显式指定模型列表；非空时跳过价格筛选。
	Models []string `json:"models"`
	// CatchUpHours 补触发窗口（自计划时刻起算的小时数），默认 4。
	CatchUpHours *int `json:"catchUpHours"`
	// RetryMinutes 补触发重试间隔（分钟），默认 10。
	RetryMinutes *int `json:"retryMinutes"`
	// MaxRateLimitRetries 单个模型连续被限流（6004）多少次后放弃本轮，默认 3。
	// 设为 0 表示不启用熔断（回到旧行为：刷到补触发窗口结束）。
	MaxRateLimitRetries *int `json:"maxRateLimitRetries"`
	// Windows 允许触发的时间段列表，形如 ["06:00-21:00"]，可配置多段。
	// 非空时优先于 Time：每个时段的开始时刻触发一轮；时段内模型冷却恢复也会补触发；
	// 时段外不触发，避免半夜白白消耗额度。End <= Start 视为跨天。
	Windows []string `json:"windows"`
	// CooldownTrigger 模型冷却恢复后立即触发一轮，默认 true。
	CooldownTrigger *bool `json:"cooldownTrigger"`
	// CooldownTriggerMinMinutes 冷却恢复触发的最小间隔（分钟），默认 10，防抖动刷屏。
	CooldownTriggerMinMinutes *int `json:"cooldownTriggerMinMinutes"`
	// Notify 是否在每轮结束后推送结果（走 notify 通道），默认 false。
	Notify *bool `json:"notify"`
}

// probeScheduleConfig 是 config.json 的 probe.schedule 段（每日主动探测时刻）。
type probeScheduleConfig struct {
	// Enabled 每日定时主动探测总开关，默认 true。
	Enabled *bool `json:"enabled"`
	// FollowWarmup 是否跟随 warmup 时刻，默认 true。
	// warmup 开启 → 探测与 warmup 同刻执行；warmup 关闭 → 用 Time（默认 06:00）。
	FollowWarmup *bool `json:"followWarmup"`
	// Time warmup 关闭（或显式关闭跟随）时的每日探测时刻，默认 06:00。
	Time string `json:"time"`
	// Notify 每日探测结论汇总是否推送（默认 false）。
	// 探测收敛前是 2 分钟一轮，逐轮推送纯属噪音，故按「每天一条」汇总发送。
	Notify *bool `json:"notify"`
}

// timeWindow 一个允许触发的时间段（HH:MM-HH:MM，配置时区）。
// End 早于或等于 Start 视为跨天（例如 22:00-06:00）。
type timeWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// parseTimeWindow 解析 "HH:MM-HH:MM"。
func parseTimeWindow(s string) (timeWindow, bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return timeWindow{}, false
	}
	a := strings.TrimSpace(parts[0])
	b := strings.TrimSpace(parts[1])
	if _, ok := parseClock(a); !ok {
		return timeWindow{}, false
	}
	if _, ok := parseClock(b); !ok {
		return timeWindow{}, false
	}
	return timeWindow{Start: a, End: b}, true
}

// startOn 返回该窗口在 day 所属自然日的开始时刻。
func (w timeWindow) startOn(day time.Time) time.Time {
	return w.clockOn(day, w.Start, 0, 0)
}

// endOn 返回该窗口的结束时刻；End <= Start 时顺延到次日（跨天窗口）。
func (w timeWindow) endOn(day time.Time) time.Time {
	e := w.clockOn(day, w.End, 0, 0)
	if !e.After(w.startOn(day)) {
		e = e.AddDate(0, 0, 1)
	}
	return e
}

func (w timeWindow) clockOn(day time.Time, clock string, defHour, defMin int) time.Time {
	loc := warmupLocation()
	local := day.In(loc)
	m, _ := parseClockOr(clock, defHour, defMin)
	return time.Date(local.Year(), local.Month(), local.Day(), m/60, m%60, 0, 0, loc)
}

// contains 判断 now 是否落在该窗口内（含跨天情况）。
func (w timeWindow) contains(now time.Time) bool {
	for _, d := range []time.Time{now, now.AddDate(0, 0, -1)} {
		s := w.startOn(d)
		e := w.endOn(d)
		if !now.Before(s) && now.Before(e) {
			return true
		}
	}
	return false
}

// currentWindowIndex 返回 now 所处窗口的下标；不在任何窗口内返回 -1。
func currentWindowIndex(now time.Time, windows []timeWindow) int {
	for i, w := range windows {
		if w.contains(now) {
			return i
		}
	}
	return -1
}

// nextWindowStartAfter 返回严格晚于 now 的最近一个窗口开始时刻及其下标。
func nextWindowStartAfter(now time.Time, windows []timeWindow) (time.Time, int) {
	var best time.Time
	idx := -1
	for i, w := range windows {
		for _, d := range []time.Time{now, now.AddDate(0, 0, 1)} {
			c := w.startOn(d)
			if c.After(now) && (best.IsZero() || c.Before(best)) {
				best = c
				idx = i
			}
		}
	}
	return best, idx
}

func (w timeWindow) String() string { return w.Start + "-" + w.End }

// warmupRuntime 是装载后的生效配置。
type warmupRuntime struct {
	Enabled             bool
	Time                string
	MaxPrice            float64
	Models              []string
	CatchUpHours        int
	RetryMinutes        int
	Notify              bool
	MaxRateLimitRetries int
	// Windows 允许触发的时间段；非空时优先于 Time。
	Windows []timeWindow
	// CooldownTrigger 模型冷却恢复后立即补触发一轮，默认 true。
	CooldownTrigger bool
	// CooldownTriggerMinMinutes 冷却恢复触发的最小间隔（分钟），默认 10。
	CooldownTriggerMinMinutes int
}

// probeRuntime 是装载后的生效探测调度配置。
type probeRuntime struct {
	Enabled      bool
	FollowWarmup bool
	Time         string
	Notify       bool
}

var (
	warmupMu          sync.Mutex
	warmupCurrent     = defaultWarmupRuntime()
	probeCurrent      = defaultProbeRuntime()
	warmupTrigger     = make(chan struct{}, 1) // 立即执行一轮（HTTP run / 配置变更）
	warmupWake        = make(chan struct{}, 1)
	probeScheduleDate string // 进程内记录已触发过的探测日期，防止同日重复

	// warmupWakeReason 记录下一轮 runWarmupOnce 的触发来源：
	// scheduled（每日定时）/ cooldown（模型冷却恢复）/ manual（HTTP run）。
	// 三者的通知标题与去重键必须分开，否则主人分不清是哪一路触发的。
	warmupReasonMu   sync.Mutex
	warmupWakeReason string
)

func setWarmupWakeReason(r string) {
	setWarmupWake(r, nil)
}

// warmupRecoveredKeys 传递「本次冷却恢复的具体项」（账号|模型 列表）。
// 与 warmupWakeReason 同生命周期：watch loop 设置、runWarmupOnce 取走清空。
// 仅 cooldown 路径非空，通知用它写明「本次触发的是哪个模型」。
var (
	warmupRecoveredMu   sync.Mutex
	warmupRecoveredKeys []string
)

// setWarmupWake 设置唤醒来源与恢复项；recovered 仅 cooldown 路径使用。
func setWarmupWake(reason string, recovered []string) {
	warmupReasonMu.Lock()
	warmupWakeReason = reason
	warmupReasonMu.Unlock()
	warmupRecoveredMu.Lock()
	warmupRecoveredKeys = recovered
	warmupRecoveredMu.Unlock()
}

// takeWarmupRecovered 取出并清空恢复项；仅 reason=="cooldown" 时非空。
func takeWarmupRecovered() []string {
	warmupRecoveredMu.Lock()
	defer warmupRecoveredMu.Unlock()
	r := warmupRecoveredKeys
	warmupRecoveredKeys = nil
	return r
}

// peekWarmupWakeReason 读取当前触发来源但不清除。
// warmupLoop 用它判断要不要穿透门闸，真正的清除仍在 runWarmupOnce 里做，
// 所以这里绝不能消费掉，否则 runWarmupOnce 会退化成 scheduled。
func peekWarmupWakeReason() string {
	warmupReasonMu.Lock()
	defer warmupReasonMu.Unlock()
	return warmupWakeReason
}

// takeWarmupWakeReason 取出并清空触发来源；未设置时视为定时触发。
func takeWarmupWakeReason() string {
	warmupReasonMu.Lock()
	defer warmupReasonMu.Unlock()
	r := warmupWakeReason
	warmupWakeReason = ""
	if r == "" {
		return "scheduled"
	}
	return r
}

func defaultWarmupRuntime() warmupRuntime {
	return warmupRuntime{
		Enabled:                   true,
		Time:                      warmupDefaultTime,
		MaxPrice:                  warmupDefaultPrice,
		CatchUpHours:              warmupDefaultCatchUpHours,
		RetryMinutes:              warmupDefaultRetryMinutes,
		MaxRateLimitRetries:       warmupDefaultMaxRateLimitRetries,
		CooldownTrigger:           true,
		CooldownTriggerMinMinutes: warmupDefaultCooldownTriggerMinMinutes,
	}
}

func defaultProbeRuntime() probeRuntime {
	return probeRuntime{Enabled: true, FollowWarmup: true, Time: probeDefaultTime, Notify: false}
}

// setWarmup 装载 warmup 段，缺失字段回落默认值。
func setWarmup(cfg warmupConfig) {
	rt := defaultWarmupRuntime()
	if cfg.Enabled != nil {
		rt.Enabled = *cfg.Enabled
	}
	if t := strings.TrimSpace(cfg.Time); t != "" {
		rt.Time = t
	}
	if cfg.MaxPrice != nil && *cfg.MaxPrice >= 0 {
		rt.MaxPrice = *cfg.MaxPrice
	}
	models := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}
	rt.Models = models
	if cfg.CatchUpHours != nil && *cfg.CatchUpHours > 0 {
		rt.CatchUpHours = *cfg.CatchUpHours
	}
	if cfg.RetryMinutes != nil && *cfg.RetryMinutes > 0 {
		rt.RetryMinutes = *cfg.RetryMinutes
	}
	if cfg.MaxRateLimitRetries != nil && *cfg.MaxRateLimitRetries >= 0 {
		rt.MaxRateLimitRetries = *cfg.MaxRateLimitRetries
	}
	if cfg.Notify != nil {
		rt.Notify = *cfg.Notify
	}
	if len(cfg.Windows) > 0 {
		ws := make([]timeWindow, 0, len(cfg.Windows))
		for _, s := range cfg.Windows {
			if w, ok := parseTimeWindow(s); ok {
				ws = append(ws, w)
			}
		}
		rt.Windows = ws
	}
	if cfg.CooldownTrigger != nil {
		rt.CooldownTrigger = *cfg.CooldownTrigger
	}
	if cfg.CooldownTriggerMinMinutes != nil && *cfg.CooldownTriggerMinMinutes >= 0 {
		rt.CooldownTriggerMinMinutes = *cfg.CooldownTriggerMinMinutes
	}
	warmupMu.Lock()
	warmupCurrent = rt
	warmupMu.Unlock()
}

// cycleKeyOf 不再使用：定时触发与冷却恢复补触发共享「自然日」周期。
// 保留空实现会导致死代码，故移除（周期键统一用 cycleDateOf）。

// lastWindowStartOnOrBefore 返回不晚于 now 的最近一个窗口开始时刻；
// 没有任何窗口已开始时返回零值。
func lastWindowStartOnOrBefore(now time.Time, windows []timeWindow) time.Time {
	var best time.Time
	for _, w := range windows {
		for _, d := range []time.Time{now, now.AddDate(0, 0, -1)} {
			c := w.startOn(d)
			if !c.After(now) && (best.IsZero() || c.After(best)) {
				best = c
			}
		}
	}
	return best
}

func warmupSnapshot() warmupRuntime {
	warmupMu.Lock()
	defer warmupMu.Unlock()
	return warmupCurrent
}

func setProbeSchedule(cfg probeScheduleConfig) {
	rt := defaultProbeRuntime()
	if cfg.Enabled != nil {
		rt.Enabled = *cfg.Enabled
	}
	if cfg.FollowWarmup != nil {
		rt.FollowWarmup = *cfg.FollowWarmup
	}
	if t := strings.TrimSpace(cfg.Time); t != "" {
		rt.Time = t
	}
	if cfg.Notify != nil {
		rt.Notify = *cfg.Notify
	}
	warmupMu.Lock()
	probeCurrent = rt
	warmupMu.Unlock()
}

func probeScheduleSnapshot() probeRuntime {
	warmupMu.Lock()
	defer warmupMu.Unlock()
	return probeCurrent
}

// -----------------------------------------------------------------------------
// 状态落盘
// -----------------------------------------------------------------------------

type warmupModelState struct {
	Status   string `json:"status"` // pending | ok | failed | skipped
	Site     string `json:"site,omitempty"`
	Account  string `json:"account,omitempty"`
	Verdict  string `json:"verdict,omitempty"` // free | paid
	HTTP     int    `json:"http,omitempty"`
	Attempts int    `json:"attempts"`
	LastAt   int64  `json:"lastAt,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// RateLimitHits 累计本周期内该模型被限流（6004）的次数，成功即清零。
	// 达到 warmupRuntime.MaxRateLimitRetries 时放弃本周期，不再重试。
	RateLimitHits int `json:"rateLimitHits,omitempty"`
}

type warmupCycle struct {
	Date          string                       `json:"date"`
	ScheduledAt   int64                        `json:"scheduledAt"`
	StartedAt     int64                        `json:"startedAt,omitempty"`
	CompletedAt   int64                        `json:"completedAt,omitempty"`
	LastAttemptAt int64                        `json:"lastAttemptAt,omitempty"`
	Attempts      int                          `json:"attempts"`
	Models        map[string]*warmupModelState `json:"models"`
}

type warmupHistoryEntry struct {
	Date        string `json:"date"`
	ScheduledAt int64  `json:"scheduledAt"`
	OK          int    `json:"ok"`
	Failed      int    `json:"failed"`
	Skipped     int    `json:"skipped"`
	CompletedAt int64  `json:"completedAt,omitempty"`
	// CatchUp 标识本轮是靠补触发完成的（计划时刻服务未运行或首次未成功）。
	CatchUp bool `json:"catchUp,omitempty"`
}

type warmupState struct {
	Schema    int                  `json:"schema"`
	UpdatedAt int64                `json:"updatedAt"`
	Current   *warmupCycle         `json:"current,omitempty"`
	History   []warmupHistoryEntry `json:"history,omitempty"`
}

var warmupStateMu sync.Mutex

func loadWarmupState() *warmupState {
	data, err := os.ReadFile(warmupStateFile)
	if err != nil {
		return &warmupState{Schema: warmupStateSchema}
	}
	var st warmupState
	if err := json.Unmarshal(data, &st); err != nil || st.Schema != warmupStateSchema {
		return &warmupState{Schema: warmupStateSchema}
	}
	if st.Current != nil && st.Current.Models == nil {
		st.Current.Models = map[string]*warmupModelState{}
	}
	return &st
}

// saveWarmupStateLocked 原子落盘（调用方持有 warmupStateMu）。
func saveWarmupStateLocked(st *warmupState) {
	st.Schema = warmupStateSchema
	st.UpdatedAt = time.Now().Unix()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := warmupStateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, warmupStateFile)
}

// -----------------------------------------------------------------------------
// 时刻计算
// -----------------------------------------------------------------------------

// warmupLocation 返回 warmup 使用的时区（与 routing 同配置，无 tzdata 回落 UTC+8）。
func warmupLocation() *time.Location {
	return routingLocation(routingSnapshot())
}

// parseClockOr 解析 HH:MM，失败回落给定默认值（当日分钟数 + 是否是次日）。
func parseClockOr(v string, defHour, defMin int) (int, bool) {
	if m, ok := parseClock(v); ok {
		return m, true
	}
	return defHour*60 + defMin, false
}

// scheduledTimeOn 计算某个自然日（配置时区）上「HH:MM」对应的绝对时间。
func scheduledTimeOn(day time.Time, clock string, defHour, defMin int) time.Time {
	loc := warmupLocation()
	local := day.In(loc)
	m, _ := parseClockOr(clock, defHour, defMin)
	return time.Date(local.Year(), local.Month(), local.Day(), m/60, m%60, 0, 0, loc)
}

// cycleDateOf 返回该时刻所属的计划日期（配置时区的自然日）。
func cycleDateOf(t time.Time) string {
	return t.In(warmupLocation()).Format("2006-01-02")
}

// nextScheduledAfter 返回严格晚于 now 的下一个计划时刻。
func nextScheduledAfter(now time.Time, clock string, defHour, defMin int) time.Time {
	next := scheduledTimeOn(now, clock, defHour, defMin)
	if !next.After(now) {
		next = scheduledTimeOn(now.AddDate(0, 0, 1), clock, defHour, defMin)
	}
	return next
}

// -----------------------------------------------------------------------------
// 模型筛选
// -----------------------------------------------------------------------------

// warmupCandidateSites 返回该模型当前允许触发的站点（便宜优先）。
// 返回空切片表示没有可安全触发的站点。
//
// ignorePrice=true（显式指定模型）：只要 routing 允许就触发，不套价格上限——
// 用户点名的模型按其意图执行。
// ignorePrice=false（价格筛选）：只对「确认免费」或「价格明确已知且不超上限」
// 的站点下手；价格未知或「付费但无法定标」一律跳过（不因猜测触发真实计费）。
func warmupCandidateSites(model string, maxPrice float64, ignorePrice bool, now time.Time) []string {
	blocked, states := routingRejection(model, now)
	if blocked {
		return nil
	}
	var out []string
	for _, site := range allRoutingSites() {
		if states != nil {
			if st, ok := states[site]; ok && st == siteForbidden {
				continue
			}
		}
		if ignorePrice {
			out = append(out, site)
			continue
		}
		mult, conf := classifyPrice(site, model)
		switch conf {
		case priceFree:
			out = append(out, site)
		case pricePaidNum:
			// maxPrice=0 的语义：只触发免费模型，付费一律跳过。
			if maxPrice > 0 && mult <= maxPrice {
				out = append(out, site)
			}
		default:
			// priceUnknown / pricePaidUnk：不因猜测触发真实计费。
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return effectiveMultiplier(out[i], model) < effectiveMultiplier(out[j], model)
	})
	return out
}

// warmupSelectModels 选出本轮要触发的模型。
// 显式 Models 优先（仅过滤被网关禁用的模型）；否则按价格上限筛选全目录。
func warmupSelectModels(rt warmupRuntime, now time.Time) []string {
	if len(rt.Models) > 0 {
		out := make([]string, 0, len(rt.Models))
		seen := map[string]bool{}
		for _, m := range rt.Models {
			key := normalizeModelName(m)
			if key == "" || seen[key] {
				continue
			}
			if disabled, _ := modelDisabled(m); disabled {
				log.Printf("[Warmup] 模型 %s 已被网关禁用（黑名单/白名单），跳过触发", m)
				continue
			}
			seen[key] = true
			out = append(out, m)
		}
		return out
	}
	ids, _ := mergedModelIDs()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if disabled, _ := modelDisabled(id); disabled {
			continue
		}
		if len(warmupCandidateSites(id, rt.MaxPrice, false, now)) > 0 {
			out = append(out, id)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// 执行
// -----------------------------------------------------------------------------

// warmupTriggerModel 对单个模型发一次最小请求，成功即返回。
func warmupTriggerModel(model string, rt warmupRuntime, now time.Time, ms *warmupModelState) {
	ignorePrice := len(rt.Models) > 0
	sites := warmupCandidateSites(model, rt.MaxPrice, ignorePrice, now)
	if len(sites) == 0 {
		ms.Status = "skipped"
		ms.Detail = "无符合价格上限且允许调度的站点"
		ms.LastAt = now.Unix()
		return
	}
	var lastDetail string
	var lastStatus string
	var retryAt time.Time
	for _, site := range sites {
		acc := pickProbeAccountForModel(site, model)
		if acc == nil {
			lastStatus = "unavailable"
			lastDetail = fmt.Sprintf("站点 %s 无可用账号", site)
			continue
		}
		out := probeModelPriceEx(acc, model)
		ms.Attempts++
		ms.LastAt = time.Now().Unix()
		ms.Site = site
		ms.Account = filepath.Base(acc.Path)
		ms.HTTP = out.HTTP
		ms.Detail = out.Detail
		if out.ok() {
			ms.Status = "ok"
			ms.Verdict = out.Verdict
			ms.RateLimitHits = 0 // 成功即清零，只统计「连续」被限流
			log.Printf("[Warmup] 模型=%s 站点=%s 账号=%s 触发成功（HTTP %d，%s）", model, site, ms.Account, out.HTTP, out.Detail)
			return
		}
		lastStatus = out.Status
		lastDetail = fmt.Sprintf("站点 %s：%s", site, out.Detail)
		if out.Status == "rate_limited" {
			ms.RateLimitHits++
		}
		if out.RetryAt.After(retryAt) {
			retryAt = out.RetryAt
		}
		log.Printf("[Warmup] 模型=%s 站点=%s 账号=%s 触发未成功：%s", model, site, ms.Account, out.Detail)
	}
	ms.Detail = lastDetail
	// 授权失效属于账号本身不可用，重试没有意义；
	// 「无可用账号」可能只是账号冷却中或尚未热加载进来，保留 pending 让补触发窗口内再试。
	if lastStatus == "auth_failed" {
		ms.Status = "failed"
		return
	}
	// 连续限流熔断：6004 这类模型级限流往往整个补触发窗口都不恢复，
	// 没有上限时一个卡住的模型会刷满窗口（实测 13 轮全部无效，白白消耗上游请求）。
	// 达到阈值即判本周期失败，不再重试。阈值 <=0 表示不启用熔断（旧行为）。
	if rt.MaxRateLimitRetries > 0 && ms.RateLimitHits >= rt.MaxRateLimitRetries {
		ms.Status = "failed"
		ms.Detail += fmt.Sprintf("（连续被限流 %d 次，已达上限 %d，本周期放弃重试）",
			ms.RateLimitHits, rt.MaxRateLimitRetries)
		log.Printf("[Warmup] 模型=%s 连续被限流 %d 次（上限 %d），本周期放弃重试",
			model, ms.RateLimitHits, rt.MaxRateLimitRetries)
		return
	}
	ms.Status = "pending"
	if retryAt.After(time.Now()) {
		// 冷却未过：把下次重试时间写进 Detail 不便机器读取，这里用 LastAt 之外
		// 的字段记录——调度器按固定间隔重试，冷却由候选站点筛选自然规避。
		ms.Detail += fmt.Sprintf("（上游建议 %s 后重试）", formatDisplayTime(retryAt))
	}
}

// runWarmupOnce 执行一轮：只处理 pending 模型，已 ok/failed/skipped 的不重复打。
// 返回本轮结束后的统计。
func runWarmupOnce(rt warmupRuntime, now time.Time) (ok, failed, skipped, pending int, catchUp bool) {
	reason := takeWarmupWakeReason()
	recoveredKeys := takeWarmupRecovered()
	models := warmupSelectModels(rt, now)
	if len(models) == 0 {
		log.Printf("[Warmup] 本轮没有符合条件的模型（上限=%s，显式模型=%d）", formatQuota(rt.MaxPrice), len(rt.Models))
		return 0, 0, 0, 0, false
	}

	warmupStateMu.Lock()
	st := loadWarmupState()
	// 周期键按自然日：定时触发与冷却恢复补触发共享同一周期，
	// runWarmupOnce 只处理 pending 模型，所以同一天多次调用不会重复打。
	date := cycleDateOf(now)
	scheduledAt := scheduledTimeOn(now, rt.Time, 4, 0)
	cy := st.Current
	if cy == nil || cy.Date != date {
		// 新的一天：开启新周期。若 now 已过计划时刻（服务启动晚于触发点），
		// 本轮即为补触发。
		catchUp = now.After(scheduledAt)
		cy = &warmupCycle{Date: date, ScheduledAt: scheduledAt.Unix(), Models: map[string]*warmupModelState{}}
		st.Current = cy
	}
	if cy.Models == nil {
		cy.Models = map[string]*warmupModelState{}
	}
	if cy.StartedAt == 0 {
		cy.StartedAt = now.Unix()
	}
	cy.Attempts++
	cy.LastAttemptAt = now.Unix()
	// 只补 pending 的；新模型初始化为 pending。
	for _, m := range models {
		key := normalizeModelName(m)
		if _, exists := cy.Models[key]; !exists {
			cy.Models[key] = &warmupModelState{Status: "pending"}
		}
	}
	pendingModels := make([]string, 0, len(cy.Models))
	for k, v := range cy.Models {
		if v.Status == "pending" {
			pendingModels = append(pendingModels, k)
		}
	}
	sort.Strings(pendingModels)
	// 先落盘一次，保证「已开始」可观测（进程被杀也能看出做过什么）。
	saveWarmupStateLocked(st)
	warmupStateMu.Unlock()

	if len(pendingModels) == 0 {
		// 冷却恢复触发但无待触发模型（例如当天已全部成功）：仍发通知避免静默，
		// 但计数必须用当天周期真实结果 —— 绝不硬编码 0/0/0，否则主人分不清
		// 「本轮没触发任何模型」还是「当天真的全部为零」；正文同时写明恢复项。
		if rt.Notify && reason == "cooldown" {
			cok, cfail, cskip, _, _ := countCycle(cy)
			sendWarmupNotify(rt, cok, cfail, cskip, reason, recoveredKeys)
		}
		return countCycle(cy)
	}
	log.Printf("[Warmup] 开始第 %d 轮触发：待触发=%d，总计=%d，计划时刻=%s，上限=%s",
		cy.Attempts, len(pendingModels), len(cy.Models), formatDisplayTime(time.Unix(cy.ScheduledAt, 0)), formatQuota(rt.MaxPrice))

	for _, key := range pendingModels {
		// 用原始大小写（模型名对上游大小写敏感）：从目录中取回展示名。
		model := modelDisplayName(key)
		warmupStateMu.Lock()
		cy := loadWarmupState().Current
		ms := cy.Models[key]
		warmupStateMu.Unlock()
		if ms == nil {
			continue
		}
		warmupTriggerModel(model, rt, now, ms)
		warmupStateMu.Lock()
		st := loadWarmupState()
		if st.Current != nil && st.Current.Date == date {
			st.Current.Models[key] = ms
			saveWarmupStateLocked(st)
		}
		warmupStateMu.Unlock()
	}

	warmupStateMu.Lock()
	st = loadWarmupState()
	if st.Current != nil && st.Current.Date == date {
		ok, failed, skipped, pending, _ = countCycleLocked(st.Current)
		if pending == 0 {
			st.Current.CompletedAt = time.Now().Unix()
		}
		saveWarmupStateLocked(st)
	}
	warmupStateMu.Unlock()

	log.Printf("[Warmup] 第 %d 轮结束：成功=%d，失败=%d，跳过=%d，待补触发=%d",
		cy.Attempts, ok, failed, skipped, pending)
	if rt.Notify && pending == 0 {
		sendWarmupNotify(rt, ok, failed, skipped, reason, recoveredKeys)
	}
	return ok, failed, skipped, pending, catchUp
}

func countCycle(cy *warmupCycle) (ok, failed, skipped, pending int, catchUp bool) {
	return countCycleLocked(cy)
}

func countCycleLocked(cy *warmupCycle) (ok, failed, skipped, pending int, catchUp bool) {
	for _, v := range cy.Models {
		switch v.Status {
		case "ok":
			ok++
		case "failed":
			failed++
		case "skipped":
			skipped++
		default:
			pending++
		}
	}
	catchUp = cy.StartedAt > cy.ScheduledAt
	return
}

// modelDisplayName 从模型目录取回原始大小写的模型名；取不到就用归一化的名字。
func modelDisplayName(key string) string {
	ids, _ := mergedModelIDs()
	for _, id := range ids {
		if normalizeModelName(id) == key {
			return id
		}
	}
	return key
}

// sendWarmupNotify 一轮结束后推送结果（复用 notify 通道）。
// reason 区分触发来源（scheduled 定时 / cooldown 冷却恢复 / manual 手动），
// 三者的标题与去重键必须不同，否则主人分不清是哪一路触发的。
func sendWarmupNotify(rt warmupRuntime, ok, failed, skipped int, reason string, recovered []string) {
	now := time.Now()
	var title, keyPrefix string
	switch reason {
	case "cooldown":
		title = "🔄 workbuddy 冷却恢复窗口触发"
		if failed > 0 {
			title = "⚠️ workbuddy 冷却恢复窗口触发（部分失败）"
		}
		keyPrefix = "warmup-cooldown|"
	default: // scheduled 及其他
		title = "✅ workbuddy 5 小时窗口已触发"
		if failed > 0 {
			title = "⚠️ workbuddy 5 小时窗口触发（部分失败）"
		}
		keyPrefix = "warmup|"
	}
	body := fmt.Sprintf("触发时刻: %s\n价格上限: %s\n成功: %d\n失败: %d\n跳过: %d",
		formatDisplayTime(now), formatQuota(rt.MaxPrice), ok, failed, skipped)
	html := tgTitle(title) + tgKV("触发时刻", formatDisplayTime(now)) + tgKV("价格上限", formatQuota(rt.MaxPrice))
	if len(recovered) > 0 {
		// 写明本次触发由哪些「账号|模型」的冷却恢复引起，便于对账。
		names := strings.Join(recovered, ", ")
		body += "\n恢复项: " + names
		html += tgKV("恢复项", names)
	}
	html += tgKV("成功", fmt.Sprint(ok)) + tgKV("失败", fmt.Sprint(failed)) + tgKV("跳过", fmt.Sprint(skipped))
	sendNotify(notifyEvent{
		Kind: notifyEventWarmup,
		// 定时与冷却恢复分开去重：冷却恢复一天可能多次，不能共用同一把锁。
		Key:   keyPrefix + cycleDateOf(now),
		Level: "info",
		Title: title,
		Body:  body,
		HTML:  html,
	})
}

// -----------------------------------------------------------------------------
// 模型探测每日汇总通知
// -----------------------------------------------------------------------------

// probeSummaryDelay 当日首次探测后多久汇总推送。
// 探测收敛前是 2 分钟一轮，等一会儿再汇总能覆盖更多模型，避免只报头几个。
const probeSummaryDelay = 30 * time.Minute

// probeDailyFile 每日探测汇总计数落盘文件。
//
// 背景（2026-10-10 事故）：计数原本仅存内存，服务重启即清零 ——
// 当天已探到的结论丢失，且「当日首次探测时刻」被重置为重启后首个探测，
// 导致「每日探测汇总」通知的数字明显偏低（当日 01:13 已探到 1 个 free，
// 02:11 重启后 03:11 的通知只报了重启后探到的 1 个）。落盘后按自然日恢复。
const probeDailyFile = "wb-probe-daily.json"

// probeVerdictTotals 统计目录内已确认的价格结论总数（free / paid）。
//
// 用途：「每日探测汇总」只报当日新探到的结论，若不给「当前已知」对照，
// 读者容易把「免费 N」误读成全平台免费模型总数（2026-10-10 主人即如此理解）。
func probeVerdictTotals() (free, paid int) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	for _, p := range modelProbes {
		switch p.Verdict {
		case "free":
			free++
		case "paid":
			paid++
		}
	}
	return free, paid
}

// probeDailyPersist 是落盘结构：跨重启恢复当日汇总计数。
type probeDailyPersist struct {
	Date   string   `json:"date"` // 计划日期（配置时区自然日），不匹配则忽略
	Free   int      `json:"free"`
	Paid   int      `json:"paid"`
	Other  int      `json:"other"`
	Models []string `json:"models,omitempty"`
	Sent   bool     `json:"sent"`
	First  int64    `json:"first,omitempty"` // 当日首次探测 Unix 秒
}

// loadProbeDaily 启动时恢复当日汇总计数（仅当落盘日期与当前计划日期一致）。
func loadProbeDaily() {
	data, err := os.ReadFile(probeDailyFile)
	if err != nil {
		return
	}
	var p probeDailyPersist
	if err := json.Unmarshal(data, &p); err != nil {
		log.Printf("[ModelPrice] 每日汇总计数文件无效，忽略: %v", err)
		return
	}
	if p.Date != cycleDateOf(time.Now()) {
		return // 跨天：不恢复，等首个探测自然重置
	}
	probeDailyMu.Lock()
	probeDailyDate = p.Date
	probeDailyFree, probeDailyPaid, probeDailyOther = p.Free, p.Paid, p.Other
	probeDailyModels = p.Models
	probeDailySent = p.Sent
	if p.First > 0 {
		probeDailyFirst = time.Unix(p.First, 0)
	}
	probeDailyMu.Unlock()
	log.Printf("[ModelPrice] 已恢复当日探测汇总计数：日期=%s 免费=%d 收费=%d 未判定=%d 已发送=%v",
		p.Date, p.Free, p.Paid, p.Other, p.Sent)
}

// persistProbeDailyLocked 落盘当日汇总计数。调用方必须已持有 probeDailyMu。
func persistProbeDailyLocked() {
	var first int64
	if !probeDailyFirst.IsZero() {
		first = probeDailyFirst.Unix()
	}
	data, err := json.MarshalIndent(probeDailyPersist{
		Date: probeDailyDate, Free: probeDailyFree, Paid: probeDailyPaid,
		Other: probeDailyOther, Models: probeDailyModels, Sent: probeDailySent,
		First: first,
	}, "", "  ")
	if err != nil {
		return
	}
	tmp := probeDailyFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("[ModelPrice] 每日汇总计数写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, probeDailyFile); err != nil {
		log.Printf("[ModelPrice] 每日汇总计数写入失败: %v", err)
	}
}

var (
	probeDailyMu     sync.Mutex
	probeDailyDate   string
	probeDailyFree   int
	probeDailyPaid   int
	probeDailyOther  int
	probeDailyModels []string
	probeDailySent   bool
	probeDailyFirst  time.Time
)

// recordProbeDailyResult 累计当日探测结论，供每日汇总通知使用。
func recordProbeDailyResult(site, model, verdict string, now time.Time) {
	probeDailyMu.Lock()
	defer probeDailyMu.Unlock()
	date := cycleDateOf(now)
	if probeDailyDate != date {
		// 跨天重置：只统计当天结论，且每天允许再发一条。
		probeDailyDate = date
		probeDailyFree, probeDailyPaid, probeDailyOther = 0, 0, 0
		probeDailyModels = nil
		probeDailySent = false
		probeDailyFirst = now
	}
	label := site + "/" + model
	switch verdict {
	case "free":
		probeDailyFree++
		label += " 免费"
	case "paid":
		probeDailyPaid++
		label += " 收费"
	default:
		probeDailyOther++
		label += " 未判定"
	}
	if len(probeDailyModels) < 20 {
		probeDailyModels = append(probeDailyModels, label)
	}
	// 计数变化即落盘，保证重启后不丢当日已累计的结论。
	persistProbeDailyLocked()
}

// maybeSendProbeDailyNotify 当日探测累计够久后推送一条汇总（每天最多一条）。
// 未开开关、当天已发过、或距首次探测不足 probeSummaryDelay 时直接返回。
func maybeSendProbeDailyNotify(now time.Time) {
	pr := probeScheduleSnapshot()
	if !pr.Enabled || !pr.Notify {
		return
	}
	probeDailyMu.Lock()
	if probeDailySent || probeDailyFirst.IsZero() || probeDailyDate != cycleDateOf(now) {
		probeDailyMu.Unlock()
		return
	}
	if now.Sub(probeDailyFirst) < probeSummaryDelay {
		probeDailyMu.Unlock()
		return
	}
	free, paid, other := probeDailyFree, probeDailyPaid, probeDailyOther
	models := append([]string(nil), probeDailyModels...)
	probeDailySent = true
	probeDailyMu.Unlock()

	// 文案口径（2026-10-10）：只给「免费 N」会被读作全平台免费模型总数。
	// 标题与取值行都显式标注「当日新增」，并附「当前已知」累计对照。
	title := "🔍 workbuddy 模型探测结果（当日新增）"
	date := cycleDateOf(now)
	totFree, totPaid := probeVerdictTotals()
	newLine := fmt.Sprintf("免费 %d / 收费 %d / 未判定 %d", free, paid, other)
	knownLine := fmt.Sprintf("免费 %d / 收费 %d", totFree, totPaid)

	body := fmt.Sprintf("探测日期: %s\n当日新增: %s\n当前已知(累计): %s", date, newLine, knownLine)
	if len(models) > 0 {
		body += "\n明细:\n  " + strings.Join(models, "\n  ")
	}
	body += "\n口径: 「当日新增」为今天新探测出的价格结论；" +
		"「当前已知」为目录内已确认的判断总数，两者不是同一个数"

	var b strings.Builder
	b.WriteString(tgTitle(title))
	b.WriteString(tgKV("探测日期", date))
	b.WriteString(tgKV("当日新增", newLine))
	b.WriteString(tgKV("当前已知", knownLine))
	tgSection(&b, "明细")
	if len(models) == 0 {
		b.WriteString("（无）\n")
	} else {
		for _, m := range models {
			b.WriteString(tgEntry(m) + "\n")
		}
	}
	b.WriteString(tgKV("口径", "当日新增=今天新探出的结论；当前已知=目录内已确认总数"))

	sendNotify(notifyEvent{
		Kind:  notifyEventProbe,
		Key:   "probe|" + date,
		Level: "info",
		Title: title,
		Body:  body,
		HTML:  b.String(),
	})
}

// -----------------------------------------------------------------------------
// 调度循环
// -----------------------------------------------------------------------------

// warmupLoop 每日定时触发 + 补触发。
//
// 补触发覆盖三类「到点没成功」：
//  1. 计划时刻服务没运行 —— 进程启动后发现当天周期未完成，立即补一轮；
//  2. 触发时模型正在冷却（6004）—— 保留 pending，按重试间隔再来；
//  3. 网络不畅 / 上游错误 —— 同上。
//
// 超过补触发窗口（ScheduledAt + CatchUpHours）仍未成功的，标记为失败并归档。
// warmupLoopAction 描述 warmupLoop 一次迭代要做的事。
// 抽成值而非内联分支，是为了让「门闸是否吞掉冷却恢复信号」可被单测覆盖：
// v1.27.1 及以前门闸直接 continue，导致该路通知静默（2026-10-09 六次全无通知）。
type warmupLoopAction struct {
	run       bool          // 立即执行一轮
	archive   bool          // 归档当前周期（超出补触发窗口且未完成）
	wait      time.Duration // 本轮不执行时的等待时长
	skipProbe bool          // 执行一轮后是否跳过主动探测（冷却恢复补触发不跟随探测）
}

// decideWarmupLoop 根据唤醒来源与周期状态决定本轮动作。
// 入参显式、不直接读周期状态，便于回归测试精确构造各道门闸场景。
//
// 关键约束：reason=="cooldown" 时必须放行执行，绝不能落到「当天已完成」
// 或「超出补触发窗口」的等待分支 —— 一旦等待，reason 就没机会被
// runWarmupOnce 取走，冷却恢复通知将彻底静默。
func decideWarmupLoop(now time.Time, rt warmupRuntime, cy *warmupCycle, reason string) warmupLoopAction {
	// 冷却恢复唤醒：穿透所有定时语义门闸，立即执行一轮。
	if reason == "cooldown" {
		return warmupLoopAction{run: true, skipProbe: true}
	}

	date := cycleDateOf(now)
	scheduled := scheduledTimeOn(now, rt.Time, 4, 0)
	sameDay := cy != nil && cy.Date == date

	// 当天周期还没建立，且还没到点：等到点。
	if !sameDay && now.Before(scheduled) {
		return warmupLoopAction{wait: scheduled.Sub(now) + time.Second}
	}
	// 当天已完成：等明天。
	if sameDay && cy.CompletedAt > 0 {
		return warmupLoopAction{wait: nextScheduledAfter(now, rt.Time, 4, 0).Sub(now) + time.Second}
	}
	deadline := scheduled.Add(time.Duration(rt.CatchUpHours) * time.Hour)
	if now.After(deadline) {
		// 超出补触发窗口：归档为未完成（窗口已过，再触发没有意义）。
		return warmupLoopAction{
			archive: sameDay && cy.CompletedAt == 0,
			wait:    nextScheduledAfter(now, rt.Time, 4, 0).Sub(now) + time.Second,
		}
	}
	// 需要执行（到点 / 补触发）。pending 项按重试间隔节流。
	if sameDay && cy.LastAttemptAt > 0 {
		if wait := time.Duration(rt.RetryMinutes)*time.Minute - time.Since(time.Unix(cy.LastAttemptAt, 0)); wait > 0 {
			return warmupLoopAction{wait: wait}
		}
	}
	return warmupLoopAction{run: true}
}

func warmupLoop() {
	for {
		rt := warmupSnapshot()
		if !rt.Enabled {
			waitWarmupSignal(time.Hour)
			continue
		}
		now := time.Now()
		warmupStateMu.Lock()
		st := loadWarmupState()
		cy := st.Current
		warmupStateMu.Unlock()

		// 唤醒来源只 peek 不消费：真正的清除由 runWarmupOnce 完成，
		// 这里消费掉会让通知标题退化成 scheduled。
		act := decideWarmupLoop(now, rt, cy, peekWarmupWakeReason())
		if act.archive && cy != nil {
			archiveCycle(rt, cy, now)
		}
		if !act.run {
			waitWarmupSignal(act.wait)
			continue
		}

		_, _, _, pending, _ := runWarmupOnce(rt, now)
		if !act.skipProbe {
			// 主动探测与本功能同刻执行（主人要求：开启 warmup 时探测跟随同一时刻）。
			requestModelsProbe()
		}
		if pending > 0 {
			waitWarmupSignal(time.Duration(rt.RetryMinutes) * time.Minute)
			continue
		}
		waitWarmupSignal(time.Until(nextScheduledAfter(time.Now(), rt.Time, 4, 0)) + time.Second)
	}
}

// activeCooldownKeys 返回当前仍处于冷却中的「账号|模型」及其截止时间。
// 账号级冷却的 model 部分为空。
func activeCooldownKeys(now time.Time) map[string]time.Time {
	out := make(map[string]time.Time)
	accountMu.Lock()
	defer accountMu.Unlock()
	for _, acc := range accounts {
		if acc.Disabled {
			continue
		}
		name := filepath.Base(acc.Path)
		if acc.CooldownUntil.After(now) {
			out[name+"|"] = acc.CooldownUntil
		}
		for model, st := range acc.ModelStates {
			if st == nil || !st.CooldownUntil.After(now) {
				continue
			}
			out[name+"|"+model] = st.CooldownUntil
		}
	}
	return out
}

// warmupRecoveredInScope 从恢复项中筛出「在 warmup 触发名单内」的模型项。
// key 形如 "账号|模型"；账号级冷却（无模型部分）不属于特定模型，一律视为名单外。
// allowed 为 normalize 后的模型名集合（warmupSelectModels 输出归一化）。
//
// 为什么必须过滤：冷却监听覆盖「所有账号 × 所有模型」，而 warmup 只触发名单内
// （价格上限/显式指定）的模型 —— 名单外模型的冷却恢复与本功能无关，唤醒只会
// 空跑一轮并发一条无意义通知（实测 v1.27.3 本机 7/7 次恢复全属此类）。
func warmupRecoveredInScope(recovered []string, allowed map[string]bool) []string {
	out := make([]string, 0, len(recovered))
	for _, key := range recovered {
		idx := strings.Index(key, "|")
		if idx < 0 || idx+1 >= len(key) {
			// 账号级冷却恢复：无模型部分，不作为唤醒依据。
			continue
		}
		if allowed[normalizeModelName(key[idx+1:])] {
			out = append(out, key)
		}
	}
	return out
}

// warmupCooldownWatchLoop 监听模型冷却：一旦发现某个「账号|模型」从冷却中恢复，
// 且当前处于允许触发的时段内，立即补触发一轮（按最小间隔节流，防抖动刷屏）。
//
// 为什么需要它：warmup 只按固定时刻触发，模型被 6004 屏蔽后要等到下个周期才重试；
// 冷却往往几小时内就恢复，等到次日白白浪费窗口。
func warmupCooldownWatchLoop() {
	var prev map[string]time.Time
	var lastTrigger time.Time
	for {
		time.Sleep(warmupCooldownWatchTick)
		rt := warmupSnapshot()
		if !rt.Enabled || !rt.CooldownTrigger {
			prev = nil
			continue
		}
		now := time.Now()
		if len(rt.Windows) > 0 && currentWindowIndex(now, rt.Windows) < 0 {
			// 时段外不跟踪：否则跨时段会被误判成「冷却恢复」。
			prev = nil
			continue
		}
		cur := activeCooldownKeys(now)
		if prev == nil {
			prev = cur
			continue
		}
		recovered := make([]string, 0, len(prev))
		for k := range prev {
			if _, still := cur[k]; !still {
				recovered = append(recovered, k)
			}
		}
		prev = cur
		if len(recovered) == 0 {
			continue
		}
		// 只认 warmup 触发名单内模型的恢复：名单外模型（如日常业务高频使用、
		// 但不在价格上限内的模型）的冷却恢复，唤醒后必然没有待触发项，只会
		// 空跑一轮并发无意义通知 —— 此处只记日志，不唤醒。
		allowed := map[string]bool{}
		for _, m := range warmupSelectModels(rt, now) {
			allowed[normalizeModelName(m)] = true
		}
		inScope := warmupRecoveredInScope(recovered, allowed)
		if len(inScope) == 0 {
			sort.Strings(recovered)
			shown := recovered
			if len(shown) > 8 {
				shown = shown[:8]
			}
			log.Printf("[Warmup] 冷却恢复 %d 项均不在触发名单内，忽略（%s）",
				len(recovered), strings.Join(shown, ", "))
			continue
		}
		minGap := time.Duration(rt.CooldownTriggerMinMinutes) * time.Minute
		if minGap > 0 && now.Sub(lastTrigger) < minGap {
			continue
		}
		sort.Strings(inScope)
		shown := inScope
		if len(shown) > 8 {
			shown = shown[:8]
		}
		log.Printf("[Warmup] 检测到冷却恢复（%d 项：%s），立即触发一轮（最小间隔 %v）",
			len(inScope), strings.Join(shown, ", "), minGap)
		lastTrigger = now
		setWarmupWake("cooldown", inScope)
		requestWarmupRun()
	}
}

// probeScheduleLoop 每日定时主动探测：warmup 开启时跟随 warmup 时刻（由 warmupLoop
// 顺带触发，这里只在 warmup 关闭/未跟随的兜底场景独立触发）。
func probeScheduleLoop() {
	for {
		pr := probeScheduleSnapshot()
		warm := warmupSnapshot()
		if !pr.Enabled {
			waitProbeSignal(time.Hour)
			continue
		}
		follow := pr.FollowWarmup && warm.Enabled
		if follow {
			// 与 warmup 同刻：交给 warmupLoop 触发，这里只做「warmup 关闭中途开启」
			// 的兜底——若 warmup 当前周期的探测尚未触发且已到点，补一次。
			waitProbeSignal(5 * time.Minute)
			continue
		}
		now := time.Now()
		target := scheduledTimeOn(now, pr.Time, 6, 0)
		if !target.After(now) {
			// 已过今日探测点：若今天还没触发过，立即补一次（服务起晚了）。
			warmupMu.Lock()
			fired := probeScheduleDate == cycleDateOf(now)
			warmupMu.Unlock()
			if !fired {
				markProbeFired(now)
				log.Printf("[Probe] 已过今日定时探测点 %s，服务在此期间未运行，立即补一次探测", pr.Time)
				requestModelsProbe()
			}
			waitProbeSignal(time.Until(nextScheduledAfter(now, pr.Time, 6, 0)) + time.Second)
			continue
		}
		waitProbeSignal(time.Until(target) + time.Second)
		warmupMu.Lock()
		fired := probeScheduleDate == cycleDateOf(time.Now())
		warmupMu.Unlock()
		if !fired {
			markProbeFired(time.Now())
			requestModelsProbe()
		}
	}
}

func markProbeFired(now time.Time) {
	warmupMu.Lock()
	probeScheduleDate = cycleDateOf(now)
	warmupMu.Unlock()
}

// waitWarmupSignal 等待指定时长，期间可被立即执行信号打断。
func waitWarmupSignal(d time.Duration) {
	if d <= 0 {
		d = time.Second
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-warmupTrigger:
	}
}

func waitProbeSignal(d time.Duration) {
	if d <= 0 {
		d = time.Second
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-warmupWake:
	}
}

func requestWarmupRun() {
	select {
	case warmupTrigger <- struct{}{}:
	default:
	}
}

// archiveCycle 补触发窗口结束仍未完成：归档并标记未完成项。
func archiveCycle(rt warmupRuntime, cy *warmupCycle, now time.Time) {
	warmupStateMu.Lock()
	defer warmupStateMu.Unlock()
	st := loadWarmupState()
	if st.Current == nil || st.Current.Date != cy.Date {
		return
	}
	for _, v := range st.Current.Models {
		if v.Status == "pending" {
			v.Status = "failed"
			if v.Detail == "" {
				v.Detail = "补触发窗口内未成功"
			} else {
				v.Detail += "（补触发窗口内未成功）"
			}
		}
	}
	st.Current.CompletedAt = now.Unix()
	ok, failed, skipped, pending, catchUp := countCycleLocked(st.Current)
	st.History = append([]warmupHistoryEntry{{
		Date:        st.Current.Date,
		ScheduledAt: st.Current.ScheduledAt,
		OK:          ok, Failed: failed, Skipped: skipped + pending,
		CompletedAt: now.Unix(), CatchUp: catchUp,
	}}, st.History...)
	if len(st.History) > warmupHistoryLimit {
		st.History = st.History[:warmupHistoryLimit]
	}
	st.Current = nil
	saveWarmupStateLocked(st)
	log.Printf("[Warmup] %s 周期补触发窗口结束：成功=%d，失败=%d，跳过=%d，已归档",
		cy.Date, ok, failed, skipped+pending)
	if rt.Notify {
		sendWarmupNotify(rt, ok, failed+pending, skipped, "scheduled", nil)
	}
}

// -----------------------------------------------------------------------------
// HTTP 管理接口
// -----------------------------------------------------------------------------

type warmupStatusResponse struct {
	Enabled      bool     `json:"enabled"`
	Time         string   `json:"time"`
	MaxPrice     float64  `json:"maxPrice"`
	Models       []string `json:"models,omitempty"`
	CatchUpHours int      `json:"catchUpHours"`
	RetryMinutes int      `json:"retryMinutes"`
	Notify       bool     `json:"notify"`
	TZ           string   `json:"tz"`

	ProbeEnabled      bool   `json:"probeEnabled"`
	ProbeFollowWarmup bool   `json:"probeFollowWarmup"`
	ProbeTime         string `json:"probeTime"`
	ProbeEffectiveAt  string `json:"probeEffectiveAt"`
	ProbeNotify       bool   `json:"probeNotify"`

	// Windows 允许触发的时段（HH:MM-HH:MM）；非空时优先于 Time。
	Windows []string `json:"windows,omitempty"`
	// CooldownTrigger 模型冷却恢复后立即触发一轮。
	CooldownTrigger bool `json:"cooldownTrigger"`
	// CooldownTriggerMinMinutes 冷却恢复触发的最小间隔（分钟）。
	CooldownTriggerMinMinutes int `json:"cooldownTriggerMinMinutes"`

	Cycle    *warmupCycle         `json:"cycle,omitempty"`
	History  []warmupHistoryEntry `json:"history,omitempty"`
	Selected []string             `json:"selected,omitempty"`
}

// windowsToStrings 把时段结构转回配置里的字符串形式，供状态展示。
func windowsToStrings(ws []timeWindow) []string {
	if len(ws) == 0 {
		return nil
	}
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.String())
	}
	return out
}

func warmupStatusNow() warmupStatusResponse {
	rt := warmupSnapshot()
	pr := probeScheduleSnapshot()
	now := time.Now()
	resp := warmupStatusResponse{
		Enabled:                   rt.Enabled,
		Time:                      rt.Time,
		MaxPrice:                  rt.MaxPrice,
		Models:                    rt.Models,
		CatchUpHours:              rt.CatchUpHours,
		RetryMinutes:              rt.RetryMinutes,
		Notify:                    rt.Notify,
		TZ:                        warmupLocation().String(),
		ProbeEnabled:              pr.Enabled,
		ProbeFollowWarmup:         pr.FollowWarmup,
		ProbeTime:                 pr.Time,
		ProbeNotify:               pr.Notify,
		Windows:                   windowsToStrings(rt.Windows),
		CooldownTrigger:           rt.CooldownTrigger,
		CooldownTriggerMinMinutes: rt.CooldownTriggerMinMinutes,
	}
	// 探测时刻跟随 warmup 的每日定时触发（time），与冷却恢复时段 windows 无关。
	if rt.Enabled {
		resp.ProbeEffectiveAt = scheduledTimeOn(now, rt.Time, 4, 0).Format("2006-01-02 15:04:05 MST")
		resp.Selected = warmupSelectModels(rt, now)
	} else {
		resp.ProbeEffectiveAt = scheduledTimeOn(now, pr.Time, 6, 0).Format("2006-01-02 15:04:05 MST")
	}
	warmupStateMu.Lock()
	st := loadWarmupState()
	warmupStateMu.Unlock()
	resp.Cycle = st.Current
	resp.History = st.History
	return resp
}

// handleAdminWarmup 提供 warmup 状态查询与立即执行（仅回环）。
func handleAdminWarmup(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeOpenAIError(w, http.StatusForbidden, "warmup_local_only", "warmup 管理接口仅允许本机回环地址调用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(warmupStatusNow())
	case http.MethodPost:
		rt := warmupSnapshot()
		if !rt.Enabled {
			writeOpenAIError(w, http.StatusConflict, "warmup_disabled", "5 小时窗口主动触发当前为关闭状态（warmup on 开启后再试）")
			return
		}
		go func() {
			runWarmupOnce(rt, time.Now())
			requestModelsProbe()
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "已触发一轮（后台执行）"})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET / POST 请求")
	}
}

// handleAdminReload 让运行中的进程重新读取 config.json（仅回环）。
// 供 warmup 子命令改完配置后立即生效，免重启。
func handleAdminReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}
	if !isLoopbackRequest(r) {
		writeOpenAIError(w, http.StatusForbidden, "reload_local_only", "reload 接口仅允许本机回环地址调用")
		return
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		log.Printf("[Config] 热加载失败: %v", err)
		writeOpenAIError(w, http.StatusInternalServerError, "reload_failed", "重新加载配置失败: "+err.Error())
		return
	}
	log.Printf("[Config] 已按 /admin/reload 重新加载 %s", runtimeConfigFile)
	// 配置变更可能改变触发时刻或筛选条件，唤醒调度循环重新计算。
	select {
	case warmupWake <- struct{}{}:
	default:
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(warmupStatusNow())
}

func printWarmupHelp() {
	fmt.Println(`用法:
  workbuddy-gateway warmup <子命令> [参数]

子命令:
  status              查看当前配置与最近触发结果（默认）
  on                  开启 5 小时窗口主动触发
  off                 关闭 5 小时窗口主动触发
  time <HH:MM>        设置每日触发时刻（配置时区，默认 04:00）
  price <n>           设置价格上限（默认 0.06；0 = 只触发已确认免费的模型）
  models <a,b>        指定模型列表（逗号分隔），优先级高于价格筛选；clear 清除
  catch-up <hours>    设置补触发窗口小时数（默认 4）
  retry <minutes>     设置补触发重试间隔分钟数（默认 10）
  notify on|off       每轮结束后是否推送结果（默认 off）
  probe-time <HH:MM>  设置 warmup 关闭时的每日主动探测时刻（默认 06:00）
  probe-follow on|off 主动探测是否跟随 warmup 时刻（默认 on）
  probe-notify on|off 每日探测结论汇总是否推送（默认 off，每天最多一条）
  windows <a-b,c-d>   冷却恢复补触发的允许时段（如 06:00-21:00），多段逗号分隔；
                      仅约束「冷却恢复触发」，不影响 time 的每日定时触发；clear 清除
  cooldown-trigger on|off 模型冷却恢复后立即触发一轮（默认 on）
  cooldown-min <min>  冷却恢复触发的最小间隔分钟数（默认 10，防抖动）
  run                 立即执行一轮（需 serve 正在运行）

示例:
  workbuddy-gateway warmup status
  workbuddy-gateway warmup time 04:30
  workbuddy-gateway warmup price 0
  workbuddy-gateway warmup models hy4-preview-f,glm-5.3-flash
  workbuddy-gateway warmup models clear
  workbuddy-gateway warmup run`)
}

// runWarmup 是 warmup 子命令：读写工作目录 config.json 并通知运行中的 serve。
func runWarmup() {
	args := os.Args[2:]
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "status", "":
		printWarmupStatus()
		return
	case "help", "-h", "--help":
		printWarmupHelp()
		return
	case "run":
		runWarmupImmediate()
		return
	}

	var mutate func(map[string]any) error
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "on", "off":
		val := strings.ToLower(args[0]) == "on"
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["enabled"] = val
			m["warmup"] = w
			return nil
		}
	case "time":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup time <HH:MM>")
			os.Exit(1)
		}
		v := strings.TrimSpace(args[1])
		if _, ok := parseClock(v); !ok {
			fmt.Printf("时刻格式无效: %s（应为 HH:MM，例如 04:30）\n", v)
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["time"] = v
			m["warmup"] = w
			return nil
		}
	case "price":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup price <n>（0 = 只触发免费模型）")
			os.Exit(1)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(args[1]), 64)
		if err != nil || v < 0 {
			fmt.Printf("价格无效: %s\n", args[1])
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["maxPrice"] = v
			m["warmup"] = w
			return nil
		}
	case "models":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup models <a,b>（clear 清除）")
			os.Exit(1)
		}
		raw := strings.TrimSpace(args[1])
		var list []string
		if !strings.EqualFold(raw, "clear") {
			for _, m := range strings.Split(raw, ",") {
				if m = strings.TrimSpace(m); m != "" {
					list = append(list, m)
				}
			}
			if len(list) == 0 {
				fmt.Println("模型列表为空；如需恢复价格筛选请用: warmup models clear")
				os.Exit(1)
			}
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			if list == nil {
				delete(w, "models")
			} else {
				w["models"] = list
			}
			m["warmup"] = w
			return nil
		}
	case "catch-up", "catchup":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup catch-up <hours>")
			os.Exit(1)
		}
		v, err := strconv.ParseInt(strings.TrimSpace(args[1]), 10, 64)
		if err != nil || v <= 0 || v > 23 {
			fmt.Printf("小时数无效: %s（应为 1-23）\n", args[1])
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["catchUpHours"] = v
			m["warmup"] = w
			return nil
		}
	case "retry":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup retry <minutes>")
			os.Exit(1)
		}
		v, err := strconv.ParseInt(strings.TrimSpace(args[1]), 10, 64)
		if err != nil || v <= 0 || v > 720 {
			fmt.Printf("分钟数无效: %s（应为 1-720）\n", args[1])
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["retryMinutes"] = v
			m["warmup"] = w
			return nil
		}
	case "notify":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup notify on|off")
			os.Exit(1)
		}
		val, ok := parseOnOff(args[1])
		if !ok {
			fmt.Printf("取值无效: %s（应为 on 或 off）\n", args[1])
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["notify"] = val
			m["warmup"] = w
			return nil
		}
	case "probe-time":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup probe-time <HH:MM>")
			os.Exit(1)
		}
		v := strings.TrimSpace(args[1])
		if _, ok := parseClock(v); !ok {
			fmt.Printf("时刻格式无效: %s（应为 HH:MM，例如 06:00）\n", v)
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			p := probeSection(m)
			p["time"] = v
			m["probe"] = map[string]any{"schedule": p}
			return nil
		}
	case "probe-follow":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup probe-follow on|off")
			os.Exit(1)
		}
		val, ok := parseOnOff(args[1])
		if !ok {
			fmt.Printf("取值无效: %s（应为 on 或 off）\n", args[1])
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			p := probeSection(m)
			p["followWarmup"] = val
			m["probe"] = map[string]any{"schedule": p}
			return nil
		}
	case "probe-notify":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup probe-notify on|off")
			os.Exit(1)
		}
		val, ok := parseOnOff(args[1])
		if !ok {
			fmt.Printf("取值无效: %s（应为 on 或 off）\n", args[1])
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			p := probeSection(m)
			p["notify"] = val
			m["probe"] = map[string]any{"schedule": p}
			return nil
		}
	case "windows":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup windows <HH:MM-HH:MM[,HH:MM-HH:MM]> | clear")
			os.Exit(1)
		}
		raw := strings.TrimSpace(args[1])
		if strings.EqualFold(raw, "clear") {
			mutate = func(m map[string]any) error {
				w := warmupSection(m)
				delete(w, "windows")
				m["warmup"] = w
				return nil
			}
			break
		}
		parts := strings.Split(raw, ",")
		list := make([]string, 0, len(parts))
		for _, p := range parts {
			if _, ok := parseTimeWindow(p); !ok {
				fmt.Printf("时段格式无效: %s（应为 HH:MM-HH:MM，例如 06:00-21:00）\n", strings.TrimSpace(p))
				os.Exit(1)
			}
			list = append(list, strings.TrimSpace(p))
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["windows"] = list
			m["warmup"] = w
			return nil
		}
	case "cooldown-trigger":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup cooldown-trigger on|off")
			os.Exit(1)
		}
		val, ok := parseOnOff(args[1])
		if !ok {
			fmt.Printf("取值无效: %s（应为 on 或 off）\n", args[1])
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["cooldownTrigger"] = val
			m["warmup"] = w
			return nil
		}
	case "cooldown-min":
		if len(args) < 2 {
			fmt.Println("用法: workbuddy-gateway warmup cooldown-min <minutes>")
			os.Exit(1)
		}
		v, err := strconv.Atoi(strings.TrimSpace(args[1]))
		if err != nil || v < 0 {
			fmt.Printf("取值无效: %s（应为非负整数分钟数）\n", args[1])
			os.Exit(1)
		}
		mutate = func(m map[string]any) error {
			w := warmupSection(m)
			w["cooldownTriggerMinMinutes"] = v
			m["warmup"] = w
			return nil
		}
	default:
		fmt.Printf("未知子命令: %s\n\n", args[0])
		printWarmupHelp()
		os.Exit(1)
	}

	if err := updateRuntimeConfigFile(mutate); err != nil {
		fmt.Printf("写入 config.json 失败: %v\n", err)
		os.Exit(1)
	}
	// 让运行中的 serve 立即生效；没运行就等下次启动/热加载。
	if err := reloadRemoteConfig(); err != nil {
		fmt.Printf("已写入 config.json（serve 未在监听，将在其启动或下一次热加载时生效: %v）\n", err)
	} else {
		fmt.Println("已写入 config.json 并通知运行中的 serve 生效")
	}
	printWarmupStatus()
}

func parseOnOff(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "1", "yes":
		return true, true
	case "off", "false", "0", "no":
		return false, true
	}
	return false, false
}

// warmupSection 取出 config.json 的 warmup 段（不存在则新建）。
func warmupSection(root map[string]any) map[string]any {
	if v, ok := root["warmup"].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// probeSection 取出 config.json 的 probe.schedule 段（不存在则新建）。
func probeSection(root map[string]any) map[string]any {
	if v, ok := root["probe"].(map[string]any); ok {
		if s, ok := v["schedule"].(map[string]any); ok {
			return s
		}
	}
	return map[string]any{}
}

// updateRuntimeConfigFile 原子改写工作目录 config.json，只变更传入的段。
func updateRuntimeConfigFile(mutate func(map[string]any) error) error {
	root := map[string]any{}
	if data, err := os.ReadFile(runtimeConfigFile); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("解析 %s: %w", runtimeConfigFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取 %s: %w", runtimeConfigFile, err)
	}
	if err := mutate(root); err != nil {
		return err
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := runtimeConfigFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, runtimeConfigFile)
}

// reloadRemoteConfig 通知运行中的 serve 重新加载 config.json。
func reloadRemoteConfig() error {
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	if cfg.Addr == "0.0.0.0" || cfg.Addr == "::" {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/admin/reload", nil)
	if err != nil {
		return err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// runWarmupImmediate 调用运行中服务的 /admin/warmup 立即执行一轮。
func runWarmupImmediate() {
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	if cfg.Addr == "0.0.0.0" || cfg.Addr == "::" {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/admin/warmup", nil)
	if err != nil {
		fmt.Printf("构造请求失败: %v\n", err)
		return
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Printf("调用失败: %v\n请确认 serve 正在运行，且 -addr/-port 与之一致。\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("服务返回 HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return
	}
	fmt.Printf("已请求 %s/admin/warmup：%s\n", addr, strings.TrimSpace(string(body)))
	fmt.Println("触发在后台执行，稍后用 warmup status 查看结果。")
}

// printWarmupStatus 展示配置与最近触发结果。
// serve 在运行时优先读其实时状态（含已选模型），否则只读本地文件。
func printWarmupStatus() {
	if st, err := fetchRemoteWarmupStatus(); err == nil {
		printRemoteWarmupStatus(st)
		return
	}
	// serve 未运行：从 config.json + 状态文件读取。
	var fileCfg runtimeFileConfig
	if data, err := os.ReadFile(runtimeConfigFile); err == nil {
		_ = json.Unmarshal(data, &fileCfg)
	}
	setWarmup(fileCfg.Warmup)
	setProbeSchedule(fileCfg.Probe.Schedule)
	st := warmupStatusNow()
	st.Selected = nil
	fmt.Println("（serve 未在运行，以下为 config.json 的静态配置）")
	printRemoteWarmupStatus(st)
}

func fetchRemoteWarmupStatus() (warmupStatusResponse, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	if cfg.Addr == "0.0.0.0" || cfg.Addr == "::" {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/admin/warmup", nil)
	if err != nil {
		return warmupStatusResponse{}, err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return warmupStatusResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return warmupStatusResponse{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out warmupStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return warmupStatusResponse{}, err
	}
	return out, nil
}

func printRemoteWarmupStatus(st warmupStatusResponse) {
	state := "开启"
	if !st.Enabled {
		state = "关闭"
	}
	fmt.Println("5 小时窗口主动触发")
	fmt.Printf("  状态:        %s\n", state)
	fmt.Printf("  触发时刻:    %s（%s）\n", st.Time, st.TZ)
	if len(st.Windows) > 0 {
		fmt.Printf("  冷却触发时段: %s\n", strings.Join(st.Windows, ", "))
	} else {
		fmt.Println("  冷却触发时段: （不限，全天）")
	}
	fmt.Printf("  冷却恢复触发: %s（最小间隔 %d 分钟）\n", onOff(st.CooldownTrigger), st.CooldownTriggerMinMinutes)
	if st.MaxPrice == 0 {
		fmt.Printf("  价格上限:    0（只触发已确认免费的模型）\n")
	} else {
		fmt.Printf("  价格上限:    %s\n", formatQuota(st.MaxPrice))
	}
	if len(st.Models) > 0 {
		fmt.Printf("  指定模型:    %s\n", strings.Join(st.Models, ", "))
	} else {
		fmt.Printf("  指定模型:    （未指定，按价格上限筛选）\n")
	}
	fmt.Printf("  补触发窗口:  %d 小时 / 每 %d 分钟重试\n", st.CatchUpHours, st.RetryMinutes)
	fmt.Printf("  结果通知:    %s\n", onOff(st.Notify))
	fmt.Printf("  主动探测:    %s（%s）\n", onOff(st.ProbeEnabled), st.ProbeEffectiveAt)
	if st.ProbeFollowWarmup && st.Enabled {
		fmt.Println("              跟随 5 小时窗口触发时刻一起执行")
	} else {
		fmt.Printf("              独立时刻 %s\n", st.ProbeTime)
	}
	fmt.Printf("  探测结果通知: %s\n", onOff(st.ProbeNotify))
	if len(st.Selected) > 0 {
		fmt.Printf("  命中模型:    %d 个：%s\n", len(st.Selected), strings.Join(st.Selected, ", "))
	}
	if st.Cycle != nil {
		fmt.Printf("\n当前周期 %s（计划 %s）\n", st.Cycle.Date,
			formatDisplayTime(time.Unix(st.Cycle.ScheduledAt, 0)))
		keys := make([]string, 0, len(st.Cycle.Models))
		for k := range st.Cycle.Models {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			fmt.Println("  尚未执行")
		}
		for _, k := range keys {
			v := st.Cycle.Models[k]
			fmt.Printf("  - %-24s %-8s %s\n", k, v.Status, v.Detail)
			if v.Account != "" {
				fmt.Printf("    └ 站点=%s 账号=%s 尝试=%d次\n", v.Site, v.Account, v.Attempts)
			}
		}
	}
	if len(st.History) > 0 {
		fmt.Println("\n最近记录")
		for _, h := range st.History {
			tag := ""
			if h.CatchUp {
				tag = "（补触发）"
			}
			fmt.Printf("  %s  成功=%d 失败=%d 跳过=%d%s\n", h.Date, h.OK, h.Failed, h.Skipped, tag)
		}
	}
}

func onOff(v bool) string {
	if v {
		return "开启"
	}
	return "关闭"
}
