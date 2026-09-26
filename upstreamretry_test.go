package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

// setupRetryUpstream 启动伪上游并覆盖国内站路由与 HTTP 客户端，测试结束自动还原。
func setupRetryUpstream(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Base, profileCN.Origin = upstream.URL, upstream.URL
	cfg.HttpClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: upstreamHeaderTimeout}}
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		upstream.Close()
	})
}

// setupRetryAccounts 注入临时账号池并重置轮询起点，测试结束自动还原。
func setupRetryAccounts(t *testing.T, accs ...*Account) {
	t.Helper()
	oldAccounts := accounts
	oldRR := rrIndex
	accountMu.Lock()
	accounts = accs
	rrIndex = 0
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		rrIndex = oldRR
		accountMu.Unlock()
	})
}

func retryAccount(path, token string) *Account {
	return &Account{
		Path: path,
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: token, ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}
}

func retryPostChat(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)
	return rec
}

func retryWriteSSE(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
	f.Flush()
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	f.Flush()
}

// 网络层失败：同一账号应先原地重试，重连恢复时不回退账号。
//
// 语义边界：只有「请求头尚未写出」的失败才允许重放，因为此时上游不可能处理过
// 这次 POST。已经到达服务端、由服务端断开的情况不会重放，而是回退下一个账号
// （见 netretry_test.go 的 TestUpstreamDoesNotRetryAfterServerReceivesRequest）。
func TestNetworkErrorRetriesSameAccountThenSucceeds(t *testing.T) {
	oldRetries, oldDelay := upstreamTransientRetries, upstreamRetryBackoff
	upstreamTransientRetries, upstreamRetryBackoff = 3, time.Millisecond
	defer func() { upstreamTransientRetries, upstreamRetryBackoff = oldRetries, oldDelay }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		retryWriteSSE(t, w)
	}))
	defer upstream.Close()
	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Base, profileCN.Origin = upstream.URL, upstream.URL
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
	}()

	var hits atomic.Int64
	// 前 3 次在建连阶段就失败（请求头未写出），属于可安全重放的瞬时故障。
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		if hits.Add(1) <= int64(upstreamTransientRetries) {
			return nil, io.EOF
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	setupRetryAccounts(t, retryAccount("retry.json", "token-a"))

	rec := retryPostChat(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("原地重试应恢复成功, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := hits.Load(); n != 1+int64(upstreamTransientRetries) {
		t.Fatalf("重试 %d 次时上游应恰好收到 %d 次请求，实际 %d",
			upstreamTransientRetries, 1+upstreamTransientRetries, n)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("重试成功后流应完整结束:\n%s", rec.Body.String())
	}
}

// 网络层失败且原地重试仍失败：应回退到账号池下一个账号，而不是直接 502。
func TestNetworkErrorFallsBackToNextAccount(t *testing.T) {
	oldRetries, oldDelay := upstreamTransientRetries, upstreamRetryBackoff
	upstreamTransientRetries, upstreamRetryBackoff = 0, time.Millisecond
	defer func() { upstreamTransientRetries, upstreamRetryBackoff = oldRetries, oldDelay }()

	var aHits, bHits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			aHits.Add(1)
			panic(http.ErrAbortHandler)
		}
		bHits.Add(1)
		retryWriteSSE(t, w)
	})
	setupRetryAccounts(t, retryAccount("a.json", "token-a"), retryAccount("b.json", "token-b"))

	rec := retryPostChat(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("账号回退应恢复成功, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := aHits.Load(); n != 1 {
		t.Fatalf("账号 a 应被尝试 1 次，实际 %d", n)
	}
	if n := bHits.Load(); n != 1 {
		t.Fatalf("账号 b 应被回退尝试 1 次，实际 %d", n)
	}
}

// 上游 408/5xx 瞬时错误：应换下一个账号代偿。
func TestUpstream5xxFallsBackToNextAccount(t *testing.T) {
	oldRetries, oldDelay := upstreamTransientRetries, upstreamRetryBackoff
	upstreamTransientRetries, upstreamRetryBackoff = 0, time.Millisecond
	defer func() { upstreamTransientRetries, upstreamRetryBackoff = oldRetries, oldDelay }()

	var aHits, bHits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			aHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"internal"}`)
			return
		}
		bHits.Add(1)
		retryWriteSSE(t, w)
	})
	setupRetryAccounts(t, retryAccount("a.json", "token-a"), retryAccount("b.json", "token-b"))

	rec := retryPostChat(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("5xx 回退应恢复成功, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := aHits.Load(); n != 1 {
		t.Fatalf("账号 a 应被尝试 1 次，实际 %d", n)
	}
	if n := bHits.Load(); n != 1 {
		t.Fatalf("账号 b 应被回退尝试 1 次，实际 %d", n)
	}
}

// 所有账号的网络调用都失败：应返回 502 upstream_network_error，且完成原地重试。
func TestAllAccountsNetworkFailureReturns502(t *testing.T) {
	oldRetries, oldDelay := upstreamTransientRetries, upstreamRetryBackoff
	upstreamTransientRetries, upstreamRetryBackoff = 1, 5*time.Millisecond
	defer func() { upstreamTransientRetries, upstreamRetryBackoff = oldRetries, oldDelay }()

	var hits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		panic(http.ErrAbortHandler)
	})
	setupRetryAccounts(t, retryAccount("solo.json", "token-a"))

	rec := retryPostChat(t)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("全部账号网络失败应返回 502, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_network_error") {
		t.Fatalf("错误体应包含 upstream_network_error:\n%s", rec.Body.String())
	}
	// 服务端已收到完整请求头后才断开：POST 可能已被处理，不得重放，
	// 因此每个账号只尝试 1 次，失败即回退下一个账号（此处账号池已耗尽 → 502）。
	if n := hits.Load(); n != 1 {
		t.Fatalf("请求头已写出时不得重放，单账号应只尝试 1 次，实际 %d", n)
	}
}

// 上游返回 400 类请求级错误：换账号无意义，不应回退。
func TestUpstream400DoesNotFallBack(t *testing.T) {
	oldRetries, oldDelay := upstreamTransientRetries, upstreamRetryBackoff
	upstreamTransientRetries, upstreamRetryBackoff = 0, time.Millisecond
	defer func() { upstreamTransientRetries, upstreamRetryBackoff = oldRetries, oldDelay }()

	var aHits, bHits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			aHits.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":11101,"message":"bad request"}`)
			return
		}
		bHits.Add(1)
		retryWriteSSE(t, w)
	})
	setupRetryAccounts(t, retryAccount("a.json", "token-a"), retryAccount("b.json", "token-b"))

	rec := retryPostChat(t)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("请求级错误应原样透传 400, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := bHits.Load(); n != 0 {
		t.Fatalf("4xx 不应回退其他账号，账号 b 被尝试 %d 次", n)
	}
}

func retryPostChatNonStream(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)
	return rec
}

// retryWritePartialThenAbort 先输出一个 SSE 分片再断开连接，模拟流传输中途网络中断。
func retryWritePartialThenAbort(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n")
	f.Flush()
	panic(http.ErrAbortHandler)
}

// 非流式请求：聚合期间上游流被中断（尚未向客户端写出任何字节），应回退下一个账号重试。
func TestNonStreamAggregateInterruptedFallsBackToNextAccount(t *testing.T) {
	var aHits, bHits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			aHits.Add(1)
			retryWritePartialThenAbort(t, w)
			return
		}
		bHits.Add(1)
		retryWriteSSE(t, w)
	})
	setupRetryAccounts(t, retryAccount("a.json", "token-a"), retryAccount("b.json", "token-b"))

	rec := retryPostChatNonStream(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("非流式聚合中断回退应恢复成功, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("非流式结果应包含账号 b 的完整内容:\n%s", rec.Body.String())
	}
	if n := aHits.Load(); n != 1 {
		t.Fatalf("账号 a 应被尝试 1 次，实际 %d", n)
	}
	if n := bHits.Load(); n != 1 {
		t.Fatalf("账号 b 应被回退尝试 1 次，实际 %d", n)
	}
}

// 非流式请求：所有账号的流都在聚合期间中断 → 返回 502 错误，绝不返回截断的伪完整结果。
func TestNonStreamAggregateInterruptedReturnsErrorNotTruncated(t *testing.T) {
	var hits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		retryWritePartialThenAbort(t, w)
	})
	setupRetryAccounts(t, retryAccount("solo.json", "token-a"))

	rec := retryPostChatNonStream(t)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("聚合中断且无法恢复应返回 502, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "aggregate_error") {
		t.Fatalf("错误体应包含 aggregate_error:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"object":"chat.completion"`) {
		t.Fatalf("绝不能把截断内容伪装成完整结果返回:\n%s", rec.Body.String())
	}
	if n := hits.Load(); n != maxNonStreamAttempts {
		t.Fatalf("应端到端尝试 %d 次，实际上游收到 %d 次请求", maxNonStreamAttempts, n)
	}
}

// 403 + 无鉴权特征响应体（疑似 WAF/CDN 拦截页）：只短冷却换号，不禁用账号不删凭据。
func TestWAF403FallsBackWithoutDisablingAccount(t *testing.T) {
	var aHits, bHits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			aHits.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<html>Request blocked by EdgeOne WAF</html>")
			return
		}
		bHits.Add(1)
		retryWriteSSE(t, w)
	})
	setupRetryAccounts(t, retryAccount("a.json", "token-a"), retryAccount("b.json", "token-b"))

	rec := retryPostChat(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("WAF 拦截回退应恢复成功, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := bHits.Load(); n != 1 {
		t.Fatalf("账号 b 应被回退尝试 1 次，实际 %d", n)
	}
	accountMu.Lock()
	disabled := accounts[0].Disabled
	accountMu.Unlock()
	if disabled {
		t.Fatalf("疑似 WAF/CDN 拦截的 403 不应禁用账号")
	}
}

// aggregateCompletion 必须暴露流读取错误，而不是静默返回截断结果。
func TestAggregateCompletionSurfacesReadError(t *testing.T) {
	r := io.MultiReader(
		strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"),
		iotest.ErrReader(errors.New("boom")),
	)
	if _, err := aggregateCompletion(r, "m"); err == nil {
		t.Fatalf("流读取中断应返回错误，而不是静默返回截断结果")
	}
}

// 403 + 鉴权失败特征响应体：仍然禁用账号并回退下一个账号。
func TestAuth403StillDisablesAccount(t *testing.T) {
	var aHits, bHits atomic.Int64
	setupRetryUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			aHits.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"invalid token"}`)
			return
		}
		bHits.Add(1)
		retryWriteSSE(t, w)
	})
	setupRetryAccounts(t, retryAccount("a.json", "token-a"), retryAccount("b.json", "token-b"))

	rec := retryPostChat(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("授权失效回退应恢复成功, status=%d body=%s", rec.Code, rec.Body.String())
	}
	accountMu.Lock()
	disabled := accounts[0].Disabled
	accountMu.Unlock()
	if !disabled {
		t.Fatalf("带鉴权失败特征的 403 应禁用账号")
	}
}
