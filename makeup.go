package main

// 国内站断登自动补签：连登断点用补登卡自动补上，保住连登奖励。
//
// 移植自 88lin/workbuddy-auto-signin 2026-10-01 修复后的补登契约（245f313，
// 依据官方 GrowthCenterPage / AllTasksPage 前端与真实只读接口核对）。硬约束：
//
//  1. GET /streak 的 streak.makeup_dates 是「已补登记录」，不是待办清单；
//     真正的断点来自 GET /heatmap 中 score == 0 的历史日期。
//  2. 只补「当月、今天之前（用 heatmap.today 的服务端日期，不依赖机器时区）、
//     max(当月月初, launch_date, 2026-06-17 活动上线日) 之后」的零分日期。
//     日历缺项不推测为零；结构异常整段停止，不能靠猜消耗补登卡。
//  3. 每轮最多 1 张卡（官方规则补登卡上限 4 张，留余量给真实断登）；
//     最近的断点优先，尽早恢复当前连登。
//  4. 写请求（makeup-cards/use）绝不自动重试，避免超时发生在服务端处理完成
//     之后造成重复提交；HTTP 2xx 但业务码非零同样按失败处理。
//  5. 400 + "date is not broken, no makeup needed" 表示查询后日期已被客户端
//     或另一轮补上，是正常竞争，按无需操作处理并停止本轮。
//  6. 没有卡时不查日历；任一前置查询失败不花卡，只报告。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// makeupStreakPath 连登状态：launch_date、streak.makeup_dates、makeup_cards 余额。
	makeupStreakPath = "/v2/activity/growth/streak"
	// makeupHeatmapPath 活跃日历：today.date（服务端日期）+ cells[{date,score}]。
	makeupHeatmapPath = "/v2/activity/growth/heatmap"
	// makeupUsePath 补登提交，请求体 {"target_date": "YYYY-MM-DD"}。
	makeupUsePath = "/v2/activity/growth/makeup-cards/use"

	// makeupFloorDate 官方成长活动上线日（AllTasksPage canMakeup 的绝对下界）。
	makeupFloorDate = "2026-06-17"
	// makeupMaxPerRun 每轮最多补 1 天：官方补登卡上限 4 张，自动补留余量。
	makeupMaxPerRun = 1
	// makeupTimeout 单账号补签预算：两个查询 + 至多一次提交。
	makeupTimeout = 60 * time.Second
	// makeupDateLayout 上游日期格式。
	makeupDateLayout = "2006-01-02"
	// makeupBenignMark 服务端「该日期无需补登」提示的稳定片段
	//（查询后日期已被补上的正常竞争，不算失败）。
	makeupBenignMark = "date is not broken"
)

// cnMakeupEnabled 断登自动补签开关（config.json checkin.cn.makeup，默认开启）。
var cnMakeupEnabled = true

// -----------------------------------------------------------------------------
// 响应结构
// -----------------------------------------------------------------------------

// makeupStreakData 连登状态接口的 data 段。makeup_cards 余额有两种形状
// （{"balance":2} 与 2），先按 RawMessage 收下再统一解析。
type makeupStreakData struct {
	LaunchDate  string          `json:"launch_date"`
	Streak      *makeupStreak   `json:"streak"`
	MakeupCards json.RawMessage `json:"makeup_cards"`
}

// makeupStreak 连登明细：makeup_dates 是已补登记录。对象缺失视为结构异常
// （对齐参考实现：streak 不是 dict 直接报错，不靠猜）。
type makeupStreak struct {
	MakeupDates []string `json:"makeup_dates"`
}

// makeupHeatmapData 活跃日历接口的 data 段。score 用 any 收下，保证
// 「非数字 / null / 负数」都能按结构异常处理而不是被零值吞掉。
type makeupHeatmapData struct {
	Today struct {
		Date string `json:"date"`
	} `json:"today"`
	Cells []struct {
		Date  string `json:"date"`
		Score any    `json:"score"`
	} `json:"cells"`
}

// -----------------------------------------------------------------------------
// 解析与校验
// -----------------------------------------------------------------------------

// makeupDate 解析 YYYY-MM-DD。time.Parse 天然拒绝 2026-02-30 这类不存在日期。
func makeupDate(value string) (time.Time, error) {
	t, err := time.Parse(makeupDateLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("补登日期格式无效: %q", value)
	}
	return t, nil
}

// makeupBalanceValue 余额值解析：数字 / 数字字符串按参考实现 as_int 语义接受；
// null 表示字段缺失，按 0 处理（无卡跳过，不是结构异常）。
func makeupBalanceValue(v any) (int, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n != math.Trunc(n) {
			return 0, fmt.Errorf("补登卡余额无效: %v", n)
		}
		return int(n), nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return makeupBalanceValue(f)
		}
	}
	return 0, fmt.Errorf("补登卡余额格式无效")
}

// makeupCardBalance 解析补登卡余额，兼容 {"balance":2}、2、"2" 三种形状；
// 字段缺失或 null 按余额 0 处理。完全无法理解的形状返回错误。
func makeupCardBalance(raw json.RawMessage) (int, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, nil
	}
	var obj struct {
		Balance any `json:"balance"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return makeupBalanceValue(obj.Balance)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		return makeupBalanceValue(v)
	}
	return 0, fmt.Errorf("补登卡余额格式无效: %s", truncate(s, 40))
}

// makeupCandidates 从日历选断登日期：当月、今天之前（服务端日期）、
// max(当月月初, launch_date, 2026-06-17) 之后、score == 0、且不在已补登记录里。
// 返回按日期倒序（最近断点优先）。任何结构异常返回错误，调用方必须整段停止。
// 逐天扫描（≤31 天）同时天然满足「日历缺项不推测为零」：没出现在日历里的日期直接跳过。
func makeupCandidates(streak makeupStreakData, heat makeupHeatmapData) ([]string, error) {
	today, err := makeupDate(heat.Today.Date)
	if err != nil {
		return nil, fmt.Errorf("活跃日历缺少服务端日期: %w", err)
	}
	launch, err := makeupDate(streak.LaunchDate)
	if err != nil {
		return nil, fmt.Errorf("连登状态缺少活动上线日期: %w", err)
	}
	floor, _ := makeupDate(makeupFloorDate)
	start := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	if launch.After(start) {
		start = launch
	}
	if floor.After(start) {
		start = floor
	}
	if streak.Streak == nil {
		return nil, fmt.Errorf("连登状态格式无效")
	}
	madeUp := map[string]bool{}
	for _, d := range streak.Streak.MakeupDates {
		if _, err := makeupDate(d); err != nil {
			return nil, fmt.Errorf("已补登记录格式无效: %w", err)
		}
		madeUp[d] = true
	}
	if heat.Cells == nil {
		return nil, fmt.Errorf("活跃日历缺少日期列表")
	}
	scores := map[string]float64{}
	for _, cell := range heat.Cells {
		if _, err := makeupDate(cell.Date); err != nil {
			return nil, fmt.Errorf("活跃日历日期无效: %w", err)
		}
		score, ok := cell.Score.(float64)
		if !ok || math.IsInf(score, 0) || score < 0 {
			return nil, fmt.Errorf("活跃日历分数无效: %s", cell.Date)
		}
		if prev, seen := scores[cell.Date]; seen && prev != score {
			return nil, fmt.Errorf("活跃日历同一日期记录冲突: %s", cell.Date)
		}
		scores[cell.Date] = score
	}
	day := start
	var out []string
	for day.Before(today) {
		key := day.Format(makeupDateLayout)
		if score, ok := scores[key]; ok && score == 0 && !madeUp[key] {
			out = append(out, key)
		}
		day = day.AddDate(0, 0, 1)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// -----------------------------------------------------------------------------
// 请求结果描述
// -----------------------------------------------------------------------------

// makeupDescribe 统一描述一次请求结果，供日志定位。
func makeupDescribe(status int, err error, env growthEnvelope) string {
	if err != nil {
		if env.Msg != "" {
			return fmt.Sprintf("HTTP %d（%s）", status, env.Msg)
		}
		return err.Error()
	}
	if env.Code != 0 {
		return fmt.Sprintf("业务码 %d: %s", env.Code, env.Msg)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// makeupQueryOK 前置查询（补登判断所必需）成功条件：HTTP 2xx 且业务码为 0。
// 失败必须停止补签并提醒，不能把未知状态解释为「无需补登」。
func makeupQueryOK(env growthEnvelope, status int, err error) bool {
	if err != nil {
		return false
	}
	return status >= 200 && status < 300 && env.Code == 0
}

// -----------------------------------------------------------------------------
// 主流程
// -----------------------------------------------------------------------------

// runMakeupCards 为单个国内站账号执行断登自动补签。结果只记日志；
// 由签到成功后的国内站附加动作调用，调用方需已持有该账号的凭据快照。
func runMakeupCards(ctx context.Context, auth *StoredAuth, prof *upstreamProfile, path string) {
	ctx, cancel := context.WithTimeout(ctx, makeupTimeout)
	defer cancel()
	add := func(format string, args ...any) {
		log.Printf("[Makeup] 账号 %s %s", path, fmt.Sprintf(format, args...))
	}

	// 1. 查连登状态：拿卡余额与已补登记录。
	env, status, err := growthRequest(ctx, http.MethodGet, prof.Base+makeupStreakPath,
		growthHeaders(prof, auth), nil)
	if !makeupQueryOK(env, status, err) {
		add("查连登状态失败，本轮不补签: %s", makeupDescribe(status, err, env))
		return
	}
	var streak makeupStreakData
	if err := json.Unmarshal(env.Data, &streak); err != nil {
		add("连登状态结构异常，本轮不补签: %v", err)
		return
	}
	cards, err := makeupCardBalance(streak.MakeupCards)
	if err != nil {
		add("%v，本轮不补签", err)
		return
	}
	if cards <= 0 {
		add("无补登卡，跳过")
		return
	}

	// 2. 有卡才查日历，从 heatmap 选断点。
	henv, hstatus, herr := growthRequest(ctx, http.MethodGet, prof.Base+makeupHeatmapPath,
		growthHeaders(prof, auth), nil)
	if !makeupQueryOK(henv, hstatus, herr) {
		add("查补登日历失败，本轮不补签（不消耗卡）: %s", makeupDescribe(hstatus, herr, henv))
		return
	}
	var heat makeupHeatmapData
	if err := json.Unmarshal(henv.Data, &heat); err != nil {
		add("补登日历结构异常，本轮不补签（不消耗卡）: %v", err)
		return
	}
	dates, err := makeupCandidates(streak, heat)
	if err != nil {
		add("%v，本轮不补签（不消耗卡）", err)
		return
	}
	if len(dates) == 0 {
		add("无断登日期需要补签（卡余额 %d）", cards)
		return
	}

	// 3. 补登：每轮最多 1 张卡；写失败即停，不重试不换日期。
	limit := cards
	if limit > makeupMaxPerRun {
		limit = makeupMaxPerRun
	}
	remaining := cards
	successes := 0
	for _, d := range dates[:limit] {
		payload, _ := json.Marshal(map[string]any{"target_date": d})
		uenv, ustatus, uerr := growthRequest(ctx, http.MethodPost, prof.Base+makeupUsePath,
			growthHeaders(prof, auth), strings.NewReader(string(payload)))
		if uerr == nil && ustatus >= 200 && ustatus < 300 && uenv.Code == 0 {
			successes++
			remaining--
			// 优先报服务端给的余额，本地递减只是接口没给时的兜底。
			var useData struct {
				MakeupCards json.RawMessage `json:"makeup_cards"`
			}
			if len(uenv.Data) > 0 {
				_ = json.Unmarshal(uenv.Data, &useData)
			}
			// 仅在响应确实携带余额时才覆盖本地递减值；缺字段保持本地计算。
			if len(useData.MakeupCards) > 0 {
				if left, lerr := makeupCardBalance(useData.MakeupCards); lerr == nil {
					remaining = left
				}
			}
			add("补登 %s 成功（剩 %d 张卡）", d, remaining)
			continue
		}
		detail := strings.TrimSpace(uenv.Msg)
		if detail == "" && uerr != nil {
			detail = uerr.Error()
		}
		if ustatus == http.StatusBadRequest && strings.Contains(strings.ToLower(detail), makeupBenignMark) {
			add("%s 已活跃或已被补登，无需再次补签", d)
			return
		}
		add("补登 %s 失败: %s", d, makeupDescribe(ustatus, uerr, uenv))
		return
	}
	if successes > 0 && len(dates) > successes && remaining > 0 {
		add("另有 %d 天可补、剩 %d 张卡，下轮继续", len(dates)-successes, remaining)
	}
}
