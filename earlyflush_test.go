package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withEarlyFlushUpstream 搭单账号 + 指定上游的测试现场（对齐 streamtimeout_test.go 夹具）。
func withEarlyFlushUpstream(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Base, profileCN.Origin = upstream.URL, upstream.URL
	cfg.HttpClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: upstreamHeaderTimeout}}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "early-flush.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}}
	accountMu.Unlock()
	t.Cleanup(func() {
		upstream.Close()
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
}

func withEarlyFlushGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := upstreamEarlyFlushGrace
	upstreamEarlyFlushGrace = d
	t.Cleanup(func() { upstreamEarlyFlushGrace = old })
}

func postChatStream(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)
	return rec
}

// 宽限期耗尽 + 上游最终失败：必须已提前发 200+SSE 头，错误降级为 SSE 事件，
// 且绝不伪造 [DONE]（同 streamChatResponse 中断处理的红线）。
func TestEarlyFlushAfterGraceDegradesErrorToSSE(t *testing.T) {
	withEarlyFlushGrace(t, 5*time.Millisecond)
	withEarlyFlushUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond) // 远超宽限期，preFlush 必然先触发
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	rec := postChatStream(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("提前发头后应保持 200，实际=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"upstream_error"`) {
		t.Fatalf("应有 SSE error 事件承载 upstream_error:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("失败请求绝不允许伪造 [DONE]:\n%s", rec.Body.String())
	}
}

// 禁用（宽限期=0）时保持旧行为：真实状态码直出，无任何 SSE 事件。
func TestEarlyFlushDisabledKeepsNativeErrorStatus(t *testing.T) {
	withEarlyFlushGrace(t, 0)
	withEarlyFlushUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	rec := postChatStream(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("禁用时应维持原生 502，实际=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "data: ") {
		t.Fatalf("禁用时不应出现 SSE 事件:\n%s", rec.Body.String())
	}
}

// 宽限期内提前发了头、上游随后成功：照常完整透传流，客户端无感。
func TestEarlyFlushThenUpstreamSuccessStreamsNormally(t *testing.T) {
	withEarlyFlushGrace(t, 5*time.Millisecond)
	withEarlyFlushUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond) // 超过宽限期，响应头已提前发出
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		f.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	})
	rec := postChatStream(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应为 200，实际=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "finish_reason") || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("上游恢复成功时应照常完整透传:\n%s", rec.Body.String())
	}
}
