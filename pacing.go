package main

// pacing.go —— 出站节奏治理（11140 风控加固，配置显式启用）
//
// 上游风控（11140「账号存在异常行为」）的触发画像之一是「程序化轮询」：
// 被拒后立即换下一个账号连发、后台任务逐账号无缝推进。hub #422 的整池连坐
// 事故与维护者复盘指向同一处置：所有「换目标再打」的路径都必须有随机间隔
// （hub #394 的 2–5s 错峰是社区验证过的起步值）。
//
// 三处落点（默认全部关闭 = 旧行为，由 config.json 的 upstream 段显式启用，
// 与 requestProfile 同一套 opt-in 模式；生产示例配置给出推荐值 2–5s）：
//  1. 模型请求换号轮询：paceAccountRotation（rotationPaceMin/MaxSeconds）；
//  2. warmup / 价格探测的逐账号循环：paceBackgroundTask（backgroundTaskGapMin/Max）；
//  3. 续期保活：保留固定 1s 基础间隔（keepaliveAccountGap），在其上叠加
//     backgroundJitter() 的随机量，把「每号精确 1s」的固定特征抹掉。
//
// 约束：窗口 {0,0}（默认/显式归零）= 关闭；min>max 自动交换；等待上限由 max 兜住。

import (
	"context"
	"math/rand"
	"time"
)

var (
	rotationPaceMin      time.Duration
	rotationPaceMax      time.Duration
	backgroundTaskGapMin time.Duration
	backgroundTaskGapMax time.Duration
)

// paceSetting 是一对间隔上下界；零值表示该路径的节奏等待关闭。
type paceSetting struct {
	min, max time.Duration
}

// sanitize 把负值钳 0、min>max 交换；{0,0} 保持为「关闭」语义。
func (p paceSetting) sanitize() paceSetting {
	if p.min < 0 {
		p.min = 0
	}
	if p.max < 0 {
		p.max = 0
	}
	if p.max > 0 && p.min > p.max {
		p.min, p.max = p.max, p.min
	}
	return p
}

func secondsDuration(seconds *int) time.Duration {
	if seconds == nil || *seconds <= 0 {
		return 0
	}
	return time.Duration(*seconds) * time.Second
}

func setPaceConfig(rotation, background paceSetting) {
	rotation = rotation.sanitize()
	background = background.sanitize()
	upstreamConfigMu.Lock()
	rotationPaceMin, rotationPaceMax = rotation.min, rotation.max
	backgroundTaskGapMin, backgroundTaskGapMax = background.min, background.max
	upstreamConfigMu.Unlock()
}

func rotationPace() paceSetting {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return paceSetting{rotationPaceMin, rotationPaceMax}
}

func backgroundGap() paceSetting {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return paceSetting{backgroundTaskGapMin, backgroundTaskGapMax}
}

// paceSleep 等待 [min,max] 内的随机时长；since 非零时扣除该时刻以来已消耗的
// 时间（换号重试里上一发上游请求本身也占用了间隔的一部分）。ctx 取消立即返回。
func paceSleep(ctx context.Context, window paceSetting, since time.Time) {
	if window.max <= 0 || window.max < window.min {
		return
	}
	wait := window.min
	if span := window.max - window.min; span > 0 {
		wait += time.Duration(rand.Int63n(int64(span) + 1))
	}
	if !since.IsZero() {
		elapsed := time.Since(since)
		if elapsed >= wait {
			return
		}
		wait -= elapsed
	}
	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// paceAccountRotation 在模型请求换号重试前等待随机间隔；since 传上一次实际
// 发出上游请求的时刻，lastDispatch 为零（上一轮没发出过请求）时不等待。
// 未配置节奏窗口时是 no-op（旧行为）。
func paceAccountRotation(ctx context.Context, since time.Time) {
	if since.IsZero() {
		return
	}
	paceSleep(ctx, rotationPace(), since)
}

// paceBackgroundTask 在后台任务（warmup / 价格探测）逐账号循环之间等待随机
// 间隔。未配置时是 no-op。用独立背景 context：后台任务的退出由各自的循环
// 条件与限次兜底，不绑定某个 HTTP 请求的生命周期。
func paceBackgroundTask() {
	paceSleep(context.Background(), backgroundGap(), time.Time{})
}

// backgroundJitter 返回 [min,max] 内的随机附加间隔；未配置时为 0。
// 供已有固定间隔的循环（续期保活 1s）叠加，抹平「精确等长」的机器特征。
func backgroundJitter() time.Duration {
	window := backgroundGap()
	if window.max <= 0 || window.max < window.min {
		return 0
	}
	if span := window.max - window.min; span > 0 {
		return window.min + time.Duration(rand.Int63n(int64(span)+1))
	}
	return window.min
}
