package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestForwardEngineAsyncTargetsAreIsolated(t *testing.T) {
	cfg := asyncForwardTestConfig("slow", "fast")
	engine := NewForwardEngine(cfg)
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	fastDone := make(chan struct{})
	engine.sendTarget = func(_ context.Context, target *TargetEntry, _ ClipboardMessage, _ string) {
		switch target.ID {
		case "slow":
			close(slowStarted)
			<-releaseSlow
		case "fast":
			close(fastDone)
		}
	}

	if !engine.DispatchMessage("system", ClipboardMessage{UUID: "one", Type: ContentTypeText}) {
		t.Fatal("异步消息未被接受")
	}
	waitTestSignal(t, slowStarted, "慢目标开始")
	waitTestSignal(t, fastDone, "快目标完成")
	close(releaseSlow)
	shutdownForwardEngineForTest(t, engine)
}

func TestForwardEngineAsyncTargetSerializesAndKeepsLatestPending(t *testing.T) {
	cfg := asyncForwardTestConfig("only")
	engine := NewForwardEngine(cfg)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var active atomic.Int32
	var maxActive atomic.Int32
	var mu sync.Mutex
	var sent []string
	engine.sendTarget = func(_ context.Context, _ *TargetEntry, msg ClipboardMessage, _ string) {
		current := active.Add(1)
		defer active.Add(-1)
		updateAtomicMax(&maxActive, current)
		mu.Lock()
		sent = append(sent, msg.UUID)
		mu.Unlock()
		if msg.UUID == "one" {
			close(firstStarted)
			<-releaseFirst
		}
	}

	engine.DispatchMessage("system", ClipboardMessage{UUID: "one", Type: ContentTypeText})
	waitTestSignal(t, firstStarted, "第一条发送开始")
	engine.DispatchMessage("system", ClipboardMessage{UUID: "two", Type: ContentTypeText})
	engine.DispatchMessage("system", ClipboardMessage{UUID: "three", Type: ContentTypeText})
	close(releaseFirst)
	shutdownForwardEngineForTest(t, engine)

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] != "one" || sent[1] != "three" {
		t.Fatalf("发送顺序 = %v，期望 [one three]", sent)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("单目标最大并发 = %d，期望 1", got)
	}
}

func TestForwardEngineClipboardEncodingQueueKeepsFirstAndLatest(t *testing.T) {
	cfg := asyncForwardTestConfig("only")
	engine := NewForwardEngine(cfg)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var processed []uint64
	engine.processChange = func(change ClipboardChange) (*ClipboardMessage, error) {
		mu.Lock()
		processed = append(processed, change.Generation)
		mu.Unlock()
		if change.Generation == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return &ClipboardMessage{UUID: string(rune('0' + change.Generation)), Type: ContentTypeText}, nil
	}
	engine.sendTarget = func(context.Context, *TargetEntry, ClipboardMessage, string) {}

	engine.EnqueueClipboardChange(ClipboardChange{Generation: 1, Content: []byte("one")})
	waitTestSignal(t, firstStarted, "第一条消息编码开始")
	engine.EnqueueClipboardChange(ClipboardChange{Generation: 2, Content: []byte("two")})
	engine.EnqueueClipboardChange(ClipboardChange{Generation: 3, Content: []byte("three")})
	close(releaseFirst)
	shutdownForwardEngineForTest(t, engine)

	mu.Lock()
	defer mu.Unlock()
	if len(processed) != 2 || processed[0] != 1 || processed[1] != 3 {
		t.Fatalf("后台构建消息顺序 = %v，期望 [1 3]", processed)
	}
}

func TestForwardEngineShutdownCancelsInflightTarget(t *testing.T) {
	cfg := asyncForwardTestConfig("blocked")
	engine := NewForwardEngine(cfg)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	engine.sendTarget = func(ctx context.Context, _ *TargetEntry, _ ClipboardMessage, _ string) {
		close(started)
		<-ctx.Done()
		close(cancelled)
	}
	engine.DispatchMessage("system", ClipboardMessage{UUID: "one", Type: ContentTypeText})
	waitTestSignal(t, started, "阻塞目标开始")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := engine.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown 错误 = %v，期望 context deadline exceeded", err)
	}
	waitTestSignal(t, cancelled, "在途目标取消")
	if engine.DispatchMessage("system", ClipboardMessage{UUID: "late", Type: ContentTypeText}) {
		t.Fatal("关闭后的消息不应被接受")
	}
}

func asyncForwardTestConfig(targetIDs ...string) *Config {
	cfg := &Config{
		Device:     DeviceConfig{Name: "test"},
		targetByID: make(map[string]*TargetEntry),
	}
	rule := ForwardRule{From: []string{"system"}}
	for _, id := range targetIDs {
		target := TargetEntry{ID: id}
		cfg.Targets = append(cfg.Targets, target)
		cfg.targetByID[id] = &cfg.Targets[len(cfg.Targets)-1]
		rule.To = append(rule.To, id)
	}
	// Rebuild pointers after all appends in case the slice moved.
	for i := range cfg.Targets {
		cfg.targetByID[cfg.Targets[i].ID] = &cfg.Targets[i]
	}
	cfg.Forward = []ForwardRule{rule}
	return cfg
}

func shutdownForwardEngineForTest(t *testing.T, engine *ForwardEngine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("关闭 ForwardEngine: %v", err)
	}
}

func waitTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("等待%s超时", description)
	}
}

func updateAtomicMax(maximum *atomic.Int32, value int32) {
	for {
		current := maximum.Load()
		if value <= current || maximum.CompareAndSwap(current, value) {
			return
		}
	}
}
