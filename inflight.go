package main

// inflight.go —— 进程级在途请求计数（当前值 + 进程内峰值）
//
// 看板「实时/并发」小卡的数据真源。只统计真正打到本网关的 HTTP 请求
//（不含 health/models 等观测端点），用原子计数避免锁竞争；
// 峰值是**进程启动以来**的最大值，重启归零，语义与 autoclaw 的
// guard.inflight / max_concurrency 一致。

import "sync/atomic"

var (
	gatewayInflight     atomic.Int64
	gatewayInflightPeak atomic.Int64
)

// beginInflight 在请求入口调用，返回本次的当前在途数并刷新峰值。
func beginInflight() int64 {
	cur := gatewayInflight.Add(1)
	for {
		old := gatewayInflightPeak.Load()
		if cur <= old || gatewayInflightPeak.CompareAndSwap(old, cur) {
			break
		}
	}
	return cur
}

// endInflight 必须在请求出口 defer 调用，漏一次会永久虚高。
func endInflight() { gatewayInflight.Add(-1) }

// inflightSnapshot 供 /health 输出，不含任何请求身份信息。
func inflightSnapshot() map[string]any {
	limits, total, _ := modelConcurrency.snapshot("")
	// 上限口径（2026-11-06 主人定）：MaxGlobal>0 用全局闸；否则只报**每账号**
	// 并发闸 MaxAcc。不再用 MaxAcc×账号数 推导全局值 —— 活跃账号数随时间变
	// （intl 号被风控后 9→4），推导值会把失效容量也算进去，误导。
	cap := limits.MaxGlobal
	if cap == 0 && limits.MaxAcc > 0 {
		cap = limits.MaxAcc
	}
	return map[string]any{
		"inflight":        gatewayInflight.Load(),
		"inflight_peak":   gatewayInflightPeak.Load(),
		"max_concurrency": cap,
		"max_per_account": limits.MaxAcc,
		"max_global":      limits.MaxGlobal,
		"limiter_active":  total,
	}
}
