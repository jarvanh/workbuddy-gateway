package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// travelSeedFlags 覆盖签到与旅行开关，测试结束自动还原。
func travelSeedFlags(t *testing.T, cn, travel, intl bool) {
	t.Helper()
	oldCN, oldTravel, oldIntl := cnCheckinEnabled, cnTravelEnabled, intlCheckinEnabled
	cnCheckinEnabled, cnTravelEnabled, intlCheckinEnabled = cn, travel, intl
	t.Cleanup(func() {
		cnCheckinEnabled, cnTravelEnabled, intlCheckinEnabled = oldCN, oldTravel, oldIntl
	})
}

// travelSeedAccount 注入单个临时账号，测试结束自动还原账号池。
func travelSeedAccount(t *testing.T, edition string) *Account {
	t.Helper()
	acc := &Account{Path: edition + ".json", Auth: &StoredAuth{
		Edition: edition,
		Auth:    StoredTokens{AccessToken: "test-access"},
		Account: StoredAccount{UID: "user-1"},
	}}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	return acc
}

// fakeBuddyTravel 是 Buddy 活动接口的有状态假上游。
// state 字段决定 /status 的返回；claim 后到达奖励被领取、状态转为 idle；
// depart 后状态转为 traveling 并记录派遣请求体。
type fakeBuddyTravel struct {
	mu           sync.Mutex
	state        string // idle | traveling | arrived
	dailyLimit   bool
	buddyActive  bool
	statusHits   int
	claimHits    int
	departHits   int
	configHits   int
	infoHits     int
	departBodies []string
}

func (f *fakeBuddyTravel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/activity/growth/buddy/travel/status" && r.Method == http.MethodGet:
		f.statusHits++
		switch f.state {
		case "arrived":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"arrived","location":{"id":7,"name":"海边"},"reward_credit":0.5}}`))
		case "traveling":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"traveling"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"idle","daily_limit_reached":` + boolJSON(f.dailyLimit) + `}}`))
		}
	case r.URL.Path == "/activity/growth/buddy/travel/claim" && r.Method == http.MethodPost:
		f.claimHits++
		f.state = "idle"
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"reward_credit":0.5}}`))
	case r.URL.Path == "/activity/growth/buddy/travel/depart" && r.Method == http.MethodPost:
		f.departHits++
		raw, _ := io.ReadAll(r.Body)
		f.departBodies = append(f.departBodies, string(raw))
		f.state = "traveling"
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"arrive_at":1737000000}}`))
	case r.URL.Path == "/activity/growth/buddy/travel/config" && r.Method == http.MethodGet:
		f.configHits++
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"locations":[{"id":1,"name":"森林"},{"id":2,"name":"雪山"}]}}`))
	case r.URL.Path == "/activity/growth/buddy/info" && r.Method == http.MethodGet:
		f.infoHits++
		if f.buddyActive {
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"buddy":{"instance_id":"42"}}}`))
		} else {
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"buddy":null}}`))
		}
	default:
		_, _ = w.Write([]byte(`{"code":-1,"msg":"unknown operation"}`))
	}
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// travelSetup 将旅行活动接口指向假上游并还原。
func travelSetup(t *testing.T, fake *fakeBuddyTravel) {
	t.Helper()
	server := httptest.NewServer(fake)
	oldBase := buddyTravelBase
	oldClient := cfg.HttpClient
	buddyTravelBase = server.URL
	cfg.HttpClient = server.Client()
	t.Cleanup(func() {
		buddyTravelBase = oldBase
		cfg.HttpClient = oldClient
		server.Close()
	})
}

// 到达 → 领取奖励 → 派遣随机地点 的完整流程。
func TestPerformBuddyTravelClaimAndDepart(t *testing.T) {
	fake := &fakeBuddyTravel{state: "arrived", buddyActive: true}
	travelSetup(t, fake)
	acc := travelSeedAccount(t, "cn")

	result, err := performBuddyTravel(context.Background(), acc)
	if err != nil || result != "ok" {
		t.Fatalf("完整旅行流程应成功, result=%q err=%v", result, err)
	}
	if fake.claimHits != 1 {
		t.Fatalf("到达状态应领取 1 次奖励，实际 %d", fake.claimHits)
	}
	if fake.departHits != 1 {
		t.Fatalf("空闲状态应派遣 1 次，实际 %d", fake.departHits)
	}
	if fake.configHits != 1 {
		t.Fatalf("派遣前应查询地点配置 1 次，实际 %d", fake.configHits)
	}
	if len(fake.departBodies) != 1 {
		t.Fatalf("应发送 1 次派遣请求，实际 %d", len(fake.departBodies))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(fake.departBodies[0]), &body); err != nil {
		t.Fatalf("派遣请求体应为 JSON: %v", err)
	}
	locID, _ := body["location_id"].(float64)
	if locID != 1 && locID != 2 {
		t.Fatalf("派遣地点应来自配置列表 {1,2}，实际 %v", body["location_id"])
	}
}

// 旅行中：无需领取与派遣。
func TestPerformBuddyTravelSkipsWhenTraveling(t *testing.T) {
	fake := &fakeBuddyTravel{state: "traveling", buddyActive: true}
	travelSetup(t, fake)
	acc := travelSeedAccount(t, "cn")

	result, err := performBuddyTravel(context.Background(), acc)
	if err != nil || result != "skipped" {
		t.Fatalf("旅行中应跳过, result=%q err=%v", result, err)
	}
	if fake.statusHits != 1 || fake.claimHits != 0 || fake.departHits != 0 {
		t.Fatalf("旅行中只应查询状态一次, status=%d claim=%d depart=%d", fake.statusHits, fake.claimHits, fake.departHits)
	}
}

// 今日派遣已达上限：不派遣。
func TestPerformBuddyTravelSkipsOnDailyLimit(t *testing.T) {
	fake := &fakeBuddyTravel{state: "idle", dailyLimit: true, buddyActive: true}
	travelSetup(t, fake)
	acc := travelSeedAccount(t, "cn")

	result, err := performBuddyTravel(context.Background(), acc)
	if err != nil || result != "skipped" {
		t.Fatalf("达到派遣上限应跳过, result=%q err=%v", result, err)
	}
	if fake.departHits != 0 || fake.configHits != 0 {
		t.Fatalf("达到上限不应查询地点或派遣, config=%d depart=%d", fake.configHits, fake.departHits)
	}
}

// 尚未领取 Buddy：跳过并提示，不派遣。
func TestPerformBuddyTravelSkipsWithoutBuddy(t *testing.T) {
	fake := &fakeBuddyTravel{state: "idle", buddyActive: false}
	travelSetup(t, fake)
	acc := travelSeedAccount(t, "cn")

	result, err := performBuddyTravel(context.Background(), acc)
	if err != nil || result != "skipped" {
		t.Fatalf("未领取 Buddy 应跳过, result=%q err=%v", result, err)
	}
	if fake.infoHits != 1 || fake.configHits != 0 || fake.departHits != 0 {
		t.Fatalf("未领取 Buddy 只应查询 info, info=%d config=%d depart=%d", fake.infoHits, fake.configHits, fake.departHits)
	}
}

// checkin 段开关：国内站签到后旅行可独立关闭；国际站签到可独立关闭。
func TestCheckinFlagsControlAutomation(t *testing.T) {
	fake := &fakeBuddyTravel{state: "idle", buddyActive: true}
	travelSetup(t, fake)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			t.Errorf("unexpected checkin path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	t.Cleanup(server.Close)

	oldCNOrigin, oldIntlOrigin := profileCN.Origin, profileINTL.Origin
	oldClient := cfg.HttpClient
	profileCN.Origin = server.URL
	profileINTL.Origin = server.URL
	cfg.HttpClient = server.Client()
	t.Cleanup(func() {
		profileCN.Origin = oldCNOrigin
		profileINTL.Origin = oldIntlOrigin
		cfg.HttpClient = oldClient
	})

	// A: 全开 → 国内站签到成功且旅行执行
	travelSeedFlags(t, true, true, true)
	cn := travelSeedAccount(t, "cn")
	if result, err := checkinAccount(context.Background(), cn); err != nil || result != "ok" {
		t.Fatalf("cn checkin result=%q err=%v", result, err)
	}
	if fake.statusHits != 1 || fake.departHits != 1 {
		t.Fatalf("旅行应执行, status=%d depart=%d", fake.statusHits, fake.departHits)
	}

	// B: cn.travel=false → 签到照常，旅行不执行
	travelSeedFlags(t, true, false, true)
	fake.mu.Lock()
	fake.state, fake.statusHits, fake.departHits = "idle", 0, 0
	fake.mu.Unlock()
	if result, err := checkinAccount(context.Background(), cn); err != nil || result != "ok" {
		t.Fatalf("cn checkin result=%q err=%v", result, err)
	}
	if fake.statusHits != 0 || fake.departHits != 0 {
		t.Fatalf("关闭 travel 后不应执行旅行, status=%d depart=%d", fake.statusHits, fake.departHits)
	}

	// C: cn.enabled=false → 国内站跳过
	travelSeedFlags(t, false, true, true)
	if result, err := checkinAccount(context.Background(), cn); result != "global_skipped" || err != nil {
		t.Fatalf("关闭国内站签到应跳过, result=%q err=%v", result, err)
	}

	// D: intl.enabled=false → 国际站跳过
	travelSeedFlags(t, true, true, false)
	intl := travelSeedAccount(t, "intl")
	if result, err := checkinAccount(context.Background(), intl); result != "global_skipped" || err != nil {
		t.Fatalf("关闭国际站签到应跳过, result=%q err=%v", result, err)
	}

	// E: 国际站默认开启签到，但不触发旅行
	travelSeedFlags(t, true, true, true)
	fake.mu.Lock()
	fake.statusHits, fake.departHits = 0, 0
	fake.mu.Unlock()
	if result, err := checkinAccount(context.Background(), intl); err != nil || result != "ok" {
		t.Fatalf("intl checkin result=%q err=%v", result, err)
	}
	if fake.statusHits != 0 || fake.departHits != 0 {
		t.Fatalf("国际站账号不应触发旅行, status=%d depart=%d", fake.statusHits, fake.departHits)
	}
}

// config.json 的 checkin 段应能正确加载（省略恢复默认开启，显式 false 关闭）。
func TestRuntimeConfigLoadsCheckinFlags(t *testing.T) {
	travelSeedFlags(t, false, false, false)
	dir := t.TempDir()
	path := dir + "/config.json"

	if err := os.WriteFile(path, []byte(`{"checkin":{"cn":{"enabled":false,"travel":false},"intl":{"enabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if cnCheckinEnabled || cnTravelEnabled || intlCheckinEnabled {
		t.Fatalf("显式 false 应全部关闭: cn=%v travel=%v intl=%v", cnCheckinEnabled, cnTravelEnabled, intlCheckinEnabled)
	}

	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if !cnCheckinEnabled || !cnTravelEnabled || !intlCheckinEnabled {
		t.Fatalf("省略 checkin 段应恢复默认开启: cn=%v travel=%v intl=%v", cnCheckinEnabled, cnTravelEnabled, intlCheckinEnabled)
	}
}
