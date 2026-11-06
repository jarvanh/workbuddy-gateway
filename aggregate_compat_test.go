package main

// 本文件只为上游 v1.14.45 带来的 identity_restore_regression_test.go 提供
// 聚合路径靶子。fork 生产链路早已把 writeChatAggregate / writeResponsesAggregate
// 重构为单一的 handleNonStreamUpstream + buildResponse 闭包（内部已做身份还原），
// 因此这里保留上游两条「直接吃 *http.Response 的聚合写出」仅用于回归测试，
// 不进入生产调用链，避免与主链路形成两套并行聚合器。

import (
	"log"
	"net/http"
	"time"
)

func writeChatAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	completionJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		logAggregateFailure(r, reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
		return
	}
	completionJSON = restoreChatCompletionIdentity(r, completionJSON)
	usage := usageFromCompletion(completionJSON)
	statsObserveUsage(r, usage, true)
	observeModelCredit(r, acc, modelName, usage, reqID)
	recordModelTokens(modelName, usage, reqID)
	recordModelLatency(modelName, time.Since(startTime))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(completionJSON)
	debugEvent(r, "info", "aggregate_response_completed", map[string]any{
		"status_code":    http.StatusOK,
		"response_bytes": len(completionJSON),
	})
	log.Printf("[#%d] 非流式响应完成 (账号 %s [%s], 耗时 %v, 首Token(v2) %s)", reqID, acc.Path, prof.Label, time.Since(startTime), body.text())
}

func writeResponsesAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	completionJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		logAggregateFailure(r, reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
		return
	}
	usage := usageFromCompletion(completionJSON)
	statsObserveUsage(r, usage, true)
	observeModelCredit(r, acc, modelName, usage, reqID)
	recordModelTokens(modelName, usage, reqID)
	recordModelLatency(modelName, time.Since(startTime))
	out, err := chatCompletionToResponsesWithTools(completionJSON, modelName, responsesToolMapFor(r))
	if err != nil {
		debugEvent(r, "error", "response_translation_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		log.Printf("[#%d] Responses 转换失败: %v", reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "translate_error", "转换 Responses 响应失败: "+err.Error())
		return
	}
	out = restoreResponsesIdentity(r, out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	debugEvent(r, "info", "aggregate_response_completed", map[string]any{
		"status_code":    http.StatusOK,
		"response_bytes": len(out),
	})
	log.Printf("[#%d] Responses 非流式响应完成 (账号 %s [%s], 耗时 %v)", reqID, acc.Path, prof.Label, time.Since(startTime))
}
