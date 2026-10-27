package main

// restricted_test.go —— 上游风控限制（11140）处置的行为测试。
// 对应 2026-10-26 国际站 5 账号事故：旧逻辑 60s 冷却 + 换号重试持续送连坐证据。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const restricted11140Body = `{"code":11140,"msg":"request illegal","requestId":"t","displayMsg":{"en":"Your account has been restricted due to unusual activity","zh":"账号存在异常行为，已被限制使用"},"displayTips":{"en":"We detected unusual activity on this account. Submit feedback to appeal this restriction","zh":"系统检测到该账号存在异常使用行为，如需申诉请提交反馈"},"actions":["EDIT_INPUT","SUBMIT_FEEDBACK"]}`

// resetRestrictedTestState 保存/恢复受限状态全局量，避免测试间串扰。
// 开始前必须清零：上一个测试留下的站点命中事件会污染窗口计数。
func resetRestrictedTestState(t *testing.T) {
	t.Helper()
	oldEvents := restrictedWave.events
	oldHoldSite, oldHoldUntil := restrictedWave.holdSite, restrictedWave.holdUntil
	oldRotate := restricted403RotateMax
	restrictedWave.Lock()
	restrictedWave.events = map[string][]time.Time{}
	restrictedWave.holdSite, restrictedWave.holdUntil = "", time.Time{}
	restrictedWave.Unlock()
	t.Cleanup(func() {
		restrictedWave.Lock()
		restrictedWave.events = oldEvents
		restrictedWave.holdSite, restrictedWave.holdUntil = oldHoldSite, oldHoldUntil
		restrictedWave.Unlock()
		restricted403RotateMax = oldRotate
	})
}

// 分类必须独立于限流/额度/鉴权：11140 与其变体命中，其余一律不命中。
func TestIsRestrictedAccountClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"11140数字码", 403, restricted11140Body, true},
		{"简中文案", 403, `{"msg":"账号存在异常行为，已被限制使用"}`, true},
		{"繁中文案", 403, `{"msg":"帳號存在異常行為，已被限制使用"}`, true},
		{"英文文案", 403, `{"error":"Your account has been restricted due to unusual activity"}`, true},
		{"WAF拦截页", 403, `<html>Request blocked by EdgeOne WAF</html>`, false},
		{"模型限流6004", 200, `{"code":6004,"msg":"请切换其他模型"}`, false},
		{"额度耗尽14018", 429, `{"code":14018,"msg":"额度已用尽"}`, false},
		{"登录失效", 401, `{"msg":"登录已失效"}`, false},
	}
	for _, tc := range cases {
		if got := isRestrictedAccount(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: isRestrictedAccount=%v, 期望 %v", tc.name, got, tc.want)
		}
	}
}

// 命中即停：第一个账号撞 11140 后，同一请求绝不能换号再打（连坐红线）；
// 返回 429 + 申诉指引；账号不禁用、凭据保留，进入 2 小时受限窗口。
func TestRestricted403StopsRotationAndKeepsAccount(t *testing.T) {
	chdirTemp(t)
	resetRestrictedTestState(t)
	var bHits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, restricted11140Body)
			return
		}
		bHits.Add(1)
		retryWriteSSE(t, w)
	})
	setupRetryAccounts(t, retryAccount("a.json", "token-a"), retryAccount("b.json", "token-b"))

	rec := retryPostChat(t)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("命中 11140 应命中即停返回 429，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "account_restricted") || !strings.Contains(rec.Body.String(), "a.json") {
		t.Fatalf("错误体应含 account_restricted 与受限账号名: %s", rec.Body.String())
	}
	if n := bHits.Load(); n != 0 {
		t.Fatalf("命中即停：账号 b 绝不能被尝试，实际被尝试 %d 次", n)
	}
	accountMu.Lock()
	disabled, until := accounts[0].Disabled, accounts[0].RestrictedUntil
	accountMu.Unlock()
	if disabled {
		t.Fatalf("风控限制不是凭据失效，不应禁用账号")
	}
	if !until.After(time.Now()) {
		t.Fatalf("账号 a 应进入受限状态，RestrictedUntil=%v", until)
	}
}

// 整池熔断：同站点 15 分钟窗口内第 2 个账号被限 → 熔断生效；
// 熔断站点在选号层（含免费/预算轮）必须被完全跳过。
func TestSiteHoldAfterSecondRestriction(t *testing.T) {
	resetRestrictedTestState(t)
	if armed, _ := recordSiteRestrictedWave("cn"); armed {
		t.Fatalf("单次命中不应触发熔断")
	}
	if siteRestrictedHold("cn") {
		t.Fatalf("单次命中后不应有熔断")
	}
	armed, site := recordSiteRestrictedWave("cn")
	if !armed || site != "cn" {
		t.Fatalf("窗口内第二次命中应触发整池熔断，armed=%v site=%s", armed, site)
	}
	if !siteRestrictedHold("cn") {
		t.Fatalf("熔断应处于生效状态")
	}
	if siteRestrictedHold("intl") {
		t.Fatalf("熔断只应影响命中站点")
	}
	acc := retryAccount("a.json", "token-a")
	setupRetryAccounts(t, acc)
	if _, _, err := nextAccountForModel("m", map[*Account]bool{}, 0); err == nil {
		t.Fatalf("站点熔断期间不应选出该站点的账号")
	}
}

// 持久化：受限状态写入 wb-restricted-state.json，模拟重启（内存清零）后必须恢复。
func TestRestrictedStatePersistsAcrossRestart(t *testing.T) {
	chdirTemp(t)
	resetRestrictedTestState(t)
	acc := retryAccount("a.json", "token-a")
	setupRetryAccounts(t, acc)
	until := markAccountRestricted(acc, restricted11140Body)
	if !accountRestrictedLocked(acc) {
		t.Fatalf("标记后应处于受限状态")
	}
	accountMu.Lock()
	accounts[0].RestrictedUntil, accounts[0].RestrictedAt, accounts[0].RestrictedMsg = time.Time{}, time.Time{}, ""
	accountMu.Unlock()
	loadRestrictedState()
	if !accountRestrictedLocked(accounts[0]) {
		t.Fatalf("重启后受限状态应从 %s 恢复", restrictedStateFile)
	}
	accountMu.Lock()
	restored := accounts[0].RestrictedUntil
	accountMu.Unlock()
	if restored.Unix() != until.Unix() {
		t.Fatalf("恢复的解除时刻应一致: got %v want %v", restored, until)
	}
}

// 窗口不滚动：重复命中保留更晚的既有解除时刻，不把 2 小时窗口重置；
// 解除只能显式触发，重复清除返回 false。
func TestRestrictedWindowDoesNotRollAndClearRequiresExplicit(t *testing.T) {
	chdirTemp(t)
	resetRestrictedTestState(t)
	acc := retryAccount("a.json", "token-a")
	setupRetryAccounts(t, acc)
	markAccountRestricted(acc, restricted11140Body)
	accountMu.Lock()
	extended := time.Now().Add(5 * time.Hour)
	accounts[0].RestrictedUntil = extended
	accountMu.Unlock()
	markAccountRestricted(acc, restricted11140Body)
	accountMu.Lock()
	got := accounts[0].RestrictedUntil
	accountMu.Unlock()
	if !got.Equal(extended) {
		t.Fatalf("重复命中不应滚动重置窗口: got %v want %v", got, extended)
	}
	if !clearAccountRestricted(acc, "手动确认申诉通过") {
		t.Fatalf("手动清除应返回成功")
	}
	if accountRestrictedLocked(acc) {
		t.Fatalf("清除后不应再处于受限状态")
	}
	if clearAccountRestricted(acc, "重复清除") {
		t.Fatalf("无受限状态时清除应返回 false")
	}
}

// 后台任务跳过：受限账号不得被 warmup/价格探测选中。
func TestPickProbeAccountSkipsRestricted(t *testing.T) {
	resetRestrictedTestState(t)
	acc := retryAccount("a.json", "token-a")
	setupRetryAccounts(t, acc)
	if pickProbeAccountForModel("cn", "m") == nil {
		t.Fatalf("未受限账号应可选")
	}
	accountMu.Lock()
	accounts[0].RestrictedUntil = time.Now().Add(time.Hour)
	accountMu.Unlock()
	if got := pickProbeAccountForModel("cn", "m"); got != nil {
		t.Fatalf("受限账号不应被 warmup/探测选中")
	}
}

// 恢复通知 + 首发复验闭环：真实成功清除（含窗口已到期但记录仍在）必须发
// restricted 恢复通知；记录清零后的重复清除不再发。
func TestRecoverNotifyOnActualSuccess(t *testing.T) {
	chdirTemp(t)
	resetRestrictedTestState(t)
	resetNotifyState()
	ch := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		ch <- m
		w.WriteHeader(200)
	}))
	defer srv.Close()
	setNotify(notifyConfig{Enabled: true, Type: "webhook", Webhook: srv.URL,
		Events: []string{notifyEventRestricted}, MinIntervalSeconds: 0})
	defer setNotify(notifyConfig{})

	acc := retryAccount("a.json", "token-a")
	setupRetryAccounts(t, acc)
	// 窗口已到期、记录仍在：此刻真实成功=「上游确实解封」的复验信号，必须通知。
	accountMu.Lock()
	acc.RestrictedUntil = time.Now().Add(-time.Minute)
	acc.RestrictedAt = time.Now().Add(-2 * time.Hour)
	accountMu.Unlock()
	if !clearAccountRestricted(acc, "chat 请求真实成功") {
		t.Fatalf("到期未清的记录应在真实成功时清除并通知")
	}
	select {
	case m := <-ch:
		title, _ := m["title"].(string)
		if !strings.Contains(title, "解封") || m["kind"] != notifyEventRestricted {
			t.Fatalf("恢复通知字段不符: %v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("恢复通知未发出")
	}
	if clearAccountRestricted(acc, "重复清除") {
		t.Fatal("记录已清零时清除应返回 false")
	}
}

// Retry-After 加固：本地拒绝的 503/429 应带退避倒计时头（标准客户端可据此退避，
// 从源头减少无效重试风暴）。
func TestRetryAfterHeaders(t *testing.T) {
	chdirTemp(t)
	resetRestrictedTestState(t)

	// 场景1：唯一账号受限 → 503 no_available_account + Retry-After ≈2h
	acc := retryAccount("a.json", "token-a")
	setupRetryAccounts(t, acc)
	markAccountRestricted(acc, restricted11140Body)
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) { retryWriteSSE(t, w) })
	rec := retryPostChat(t)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	ra := rec.Header().Get("Retry-After")
	if n, err := strconv.Atoi(ra); err != nil || n < 7000 || n > 7201 {
		t.Fatalf("503 的 Retry-After 应≈7200s，实际 %q", ra)
	}

	// 场景2：上游直接返回 11140 → 429 account_restricted + Retry-After
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, restricted11140Body)
	})
	accountMu.Lock()
	acc.RestrictedUntil, acc.RestrictedAt, acc.RestrictedMsg = time.Time{}, time.Time{}, ""
	accountMu.Unlock()
	rec2 := retryPostChat(t)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("期望 429，实际 %d body=%s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatal("429 account_restricted 应带 Retry-After")
	}
}
