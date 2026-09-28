package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 响应头先行（early flush）
//
// 背景：客户端经 Cloudflare Tunnel 访问时，CDN 边缘对「已建连但迟迟拿不到
// 响应头」的请求有 ~100s 硬超时（HTTP 524）。实测大上下文 + 并发排队时，
// 账号选择与上游首字延迟叠加会把 TTFB 推过 100s：请求明明活着，却被边缘
// 掐断（2026-09-28 三方 TTFB 对比 + 5 路并发压测定位「排队放大首字延迟」）。
//
// 方案：流式请求在宽限期内保持原生语义（失败返回真实 HTTP 状态码，CliRelay
// 可按状态码决定重试）；宽限期耗尽仍没拿到上游响应，就提前下发 200 + SSE
// 响应头——CDN 的「等待响应头」计时随之停止。此后上游若失败，降级为 SSE
// error 事件而非伪造正常结束；若成功则照常透传流，客户端无感。
//
// 仅流式路径启用（非流式在本地聚合成完整 JSON，提前发头会破坏协议）。
// 宽限期可由 config.json 的 upstream.earlyFlushGraceSeconds 覆盖，
// 显式设 0 表示完全禁用（回到旧行为）。
// -----------------------------------------------------------------------------

// upstreamEarlyFlushGraceDefault 是「响应头先行」的默认宽限期。
// 需显著小于 CDN 边缘 ~100s 的硬超时，同时给账号调度 / 令牌刷新留足时间。
const upstreamEarlyFlushGraceDefault = 30 * time.Second

// upstreamEarlyFlushGrace 是生效的宽限期（默认取 Default，可由 config.json 覆盖；
// <=0 视为禁用）。装载逻辑见 debug.go 的 loadRuntimeConfig。
var upstreamEarlyFlushGrace = upstreamEarlyFlushGraceDefault

// earlyFlushGate 维护「宽限期内保持原生错误语义 / 超期提前发 200+SSE 头」
// 的状态机。通过 newEarlyFlushGate 创建；nil 门的方法均为安全空操作，
// 便于非流式路径直接传 nil 复用同一套 fail() 出口。
type earlyFlushGate struct {
	w          http.ResponseWriter
	grace      time.Duration
	traceID    string
	reqID      uint64
	timer      *time.Timer
	mu         sync.Mutex
	preFlushed bool // 已提前下发 200 + SSE 响应头
	stopped    bool // 主流程已收尾，定时器不再触发
}

func newEarlyFlushGate(w http.ResponseWriter, enabled bool, traceID string, reqID uint64) *earlyFlushGate {
	if !enabled || upstreamEarlyFlushGrace <= 0 {
		return nil
	}
	ef := &earlyFlushGate{w: w, grace: upstreamEarlyFlushGrace, traceID: traceID, reqID: reqID}
	ef.timer = time.AfterFunc(ef.grace, ef.preFlush)
	return ef
}

// stop 收尾：先关闸再停定时器。返回后保证定时器侧不会再写 w，
// 主流程可安全进入 streamChatResponse 的无锁直写阶段。
func (ef *earlyFlushGate) stop() {
	if ef == nil {
		return
	}
	ef.mu.Lock()
	ef.stopped = true
	ef.mu.Unlock()
	ef.timer.Stop()
}

// preFlush 由定时器触发：加锁期间完成对 w 的全部写入，与 fail()/stop()
// 串行，避免响应头与错误体在底层连接上交错。
func (ef *earlyFlushGate) preFlush() {
	ef.mu.Lock()
	defer ef.mu.Unlock()
	if ef.stopped || ef.preFlushed {
		return
	}
	ef.preFlushed = true
	// 头集合与 streamChatResponse 保持一致；提前发出后 CDN 的
	// 「等待响应头」计时即停止，连接转入正常流式响应。
	ef.w.Header().Set("Content-Type", "text/event-stream")
	ef.w.Header().Set("Cache-Control", "no-cache")
	ef.w.Header().Set("Connection", "keep-alive")
	ef.w.Header().Set("X-Accel-Buffering", "no")
	ef.w.WriteHeader(http.StatusOK)
	if f, ok := ef.w.(http.Flusher); ok {
		f.Flush()
	}
	log.Printf("[早发响应头] traceId=%s requestId=%d 宽限期 %v 已到仍未收到上游响应头，提前下发 200+SSE 响应头以规避 CDN 边缘硬超时；此后失败将降级为 SSE error 事件",
		ef.traceID, ef.reqID, ef.grace)
}

// fail 上游调度失败的统一出口：
//   - 未提前发头：维持原生 writeOpenAIError 语义（真实状态码，CliRelay 可重试）；
//   - 已提前发头：状态码无法再改，降级为 SSE error 事件——绝不伪造正常结束
//     （与 streamChatResponse 的中断处理同一条红线）。
func (ef *earlyFlushGate) fail(w http.ResponseWriter, statusCode int, errType, message string) {
	if ef == nil {
		writeOpenAIError(w, statusCode, errType, message)
		return
	}
	ef.mu.Lock()
	defer ef.mu.Unlock()
	if !ef.preFlushed {
		writeOpenAIError(w, statusCode, errType, message)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    statusCode,
		},
	})
	_, _ = fmt.Fprintf(ef.w, "data: %s\n\n", payload)
	if f, ok := ef.w.(http.Flusher); ok {
		f.Flush()
	}
}
