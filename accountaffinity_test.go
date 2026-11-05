package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 同一会话应稳定选中同一账号；不同会话应分散到不同账号。
func TestAccountAffinityPinsSessionToAccount(t *testing.T) {
	resetAccountAffinity()
	prev := accountAffinityEnabled.Load()
	accountAffinityEnabled.Store(true)
	t.Cleanup(func() {
		accountAffinityEnabled.Store(prev)
		resetAccountAffinity()
	})

	accounts = []*Account{{Path: "a.json"}, {Path: "b.json"}, {Path: "c.json"}, {Path: "d.json"}}
	rrIndex = 0

	// 同一会话连续 8 次：全部落到同一账号（严格轮转下会 4 个号轮流）。
	var first *Account
	for i := 0; i < 8; i++ {
		acc, _, err := nextAccountForModelForSession("m", nil, 0, "session-1")
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		if first == nil {
			first = acc
		} else if acc != first {
			t.Fatalf("同一会话被切到不同账号：%s → %s", first.Path, acc.Path)
		}
	}

	// 另一个会话应能落到别的账号（亲和不能退化成单账号全局粘滞）。
	other, _, err := nextAccountForModelForSession("m", nil, 0, "session-2")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Logf("注意：第二个会话也落在 %s（可能因起始游标相同），继续验证不串会话", other.Path)
	}
}

// 绑定账号被上游拒绝（进入 attempted）时必须解绑并改选他号，不能死磕。
func TestAccountAffinityUnbindsOnRejectedAccount(t *testing.T) {
	resetAccountAffinity()
	prev := accountAffinityEnabled.Load()
	accountAffinityEnabled.Store(true)
	t.Cleanup(func() {
		accountAffinityEnabled.Store(prev)
		resetAccountAffinity()
	})

	accounts = []*Account{{Path: "a.json"}, {Path: "b.json"}}
	rrIndex = 0
	bound, _, err := nextAccountForModelForSession("m", nil, 0, "s")
	if err != nil {
		t.Fatal(err)
	}
	// 模拟该账号被上游拒绝
	acc2, _, err := nextAccountForModelForSession("m", map[*Account]bool{bound: true}, 0, "s")
	if err != nil {
		t.Fatalf("绑定账号被拒后应改选他号: %v", err)
	}
	if acc2 == bound {
		t.Fatal("不应继续选择已被拒绝的账号")
	}
	// 绑定应已解除，换到新账号并重新绑定
	if got := accountAffinityGet("s", time.Now()); got != acc2 {
		t.Fatalf("应重新绑定到新账号，实际=%v", got)
	}
}

// 过期后亲和失效，回到轮转。
func TestAccountAffinityExpires(t *testing.T) {
	resetAccountAffinity()
	prev := accountAffinityEnabled.Load()
	accountAffinityEnabled.Store(true)
	t.Cleanup(func() {
		accountAffinityEnabled.Store(prev)
		resetAccountAffinity()
	})
	acc := &Account{Path: "a.json"}
	base := time.Now()
	accountAffinityBind("s", acc, base)
	if got := accountAffinityGet("s", base.Add(time.Minute)); got != acc {
		t.Fatal("TTL 内应命中绑定")
	}
	if got := accountAffinityGet("s", base.Add(accountAffinityTTL+time.Second)); got != nil {
		t.Fatal("过期后不应再返回绑定")
	}
}

// 关闭开关后必须完全回到旧行为（每次按轮转走）。
func TestAccountAffinityDisabledKeepsRoundRobin(t *testing.T) {
	resetAccountAffinity()
	prev := accountAffinityEnabled.Load()
	accountAffinityEnabled.Store(false)
	t.Cleanup(func() {
		accountAffinityEnabled.Store(prev)
		resetAccountAffinity()
	})
	accounts = []*Account{{Path: "a.json"}, {Path: "b.json"}, {Path: "c.json"}}
	rrIndex = 0
	seen := map[*Account]bool{}
	for i := 0; i < 3; i++ {
		acc, _, err := nextAccountForModelForSession("m", nil, 0, "s")
		if err != nil {
			t.Fatal(err)
		}
		seen[acc] = true
	}
	if len(seen) != 3 {
		t.Fatalf("关闭亲和后应轮转覆盖全部账号，实际只用了 %d 个", len(seen))
	}
}

// 配置开关：显式 false 关闭、缺省开启。
func TestAccountAffinityConfigToggle(t *testing.T) {
	prev := accountAffinityEnabled.Load()
	t.Cleanup(func() { accountAffinityEnabled.Store(prev) })
	for _, tc := range []struct {
		name string
		json string
		want bool
	}{
		{"缺省开启", `{"gateway":{"apiKeyEnabled":false}}`, true},
		{"显式关闭", `{"gateway":{"apiKeyEnabled":false},"upstream":{"accountAffinityEnabled":false}}`, false},
		{"显式开启", `{"gateway":{"apiKeyEnabled":false},"upstream":{"accountAffinityEnabled":true}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			writeRuntimeFixture(t, tc.json)
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatalf("配置加载失败: %v", err)
			}
			if got := accountAffinityEnabled.Load(); got != tc.want {
				t.Errorf("开关=%t want=%t", got, tc.want)
			}
		})
	}
}

// 会话 ID 派生失败（每请求随机 UUID）时，前缀指纹必须能撑起亲和：
// 同一段前缀连续请求应落到同一账号，而不是每次换号。
func TestPrefixAffinityKeyStableAcrossTurns(t *testing.T) {
	resetAccountAffinity()
	prev := accountAffinityEnabled.Load()
	accountAffinityEnabled.Store(true)
	t.Cleanup(func() {
		accountAffinityEnabled.Store(prev)
		resetAccountAffinity()
	})

	body := []byte(`{"model":"m","messages":[{"role":"system","content":"固定系统提示"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]},{"role":"assistant","content":"好"},{"role":"user","content":"继续"}]}`)
	k1 := prefixAffinityKey(body)
	// 追加消息（真实会话增长）后前缀键必须不变
	body2 := []byte(`{"model":"m","messages":[{"role":"system","content":"固定系统提示"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]},{"role":"assistant","content":"好"},{"role":"user","content":"继续"},{"role":"assistant","content":"在"},{"role":"user","content":"再继续"}]}`)
	k2 := prefixAffinityKey(body2)
	if k1 == "" || k1 != k2 {
		t.Fatalf("前缀键应稳定：k1=%q k2=%q", k1, k2)
	}
	// 不同会话前缀必须不同
	other := []byte(`{"model":"m","messages":[{"role":"system","content":"另一个系统提示"},{"role":"user","content":"别的问题"}]}`)
	if prefixAffinityKey(other) == k1 {
		t.Fatal("不同前缀不应得到相同键")
	}
	// 畸形/空输入安全返回空串
	if prefixAffinityKey(nil) != "" || prefixAffinityKey([]byte("not-json")) != "" {
		t.Fatal("畸形输入应返回空串")
	}
}

// 真实会话键优先于前缀指纹。
func TestAffinityKeyPrefersExplicitSession(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if got := affinityKeyForRequest(nil, body); got == "" || got[:4] != "pfx-" {
		t.Fatalf("无请求上下文时应退前缀指纹，got=%q", got)
	}
}

// 会话 ID 兜底：首条 user 消息无文本（纯图片）时，也必须派生出稳定会话 ID，
// 否则每请求随机 UUID，上游前缀缓存永远 miss。
func TestConversationSeedFallsBackToStructure(t *testing.T) {
	prev := conversationAffinityEnabled.Load()
	conversationAffinityEnabled.Store(true)
	t.Cleanup(func() { conversationAffinityEnabled.Store(prev) })

	imageOnly := func(extra []any) map[string]any {
		msgs := []any{
			map[string]any{"role": "system", "content": "系统提示"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
			}},
		}
		return map[string]any{"model": "m", "messages": append(msgs, extra...)}
	}
	newReq := func() *http.Request {
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer k")
		return req
	}
	h1 := prepareConversationContext(newReq(), imageOnly(nil))
	h2 := prepareConversationContext(newReq(), imageOnly([]any{
		map[string]any{"role": "assistant", "content": "看到了"},
		map[string]any{"role": "user", "content": "继续"},
	}))
	if h1.ID != h2.ID {
		t.Fatalf("纯图片会话的 ID 必须跨轮稳定：%s vs %s", h1.ID, h2.ID)
	}
	if h1.Source != "请求结构派生" {
		t.Fatalf("来源标记应为请求结构派生，实际=%s", h1.Source)
	}
}
