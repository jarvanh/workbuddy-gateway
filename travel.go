package main

// -----------------------------------------------------------------------------
// 国内站 Buddy 旅行（参考 codebuddy2api 的活动协议实现）
//
// 活动接口与对话上游不同源，固定为官方活动域名 www.workbuddy.cn，所有国内账号
// 共用；协议为 {code, msg, data} 业务包络，code != 0 视为业务失败。
// 完整流程：查询旅行状态 → 到达则领取奖励 → 空闲且未达上限则随机地点派遣。
// 首次领取 Buddy（猫猫）涉及官方新手任务与协议确认，交由用户在官方成长中心
// 完成；账号已有 Buddy 后即可全自动派遣。
// -----------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// buddyTravelBase 是国内站 Buddy 活动接口的固定域名（定义为变量便于测试指向假上游）。
var buddyTravelBase = "https://www.workbuddy.cn"

const (
	buddyTravelPrefix = "/activity/growth/buddy/travel/"
	buddyInfoPath     = "/activity/growth/buddy/info"
	growthCenterURL   = "https://www.workbuddy.cn/profile/growth-center"
)

// travelStatus 是旅行状态接口的规范化结果。
type travelStatus struct {
	State             string // idle | traveling | arrived
	DailyLimitReached *bool
	LocationID        int64
	LocationName      string
	RewardCredit      float64
}

// buddyLocation 是旅行派遣的可选地点。
type buddyLocation struct {
	ID   int64
	Name string
}

// buddyActivityHeaders 是 Buddy 活动接口的鉴权与产品标识 Header。
func buddyActivityHeaders(auth *StoredAuth) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("Accept", "application/json")
		r.Header.Set("X-Product-Code", "workbuddy")
	}
}

// buddyActivityRequest 调用 Buddy 活动接口并解析业务数据（业务包络由 doJSONContext 处理）。
// POST 类操作成功时上游可能不返回业务数据，此时返回空 map。
func buddyActivityRequest(ctx context.Context, auth *StoredAuth, method, url string, body io.Reader) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, status, err := doJSONContext(ctx, cfg.HttpClient, method, url, buddyActivityHeaders(auth), body)
	if err != nil {
		return nil, fmt.Errorf("HTTP %d: %v", status, err)
	}
	data := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &data) // data 为 null 或缺失时保持空 map
	}
	return data, nil
}

// buddyTravelStatus 查询并规范化旅行状态。
func buddyTravelStatus(ctx context.Context, auth *StoredAuth) (travelStatus, error) {
	data, err := buddyActivityRequest(ctx, auth, http.MethodGet, buddyTravelBase+buddyTravelPrefix+"status", nil)
	if err != nil {
		return travelStatus{}, err
	}
	st := travelStatus{State: "unknown"}
	state, _ := data["state"].(string)
	switch state {
	case "idle", "traveling", "arrived":
		st.State = state
	default:
		return st, fmt.Errorf("旅行状态异常: %v", data["state"])
	}
	if v, ok := data["daily_limit_reached"].(bool); ok {
		st.DailyLimitReached = &v
	}
	if loc, ok := data["location"].(map[string]any); ok {
		if id, ok := loc["id"].(float64); ok {
			st.LocationID = int64(id)
		}
		if name, ok := loc["name"].(string); ok {
			st.LocationName = name
		}
	}
	if v, ok := data["reward_credit"].(float64); ok {
		st.RewardCredit = v
	}
	return st, nil
}

// buddyActive 查询当前账号是否已有激活的 Buddy（猫猫）。
func buddyActive(ctx context.Context, auth *StoredAuth) (bool, error) {
	data, err := buddyActivityRequest(ctx, auth, http.MethodGet, buddyTravelBase+buddyInfoPath, nil)
	if err != nil {
		return false, err
	}
	buddy, exists := data["buddy"]
	if !exists {
		return false, fmt.Errorf("活动响应缺少 buddy 字段")
	}
	if buddy == nil {
		return false, nil
	}
	if _, ok := buddy.(map[string]any); ok {
		return true, nil
	}
	return false, fmt.Errorf("活动响应 buddy 字段异常")
}

// buddyTravelLocations 拉取可派遣地点列表（跳过无法解析的行）。
func buddyTravelLocations(ctx context.Context, auth *StoredAuth) ([]buddyLocation, error) {
	data, err := buddyActivityRequest(ctx, auth, http.MethodGet, buddyTravelBase+buddyTravelPrefix+"config", nil)
	if err != nil {
		return nil, err
	}
	rows, _ := data["locations"].([]any)
	locs := make([]buddyLocation, 0, len(rows))
	for _, rowAny := range rows {
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		id, idOK := row["id"].(float64)
		name, nameOK := row["name"].(string)
		if !idOK || id <= 0 || !nameOK || strings.TrimSpace(name) == "" {
			continue
		}
		locs = append(locs, buddyLocation{ID: int64(id), Name: name})
	}
	if len(locs) == 0 {
		return nil, fmt.Errorf("旅行地点配置为空")
	}
	return locs, nil
}

// performBuddyTravel 执行「查询状态 → 领取到达奖励 → 随机地点派遣」的完整旅行流程。
// 仅适用于国内站账号；调用方需已持有该账号的串行锁。
// 返回：ok（有实际领取/派遣动作）、skipped（无需动作或前置不满足）、failed（失败）。
func performBuddyTravel(ctx context.Context, acc *Account) (string, error) {
	accountMu.Lock()
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
		accountMu.Unlock()
		return "skipped", nil
	}
	auth := *acc.Auth
	accountMu.Unlock()

	st, err := buddyTravelStatus(ctx, &auth)
	if err != nil {
		return "failed", fmt.Errorf("查询旅行状态失败: %w", err)
	}
	if st.State == "arrived" {
		receipt, err := buddyActivityRequest(ctx, &auth, http.MethodPost, buddyTravelBase+buddyTravelPrefix+"claim", strings.NewReader("{}"))
		if err != nil {
			return "failed", fmt.Errorf("领取旅行奖励失败: %w", err)
		}
		credit := st.RewardCredit
		if v, ok := receipt["reward_credit"].(float64); ok {
			credit = v
		}
		log.Printf("[Travel] 账号 %s Buddy 已到达，旅行奖励领取成功（credit=%.2f）", acc.Path, credit)
		if st, err = buddyTravelStatus(ctx, &auth); err != nil {
			return "failed", fmt.Errorf("领取后查询旅行状态失败: %w", err)
		}
	}
	switch {
	case st.State == "traveling":
		log.Printf("[Travel] 账号 %s Buddy 旅行中，无需派遣", acc.Path)
		return "skipped", nil
	case st.DailyLimitReached != nil && *st.DailyLimitReached:
		log.Printf("[Travel] 账号 %s 今日派遣已达上限", acc.Path)
		return "skipped", nil
	case st.State != "idle":
		return "failed", fmt.Errorf("旅行状态异常: %s", st.State)
	}

	active, err := buddyActive(ctx, &auth)
	if err != nil {
		return "failed", fmt.Errorf("查询 Buddy 信息失败: %w", err)
	}
	if !active {
		log.Printf("[Travel] 账号 %s 尚未领取 Buddy，请先在官方成长中心领取（%s），领取后将自动派遣", acc.Path, growthCenterURL)
		return "skipped", nil
	}

	locs, err := buddyTravelLocations(ctx, &auth)
	if err != nil {
		return "failed", fmt.Errorf("查询旅行地点失败: %w", err)
	}
	loc := locs[rand.Intn(len(locs))]
	body, _ := json.Marshal(map[string]any{"location_id": loc.ID})
	if _, err := buddyActivityRequest(ctx, &auth, http.MethodPost, buddyTravelBase+buddyTravelPrefix+"depart", strings.NewReader(string(body))); err != nil {
		return "failed", fmt.Errorf("派遣 Buddy 失败: %w", err)
	}
	log.Printf("[Travel] 账号 %s Buddy 已派出，地点=%s（id=%d）", acc.Path, loc.Name, loc.ID)
	return "ok", nil
}
