package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPruneOldLogs 覆盖「启动时按保留天数清理滚动日志」。
//
// 背景（2026-10-05）：pruneOldLogs 此前实现完整但**从未被调用**，导致
// config.json 的 keepDays 完全是摆设（日志目录里堆着 19 天前的旧文件）。
// 本测试锁住两件事：① 调用后确实删除超期文件；② 不误删近期文件与无关文件。
func TestPruneOldLogs(t *testing.T) {
	dir := t.TempDir()

	// 保留天数设为 7，构造三类文件：
	old := filepath.Join(dir, "gateway-2020-01-01.log")       // 远超期 → 应删
	recent := filepath.Join(dir, "debug-" + time.Now().Format("2006-01-02") + ".jsonl") // 今天 → 应留
	other := filepath.Join(dir, "serve.log")                  // 非滚动日志 → 应留
	for _, p := range []string{old, recent, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("准备测试文件失败: %v", err)
		}
	}

	setLogConfig(dir, 7)
	if got := currentLogDir(); got != dir {
		t.Fatalf("logDir 未生效: got %q want %q", got, dir)
	}

	pruneOldLogs()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("超期日志应被删除: %s (err=%v)", old, err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("当日滚动日志不应被删除: %s (err=%v)", recent, err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("非滚动日志(serve.log)不应被删除: %s (err=%v)", other, err)
	}
}

// TestPruneOldLogsDisabled 保留天数 <=0 表示不自动清理。
func TestPruneOldLogsDisabled(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "gateway-2020-01-01.log")
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备测试文件失败: %v", err)
	}

	setLogConfig(dir, 0)
	pruneOldLogs()

	if _, err := os.Stat(old); err != nil {
		t.Errorf("keepDays<=0 时不应清理任何文件: %s (err=%v)", old, err)
	}
}

// TestDefaultLogKeepDays 锁定默认保留天数为 90（主人 2026-10-05 定）。
func TestDefaultLogKeepDays(t *testing.T) {
	if defaultLogKeepDays != 90 {
		t.Errorf("defaultLogKeepDays = %d, want 90", defaultLogKeepDays)
	}
}
