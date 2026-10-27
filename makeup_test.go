package main

// 断登自动补签测试：httptest 假上游，零真实网络。
// 契约对齐 88lin/workbuddy-auto-signin 2026-10-01 修复后的补登实现（245f313）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
)

// makeupFake 是补签三接口（/streak、/heatmap、/makeup-cards/use）的有状态假上游。
// status 为 0 表示返回 200。
type makeupFake struct {
	mu           sync.Mutex
	streakStatus int
	streakBody   string
	heatStatus   int
	heatBody     string
	useStatus    int
	useBody      string
	useDates     []string
	streakHits   int
	heatHits     int
	useHits      int
}

func (f *makeupFake) body(status int, payload string) string {
	if payload == "" {
		payload = `{"code":0,"msg":"OK","data":{}}`
	}
	return payload
}

func (f *makeupFake) install(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == makeupStreakPath:
			f.streakHits++
			if f.streakStatus != 0 {
				w.WriteHeader(f.streakStatus)
			}
			_, _ = w.Write([]byte(f.body(f.streakStatus, f.streakBody)))
		case r.Method == http.MethodGet && r.URL.Path == makeupHeatmapPath:
			f.heatHits++
			if f.heatStatus != 0 {
				w.WriteHeader(f.heatStatus)
			}
			_, _ = w.Write([]byte(f.body(f.heatStatus, f.heatBody)))
		case r.Method == http.MethodPost && r.URL.Path == makeupUsePath:
			f.useHits++
			var req struct {
				TargetDate string `json:"target_date"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.useDates = append(f.useDates, req.TargetDate)
			if f.useStatus != 0 {
				w.WriteHeader(f.useStatus)
			}
			_, _ = w.Write([]byte(f.body(f.useStatus, f.useBody)))
		default:
			t.Errorf("意外的上游请求: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	oldBase, oldClient := profileCN.Base, cfg.HttpClient
	profileCN.Base, cfg.HttpClient = server.URL, server.Client()
	t.Cleanup(func() {
		profileCN.Base, cfg.HttpClient = oldBase, oldClient
		server.Close()
	})
}

// makeupSeedStreak 构造 /streak 响应：上线日 2026-06-01、卡余额 2、已补登 10-04。
func makeupSeedStreak(makeupDates []string, cards int) string {
	dates, _ := json.Marshal(makeupDates)
	return `{"code":0,"msg":"OK","data":{"launch_date":"2026-06-01","streak":{"current":5,"makeup_dates":` +
		string(dates) + `},"makeup_cards":{"balance":` + strconv.Itoa(cards) + `}}}`
}

// makeupSeedHeatmap 构造 /heatmap 响应：服务端日期 2026-10-27 与给定的零分/ nonzero 格子。
func makeupSeedHeatmap(cells string) string {
	return `{"code":0,"msg":"OK","data":{"today":{"date":"2026-10-27"},"cells":[` + cells + `]}}`
}

// makeupParseData 从包络 JSON 里取 data 段：makeupStreakData / makeupHeatmapData
// 描述的是 data 内层结构，直接解包络会得到空结构。
func makeupParseData(t *testing.T, envelope string) json.RawMessage {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(envelope), &env); err != nil {
		t.Fatal(err)
	}
	return env.Data
}

func makeupRun(t *testing.T) {
	t.Helper()
	auth := &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test-access"}, Account: StoredAccount{UID: "u1"}}
	runMakeupCards(context.Background(), auth, &profileCN, "makeup-test.json")
}

// 正常路径：零分断点里排除已补登/上月/活动前/今天的日期，最近的断点先补，每轮 1 张。
func TestMakeupHappyPath(t *testing.T) {
	var fake makeupFake
	fake.streakBody = makeupSeedStreak([]string{"2026-10-04"}, 2)
	fake.heatBody = makeupSeedHeatmap(
		`{"date":"2026-10-03","score":0},` +
			`{"date":"2026-10-04","score":0},` +
			`{"date":"2026-10-05","score":10},` +
			`{"date":"2026-09-20","score":0},` +
			`{"date":"2026-06-10","score":0},` +
			`{"date":"2026-10-10","score":0}`)
	fake.useBody = `{"code":0,"msg":"OK","data":{"makeup_cards":{"balance":1}}}`
	fake.install(t)

	makeupRun(t)

	if fake.useHits != 1 {
		t.Fatalf("每轮最多补 1 张，实际 use 调用 %d 次", fake.useHits)
	}
	if len(fake.useDates) != 1 || fake.useDates[0] != "2026-10-10" {
		t.Fatalf("应补最近断点 2026-10-10，实际 %v", fake.useDates)
	}
	if fake.heatHits != 1 || fake.streakHits != 1 {
		t.Fatalf("查询次数异常 streak=%d heatmap=%d", fake.streakHits, fake.heatHits)
	}
}

// 无卡时只查连登状态，不查日历、不提交。
func TestMakeupNoCardsSkipsHeatmap(t *testing.T) {
	var fake makeupFake
	fake.streakBody = makeupSeedStreak(nil, 0)
	fake.heatBody = makeupSeedHeatmap(`{"date":"2026-10-03","score":0}`)
	fake.install(t)

	makeupRun(t)

	if fake.heatHits != 0 || fake.useHits != 0 {
		t.Fatalf("无卡不应继续查询/提交: heatmap=%d use=%d", fake.heatHits, fake.useHits)
	}
}

// 连登状态查询失败（HTTP/业务码）时整段停止，不花卡。
func TestMakeupStreakFailureStops(t *testing.T) {
	for name, setup := range map[string]func(f *makeupFake){
		"http500":   func(f *makeupFake) { f.streakStatus = 500 },
		"bizcode":   func(f *makeupFake) { f.streakBody = `{"code":50003,"msg":"boom"}` },
		"badauth":   func(f *makeupFake) { f.streakStatus = 401 },
		"badstruct": func(f *makeupFake) { f.streakBody = `{"code":0,"msg":"OK","data":"not-object"}` },
	} {
		t.Run(name, func(t *testing.T) {
			var fake makeupFake
			fake.heatBody = makeupSeedHeatmap(`{"date":"2026-10-03","score":0}`)
			setup(&fake)
			fake.install(t)

			makeupRun(t)

			if fake.heatHits != 0 || fake.useHits != 0 {
				t.Fatalf("%s: 前置失败不应继续: heatmap=%d use=%d", name, fake.heatHits, fake.useHits)
			}
		})
	}
}

// 日历查询失败或结构异常时不花卡；日历缺 today/分数非数字都按结构异常处理。
func TestMakeupHeatmapFailureOrBadStructStops(t *testing.T) {
	for name, setup := range map[string]func(f *makeupFake){
		"http500": func(f *makeupFake) { f.heatStatus = 500 },
		"bizcode": func(f *makeupFake) { f.heatBody = `{"code":1,"msg":"boom"}` },
		"no-today": func(f *makeupFake) {
			f.heatBody = `{"code":0,"msg":"OK","data":{"cells":[{"date":"2026-10-03","score":0}]}}`
		},
		"no-cells":  func(f *makeupFake) { f.heatBody = `{"code":0,"msg":"OK","data":{"today":{"date":"2026-10-27"}}}` },
		"score-str": func(f *makeupFake) { f.heatBody = makeupSeedHeatmap(`{"date":"2026-10-03","score":"0"}`) },
		"score-neg": func(f *makeupFake) { f.heatBody = makeupSeedHeatmap(`{"date":"2026-10-03","score":-1}`) },
		"date-bad":  func(f *makeupFake) { f.heatBody = makeupSeedHeatmap(`{"date":"2026-13-03","score":0}`) },
		"cell-dup": func(f *makeupFake) {
			f.heatBody = makeupSeedHeatmap(`{"date":"2026-10-03","score":0},{"date":"2026-10-03","score":5}`)
		},
		"launch-miss": func(f *makeupFake) {
			f.streakBody = `{"code":0,"msg":"OK","data":{"streak":{"makeup_dates":[]},"makeup_cards":2}}`
		},
		"streak-miss": func(f *makeupFake) {
			f.streakBody = `{"code":0,"msg":"OK","data":{"launch_date":"2026-06-01","makeup_cards":2}}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			var fake makeupFake
			fake.streakBody = makeupSeedStreak(nil, 2)
			setup(&fake)
			fake.install(t)

			makeupRun(t)

			if fake.useHits != 0 {
				t.Fatalf("%s: 结构异常不应消耗补登卡", name)
			}
		})
	}
}

// 400 +「date is not broken」是查询后被补上的正常竞争：不算失败，仅记录。
func TestMakeupBenignConflict(t *testing.T) {
	var fake makeupFake
	fake.streakBody = makeupSeedStreak(nil, 1)
	fake.heatBody = makeupSeedHeatmap(`{"date":"2026-10-03","score":0}`)
	fake.useStatus = http.StatusBadRequest
	fake.useBody = `{"code":400,"msg":"date is not broken, no makeup needed"}`
	fake.install(t)

	makeupRun(t)

	if fake.useHits != 1 {
		t.Fatalf("良性竞争应有且仅有一次提交，实际 %d", fake.useHits)
	}
}

// 余额形状兼容：{"balance":2}、2、"3"、null/缺失；非法形状报错。
func TestMakeupCardBalanceShapes(t *testing.T) {
	for tc, want := range map[string]int{
		`{"balance":2}`: 2, `2`: 2, `"3"`: 3, `null`: 0, ``: 0,
	} {
		got, err := makeupCardBalance([]byte(tc))
		if err != nil || got != want {
			t.Fatalf("makeupCardBalance(%s)=%d,%v want %d", tc, got, err, want)
		}
	}
	for _, bad := range []string{`{"balance":-1}`, `{"balance":"x"}`, `{"balance":1.5}`, `[1]`} {
		if _, err := makeupCardBalance([]byte(bad)); err == nil {
			t.Fatalf("makeupCardBalance(%s) 应报错", bad)
		}
	}
}

// 候选窗口：当月、今天之前、max(月初, launch, 2026-06-17) 之后；最近断点优先。
func TestMakeupCandidatesWindow(t *testing.T) {
	base := func() (makeupStreakData, makeupHeatmapData) {
		var s makeupStreakData
		if err := json.Unmarshal(makeupParseData(t, makeupSeedStreak(nil, 2)), &s); err != nil {
			t.Fatal(err)
		}
		if s.Streak == nil {
			s.Streak = &makeupStreak{}
		}
		var h makeupHeatmapData
		if err := json.Unmarshal(makeupParseData(t, makeupSeedHeatmap(
			`{"date":"2026-10-03","score":0},{"date":"2026-10-10","score":0}`)), &h); err != nil {
			t.Fatal(err)
		}
		return s, h
	}

	s, h := base()
	s.LaunchDate = "2026-10-15" // launch 晚于月初：10-03 排除，10-16 保留
	if err := json.Unmarshal(makeupParseData(t, makeupSeedHeatmap(
		`{"date":"2026-10-03","score":0},{"date":"2026-10-16","score":0}`)), &h); err != nil {
		t.Fatal(err)
	}
	got, err := makeupCandidates(s, h)
	if err != nil || len(got) != 1 || got[0] != "2026-10-16" {
		t.Fatalf("launch 窗口异常: %v %v", got, err)
	}

	// floor 2026-06-17 生效：today 必须落在六月，否则「当月月初」晚于 floor 测不到下界。
	s, h = base()
	s.LaunchDate = "2026-01-01"
	if err := json.Unmarshal(makeupParseData(t, `{"code":0,"msg":"OK","data":{"today":{"date":"2026-06-20"},"cells":[`+
		`{"date":"2026-06-16","score":0},{"date":"2026-06-18","score":0}]}}`), &h); err != nil {
		t.Fatal(err)
	}
	got, err = makeupCandidates(s, h)
	if err != nil || len(got) != 1 || got[0] != "2026-06-18" {
		t.Fatalf("floor 窗口异常: %v %v", got, err)
	}

	// 排序：最近断点在前。
	s, h = base()
	got, err = makeupCandidates(s, h)
	if err != nil || len(got) != 2 || got[0] != "2026-10-10" || got[1] != "2026-10-03" {
		t.Fatalf("候选应最近优先: %v %v", got, err)
	}
}

// 配置加载：checkin.cn.makeup 省略恢复默认开启，显式 false 关闭。
func TestRuntimeConfigLoadsMakeupFlag(t *testing.T) {
	old := cnMakeupEnabled
	t.Cleanup(func() { cnMakeupEnabled = old })

	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"checkin":{"cn":{"enabled":false,"travel":false,"growth":false,"makeup":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if cnMakeupEnabled {
		t.Error("显式 false 应关闭断登补签")
	}

	if err := os.WriteFile(path, []byte(`{"checkin":{"cn":{"enabled":true,"travel":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if !cnMakeupEnabled {
		t.Error("省略 makeup 字段应默认开启")
	}
}
