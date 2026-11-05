package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// 无显式会话键时，同一会话的连续轮次（消息累积）必须落到同一个会话 ID。
// 这是前缀缓存能被复用的前提：2026-11-04 实测固定 ID 后命中率 12.5%→87.5%。
func TestConversationAffinityStableAcrossTurns(t *testing.T) {
	prev := conversationAffinityEnabled.Load()
	conversationAffinityEnabled.Store(true)
	t.Cleanup(func() { conversationAffinityEnabled.Store(prev) })

	base := []any{
		map[string]any{"role": "system", "content": "固定系统提示词"},
		map[string]any{"role": "user", "content": "第一轮问题：请回答 1+1"},
	}
	bodyFor := func(extra []any) map[string]any {
		return map[string]any{"model": "hy4free", "messages": append(append([]any{}, base...), extra...)}
	}
	newReq := func(body map[string]any) *http.Request {
		req := httptest.NewRequest("POST", "/", nil)
		// 与线上一致：调用方指纹来自 API Key
		req.Header.Set("Authorization", "Bearer test-key")
		return req
	}
	h1 := prepareConversationContext(newReq(bodyFor(nil)), bodyFor(nil))
	h2 := prepareConversationContext(newReq(bodyFor([]any{
		map[string]any{"role": "assistant", "content": "2"},
		map[string]any{"role": "user", "content": "第二轮追问"},
	})), bodyFor([]any{
		map[string]any{"role": "assistant", "content": "2"},
		map[string]any{"role": "user", "content": "第二轮追问"},
	}))
	if h1.ID != h2.ID {
		t.Fatalf("同一会话连续轮次的会话 ID 必须稳定：h1=%s h2=%s", h1.ID, h2.ID)
	}
	if _, err := uuid.Parse(h1.ID); err != nil {
		t.Fatalf("派生 ID 必须是合法 UUID：%s (%v)", h1.ID, err)
	}
	if h1.Source != "请求前缀派生" || h2.Source != "请求前缀派生" {
		t.Fatalf("来源标记错误：%s / %s", h1.Source, h2.Source)
	}
}

// 不同首条 user 消息 / 不同模型 / 不同调用方必须得到不同分区键，避免串台。
func TestConversationAffinityIsolatesSessions(t *testing.T) {
	prev := conversationAffinityEnabled.Load()
	conversationAffinityEnabled.Store(true)
	t.Cleanup(func() { conversationAffinityEnabled.Store(prev) })
	mk := func(key, model, user string) string {
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		body := map[string]any{"model": model, "messages": []any{
			map[string]any{"role": "system", "content": "同一系统提示词"},
			map[string]any{"role": "user", "content": user},
		}}
		return prepareConversationContext(req, body).ID
	}
	a := mk("key-1", "hy4free", "会话A的问题")
	b := mk("key-1", "hy4free", "会话B的问题")
	c := mk("key-2", "hy4free", "会话A的问题")
	d := mk("key-1", "other-model", "会话A的问题")
	for name, pair := range map[string][2]string{
		"不同会话": {a, b}, "不同调用方": {a, c}, "不同模型": {a, d},
	} {
		if pair[0] == pair[1] {
			t.Errorf("%s 的会话分区键不应相同: %s", name, pair[0])
		}
	}
}

// 显式会话键始终保持最高优先级，派生逻辑绝不能覆盖既有客户端语义。
func TestConversationAffinityExplicitKeyWins(t *testing.T) {
	prev := conversationAffinityEnabled.Load()
	conversationAffinityEnabled.Store(true)
	t.Cleanup(func() { conversationAffinityEnabled.Store(prev) })
	explicit := uuid.NewString()
	body := map[string]any{"model": "hy4free", "messages": []any{
		map[string]any{"role": "user", "content": "同一个问题"},
	}}
	for _, header := range []string{conversationIDHeader, "X-Session-ID"} {
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set(header, explicit)
		h := prepareConversationContext(req, body)
		if h.ID != explicit {
			t.Errorf("%s 的显式会话键被覆盖：got=%s want=%s", header, h.ID, explicit)
		}
	}
	// 请求体会话字段同理
	req := httptest.NewRequest("POST", "/", nil)
	bodyWithField := map[string]any{"model": "hy4free", "conversation_id": explicit, "messages": body["messages"]}
	if h := prepareConversationContext(req, bodyWithField); h.ID != explicit {
		t.Errorf("请求体会话字段被覆盖：got=%s want=%s", h.ID, explicit)
	}
}

// 显式关闭时回到旧行为：每次独立生成，且来源仍是「独立生成」。
func TestConversationAffinityDisabledKeepsLegacy(t *testing.T) {
	prev := conversationAffinityEnabled.Load()
	conversationAffinityEnabled.Store(false)
	t.Cleanup(func() { conversationAffinityEnabled.Store(prev) })
	body := map[string]any{"model": "hy4free", "messages": []any{
		map[string]any{"role": "user", "content": "固定问题"},
	}}
	h1 := prepareConversationContext(httptest.NewRequest("POST", "/", nil), body)
	h2 := prepareConversationContext(httptest.NewRequest("POST", "/", nil), body)
	if h1.ID == h2.ID || h1.Source != "独立生成" {
		t.Fatalf("关闭后应每请求独立生成：%+v / %+v", h1, h2)
	}
}

// 多模态 content 数组里的文本也能参与派生；无文本 user 消息退回旧行为。
func TestConversationAffinityContentShapes(t *testing.T) {
	prev := conversationAffinityEnabled.Load()
	conversationAffinityEnabled.Store(true)
	t.Cleanup(func() { conversationAffinityEnabled.Store(prev) })
	multimodal := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "图片里是什么？"},
		}},
	}}
	h1 := prepareConversationContext(httptest.NewRequest("POST", "/", nil), multimodal)
	h2 := prepareConversationContext(httptest.NewRequest("POST", "/", nil), multimodal)
	if h1.Source != "请求前缀派生" || h1.ID != h2.ID {
		t.Fatalf("多模态文本应派生稳定 ID：%+v / %+v", h1, h2)
	}
	// 2026-11-05 行为变更：首条 user 消息无文本（纯图片/纯工具结果）时不再退回
	// 随机 UUID，而是用 messages 前两条的结构化哈希派生稳定会话 ID —— 否则每请求
	// 随机 UUID，上游前缀缓存永远 miss（workbuddy 实测 0/6）。见 accountaffinity.go。
	noText := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url"}}},
	}}
	h3 := prepareConversationContext(httptest.NewRequest("POST", "/", nil), noText)
	if h3.Source != "请求结构派生" {
		t.Fatalf("无文本 user 消息应走结构派生兜底：%+v", h3)
	}
	h4 := prepareConversationContext(httptest.NewRequest("POST", "/", nil), noText)
	if h3.ID != h4.ID {
		t.Fatalf("结构派生必须跨请求稳定：%s vs %s", h3.ID, h4.ID)
	}
}

// 配置开关：显式 false 关闭、缺省开启，且通过 loadRuntimeConfig 生效。
func TestConversationAffinityConfigToggle(t *testing.T) {
	prev := conversationAffinityEnabled.Load()
	t.Cleanup(func() { conversationAffinityEnabled.Store(prev) })
	for _, tc := range []struct {
		name string
		json string
		want bool
	}{
		{"缺省开启", `{"gateway":{"apiKeyEnabled":false}}`, true},
		{"显式关闭", `{"gateway":{"apiKeyEnabled":false},"upstream":{"conversationAffinityEnabled":false}}`, false},
		{"显式开启", `{"gateway":{"apiKeyEnabled":false},"upstream":{"conversationAffinityEnabled":true}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			writeRuntimeFixture(t, tc.json)
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatalf("配置加载失败: %v", err)
			}
			if got := conversationAffinityEnabled.Load(); got != tc.want {
				t.Errorf("开关=%t want=%t", got, tc.want)
			}
		})
	}
}

// 派生键里绝不出现原始提示词文本（避免日志/上游看到明文种子）。
func TestDerivedConversationIDIsOpaque(t *testing.T) {
	seed := "绝对不应出现在派生ID里的敏感提示词"
	id := derivedConversationID("caller", "hy4free", seed)
	if reflect.DeepEqual(id, seed) || len(id) != 36 {
		t.Fatalf("派生 ID 形态异常: %q", id)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("派生 ID 不是 UUID: %q (%v)", id, err)
	}
}
