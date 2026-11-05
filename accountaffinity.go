package main

// accountaffinity.go —— 会话 → 账号亲和（同一会话固定同一上游账号）
//
// 为什么需要：对话亲和只解决「上游按会话分区」的缓存；若上游按**账号**隔离
// prompt 缓存，同一会话被轮转到不同账号仍会整段 miss。姊妹项目实测差异明确：
//   - qoder2api：上游按账号隔离 → 做了 2h TTL 的 account affinity，
//     99.5% 会话全程单账号，命中 93.1%；跨账号会话只有 65.2%。
//   - autoclaw-proxy：无账号亲和，但选号硬分层使账号长期集中，命中 78.5%。
//   - workbuddy：4 账号严格 rrIndex 轮转（每请求换号），是三者里最不利的。
//
// 语义边界（刻意保守，不动任何既有闸门与负载均衡语义）：
//   - 只在「同一会话键」内粘滞；不同会话照旧分散到各账号。
//   - 实现方式是把绑定账号排到本轮扫描起点，**后续循环体一字不改** ——
//     黑名单/站点五态/风控熔断/冷却/额度/在途预留全部照旧跑一遍；
//     任一不满足，扫描自然落到其余账号，等价于自动回落。
//   - 上游拒绝时调用方会把该账号放进 attempted；下次进入本函数看到
//     attempted 已含绑定账号即解绑，改选新号并重新绑定（自愈）。
//   - TTL 30 分钟（与上游 prompt cache 的 idle 窗口同量级）。
//   - 容量上限 + 惰性清理，绝不无限增长。
//   - 可热加载关停（upstream.accountAffinityEnabled=false），回到严格轮转。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// accountAffinityTTL 绑定存活时长。取 30 分钟：短于常见会话中断间隔，
	// 长于上游 prompt cache 的复用窗口，过期后自然回归负载均衡。
	accountAffinityTTL = 30 * time.Minute
	// accountAffinityMaxEntries 绑定表上限，防止长跑进程无限增长。
	accountAffinityMaxEntries = 4096
)

// accountAffinityEnabled 默认开启；显式配置 false 时关闭（热加载秒回滚）。
var accountAffinityEnabled atomic.Bool

func init() { accountAffinityEnabled.Store(true) }

type accountAffinityEntry struct {
	account  *Account
	expireAt time.Time
}

var (
	accountAffinityMu      sync.Mutex
	accountAffinityEntries = map[string]accountAffinityEntry{}
)

// accountAffinityGet 取会话当前绑定的账号；过期条目顺手清除。
func accountAffinityGet(sessionKey string, now time.Time) *Account {
	if sessionKey == "" || !accountAffinityEnabled.Load() {
		return nil
	}
	accountAffinityMu.Lock()
	defer accountAffinityMu.Unlock()
	entry, ok := accountAffinityEntries[sessionKey]
	if !ok {
		return nil
	}
	if !now.Before(entry.expireAt) {
		delete(accountAffinityEntries, sessionKey)
		return nil
	}
	return entry.account
}

// accountAffinityBind 把会话绑定到账号，顺带做过期清理与容量控制。
func accountAffinityBind(sessionKey string, acc *Account, now time.Time) {
	if sessionKey == "" || acc == nil || !accountAffinityEnabled.Load() {
		return
	}
	accountAffinityMu.Lock()
	defer accountAffinityMu.Unlock()
	if len(accountAffinityEntries) >= accountAffinityMaxEntries {
		for key, entry := range accountAffinityEntries {
			if !now.Before(entry.expireAt) {
				delete(accountAffinityEntries, key)
			}
		}
		// 清理后仍满：丢一半，宁可丢粘滞也不让内存无界增长。
		if len(accountAffinityEntries) >= accountAffinityMaxEntries {
			drop := len(accountAffinityEntries) / 2
			for key := range accountAffinityEntries {
				delete(accountAffinityEntries, key)
				if drop--; drop <= 0 {
					break
				}
			}
		}
	}
	accountAffinityEntries[sessionKey] = accountAffinityEntry{
		account:  acc,
		expireAt: now.Add(accountAffinityTTL),
	}
}

// accountAffinityUnbind 解除绑定（账号已被上游拒绝、或已不在池内时调用）。
func accountAffinityUnbind(sessionKey string) {
	if sessionKey == "" {
		return
	}
	accountAffinityMu.Lock()
	delete(accountAffinityEntries, sessionKey)
	accountAffinityMu.Unlock()
}

// resetAccountAffinity 清空绑定表（账号池重载/测试隔离用）。
func resetAccountAffinity() {
	accountAffinityMu.Lock()
	accountAffinityEntries = map[string]accountAffinityEntry{}
	accountAffinityMu.Unlock()
}

// prefixAffinityKey 从请求体的「稳定前缀」派生亲和键，用于会话 ID 派生失败的场景
// （例如首条 user 消息是纯图片、无文本可哈希，此时 conversationHeaders.ID 是
// 每请求随机的 UUID —— 直接拿它做亲和键等于永远不命中）。
//
// 取 messages 的前两条（system + 首轮 user）做规范化 JSON 哈希：这两条在整段
// 对话生命周期内不变，多模态结构也能稳定参与哈希；不同对话首轮不同，仍会分散。
// 解析失败或无 messages 时返回空串（调用方按「不启用亲和」处理，行为不变）。
func prefixAffinityKey(bodyBytes []byte) string {
	if len(bodyBytes) == 0 {
		return ""
	}
	var body struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		return ""
	}
	if len(body.Messages) == 0 {
		return ""
	}
	head := body.Messages
	if len(head) > 2 {
		head = head[:2]
	}
	h := sha256.New()
	for _, raw := range head {
		h.Write(raw)
		h.Write([]byte{0})
	}
	return "pfx-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// prefixSeedFromBody 用已解析的请求体生成前缀种子（给会话 ID 兜底用）。
// 与 prefixAffinityKey 同源算法，只是输入形态不同（map 已解析 vs 原始字节）。
// 无 messages 时返回空串。
func prefixSeedFromBody(body map[string]any) string {
	if body == nil {
		return ""
	}
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		return ""
	}
	head := messages
	if len(head) > 2 {
		head = head[:2]
	}
	h := sha256.New()
	for _, msg := range head {
		blob, err := json.Marshal(msg)
		if err != nil {
			return ""
		}
		h.Write(blob)
		h.Write([]byte{0})
	}
	return "pfx-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// affinityKeyForRequest 计算本次请求用于「账号亲和」的键。
//
// 优先用真实会话键（客户端显式头 / 请求字段 / 首条 user 文本派生），它们稳定
// 且语义明确；只有在会话 ID 是「每请求随机 UUID」时，才退回前缀指纹 —— 否则
// 亲和键每次都变，绑定表只涨不命中（2026-11-05 设计复核发现）。
func affinityKeyForRequest(r *http.Request, bodyBytes []byte) string {
	if h := conversationHeadersFor(r); h != nil && h.ID != "" && h.Source != "独立生成" {
		return h.ID
	}
	return prefixAffinityKey(bodyBytes)
}

// indexAccountInLocked 返回账号在候选序列中的下标，找不到返回 -1。
// 调用方需已持有 accountMu（本函数只读切片，不加锁）。
func indexAccountInLocked(list []*Account, target *Account) int {
	if target == nil {
		return -1
	}
	for i, acc := range list {
		if acc == target {
			return i
		}
	}
	return -1
}
