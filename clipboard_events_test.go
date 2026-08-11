package main

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestClipboardEventsAreCoalescedAndProcessedSerially(t *testing.T) {
	events := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	started := make(chan int32, 2)
	release := make(chan struct{}, 2)

	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32

	go func() {
		defer close(done)
		consumeClipboardEvents(events, stop, 10*time.Millisecond, func() {
			call := calls.Add(1)
			currentActive := active.Add(1)
			for {
				currentMax := maxActive.Load()
				if currentActive <= currentMax || maxActive.CompareAndSwap(currentMax, currentActive) {
					break
				}
			}
			started <- call
			<-release
			active.Add(-1)
		})
	}()

	for range 5 {
		notifyClipboardEvent(events)
	}
	waitForWorkerCall(t, started, 1)

	// 第一次处理未结束时继续投递多个事件，只应合并成下一次串行处理。
	for range 5 {
		notifyClipboardEvent(events)
	}
	release <- struct{}{}
	waitForWorkerCall(t, started, 2)
	release <- struct{}{}

	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("剪贴板事件工作器停止超时")
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("事件处理次数 = %d，期望 2", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("事件处理发生并发，最大并发数 = %d", got)
	}
}

func waitForWorkerCall(t *testing.T, started <-chan int32, want int32) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("工作器调用序号 = %d，期望 %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("等待第 %d 次工作器调用超时", want)
	}
}
