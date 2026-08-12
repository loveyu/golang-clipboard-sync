package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"sync/atomic"
	"testing"
	"time"
)

func testClipboardConfig() ClipboardConfig {
	cfg := defaultClipboardConfig()
	cfg.ReadTimeoutMS = 500
	cfg.ImageReadDelayMS = 0
	return cfg
}

func startTestClipboardProcessor(t *testing.T, cfg ClipboardConfig) (*clipboardProcessor, chan ClipboardChange, chan struct{}, chan struct{}) {
	t.Helper()
	changes := make(chan ClipboardChange, 16)
	stop := make(chan struct{})
	done := make(chan struct{})
	processor := newClipboardProcessor(cfg, changes)
	go func() {
		defer close(done)
		processor.Run(stop)
	}()
	return processor, changes, stop, done
}

func stopTestClipboardProcessor(t *testing.T, stop chan struct{}, done chan struct{}) {
	t.Helper()
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("clipboardProcessor 停止超时")
	}
}

func TestClipboardProcessorCoalescesDirtyGenerationsAndReadsSerially(t *testing.T) {
	processor, changes, stop, done := startTestClipboardProcessor(t, testClipboardConfig())
	defer stopTestClipboardProcessor(t, stop, done)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var reads atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32

	reader := func(content string, block bool) func(context.Context, string, int64) (string, []byte, error) {
		return func(ctx context.Context, _ string, _ int64) (string, []byte, error) {
			reads.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for {
				maximum := maxActive.Load()
				if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
					break
				}
			}
			if block {
				close(firstStarted)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return "", nil, ctx.Err()
				}
			}
			return "text/plain", []byte(content), nil
		}
	}

	processor.Notify(clipboardPlatformEvent{Generation: 1, Backend: "test", Read: reader("first", true)})
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("第一次读取未开始")
	}
	for generation := uint64(2); generation <= 10; generation++ {
		content := "stale"
		if generation == 10 {
			content = "latest"
		}
		processor.Notify(clipboardPlatformEvent{Generation: generation, Backend: "test", Read: reader(content, false)})
	}
	close(releaseFirst)

	first := waitClipboardChange(t, changes)
	latest := waitClipboardChange(t, changes)
	if string(first.Content) != "first" || string(latest.Content) != "latest" {
		t.Fatalf("处理内容 = %q, %q，期望 first, latest", first.Content, latest.Content)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("读取次数 = %d，期望 2", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("最大并发读取数 = %d，期望 1", got)
	}
}

func TestClipboardProcessorRawDedupWindow(t *testing.T) {
	processor, changes, stop, done := startTestClipboardProcessor(t, testClipboardConfig())
	defer stopTestClipboardProcessor(t, stop, done)

	processor.Notify(staticClipboardEvent(1, "text/plain", []byte("same")))
	waitClipboardChange(t, changes)
	processor.Notify(staticClipboardEvent(2, "text/plain", []byte("same")))
	assertNoClipboardChange(t, changes)
	processor.Notify(staticClipboardEvent(3, "text/plain", []byte("different")))
	if got := string(waitClipboardChange(t, changes).Content); got != "different" {
		t.Fatalf("内容 = %q，期望 different", got)
	}
}

func TestClipboardProcessorPixelExactDedup(t *testing.T) {
	processor, changes, stop, done := startTestClipboardProcessor(t, testClipboardConfig())
	defer stopTestClipboardProcessor(t, stop, done)

	base := image.NewRGBA(image.Rect(0, 0, 2, 2))
	base.Set(0, 0, color.RGBA{R: 255, A: 255})
	first := encodeTestPNG(t, base, png.NoCompression)
	second := encodeTestPNG(t, base, png.BestCompression)
	if bytes.Equal(first, second) {
		t.Fatal("测试 PNG 编码应具有不同原始字节")
	}

	processor.Notify(staticClipboardEvent(1, "image/png", first))
	waitClipboardChange(t, changes)
	processor.Notify(staticClipboardEvent(2, "image/png", second))
	assertNoClipboardChange(t, changes)

	changed := image.NewRGBA(image.Rect(0, 0, 2, 2))
	changed.Set(0, 0, color.RGBA{R: 254, A: 255})
	third := encodeTestPNG(t, changed, png.BestCompression)
	processor.Notify(staticClipboardEvent(3, "image/png", third))
	waitClipboardChange(t, changes)
}

func TestClipboardProcessorSuppressesContentBoundEcho(t *testing.T) {
	processor, changes, stop, done := startTestClipboardProcessor(t, testClipboardConfig())
	defer stopTestClipboardProcessor(t, stop, done)

	token := "local-write"
	processor.registerEcho(token, "text/plain", []byte("remote"))
	processor.completeEcho(token, 20, true)
	processor.Notify(staticClipboardEvent(20, "text/plain", []byte("remote")))
	assertNoClipboardChange(t, changes)
	processor.Notify(staticClipboardEvent(21, "text/plain", []byte("remote")))
	assertNoClipboardChange(t, changes)

	processor.Notify(staticClipboardEvent(22, "text/plain", []byte("user")))
	if got := string(waitClipboardChange(t, changes).Content); got != "user" {
		t.Fatalf("内容 = %q，期望 user", got)
	}
}

func TestClipboardProcessorSuppressesOriginBeforeImageRead(t *testing.T) {
	cfg := testClipboardConfig()
	cfg.ImageReadDelayMS = 0
	processor, changes, stop, done := startTestClipboardProcessor(t, cfg)
	defer stopTestClipboardProcessor(t, stop, done)

	const token = "remote-image-write"
	processor.registerEcho(token, "image/png", []byte("remote-image"))
	processor.completeEcho(token, 20, true)
	var markerReads atomic.Int32
	var imageReads atomic.Int32
	processor.Notify(clipboardPlatformEvent{
		Generation: 20,
		MIMEs:      []string{clipboardOriginMIME, "image/png"},
		Backend:    "test",
		Read: func(_ context.Context, mime string, _ int64) (string, []byte, error) {
			if mime == clipboardOriginMIME {
				markerReads.Add(1)
				return mime, []byte(token), nil
			}
			imageReads.Add(1)
			return mime, []byte("remote-image"), nil
		},
	})
	waitForAtomicValue(t, &markerReads, 1)
	assertNoClipboardChange(t, changes)
	if got := imageReads.Load(); got != 0 {
		t.Fatalf("来源标记匹配后仍读取图片 %d 次", got)
	}
}

func TestClipboardProcessorImageDelayOnlyAppliesToImages(t *testing.T) {
	cfg := testClipboardConfig()
	cfg.ImageReadDelayMS = 200
	processor := newClipboardProcessor(cfg, make(chan ClipboardChange, 1))
	if got := processor.eventDelay(clipboardPlatformEvent{MIMEs: []string{"image/png"}}); got != 200*time.Millisecond {
		t.Fatalf("图片读取延迟 = %v，期望 200ms", got)
	}
	if got := processor.eventDelay(clipboardPlatformEvent{MIMEs: []string{"text/plain"}}); got != 0 {
		t.Fatalf("文本读取延迟 = %v，期望 0", got)
	}
	if got := processor.eventDelay(clipboardPlatformEvent{MIMEs: []string{"image/png"}, Debounce: 300 * time.Millisecond}); got != 300*time.Millisecond {
		t.Fatalf("已有更长防抖时延迟 = %v，期望 300ms", got)
	}
}

func TestClipboardProcessorReadTimeoutAndLimit(t *testing.T) {
	cfg := testClipboardConfig()
	cfg.MaxContentBytes = 1024 * 1024
	processor, changes, stop, done := startTestClipboardProcessor(t, cfg)
	defer stopTestClipboardProcessor(t, stop, done)

	processor.Notify(clipboardPlatformEvent{
		Generation: 1,
		Backend:    "test",
		Read: func(ctx context.Context, _ string, _ int64) (string, []byte, error) {
			<-ctx.Done()
			return "text/plain", nil, ctx.Err()
		},
	})
	assertNoClipboardChangeFor(t, changes, 650*time.Millisecond)

	processor.Notify(clipboardPlatformEvent{
		Generation: 2,
		Backend:    "test",
		Read: func(context.Context, string, int64) (string, []byte, error) {
			return "text/plain", make([]byte, cfg.MaxContentBytes+1), nil
		},
	})
	assertNoClipboardChange(t, changes)
}

func TestSelectClipboardMIMEPriority(t *testing.T) {
	mimes := []string{"STRING", "text/plain;charset=iso-8859-1", "image/jpeg", "image/png", "text/plain;charset=utf-8"}
	if got := selectClipboardMIME(mimes); got != "image/png" {
		t.Fatalf("MIME = %q，期望 image/png", got)
	}
	if got := selectClipboardMIME([]string{"STRING", "text/plain;charset=utf-8"}); got != "text/plain;charset=utf-8" {
		t.Fatalf("MIME = %q，期望 UTF-8 text/plain", got)
	}
}

func staticClipboardEvent(generation uint64, mime string, content []byte) clipboardPlatformEvent {
	return clipboardPlatformEvent{
		Generation: generation,
		MIMEs:      []string{mime},
		Backend:    "test",
		Read: func(context.Context, string, int64) (string, []byte, error) {
			return mime, append([]byte(nil), content...), nil
		},
	}
}

func encodeTestPNG(t *testing.T, img image.Image, level png.CompressionLevel) []byte {
	t.Helper()
	var output bytes.Buffer
	encoder := png.Encoder{CompressionLevel: level}
	if err := encoder.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func waitClipboardChange(t *testing.T, changes <-chan ClipboardChange) ClipboardChange {
	t.Helper()
	select {
	case change := <-changes:
		return change
	case <-time.After(time.Second):
		t.Fatal("等待剪贴板变更超时")
		return ClipboardChange{}
	}
}

func assertNoClipboardChange(t *testing.T, changes <-chan ClipboardChange) {
	t.Helper()
	assertNoClipboardChangeFor(t, changes, 100*time.Millisecond)
}

func assertNoClipboardChangeFor(t *testing.T, changes <-chan ClipboardChange, duration time.Duration) {
	t.Helper()
	select {
	case change := <-changes:
		t.Fatalf("意外收到剪贴板变更: mime=%s bytes=%d", change.Mime, len(change.Content))
	case <-time.After(duration):
	}
}

func waitForAtomicValue(t *testing.T, value *atomic.Int32, wanted int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value.Load() == wanted {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("原子值 = %d，期望 %d", value.Load(), wanted)
}
