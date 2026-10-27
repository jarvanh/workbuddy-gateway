package main

// restricted.go —— 上游风控限制（code 11140）的账号级处置。
//
// 背景（2026-10-26 国际站 5 账号齐中 11140 事故 + 社区复盘）：
//   上游 403 {"code":11140,"msg":"request illegal","displayMsg":{"zh":"账号存在异常行为，已被限制使用"}}
//   表示「账号被风控限制」，与 429/6004 频率限制、14018 额度耗尽、12153 凭据失效完全不同：
//   账号凭据仍然有效（Quota/套餐等只读接口正常），只有 chat 请求被策略拒绝，
//   actions=SUBMIT_FEEDBACK 表明解除途径是官方端提交反馈申诉。
//   社区红线（workbuddy2api-hub #422 实测）：命中 11140 后严禁换号继续打同一站点 ——
//   旧的「60s 冷却 → 换下一个账号 → 同请求再发」会成批送出连坐证据，一个任务打挂整池账号。
//
// 本文件照搬 workbuddy2api-hub / magpie 的成熟方案：
//   1. 独立分类：isRestrictedAccount 优先于限流/鉴权分支，文案写「风控限制需申诉」。
//   2. 命中即停：默认（upstream.restricted403RotateMax=0）不再换号，直接 429 返回申诉指引。
//   3. 持久化：wb-restricted-state.json 记录解除时刻（默认 2 小时）；重启、凭据热加载、
//      Token 续期都不会把受限账号放回池子；只有真实成功一次（chat/probe 200）才清除。
//   4. 整池熔断：同站点 15 分钟内 ≥2 个账号被限 → 该站点停发 30 分钟，期间一个上游请求都不发。
//   5. 后台任务（warmup/探测/保活/签到）跳过受限账号与熔断站点；
//      解除靠「真实成功一次」或 `restricted --clear` / 管理接口手动确认，绝不自动探测。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// restrictedStateFile 持久化受限状态。命名避开 isCredentialFile 的
	// 「workbuddy*.json」前缀匹配，防止被当成凭据文件加载进账号池。
	restrictedStateFile = "wb-restricted-state.json"
	// restrictedStateSchema 状态文件结构版本；不匹配时视为损坏并忽略（保留原文件）。
	restrictedStateSchema = 1
	// restrictedDefaultWindow 单次风控限制的默认时长。社区经验值 2 小时：
	// 上游异常行为限制不会在秒级/分钟级自动解除，60 秒冷却只会导致反复重试。
	restrictedDefaultWindow = 2 * time.Hour
	// 整池熔断参数（照搬 workbuddy2api-hub 的 wave/hold 设计）。
	restrictedWaveWindow   = 15 * time.Minute
	restrictedWaveMin      = 2
	restrictedHoldDuration = 30 * time.Minute
)

// restrictedRecord 状态文件里的单账号受限记录。
type restrictedRecord struct {
	Path  string `json:"path"`
	Site  string `json:"site,omitempty"`
	Until int64  `json:"until"`
	At    int64  `json:"at,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

// restrictedHoldDoc 状态文件里的站点熔断记录（重启后不再向受限站点发请求）。
type restrictedHoldDoc struct {
	Site  string `json:"site"`
	Until int64  `json:"until"`
}

// restrictedStateDoc 状态文件结构。
type restrictedStateDoc struct {
	Schema    int                `json:"schema"`
	UpdatedAt int64              `json:"updatedAt"`
	Accounts  []restrictedRecord `json:"accounts"`
	Hold      *restrictedHoldDoc `json:"hold,omitempty"`
}

// restrictedWave 站点级连坐熔断状态。
// 锁序约定：accountMu → restrictedWave（restrictedWave 内部绝不回调账号池）。
var restrictedWave = struct {
	sync.Mutex
	events    map[string][]time.Time // site → 窗口内命中时刻列表
	holdSite  string
	holdUntil time.Time
}{events: map[string][]time.Time{}}

// restricted403RotateMaxSnapshot 命中 11140 后允许换号重试的最大次数。
// 0（默认）= 命中即停；>0 仅用于显式要求旧行为的场景（不建议）。
func restricted403RotateMaxSnapshot() int {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return restricted403RotateMax
}

// isRestrictedAccount 识别上游风控限制（11140）。
// 必须先于 isRateLimited / isAuthFailure 调用：11140 是 403 + request illegal，
// 落进鉴权分支会误禁账号，落进 WAF 分支只会给 60 秒冷却然后换号连打。
func isRestrictedAccount(statusCode int, body string) bool {
	if strings.Contains(body, `"code":11140`) {
		return true
	}
	return strings.Contains(body, "账号存在异常行为") ||
		strings.Contains(body, "帳號存在異常行為") ||
		strings.Contains(strings.ToLower(body), "restricted due to unusual activity")
}

// markAccountRestricted 将账号标记为受限（2 小时窗口）并落盘，返回解除时刻。
// 重复命中只保留更晚的解除时刻，不重置已开始的窗口 —— 避免「每分钟重试一次
// → 每次都重新计 2 小时」的滚动续期，让账号永远差一分钟解除。
func markAccountRestricted(acc *Account, msg string) time.Time {
	now := time.Now()
	until := now.Add(restrictedDefaultWindow)
	accountMu.Lock()
	site := accSiteLocked(acc)
	if acc.RestrictedUntil.After(until) {
		until = acc.RestrictedUntil // 已在受限中：保留更晚的原解除时刻
	} else {
		acc.RestrictedAt = now
	}
	acc.RestrictedUntil = until
	acc.RestrictedMsg = truncate(msg, 200)
	path := acc.Path
	accountMu.Unlock()

	log.Printf("[Restricted] 账号 %s 被上游风控限制，站点=%s，解除时间=%s (提示: %s)",
		filepath.Base(path), site, formatDisplayTime(until), truncate(msg, 200))

	holdArmed, holdSite := recordSiteRestrictedWave(site)
	saveRestrictedState()

	// 账号级告警（Key 含路径，走 notify 最小间隔去重）。
	sendNotify(notifyEvent{
		Kind:  notifyEventRestricted,
		Key:   "restricted|" + path,
		Level: "error",
		Title: "🛑 workbuddy 账号被上游风控限制（需申诉）",
		Body: fmt.Sprintf("账号: %s\n站点: %s\n解除时间: %s\n剩余: %s\n原因: %s\n账号池: %s\n处理: 命中即停，不换号重试；请先在官方端提交反馈申诉，解除后网关会在下一次真实成功时自动恢复调度",
			filepath.Base(path), site, formatDisplayTime(until), formatCooldownRemaining(now, until), truncate(msg, 200), restrictedPoolLine()),
		HTML: restrictedNotifyHTML(path, site, until, msg),
	})
	if holdArmed {
		sendNotify(notifyEvent{
			Kind:  notifyEventRestricted,
			Key:   "restricted_hold|" + holdSite,
			Level: "error",
			Title: fmt.Sprintf("🛑 workbuddy 站点熔断：%s 短时停发（风控连坐防护）", holdSite),
			Body: fmt.Sprintf("站点: %s\n窗口 %s 内 %d 个账号被风控限制，触发整池熔断 %s\n处理: 熔断期间该站点一个上游请求都不发；请检查是否有任务在反复重试，并尽快申诉",
				holdSite, restrictedWaveWindow, restrictedWaveMin, restrictedHoldDuration),
		})
	}
	return until
}

// recordSiteRestrictedWave 记录一次站点命中并判断是否触发整池熔断。
// 返回（是否新触发熔断, 熔断站点）。
func recordSiteRestrictedWave(site string) (bool, string) {
	if site == "" {
		return false, ""
	}
	now := time.Now()
	restrictedWave.Lock()
	defer restrictedWave.Unlock()
	events := append(restrictedWave.events[site], now)
	kept := events[:0]
	for _, at := range events {
		if now.Sub(at) <= restrictedWaveWindow {
			kept = append(kept, at)
		}
	}
	restrictedWave.events[site] = kept
	if len(kept) < restrictedWaveMin {
		return false, ""
	}
	if restrictedWave.holdSite == site && restrictedWave.holdUntil.After(now) {
		return false, "" // 熔断已在生效，不重复告警
	}
	restrictedWave.holdSite = site
	restrictedWave.holdUntil = now.Add(restrictedHoldDuration)
	return true, site
}

// siteRestrictedHold 判断站点是否处于整池熔断中（可在持有 accountMu 时调用）。
func siteRestrictedHold(site string) bool {
	if site == "" {
		return false
	}
	restrictedWave.Lock()
	defer restrictedWave.Unlock()
	return restrictedWave.holdSite == site && restrictedWave.holdUntil.After(time.Now())
}

// accountRestrictedLocked 读取账号受限状态（内部自行加锁，随时可调）。
func accountRestrictedLocked(acc *Account) bool {
	accountMu.Lock()
	defer accountMu.Unlock()
	return acc.RestrictedUntil.After(time.Now())
}

// restrictedHoldSnapshot 返回当前熔断站点（无则空串）与解除时刻。
func restrictedHoldSnapshot() (string, time.Time) {
	restrictedWave.Lock()
	defer restrictedWave.Unlock()
	return restrictedWave.holdSite, restrictedWave.holdUntil
}

// clearAccountRestricted 清除账号的受限标记（仅真实成功或手动确认时调用）。
func clearAccountRestricted(acc *Account, reason string) bool {
	accountMu.Lock()
	if !acc.RestrictedUntil.After(time.Now()) {
		accountMu.Unlock()
		return false
	}
	path := acc.Path
	acc.RestrictedUntil, acc.RestrictedAt, acc.RestrictedMsg = time.Time{}, time.Time{}, ""
	accountMu.Unlock()
	log.Printf("[Restricted] 账号 %s 风控限制解除（%s），恢复正常调度", filepath.Base(path), reason)
	saveRestrictedState()
	return true
}

// restrictedPoolLine 账号池概览（含受限计数），供通知正文使用。
func restrictedPoolLine() string {
	accountMu.Lock()
	avail, cooling, restricted, disabled := 0, 0, 0, 0
	now := time.Now()
	for _, acc := range accounts {
		switch {
		case acc.Disabled:
			disabled++
		case acc.RestrictedUntil.After(now):
			restricted++
		case acc.CooldownUntil.After(now):
			cooling++
		default:
			avail++
		}
	}
	total := len(accounts)
	accountMu.Unlock()
	return fmt.Sprintf("可用 %d · 冷却 %d · 受限 %d · 失效 %d · 共 %d", avail, cooling, restricted, disabled, total)
}

// restrictedNotifyHTML 账号受限告警的 Telegram HTML 正文（与其他通知同版式）。
func restrictedNotifyHTML(path, site string, until time.Time, msg string) string {
	var b strings.Builder
	b.WriteString(tgTitle("🛑 workbuddy 账号被上游风控限制（需申诉）"))
	b.WriteString(tgPath("账号", filepath.Base(path)))
	b.WriteString(tgKV("站点", site))
	b.WriteString(tgKV("解除时间", formatDisplayTime(until)))
	b.WriteString(tgKV("剩余", formatCooldownRemaining(time.Now(), until)))
	b.WriteString(tgKV("原因", truncate(msg, 200)))
	b.WriteString(tgKV("账号池", restrictedPoolLine()))
	b.WriteString(tgKV("处理", "命中即停，不换号重试；请先在官方端提交反馈申诉"))
	if items := collectRestrictedItems(); len(items) > 0 {
		entries := make([]string, 0, len(items))
		for _, it := range items {
			entries = append(entries, "<code>"+escapeHTML(it.path)+"</code> · "+
				escapeHTML(formatDisplayTime(it.until)))
		}
		tgSection(&b, fmt.Sprintf("🔒 其他受限中 · %d", len(items)))
		b.WriteString(treeLines(entries) + "\n")
	}
	return b.String()
}

// restrictedItem 一条受限记录（通知清单用）。
type restrictedItem struct {
	path  string
	until time.Time
}

// collectRestrictedItems 汇总当前仍受限的账号，按解除时间升序。
func collectRestrictedItems() []restrictedItem {
	accountMu.Lock()
	defer accountMu.Unlock()
	now := time.Now()
	var items []restrictedItem
	for _, acc := range accounts {
		if acc.RestrictedUntil.After(now) {
			items = append(items, restrictedItem{filepath.Base(acc.Path), acc.RestrictedUntil})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].until.Before(items[j].until) })
	return items
}

// saveRestrictedState 原子落盘受限状态（持 accountMu 读取，过期条目不写入）。
// 调用方不得已持有 accountMu 的写锁以外的锁序要求；本函数自行加锁。
func saveRestrictedState() {
	accountMu.Lock()
	now := time.Now()
	doc := restrictedStateDoc{Schema: restrictedStateSchema, UpdatedAt: now.Unix()}
	for _, acc := range accounts {
		if acc.RestrictedUntil.After(now) {
			doc.Accounts = append(doc.Accounts, restrictedRecord{
				Path:  acc.Path,
				Site:  accSiteLocked(acc),
				Until: acc.RestrictedUntil.Unix(),
				At:    unixOrZero(acc.RestrictedAt),
				Msg:   acc.RestrictedMsg,
			})
		}
	}
	accountMu.Unlock()
	if site, until := restrictedHoldSnapshot(); site != "" && until.After(now) {
		doc.Hold = &restrictedHoldDoc{Site: site, Until: until.Unix()}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		log.Printf("[Restricted] 受限状态序列化失败: %v", err)
		return
	}
	tmp := restrictedStateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("[Restricted] 受限状态写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, restrictedStateFile); err != nil {
		log.Printf("[Restricted] 受限状态落盘失败: %v", err)
	}
}

// loadRestrictedState 从状态文件恢复受限账号与站点熔断。
// 在 loadAccounts（启动）后调用；凭据热加载重建账号池后也需重新调用，
// 保证「重启 / 换凭据 / 刷新 token」都不会把受限账号放回池子。
func loadRestrictedState() {
	data, err := os.ReadFile(restrictedStateFile)
	if err != nil {
		return
	}
	var doc restrictedStateDoc
	if json.Unmarshal(data, &doc) != nil || doc.Schema != restrictedStateSchema {
		log.Printf("[Restricted] 受限状态文件不可解析（schema=%d），已忽略；如需清理请删除 %s",
			doc.Schema, restrictedStateFile)
		return
	}
	now := time.Now()
	restored := 0
	accountMu.Lock()
	byPath := make(map[string]*Account, len(accounts))
	for _, acc := range accounts {
		byPath[acc.Path] = acc
	}
	for _, rec := range doc.Accounts {
		until := time.Unix(rec.Until, 0)
		if !until.After(now) {
			continue
		}
		acc, ok := byPath[rec.Path]
		if !ok || acc.Disabled {
			continue
		}
		if until.After(acc.RestrictedUntil) {
			acc.RestrictedUntil = until
			acc.RestrictedAt = timeFromUnix(rec.At)
			acc.RestrictedMsg = rec.Msg
			restored++
		}
	}
	accountMu.Unlock()
	if doc.Hold != nil {
		until := time.Unix(doc.Hold.Until, 0)
		if until.After(now) {
			restrictedWave.Lock()
			restrictedWave.holdSite = doc.Hold.Site
			restrictedWave.holdUntil = until
			restrictedWave.Unlock()
		}
	}
	if restored > 0 {
		log.Printf("[Restricted] 已从 %s 恢复 %d 个受限账号（含站点熔断状态），解封前不参与调度",
			restrictedStateFile, restored)
	}
}

// restrictedAccountNames 返回受限账号清单（含解除时刻展示），供 CLI/管理接口。
func restrictedAccountNames() []string {
	items := collectRestrictedItems()
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprintf("%s（解除 %s）", it.path, formatDisplayTime(it.until)))
	}
	return out
}

// runRestricted CLI 入口：restricted [list] / restricted --clear <文件名|all>。
// 手动解除仅用于「已在官方端申诉通过」的场景；误用只会提前把受限账号放回池子。
func runRestricted(args []string) {
	if len(args) > 0 && args[0] == "--clear" {
		target := "all"
		if len(args) > 1 {
			target = strings.TrimSpace(args[1])
		}
		if err := loadAccounts(); err != nil {
			fmt.Printf("加载账号池失败: %v\n", err)
			os.Exit(1)
		}
		loadRestrictedState()
		cleared := clearRestrictedByArg(target)
		if cleared == 0 {
			fmt.Printf("没有匹配的受限账号（目标=%s）。当前受限清单:\n", target)
			for _, line := range restrictedAccountNames() {
				fmt.Println("  " + line)
			}
			return
		}
		fmt.Printf("已解除 %d 个账号的风控限制（目标=%s）；解除依据应为官方申诉通过或真实成功一次。\n", cleared, target)
		return
	}
	if err := loadAccounts(); err != nil {
		fmt.Printf("加载账号池失败: %v\n", err)
		os.Exit(1)
	}
	loadRestrictedState()
	items := collectRestrictedItems()
	if site, until := restrictedHoldSnapshot(); site != "" && until.After(time.Now()) {
		fmt.Printf("站点熔断中: %s（解除 %s）\n", site, formatDisplayTime(until))
	}
	if len(items) == 0 {
		fmt.Println("当前没有账号处于上游风控限制（11140）中。")
		return
	}
	fmt.Printf("当前受限账号 %d 个（解除途径：官方申诉 → 真实成功一次自动恢复；或 restricted --clear 手动确认）:\n", len(items))
	for _, it := range items {
		fmt.Printf("  %s · 解除 %s\n", it.path, formatDisplayTime(it.until))
	}
}

// clearRestrictedByArg 按文件名或 all 清除受限标记，返回清除数量。
func clearRestrictedByArg(target string) int {
	accountMu.Lock()
	cleared := 0
	for _, acc := range accounts {
		if !acc.RestrictedUntil.After(time.Now()) {
			continue
		}
		if target != "all" && filepath.Base(acc.Path) != target && acc.Path != target {
			continue
		}
		acc.RestrictedUntil, acc.RestrictedAt, acc.RestrictedMsg = time.Time{}, time.Time{}, ""
		cleared++
	}
	accountMu.Unlock()
	if cleared > 0 {
		saveRestrictedState()
	}
	return cleared
}

// handleAdminRestricted 管理接口：GET 查看受限清单 / POST {"clear":"文件名|all"} 手动解除。
// 挂在 authMiddleware 之后：需 API Key；再加回环限制与 warmup 管理接口同口径。
func handleAdminRestricted(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeOpenAIError(w, http.StatusForbidden, "restricted_local_only", "restricted 管理接口仅允许本机回环地址调用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		site, holdUntil := restrictedHoldSnapshot()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts":  restrictedAccountNames(),
			"holdSite":  site,
			"holdUntil": unixOrZero(holdUntil),
		})
	case http.MethodPost:
		var body struct {
			Clear string `json:"clear"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		target := strings.TrimSpace(body.Clear)
		if target == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 clear 字段（账号文件名或 all）")
			return
		}
		cleared := clearRestrictedByArg(target)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "cleared": cleared})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET / POST 请求")
	}
}
