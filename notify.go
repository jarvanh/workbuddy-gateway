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
	"unicode/utf8"
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
	// HTML 是 Telegram 用的完整 HTML 正文（含标题/分隔线/树形条目）。
	// 与全库通知版式同源（.github/scripts/telegram/tg_notify.sh 与
	// docs/telegram-notify.md），故 Telegram 通道优先发它；webhook 通道仍发
	// Title/Body 纯文本（webhook 是通用 JSON 消费者，不假设 HTML）。
	HTML string
	Time time.Time
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

	var err error
	if strings.EqualFold(strings.TrimSpace(cfg.Type), "telegram") {
		// Telegram 走全库统一版式（HTML）：有 HTML 正文就用它，
		// 否则退回纯文本 Title+Body（调用方尚未提供 HTML 的兜底）。
		html := ev.HTML
		if html == "" {
			html = escapeHTML(ev.Title) + "\n" + escapeHTML(ev.Body)
		}
		err = sendTelegram(cfg, html)
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
	// 按 4000 字符分片（与 tg_notify.sh send_tg_chunked 同口径，Telegram 上限 4096），
	// 尽量在换行处断开，避免切断 UTF-8 多字节字符。
	for _, chunk := range chunkMessage(text, 4000) {
		payload, err := json.Marshal(map[string]any{
			"chat_id":                  chat,
			"text":                     chunk,
			"parse_mode":               "HTML",
			"disable_web_page_preview": true,
		})
		if err != nil {
			return err
		}
		resp, err := notifyClient.Post(url, "application/json", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			// HTML 解析失败不退化重发：消息本就没发出去，退化只会把版式 bug 藏起来
			// （与 tg_notify.sh _tg_send_once 同口径）。
			return fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
		}
	}
	return nil
}

// chunkMessage 按 max 字符分片，尽量在换行处断开，不切断 UTF-8 多字节字符。
// 与 tg_notify.sh send_tg_chunked 同口径（那边走 python 实现，此处是 Go 等价物）。
func chunkMessage(s string, max int) []string {
	if max <= 0 || len(s) <= max {
		return []string{s}
	}
	var chunks []string
	for len(s) > max {
		end := max
		// 回退到换行处（仅当断点不过分靠前时采用，避免碎成大量短消息）
		if idx := strings.LastIndex(s[:end], "\n"); idx > end/2 {
			end = idx + 1
		} else {
			// 无合适换行：回退到不切断 UTF-8 的边界
			for end > 0 && !utf8.RuneStart(s[end]) {
				end--
			}
		}
		chunks = append(chunks, s[:end])
		s = s[end:]
	}
	if len(s) > 0 {
		chunks = append(chunks, s)
	}
	return chunks
}

// ===== Telegram 版式助手（与 .github/scripts/telegram/tg_notify.sh 同源）=====
//
// 版式真源是 docs/telegram-notify.md：标题 + 分隔线、`标签：值` 取值行、
// 分节 `{emoji} 分节 · N`、树形条目 `<code>  ├─/└─ </code>`、收尾区。
// 全库只有三种标签：<code>（机器值）、<pre>（多行块）、<a>（链接），其余裸文本，
// 动态内容必须转义（& < > 会触发 400）。

// tgSep 统一分隔线（18 个全角横线）。
const tgSep = "━━━━━━━━━━━━━━━━━━"

// escapeHTML HTML 实体转义（Telegram parse_mode=HTML）。
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// tgTitle 标题块："标题\n分隔线\n"。
func tgTitle(title string) string {
	return escapeHTML(title) + "\n" + tgSep + "\n"
}

// tgKV 取值行（自然语言值，裸文本）："标签：值\n"。
func tgKV(label, value string) string {
	return label + "：" + escapeHTML(value) + "\n"
}

// tgPath 取值行（机器值，等宽）："标签：<code>值</code>\n"。
func tgPath(label, value string) string {
	return label + "：<code>" + escapeHTML(value) + "</code>\n"
}

// tgSection 分节标题："\n{标题}\n"（段前空一行与上一区块分隔；
// 紧跟标题分隔线时不再补空行，与 tg_notify.sh tg_add_section 同口径）。
func tgSection(b *strings.Builder, title string) {
	cur := b.String()
	switch {
	case cur == "" || strings.HasSuffix(cur, tgSep+"\n"):
	case strings.HasSuffix(cur, "\n"):
		b.WriteString("\n")
	default:
		b.WriteString("\n\n")
	}
	b.WriteString(escapeHTML(title) + "\n")
}

// tgEntry 条目行（主体是机器值）："<code>主体</code> · 元数据…"（无尾换行）。
func tgEntry(subject string, meta ...string) string {
	out := "<code>" + escapeHTML(subject) + "</code>"
	for _, m := range meta {
		if m != "" {
			out += " · " + escapeHTML(m)
		}
	}
	return out
}

// treeLines 多行条目 → 树形列表（末条 └─，其余 ├─；无尾换行）。
//
// 前缀刻意用裸文本、不套 <code>：条目主体本身以 <code> 机器值开头，前缀若也是 <code>，
// 两个相邻 code 实体之间 Telegram 会渲染出一对空反引号（实测冷却告警条目显示为
// 「  ├─  」紧跟 ``），整行看起来像排版错乱。规范 4.2 的示例前缀同样是裸空格。
func treeLines(entries []string) string {
	var sb strings.Builder
	for i, e := range entries {
		if i == len(entries)-1 {
			sb.WriteString("  └─ " + e)
		} else {
			sb.WriteString("  ├─ " + e)
		}
		if i != len(entries)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// treeSub 树形子行前缀（与 treeLines 的 ├─/└─ 定宽一致，5 字符；同用裸文本）。
func treeSub(isLast bool) string {
	if isLast {
		return "     "
	}
	return "  │  "
}
