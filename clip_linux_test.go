//go:build linux && !android

package main

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestListenClipboardChangesStopsMonitorBeforeTimedRestart(t *testing.T) {
	stop := make(chan struct{})
	var starts atomic.Int32
	var stops atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32

	startMonitor := func(func()) *clipboardMonitor {
		starts.Add(1)
		currentActive := active.Add(1)
		for {
			currentMax := maxActive.Load()
			if currentActive <= currentMax || maxActive.CompareAndSwap(currentMax, currentActive) {
				break
			}
		}

		return &clipboardMonitor{
			errCh: make(chan error),
			stopFn: func() {
				active.Add(-1)
				stops.Add(1)
			},
		}
	}

	processor := newClipboardProcessor(defaultClipboardConfig(), make(chan ClipboardChange, 1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCommandClipboardSource(processor, stop, 10*time.Millisecond, startMonitor)
	}()
	waitForCondition(t, time.Second, func() bool { return starts.Load() >= 3 })
	close(stop)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("监听器停止超时")
	}

	if got := maxActive.Load(); got != 1 {
		t.Fatalf("监听器发生重叠，最大并发数 = %d，期望 1", got)
	}
	if gotStarts, gotStops := starts.Load(), stops.Load(); gotStarts != gotStops {
		t.Fatalf("监听器未全部回收：启动 %d 次，停止 %d 次", gotStarts, gotStops)
	}
	if got := active.Load(); got != 0 {
		t.Fatalf("停止后仍有 %d 个活动监听器", got)
	}
}

func TestClipboardMonitorStopIsIdempotent(t *testing.T) {
	var stops atomic.Int32
	monitor := &clipboardMonitor{stopFn: func() { stops.Add(1) }}

	monitor.Stop()
	monitor.Stop()

	if got := stops.Load(); got != 1 {
		t.Fatalf("Stop 执行了 %d 次，期望 1 次", got)
	}
}

func TestWaylandMonitorTimedRestartDoesNotLeakProcess(t *testing.T) {
	if os.Getenv("CLIPBOARD_WAYLAND_INTEGRATION") != "1" {
		t.Skip("设置 CLIPBOARD_WAYLAND_INTEGRATION=1 后运行 Wayland 集成测试")
	}
	if !isWayland {
		t.Skip("当前不是 Wayland 会话")
	}

	stop := make(chan struct{})
	var starts atomic.Int32
	var badProcessCount atomic.Bool
	var observedProcessCount atomic.Int32
	startMonitor := func(notify func()) *clipboardMonitor {
		monitor := startClipboardMonitorPipe(notify)
		processCount := countDirectChildProcesses("wl-paste")
		if processCount != 1 {
			observedProcessCount.Store(int32(processCount))
			badProcessCount.Store(true)
		}
		starts.Add(1)
		return monitor
	}

	processor := newClipboardProcessor(defaultClipboardConfig(), make(chan ClipboardChange, 1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCommandClipboardSource(processor, stop, 25*time.Millisecond, startMonitor)
	}()
	waitForCondition(t, 2*time.Second, func() bool { return starts.Load() >= 4 })

	if badProcessCount.Load() {
		close(stop)
		<-done
		t.Fatalf("监听器启动后的 wl-paste 子进程数 = %d，期望 1", observedProcessCount.Load())
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wayland 监听器停止超时")
	}
	waitForCondition(t, 2*time.Second, func() bool {
		return countDirectChildProcesses("wl-paste") == 0
	})
}

func countDirectChildProcesses(processName string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}

	count := 0
	parentPID := os.Getpid()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}

		status, err := os.ReadFile("/proc/" + entry.Name() + "/status")
		if err != nil {
			continue
		}

		name := ""
		ppid := -1
		for _, line := range strings.Split(string(status), "\n") {
			if value, ok := strings.CutPrefix(line, "Name:\t"); ok {
				name = strings.TrimSpace(value)
			}
			if value, ok := strings.CutPrefix(line, "PPid:\t"); ok {
				ppid, _ = strconv.Atoi(strings.TrimSpace(value))
			}
		}
		if name == processName && ppid == parentPID {
			count++
		}
	}
	return count
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("等待条件满足超时")
}
