package main

// notify.go —— 事件告警（冷却 / 模型冷却 / 无可用账号）。
//
// 设计约束（重要）：
//  1. 全程异步，绝不阻塞调度：sendNotify 只做非阻塞入队，队列满则丢弃。
//  2. 同类事件按最小间隔限流（默认 60s），避免冷却风暴刷屏。
//  3. 发送失败只记日志，不影响网关任何主流程。
//  4. Telegram token 优先从环境变量读取（botTokenEnv/chatIdEnv），避免凭据落盘。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// 事件类型
const (
	notifyEventCooldown      = "cooldown"
	notifyEventModelCooldown = "model_cooldown"
	notifyEventNoAccount     = "no_account"
)

// notifyConfig 告警配置（config.json 的 notify 段）。
type notifyConfig struct {
	// Enabled 总开关，默认 false（不影响现有行为）。
	Enabled bool `json:"enabled"`
	// Type：telegram（调用 Bot API）/ webhook（POST JSON 到 URL）。默认 webhook。
	Type string `json:"type"`
	// Webhook：type=webhook 时的目标 URL。
	Webhook string `json:"webhook"`
	// Telegram：优先用 *Env 指定的环境变量名取值，取不到再用明文字段。
	BotToken    string `json:"botToken"`
	BotTokenEnv string `json:"botTokenEnv"`
	ChatID      string `json:"chatId"`
	ChatIDEnv   string `json:"chatIdEnv"`
	// MinIntervalSeconds 同一 Key 事件的最小发送间隔，默认 60 秒。
	MinIntervalSeconds int `json:"minIntervalSeconds"`
	// Events 允许通知的事件类型；省略表示全部。
	Events []string `json:"events"`
}

type notifyEvent struct {
	Kind  string // 事件类型（用于 Events 过滤）
	Key   string // 限流键（同 Key 在间隔内只发一次）
	Level string
	Title string
	Body  string
	Time  time.Time
}

var (
	notifyMu      sync.Mutex
	notifyCurrent notifyConfig
	notifyCh      = make(chan notifyEvent, 128)
	notifyLast    = map[string]time.Time{}
	notifyClient  = &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
	}
)

func init() {
	go notifyWorker()
}

func notifyWorker() {
	for ev := range notifyCh {
		dispatchNotify(ev)
	}
}

func setNotify(cfg notifyConfig) {
	notifyMu.Lock()
	notifyCurrent = cfg
	if notifyCurrent.MinIntervalSeconds <= 0 {
		notifyCurrent.MinIntervalSeconds = 60
	}
	notifyMu.Unlock()
}

func notifySnapshot() notifyConfig {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	return notifyCurrent
}

// sendNotify 非阻塞入队；队列满则丢弃，绝不阻塞调度主流程。
func sendNotify(ev notifyEvent) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	select {
	case notifyCh <- ev:
	default:
		log.Printf("[Notify] 队列已满，丢弃事件: %s", ev.Title)
	}
}

func notifyKindEnabled(kind string, cfg notifyConfig) bool {
	if !cfg.Enabled {
		return false
	}
	if len(cfg.Events) == 0 {
		return true
	}
	for _, e := range cfg.Events {
		if strings.EqualFold(strings.TrimSpace(e), kind) {
			return true
		}
	}
	return false
}

func dispatchNotify(ev notifyEvent) {
	cfg := notifySnapshot()
	if !notifyKindEnabled(ev.Kind, cfg) {
		return
	}
	notifyMu.Lock()
	if last, ok := notifyLast[ev.Key]; ok &&
		ev.Time.Sub(last) < time.Duration(cfg.MinIntervalSeconds)*time.Second {
		notifyMu.Unlock()
		return
	}
	notifyLast[ev.Key] = ev.Time
	notifyMu.Unlock()

	text := ev.Title + "\n" + ev.Body
	var err error
	if strings.EqualFold(strings.TrimSpace(cfg.Type), "telegram") {
		err = sendTelegram(cfg, text)
	} else {
		err = sendWebhook(cfg, ev)
	}
	if err != nil {
		log.Printf("[Notify] 发送失败（不影响调度）: %v", err)
	}
}

func envOrDefault(envName, fallback string) string {
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return v
		}
	}
	return fallback
}

func sendWebhook(cfg notifyConfig, ev notifyEvent) error {
	if cfg.Webhook == "" {
		return fmt.Errorf("webhook 未配置")
	}
	payload, err := json.Marshal(map[string]any{
		"kind": ev.Kind, "level": ev.Level, "title": ev.Title,
		"body": ev.Body, "time": ev.Time.Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	resp, err := notifyClient.Post(cfg.Webhook, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	return nil
}

func sendTelegram(cfg notifyConfig, text string) error {
	token := envOrDefault(cfg.BotTokenEnv, cfg.BotToken)
	chat := envOrDefault(cfg.ChatIDEnv, cfg.ChatID)
	if token == "" || chat == "" {
		return fmt.Errorf("telegram 配置不完整（botToken/chatId 均缺失）")
	}
	url := "https://api.telegram.org/bot" + token + "/sendMessage"
	payload, err := json.Marshal(map[string]string{"chat_id": chat, "text": text})
	if err != nil {
		return err
	}
	resp, err := notifyClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	return nil
}
