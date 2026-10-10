package main

// 国际站每日活跃（ACP 会话）与成长任务的测试。
//
// 背景：国际站不存在可用的 daily-checkin 接口 —— 旧实现只会拿到「已签到」文案
// 被 isAlreadyCheckedIn 误判为幂等成功（实测 5 天 0 次真签到）。现在 intl 改走
// ACP 真实会话，测试必须 mock 完整链路：
//
//   POST /console/as/conversations/             建会话
//   GET  /console/as/conversations/{id}/session  取沙箱 link+token
//   GET  <link>                                 SSE，响应头带 Acp-Connection-Id
//   POST <link> ×3                              initialize / session/load / session/prompt
//   GET  /console/as/conversations/{id}         轮询到 completed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// acpSSEPath 是假上游 SSE / JSON-RPC 端点的路径。
const acpSSEPath = "/acp-test-stream"

// acpSSEEndpoint 是沙箱 link 的绝对地址，由 setupFakeACP 填充。
var acpSSEEndpoint string

// fakeACP 是 ACP 链路的有状态假上游，统计各步骤调用次数供断言。
type fakeACP struct {
	mu sync.Mutex

	status     string // 会话轮询返回的终态
	created    int
	sessionHit int
	sseHit     int
	rpcHit     int
	statusHit  int
	rpcMethods []string

	// failSSE 为真时 SSE 不返回 Acp-Connection-Id，用于验证失败路径。
	failSSE bool
}

// conversationHits 返回建会话次数：用于断言「去重生效时确实没打上游」。
func (f *fakeACP) conversationHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// setupFakeACP 启动假上游并把国际站 Origin 指向它，测试结束自动还原。
func setupFakeACP(t *testing.T, fake *fakeACP) {
	t.Helper()
	fake.mu.Lock()
	if fake.status == "" {
		fake.status = "completed"
	}
	fake.mu.Unlock()

	server := httptest.NewServer(fake)
	acpSSEEndpoint = server.URL + acpSSEPath

	// 国际站活跃的「当日去重」名单默认落在 cwd，会让多个用例共用同一份状态：
	// 前一个用例标记成功后，后一个用例直接被去重跳过，断言必然失败。
	// 每个用例指向自己的临时文件，互相隔离。
	oldStateFile, oldDedupe := intlCheckinStateFile, intlCheckinDedupe
	intlCheckinStateFile = filepath.Join(t.TempDir(), "wb-intl-checkin.json")

	oldOrigin, oldClient := profileINTL.Origin, cfg.HttpClient
	profileINTL.Origin = server.URL
	cfg.HttpClient = server.Client()
	t.Cleanup(func() {
		profileINTL.Origin = oldOrigin
		cfg.HttpClient = oldClient
		intlCheckinStateFile, intlCheckinDedupe = oldStateFile, oldDedupe
		server.Close()
	})
}

func (f *fakeACP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == webConversationsPath && r.Method == http.MethodPost:
		f.mu.Lock()
		f.created++
		f.mu.Unlock()
		writeACPJSON(w, `{"code":0,"msg":"OK","data":{"id":"conv-test-1"}}`)

	case strings.HasSuffix(path, "/session") && r.Method == http.MethodGet:
		f.mu.Lock()
		f.sessionHit++
		f.mu.Unlock()
		writeACPJSON(w, `{"code":0,"msg":"OK","data":{"link":"`+acpSSEEndpoint+
			`","token":"tok-test","sessionId":"sess-test-1","cwd":"/workspace"}}`)

	case path == acpSSEPath && r.Method == http.MethodGet:
		f.mu.Lock()
		f.sseHit++
		fail := f.failSSE
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Acp-Connection-Id", "conn-test-1")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: {\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\"}}}\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()

	case path == acpSSEPath && r.Method == http.MethodPost:
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.rpcHit++
		f.rpcMethods = append(f.rpcMethods, req.Method)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)

	case strings.HasPrefix(path, webConversationsPath) && r.Method == http.MethodGet:
		f.mu.Lock()
		f.statusHit++
		status := f.status
		f.mu.Unlock()
		writeACPJSON(w, `{"code":0,"msg":"OK","data":{"status":"`+status+`"}}`)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeACPJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// 国际站签到应真正跑完一次 ACP 会话，而不是打 daily-checkin 后拿「已签到」当成功。
func TestCheckinIntlRunsACPConversation(t *testing.T) {
	fake := &fakeACP{status: "completed"}
	setupFakeACP(t, fake)
	travelSeedFlags(t, true, true, true)

	result, err := checkinAccount(context.Background(), travelSeedAccount(t, "intl"))
	if err != nil || result != "ok" {
		t.Fatalf("intl checkin 应成功: result=%q err=%v", result, err)
	}

	fake.mu.Lock()
	created, sessionHit, sseHit, rpcHit := fake.created, fake.sessionHit, fake.sseHit, fake.rpcHit
	methods := append([]string(nil), fake.rpcMethods...)
	fake.mu.Unlock()

	if created != 1 {
		t.Errorf("应建 1 条会话, 实际 %d", created)
	}
	if sessionHit != 1 {
		t.Errorf("应查 1 次沙箱, 实际 %d", sessionHit)
	}
	if sseHit != 1 {
		t.Errorf("应开 1 条 SSE 通道, 实际 %d", sseHit)
	}
	if rpcHit != 3 {
		t.Errorf("应发 3 个 JSON-RPC 请求, 实际 %d", rpcHit)
	}
	want := []string{"initialize", "session/load", "session/prompt"}
	if len(methods) != len(want) {
		t.Fatalf("JSON-RPC 方法序列=%v, want=%v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Errorf("第 %d 个方法=%q, want %q", i+1, methods[i], want[i])
		}
	}
}

// 沙箱没给出 Acp-Connection-Id 时应判定失败，而不是静默当成功。
func TestCheckinIntlFailsWithoutConnectionID(t *testing.T) {
	fake := &fakeACP{status: "completed", failSSE: true}
	setupFakeACP(t, fake)
	travelSeedFlags(t, true, true, true)

	result, err := checkinAccount(context.Background(), travelSeedAccount(t, "intl"))
	if result != "failed" || err == nil {
		t.Fatalf("SSE 缺少 Acp-Connection-Id 应失败: result=%q err=%v", result, err)
	}
}

// 会话终态为 failed 时应判定失败。
func TestCheckinIntlFailsWhenFailed(t *testing.T) {
	fake := &fakeACP{status: "failed"}
	setupFakeACP(t, fake)
	travelSeedFlags(t, true, true, true)

	result, err := checkinAccount(context.Background(), travelSeedAccount(t, "intl"))
	if result != "failed" || err == nil {
		t.Fatalf("会话状态 failed 应判定失败: result=%q err=%v", result, err)
	}
}

// 当日已成功活跃过，重复调用应被去重跳过，且不再请求上游。
// 背景：国际站活跃按次扣额度，网关每次重启都会重跑一轮；上游只回「今日已签」
// 的幂等文案、不会叫停，去重只能本地记账。这条用例锁住「不重复烧额度」的行为。
func TestCheckinIntlDedupesSameDay(t *testing.T) {
	fake := &fakeACP{status: "completed"}
	setupFakeACP(t, fake)
	travelSeedFlags(t, true, true, true)

	acc := travelSeedAccount(t, "intl")
	if result, err := checkinAccount(context.Background(), acc); result != "ok" || err != nil {
		t.Fatalf("首次活跃应成功: result=%q err=%v", result, err)
	}
	hitsAfterFirst := fake.conversationHits()

	// 第二次调用：模拟网关重启后重跑当日签到。
	result, err := checkinAccount(context.Background(), acc)
	if result != "already" || err != nil {
		t.Fatalf("当日重复活跃应被去重跳过: result=%q err=%v", result, err)
	}
	if got := fake.conversationHits(); got != hitsAfterFirst {
		t.Fatalf("去重后不应再请求上游: 建会话次数=%d, want %d", got, hitsAfterFirst)
	}
}

// 关掉去重开关后应恢复「每次都跑」的旧行为。
func TestCheckinIntlDedupeCanBeDisabled(t *testing.T) {
	fake := &fakeACP{status: "completed"}
	setupFakeACP(t, fake)
	travelSeedFlags(t, true, true, true)
	intlCheckinDedupe = false

	acc := travelSeedAccount(t, "intl")
	for i := 0; i < 2; i++ {
		if result, err := checkinAccount(context.Background(), acc); result != "ok" || err != nil {
			t.Fatalf("第 %d 次活跃应成功（去重已关闭）: result=%q err=%v", i+1, result, err)
		}
	}
	if got := fake.conversationHits(); got < 2 {
		t.Fatalf("关闭去重后每次都应跑会话: 建会话次数=%d, want >=2", got)
	}
}

// 关闭国际站签到时应跳过，且不建会话。
func TestCheckinIntlDisabledSkipsACP(t *testing.T) {
	fake := &fakeACP{status: "completed"}
	setupFakeACP(t, fake)
	travelSeedFlags(t, true, true, false) // intl 关闭

	result, err := checkinAccount(context.Background(), travelSeedAccount(t, "intl"))
	if result != "global_skipped" || err != nil {
		t.Fatalf("关闭国际站签到应跳过: result=%q err=%v", result, err)
	}
	fake.mu.Lock()
	created := fake.created
	fake.mu.Unlock()
	if created != 0 {
		t.Errorf("关闭后不应建会话, 实际 %d", created)
	}
}

// 国内站仍走 daily-checkin 接口，不受 ACP 改动影响。
func TestCheckinCNStillUsesDailyCheckin(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/daily-checkin" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer server.Close()

	oldOrigin, oldClient := profileCN.Origin, cfg.HttpClient
	profileCN.Origin = server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Origin = oldOrigin
		cfg.HttpClient = oldClient
	}()

	travelSeedFlags(t, true, false, true) // 关闭旅行，专注签到
	result, err := checkinAccount(context.Background(), travelSeedAccount(t, "cn"))
	if err != nil || result != "ok" || calls != 1 {
		t.Fatalf("cn checkin result=%q calls=%d err=%v", result, calls, err)
	}
}

// deriveDeviceID 必须稳定：同一 uid 每次派生出相同值（防风控抖动）。
func TestDeriveDeviceIDStableAndDistinct(t *testing.T) {
	a1 := deriveDeviceID("machine", "uid-1")
	if a1 != deriveDeviceID("machine", "uid-1") {
		t.Error("同一 uid 应派生稳定值")
	}
	if a1 == deriveDeviceID("machine", "uid-2") {
		t.Error("不同 uid 应派生不同值")
	}
	if a1 == deriveDeviceID("session", "uid-1") {
		t.Error("machine 与 session 盐值不同，结果应不同")
	}
	if len(a1) != 32 {
		t.Errorf("MD5 十六进制应为 32 位, 实际 %d", len(a1))
	}
	if deriveDeviceID("machine", "") == "" {
		t.Error("空 uid 也应返回非空标识")
	}
}

// 成长任务开关应能从 config.json 加载，省略字段恢复默认开启。
func TestRuntimeConfigLoadsGrowthFlag(t *testing.T) {
	old := cnGrowthEnabled
	t.Cleanup(func() { cnGrowthEnabled = old })

	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"checkin":{"cn":{"enabled":false,"travel":false,"growth":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if cnGrowthEnabled {
		t.Error("显式 false 应关闭成长任务")
	}

	if err := os.WriteFile(path, []byte(`{"checkin":{"cn":{"enabled":true,"travel":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if !cnGrowthEnabled {
		t.Error("省略 growth 字段应默认开启")
	}
}

// 专家/团队类事件必须使用互不相同的 id，否则上游按 (eventCode, id) 去重不推进进度。
func TestBuildGrowthEventExpertIDsDiffer(t *testing.T) {
	if len(expertIDPool) < 2 {
		t.Fatal("专家 id 池至少 2 个")
	}
	e1 := buildGrowthEvent("uid-1", "expert", 0, expertIDPool[0])
	e2 := buildGrowthEvent("uid-1", "expert", 1, expertIDPool[1])
	if e1["eventCode"] != "expert_actual_use" {
		t.Errorf("专家事件码=%v", e1["eventCode"])
	}
	if e1["id"] == e2["id"] {
		t.Error("相邻两次专家事件的 id 必须不同")
	}
	if e1["timestamp"] == nil || e1["userId"] != "uid-1" {
		t.Errorf("事件缺少必要字段: %v", e1)
	}
	if tm := buildGrowthEvent("uid-1", "team", 0, teamIDPool[0]); tm["expertType"] != "team" {
		t.Errorf("团队事件 expertType=%v", tm["expertType"])
	}
	// 未知 kind 回退 heartbeat，不应 panic。
	if hb := buildGrowthEvent("uid-1", "unknown", 0, [2]string{}); hb["eventCode"] != "heartbeat" {
		t.Errorf("未知 kind 应回退 heartbeat, 实际 %v", hb["eventCode"])
	}
}

// 桌面专属任务必须标记为需真实操作，不参与伪造；公益捐款不可伪造。
func TestUnforgeableTasksDeclared(t *testing.T) {
	for _, code := range []string{"RichMeow_Chat", "Library_read", "Buddy_App", "Buddy_App_QQ"} {
		if _, ok := desktopOnlyTasks[code]; !ok {
			t.Errorf("%s 应标记为桌面专属任务", code)
		}
	}
	if !taskSpecs["Expert_Philanthropy"].Unforgeable {
		t.Error("公益爱心捐赠应标记为不可伪造")
	}
}
