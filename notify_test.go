package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func resetNotifyState() {
	notifyMu.Lock()
	notifyLast = map[string]time.Time{}
	notifyMu.Unlock()
}

func TestNotifyWebhookSends(t *testing.T) {
	resetNotifyState()
	ch := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		ch <- m
		w.WriteHeader(200)
	}))
	defer srv.Close()

	setNotify(notifyConfig{Enabled: true, Type: "webhook", Webhook: srv.URL, MinIntervalSeconds: 0})
	defer setNotify(notifyConfig{})
	sendNotify(notifyEvent{Kind: notifyEventCooldown, Key: "t-webhook-1", Level: "warn", Title: "T1", Body: "B1"})

	select {
	case m := <-ch:
		if m["title"] != "T1" || m["kind"] != notifyEventCooldown {
			t.Fatalf("webhook 载荷不符: %v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook 未收到通知")
	}
}

func TestNotifyRateLimitSameKey(t *testing.T) {
	resetNotifyState()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	setNotify(notifyConfig{Enabled: true, Type: "webhook", Webhook: srv.URL, MinIntervalSeconds: 60})
	defer setNotify(notifyConfig{})
	sendNotify(notifyEvent{Kind: notifyEventCooldown, Key: "t-rate-1", Title: "first"})
	sendNotify(notifyEvent{Kind: notifyEventCooldown, Key: "t-rate-1", Title: "second"}) // 同 Key 应被限流

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		c := n
		mu.Unlock()
		if c >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	c := n
	mu.Unlock()
	if c != 1 {
		t.Fatalf("同 Key 应只发 1 条，实际 %d", c)
	}
}

func TestNotifyDisabledByDefault(t *testing.T) {
	resetNotifyState()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	setNotify(notifyConfig{Enabled: false, Type: "webhook", Webhook: srv.URL}) // 未启用
	defer setNotify(notifyConfig{})
	sendNotify(notifyEvent{Kind: notifyEventCooldown, Key: "t-off-1", Title: "x"})
	time.Sleep(300 * time.Millisecond)
	if called {
		t.Fatal("notify 未启用时不应发送")
	}
}
