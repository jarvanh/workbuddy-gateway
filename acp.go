package main

// ACP over streamable-HTTP 客户端：把一条网页端（云 agent）会话真正驱动起来。
//
// 移植自 workbuddy-hub/wb_webagent.py，对应上游 issue #90 的坑：
// POST /console/as/conversations/ 只是**排队**了一条会话，agent 要等客户端接上
// 沙箱（GET /{id}/session 返回的 link）并请求这一轮才会真的跑；只建会话会永远
// 停在 CREATING、没有任何输出，也就不算一次有效对话（不加分）。
//
// 完整链路：
//   1. POST   /console/as/conversations/          建会话（排队）
//   2. GET    /console/as/conversations/{id}/session  取沙箱 link+token+sessionId
//   3. GET    <link>  Accept: text/event-stream       开 SSE，取 Acp-Connection-Id
//   4. POST   <link>  ×3（initialize / session/load / session/prompt）
//   5. 轮询   /console/as/conversations/{id}          直到 status=completed

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	acpProtocolVersion = 1

	// webConversationsPath 网页端会话 API 路径（挂在站点 Origin 下）。
	webConversationsPath = "/console/as/conversations/"

	// webUserAgent 网页端 UA：网页通道不带桌面端指纹，用浏览器 UA 才像官方页面。
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"

	// acpDailyChatModel 打卡模型。hub 实测（2026-09-30）：deepseek-v4.1-flash
	// 0.52/次最省；目录标 x0.00 的 hy3 / hy4-preview-f 实测各扣 1.00/次——
	// 二者强制 thinking 且不可关闭，标注不可信，按实测值选型。
	acpDailyChatModel = "deepseek-v4.1-flash"

	// acpDailyChatPrompt 打卡提示词：一句话即可，够触发「每日活跃」。
	acpDailyChatPrompt = "Hi"

	// acpTurnTimeout 一轮最多等多久。实测一次「Hi」十几秒跑完，留足余量。
	acpTurnTimeout = 120 * time.Second
	// acpPollInterval 会话状态轮询间隔。
	acpPollInterval = 3 * time.Second
)

// deriveDeviceID 由 uid 确定性派生伪设备标识。
//
// 同一账号每次出站都得到同一个值 —— 上游看到的永远是「一台固定的物理设备」，
// 避免随机机器码抖动触发风控。（移植自 workbuddy-hub/wb_fingerprint.py）
//
// 注意：输入只有常量 salt + uid，没有密钥，因此它做的是「稳定化」而非「匿名化」：
// 任何知道 uid 的人都能复算出同一结果。防抖动，不防外部关联。
func deriveDeviceID(salt, uid string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:])
}

// -----------------------------------------------------------------------------
// 数据结构
// -----------------------------------------------------------------------------

// acpSession 沙箱连接信息（GET /{id}/session 的 data 段）。
type acpSession struct {
	Link      string `json:"link"`
	Token     string `json:"token"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	// 上游偶发用下划线命名，两个都收。
	SessionIDAlt string `json:"session_id"`
	Endpoint     string `json:"endpoint"`
}

func (s acpSession) link() string {
	if s.Link != "" {
		return s.Link
	}
	return s.Endpoint
}

func (s acpSession) sessionID(fallback string) string {
	if s.SessionID != "" {
		return s.SessionID
	}
	if s.SessionIDAlt != "" {
		return s.SessionIDAlt
	}
	return fallback
}

// acpTurnResult 一轮会话的结果。
type acpTurnResult struct {
	OK        bool
	Status    string
	Updates   int
	Chunks    int
	ElapsedMS int64
	Error     string
}

// acpChannel ACP 的 streamable-HTTP 传输：一条 SSE 收事件，POST 发请求。
type acpChannel struct {
	link         string
	token        string
	connectionID string

	mu      sync.Mutex
	updates int
	chunks  int
	cancel  context.CancelFunc
}

// -----------------------------------------------------------------------------
// 网页端会话 API
// -----------------------------------------------------------------------------

// acpWebHeaders 网页版 app 的出站头：只有 bearer + X-User-Id，没有桌面端指纹。
func acpWebHeaders(req *http.Request, token, uid, origin string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-Id", uid)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/app")
	req.Header.Set("User-Agent", webUserAgent)
}

// acpCreateConversation 建会话，返回会话 id（此时只是排队，尚未运行）。
func acpCreateConversation(ctx context.Context, auth *StoredAuth, prof *upstreamProfile, prompt, model string) (string, error) {
	body := map[string]any{
		"prompt": prompt,
		"model":  model,
		// 网页端建会话时固定带上这两项（抓包所得），保持请求形态一致。
		"conversationOrigin": "workbuddy-app",
		"plugins": []any{
			map[string]any{"name": "weixinpay", "marketplace": "codebuddy-builtin"},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, prof.Origin+webConversationsPath, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	acpWebHeaders(req, auth.Auth.AccessToken, auth.Account.UID, prof.Origin)
	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("解析建会话响应失败: %w", err)
	}
	if env.Data.ID == "" {
		return "", fmt.Errorf("建会话未返回 id（code=%d msg=%s）", env.Code, env.Msg)
	}
	return env.Data.ID, nil
}

// acpGetConversation 读会话；suffix 为空拿会话本身，"/session" 拿沙箱信息。
func acpGetConversation(ctx context.Context, auth *StoredAuth, prof *upstreamProfile, conversation, suffix string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prof.Origin+webConversationsPath+urlPathEscape(conversation)+suffix, nil)
	if err != nil {
		return nil, err
	}
	acpWebHeaders(req, auth.Auth.AccessToken, auth.Account.UID, prof.Origin)
	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// acpConversationStatus 这条会话现在什么状态（completed 就是这一轮真的跑完了）。
func acpConversationStatus(ctx context.Context, auth *StoredAuth, prof *upstreamProfile, conversation string) string {
	data, err := acpGetConversation(ctx, auth, prof, conversation, "")
	if err != nil {
		return ""
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return ""
	}
	return st.Status
}

// -----------------------------------------------------------------------------
// ACP 传输：SSE 收事件 + POST 发 JSON-RPC
// -----------------------------------------------------------------------------

// open 建立 SSE 通道，拿到服务端给的 Acp-Connection-Id。
func (c *acpChannel) open(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.link, nil)
	if err != nil {
		cancel()
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", webUserAgent)

	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		cancel()
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return fmt.Errorf("SSE 通道返回 HTTP %d", resp.StatusCode)
	}
	cid := resp.Header.Get("Acp-Connection-Id")
	if cid == "" {
		resp.Body.Close()
		cancel()
		return errors.New("SSE 通道没有返回 Acp-Connection-Id")
	}
	c.connectionID = cid
	go c.readLoop(resp.Body)
	return nil
}

// readLoop 把 SSE 流解析成事件，统计 session/update 与 agent_message_chunk 数量。
// 流断掉/超时被静默丢弃——完成判定不依赖它，而是靠 console 状态轮询。
func (c *acpChannel) readLoop(r io.Reader) {
	defer io.Copy(io.Discard, r) //nolint:errcheck
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(payload), &msg) != nil {
			continue
		}
		if msg["method"] != "session/update" {
			continue
		}
		params, _ := msg["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		c.mu.Lock()
		c.updates++
		if update["sessionUpdate"] == "agent_message_chunk" {
			c.chunks++
		}
		c.mu.Unlock()
	}
}

// request 发一个 JSON-RPC 请求。
func (c *acpChannel) request(ctx context.Context, method string, params map[string]any, id int) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.link, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Acp-Connection-Id", c.connectionID)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", webUserAgent)

	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%s 返回 HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (c *acpChannel) close() {
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *acpChannel) counters() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updates, c.chunks
}

// -----------------------------------------------------------------------------
// 主流程
// -----------------------------------------------------------------------------

// runIntlDailyChat 国际站每日活跃：真正在网页端跑完一次 agent 会话。
//
// 国际站不存在可用的 daily-checkin 接口（网关旧实现调它会拿到「已签到」文案，
// 被误判为幂等成功——实测从未真正签到）。算数的是这条 ACP 会话。
func runIntlDailyChat(ctx context.Context, auth *StoredAuth, prof *upstreamProfile) acpTurnResult {
	started := time.Now()
	res := acpTurnResult{}

	conversation, err := acpCreateConversation(ctx, auth, prof, acpDailyChatPrompt, acpDailyChatModel)
	if err != nil {
		res.Error = "建会话失败: " + err.Error()
		return res
	}
	log.Printf("[Checkin] 网页会话已创建 id=%s 模型=%s", conversation, acpDailyChatModel)

	// 建完只是排队：取沙箱凭证，否则这一轮永远不会跑（issue #90）。
	rawSession, err := acpGetConversation(ctx, auth, prof, conversation, "/session")
	if err != nil {
		res.Error = "session 查询失败: " + err.Error()
		return res
	}
	var sess acpSession
	if err := json.Unmarshal(rawSession, &sess); err != nil {
		res.Error = "解析沙箱信息失败: " + err.Error()
		return res
	}
	link, token := sess.link(), sess.Token
	if link == "" || token == "" {
		res.Error = "沙箱未就绪（没有 link/token）"
		return res
	}
	cwd := sess.Cwd
	if cwd == "" {
		cwd = "/workspace"
	}

	channel := &acpChannel{link: link, token: token}
	defer channel.close()

	status := ""
	finished := false
	turnCtx, turnCancel := context.WithTimeout(ctx, acpTurnTimeout)
	defer turnCancel()

	if err := channel.open(turnCtx); err != nil {
		res.Error = "ACP 通道建立失败: " + err.Error()
		res.Status = acpConversationStatus(ctx, auth, prof, conversation)
		return res
	}

	// 打卡只需要 agent 自己把话说完，不暴露文件系统/终端能力。
	caps := map[string]any{
		"fs": map[string]any{
			"readTextFile":  false,
			"writeTextFile": false,
		},
		"terminal": false,
	}
	sessionID := sess.sessionID(conversation)

	if err := channel.request(turnCtx, "initialize", map[string]any{
		"protocolVersion":    acpProtocolVersion,
		"clientCapabilities": caps,
	}, 1); err != nil {
		res.Error = err.Error()
		return res
	}
	if err := channel.request(turnCtx, "session/load", map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
	}, 2); err != nil {
		res.Error = err.Error()
		return res
	}
	if err := channel.request(turnCtx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": acpDailyChatPrompt}},
	}, 3); err != nil {
		res.Error = err.Error()
		return res
	}

	// 完成判定以 console 状态为准（ACP 流只用于确认有输出在推进）。
	deadline := time.Now().Add(acpTurnTimeout)
	for time.Now().Before(deadline) {
		status = acpConversationStatus(turnCtx, auth, prof, conversation)
		if status == "completed" {
			finished = true
			break
		}
		if status == "failed" || status == "error" {
			res.Error = "会话状态=" + status
			break
		}
		select {
		case <-turnCtx.Done():
		case <-time.After(acpPollInterval):
		}
		if turnCtx.Err() != nil {
			break
		}
	}

	res.Updates, res.Chunks = channel.counters()
	res.ElapsedMS = time.Since(started).Milliseconds()
	if res.Status == "" {
		res.Status = status
	}
	if !finished && res.Error == "" {
		res.Error = fmt.Sprintf("会话在 %ss 内没有跑完（状态=%s）", acpTurnTimeout/time.Second, orUnknown(status))
	}
	res.OK = finished && res.Error == ""
	return res
}

func orUnknown(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

// urlPathEscape 对会话 id 做路径转义，避免特殊字符破坏 URL。
func urlPathEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "%20"), "/", "%2F")
}
