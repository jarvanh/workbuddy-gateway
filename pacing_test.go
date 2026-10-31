package main

import (
	"context"
	"testing"
	"time"
)

// paceTestWindow 写入秒级小窗口并在结束后恢复原值，保证测试毫秒级完成。
func paceTestWindow(t *testing.T, rotation, background paceSetting) {
	t.Helper()
	oldRotation, oldBackground := rotationPace(), backgroundGap()
	setPaceConfig(rotation, background)
	t.Cleanup(func() { setPaceConfig(oldRotation, oldBackground) })
}

func TestPaceSanitize(t *testing.T) {
	if got := (paceSetting{5, 2}).sanitize(); got.min != 2 || got.max != 5 {
		t.Fatal("min>max must swap")
	}
	if got := (paceSetting{-1, -3}).sanitize(); got.min != 0 || got.max != 0 {
		t.Fatal("negatives must clamp to zero (disabled)")
	}
	if got := (paceSetting{0, 0}).sanitize(); got.min != 0 || got.max != 0 {
		t.Fatal("zero window means disabled")
	}
}

func TestPaceAccountRotationDisabledByDefault(t *testing.T) {
	isolatedGatewayConfig(t)
	if rotationPace().max != 0 || backgroundGap().max != 0 {
		t.Fatal("pace windows must default to disabled (legacy behavior)")
	}
	start := time.Now()
	paceAccountRotation(context.Background(), time.Now().Add(-time.Hour))
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("disabled pace must not wait")
	}
}

func TestPaceAccountRotationWaitsRandomWindow(t *testing.T) {
	isolatedGatewayConfig(t)
	paceTestWindow(t, paceSetting{10 * time.Millisecond, 40 * time.Millisecond}, paceSetting{})
	// since 在过去 5ms：等待应被扣除到 [5ms,35ms]，且必然非零。
	since := time.Now().Add(-5 * time.Millisecond)
	start := time.Now()
	paceAccountRotation(context.Background(), since)
	elapsed := time.Since(start)
	if elapsed <= 2*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("rotation wait out of window: %v", elapsed)
	}
	// since 已超出整窗（上一发耗时足够）：不再等待。
	since = time.Now().Add(-time.Hour)
	start = time.Now()
	paceAccountRotation(context.Background(), since)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("elapsed beyond window must skip wait")
	}
	// lastDispatch 为零（上一轮没发出请求）：不等待。
	start = time.Now()
	paceAccountRotation(context.Background(), time.Time{})
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("zero since must skip wait")
	}
}

func TestPaceAccountRotationRespectsContextCancel(t *testing.T) {
	isolatedGatewayConfig(t)
	paceTestWindow(t, paceSetting{5 * time.Second, 6 * time.Second}, paceSetting{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	paceAccountRotation(ctx, time.Now())
	if time.Since(start) > time.Second {
		t.Fatal("cancel must interrupt the pace wait")
	}
}

func TestPaceConfigHotReload(t *testing.T) {
	isolatedGatewayConfig(t)
	writeRuntimeFixture(t, `{"upstream":{"requestProfile":"official","rotationPaceMinSeconds":1,"rotationPaceMaxSeconds":3,"backgroundTaskGapMinSeconds":2,"backgroundTaskGapMaxSeconds":4,"deviceToken":"pipeline-token"}}`)
	watcher := runtimeConfigWatcher{}
	if !watcher.reload() {
		t.Fatal("config reload failed")
	}
	if got := rotationPace(); got.min != time.Second || got.max != 3*time.Second {
		t.Fatalf("rotation pace not applied: %v", got)
	}
	if got := backgroundGap(); got.min != 2*time.Second || got.max != 4*time.Second {
		t.Fatalf("background gap not applied: %v", got)
	}
	if deviceTokenSnapshot() != "pipeline-token" {
		t.Fatal("device token not applied")
	}
	writeRuntimeFixture(t, `{}`)
	if !watcher.reload() {
		t.Fatal("reset reload failed")
	}
	if rotationPace().max != 0 || backgroundGap().max != 0 || deviceTokenSnapshot() != "" {
		t.Fatal("missing fields must reset to disabled (no stale residue)")
	}
}

func TestBackgroundJitter(t *testing.T) {
	isolatedGatewayConfig(t)
	if backgroundJitter() != 0 {
		t.Fatal("jitter must be zero when disabled")
	}
	paceTestWindow(t, paceSetting{0, 0}, paceSetting{10 * time.Millisecond, 20 * time.Millisecond})
	for i := 0; i < 20; i++ {
		got := backgroundJitter()
		if got < 10*time.Millisecond || got > 20*time.Millisecond {
			t.Fatalf("jitter out of window: %v", got)
		}
	}
}

func TestPaceRequestRotationEndToEnd(t *testing.T) {
	isolatedGatewayConfig(t)
	paceTestWindow(t, paceSetting{15 * time.Millisecond, 30 * time.Millisecond}, paceSetting{})
	// 直呼 paceSleep 级别的语义已在上面覆盖；这里确认换号入口在配置窗口下确实等待。
	since := time.Now()
	start := time.Now()
	paceAccountRotation(context.Background(), since)
	elapsed := time.Since(start)
	if elapsed < 10*time.Millisecond {
		t.Fatalf("rotation pace window not applied: %v", elapsed)
	}
}
