package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 2026-10-15 intl glm-5.3-flash 事故回归：低倍率模型小样本探针不得判免费。
func TestProbeTinySampleNeverFree(t *testing.T) {
	acc := probeAccount()
	withProbeAccount(t, acc, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseWithUsage(`{"credit":0,"total_tokens":232}`)))
	})
	res := probeAccountModel(context.Background(), acc, "hy3")
	if res.Status != "unknown" {
		t.Fatalf("232 tokens credit=0 must stay unknown, got %+v", res)
	}
}

// flash 模型探针必须填充提示词并拉高 max_tokens，确保样本量达到扣费精度下限。
func TestProbeFlashRequestFillsPrompt(t *testing.T) {
	acc := probeAccount()
	var gotBody string
	withProbeAccount(t, acc, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseWithUsage(`{"credit":0,"total_tokens":12000}`)))
	})
	_ = probeAccountModel(context.Background(), acc, "glm-5.3-flash")
	if !strings.Contains(gotBody, probePromptFiller) {
		t.Fatal("flash probe request must contain prompt filler")
	}
	var payload struct {
		MaxTokens int64 `json:"max_tokens"`
	}
	_ = json.Unmarshal([]byte(gotBody), &payload)
	if payload.MaxTokens != probeTargetMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", payload.MaxTokens, probeTargetMaxTokens)
	}
}
