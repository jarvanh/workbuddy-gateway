package main

// conversationaffinity.go —— 无显式会话键时，从请求首条 user 消息派生稳定会话 ID
//
// 根因（2026-11-04 A/B 实测）：上游按 X-Conversation-ID 分区前缀缓存。
// 网关对没有会话键的请求每次生成全新 UUID（日志「来源=独立生成」），于是同一
// 会话的连续轮次被分到不同缓存分区，前缀永远不复用：4 账号轮换 8 连发同前缀
// 只有 1 次碰巧命中。对照实验：固定 X-Conversation-ID 后，同样 4 账号轮换，
// 第 2 次起连续 7 次命中 30208；消息从 2 条累积到 8 条仍稳定命中；交替两个
// 完全不同前缀的「会话」共用同一 ID 也互不串台（上游按内容前缀匹配缓存）。
//
// 结论：会话 ID 是缓存分区键，不是账号键；换 qoder / 做账号粘滞都不能根治，
// 把会话 ID 稳定下来才是根。
//
// 安全边界：
//   - 显式 X-Conversation-ID / X-Session-ID / 请求体会话字段始终最高优先级，
//     本逻辑只在「完全没有会话键」时兜底，不改动任何已有客户端语义。
//   - 派生键 = 调用方指纹 + 模型名 + 首个 user 消息文本，经 SHA-256 规范化成
//     UUIDv5 形态；不同调用方、不同模型、不同会话互不碰撞，绝不落日志。
//   - user 消息用「首个」而非「最后一个」：真实会话每轮追加新消息，首条 user
//     消息在整个会话生命周期内保持不变，正是稳定的分区锚点。
//   - 内容含多模态数组时取其中文本，取不到就退回随机 UUID（保持旧行为）。
//   - 仅影响 X-Conversation-ID；RequestID / MessageID 仍每请求新生成，符合
//     上游「同一逻辑请求的重试和换号沿用同一组标识」的既有约定。

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync/atomic"
)

// conversationAffinityEnabled 默认开启；显式配置为 false 时关闭（可热加载秒回滚）。
var conversationAffinityEnabled atomic.Bool

func init() { conversationAffinityEnabled.Store(true) }

// messageTextForAffinity 取一条消息里的纯文本，兼容 string 与多模态 content 数组。
// 与 OmniRoute 生成 X-Conversation-ID 时的取值口径一致（取 text 字段拼行）。
func messageTextForAffinity(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok && text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

// conversationSeedFromBody 返回首个 user 消息的稳定文本；无可用文本时返回空串。
// 只取前 4096 字节做哈希输入：既避免超长 prompt 的哈希开销，又保证不同会话
// 的区分度（真实会话首条 user 消息远短于此，且系统提示词不参与派生）。
func conversationSeedFromBody(body map[string]any) string {
	if body == nil {
		return ""
	}
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		text := strings.TrimSpace(messageTextForAffinity(m["content"]))
		if text == "" {
			return "" // 首条 user 消息无文本 → 不做派生，保持旧行为
		}
		if len(text) > 4096 {
			text = text[:4096]
		}
		return text
	}
	return ""
}

// derivedConversationID 生成会话分区键。输入混入调用方指纹、模型名与首条 user
// 消息，输出固定 UUID 文本形态；不记录任何提示词或令牌。
func derivedConversationID(caller, model, seed string) string {
	h := sha256.Sum256([]byte(caller + "\x00" + model + "\x00" + seed))
	// 取前 16 字节并按 UUIDv5 置位，形态与 normalizedConversationID 一致。
	b := append([]byte(nil), h[:16]...)
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	hexStr := hex.EncodeToString(b)
	return hexStr[0:8] + "-" + hexStr[8:12] + "-" + hexStr[12:16] + "-" + hexStr[16:20] + "-" + hexStr[20:32]
}
