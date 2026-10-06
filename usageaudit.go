package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 用量明细落盘 (usage audit)
//
// 目的：把每请求的用量明细追加写入 JSONL，供额度看板区分「探测用量」与「真实用量」。
// 调用方（客户端 / 探活脚本）在请求头带上 X-Probe: 1，本次用量即被标记为探测。
//
// 安全边界：
//   - 纯 best-effort：任何失败（建目录失败、打不开文件、marshal 失败、写失败）
//     都只打一条日志，绝不返回错误、绝不 panic、绝不影响调度主流程；
//   - 单文件写入用进程内互斥锁串行化，配合 O_APPEND 保证并发追加不互相覆盖；
//   - 文件名按北京时间（UTC+8）按天切分，与看板的自然日口径保持一致。
// -----------------------------------------------------------------------------

// usageAuditDir 是使用量明细 JSONL 的落盘目录。
// 2026-10-06 起改为 cwd 相对：serve 启动时若 config.json 配了 data.dir 会先 chdir 过去，
// 于是用量流水跟着数据目录走（可指向持久化挂载）；未配置时 cwd = 启动目录，行为同旧版。
// 注意：用量只在 serve 请求处理时写入，chdir 发生在 main() 里先于一切请求。
const usageAuditDir = "."

// beijingZone 是北京时间固定时区（UTC+8），用于按自然日切分用量文件。
var beijingZone = time.FixedZone("CST", 8*3600)

// usageAuditMu 串行化对用量 JSONL 的追加写入。
var usageAuditMu sync.Mutex

// usageAuditLine 是写入 JSONL 的单条用量明细。
// credit 为指针：上游未返回 usage.credit 时落盘为 null，避免把「未知」伪装成 0。
type usageAuditLine struct {
	T      int64    `json:"t"`
	Model  string   `json:"model"`
	Probe  bool     `json:"probe"`
	In     int64    `json:"in"`
	Out    int64    `json:"out"`
	Credit *float64 `json:"credit"`
}

// isProbeRequest 判定本次请求是否为「探测」请求。
// 约定：请求头 X-Probe 取值为 1 / true / yes（大小写不敏感，允许首尾空格）即视为探测；
// 其余任何取值（含空、0、false、no、乱码）一律按真实用量处理。
func isProbeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("X-Probe"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// recordUsageLine 把一次请求的用量明细追加到当日用量 JSONL。
// 这是 best-effort 旁路逻辑：调用方无需检查返回值，任何错误都已就地记录日志。
func recordUsageLine(r *http.Request, model string, usage map[string]any) {
	// 兜底：本函数绝不能把panic泄漏给调度主流程。
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[用量落盘] 结果=跳过 原因=内部异常 异常=%v 业务影响=本次用量未落盘，请求本身不受影响", rec)
		}
	}()

	// usage 为 nil（上游未返回用量）时仍落盘，保证每请求一条记录：
	// 此时 in/out 为 0、credit 为 null，表示「本次无可计量用量」。
	in, _ := usageNumber(usage, "prompt_tokens")
	out, _ := usageNumber(usage, "completion_tokens")
	line := usageAuditLine{
		T:     time.Now().Unix(),
		Model: model,
		Probe: isProbeRequest(r),
		In:    in,
		Out:   out,
	}
	if credit, ok := usageCredit(usage); ok {
		value := credit
		line.Credit = &value
	}

	payload, err := json.Marshal(line)
	if err != nil {
		log.Printf("[用量落盘] 结果=失败 模型=%s 原因=序列化异常 异常=%v 业务影响=本次用量未落盘，请求本身不受影响", model, err)
		return
	}
	payload = append(payload, '\n')

	day := time.Now().In(beijingZone).Format("2006-01-02")
	path := filepath.Join(usageAuditDir, "usage-"+day+".jsonl")

	usageAuditMu.Lock()
	defer usageAuditMu.Unlock()

	if mkErr := os.MkdirAll(usageAuditDir, 0o755); mkErr != nil {
		log.Printf("[用量落盘] 结果=失败 路径=%s 原因=目录不可建 异常=%v 业务影响=本次用量未落盘，请求本身不受影响", path, mkErr)
		return
	}
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		log.Printf("[用量落盘] 结果=失败 路径=%s 原因=文件打不开 异常=%v 业务影响=本次用量未落盘，请求本身不受影响", path, openErr)
		return
	}
	defer f.Close()

	if _, writeErr := f.Write(payload); writeErr != nil {
		log.Printf("[用量落盘] 结果=失败 路径=%s 原因=写入失败 异常=%v 业务影响=本次用量未落盘，请求本身不受影响", path, writeErr)
		return
	}
	log.Printf("[用量落盘] 结果=成功 路径=%s 模型=%s 探测=%t in=%d out=%d", path, model, line.Probe, in, out)
}
