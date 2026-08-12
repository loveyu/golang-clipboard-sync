package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

var errClipboardContentTooLarge = errors.New("clipboard content exceeds configured limit")

const (
	clipboardOriginMIME     = "application/x-clipboard-sync-origin"
	clipboardOriginMaxBytes = 1024
)

// clipboardPlatformEvent describes a selection without reading its contents.
// Platform callbacks only publish this lightweight value; Read is invoked by
// the single clipboardProcessor worker.
type clipboardPlatformEvent struct {
	Generation uint64
	MIMEs      []string
	Backend    string
	Debounce   time.Duration
	Read       func(context.Context, string, int64) (string, []byte, error)
	Release    func()
}

type clipboardEventQueue struct {
	mu      sync.Mutex
	latest  clipboardPlatformEvent
	pending bool
	merged  int
	signal  chan struct{}
}

func newClipboardEventQueue() *clipboardEventQueue {
	return &clipboardEventQueue{signal: make(chan struct{}, 1)}
}

func (q *clipboardEventQueue) notify(event clipboardPlatformEvent) {
	q.mu.Lock()
	var release func()
	if q.pending {
		release = q.latest.Release
		q.merged++
	}
	q.latest = event
	q.pending = true
	q.mu.Unlock()

	if release != nil {
		release()
	}
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *clipboardEventQueue) take() (clipboardPlatformEvent, int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.pending {
		return clipboardPlatformEvent{}, 0, false
	}
	event := q.latest
	merged := q.merged
	q.latest = clipboardPlatformEvent{}
	q.pending = false
	q.merged = 0
	return event, merged, true
}

func (q *clipboardEventQueue) releasePending() {
	q.mu.Lock()
	release := q.latest.Release
	q.latest = clipboardPlatformEvent{}
	q.pending = false
	q.merged = 0
	q.mu.Unlock()
	if release != nil {
		release()
	}
}

type clipboardFingerprintEntry struct {
	kind       string
	raw        [sha256.Size]byte
	at         time.Time
	imageKey   clipboardImageKey
	imageKeyOK bool
	pixel      [sha256.Size]byte
	pixelOK    bool
	pixelTried bool
	imageData  []byte
}

type clipboardImageKey struct {
	format        string
	width, height int
}

type clipboardEchoEntry struct {
	token           string
	kind            string
	raw             [sha256.Size]byte
	startGeneration uint64
	generation      uint64
	expiresAt       time.Time
}

type clipboardDedupTask struct {
	change  ClipboardChange
	raw     [sha256.Size]byte
	backend string
	read    time.Duration
	merged  int
}

// clipboardDedupQueue keeps at most one not-yet-started task. If exact image
// comparison is still running, newer clipboard contents replace stale pending
// contents so capture callbacks never wait for image decoding.
type clipboardDedupQueue struct {
	mu         sync.Mutex
	ready      clipboardDedupTask
	readySet   bool
	working    bool
	pending    clipboardDedupTask
	pendingSet bool
	closed     bool
	signal     chan struct{}
}

func newClipboardDedupQueue() *clipboardDedupQueue {
	return &clipboardDedupQueue{signal: make(chan struct{}, 1)}
}

func (q *clipboardDedupQueue) notify(task clipboardDedupTask) (clipboardDedupTask, bool) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return clipboardDedupTask{}, false
	}
	var replaced clipboardDedupTask
	var hadPending bool
	if !q.working && !q.readySet {
		q.ready = task
		q.readySet = true
	} else {
		replaced = q.pending
		hadPending = q.pendingSet
		q.pending = task
		q.pendingSet = true
	}
	q.mu.Unlock()

	select {
	case q.signal <- struct{}{}:
	default:
	}
	return replaced, hadPending
}

func (q *clipboardDedupQueue) take() (clipboardDedupTask, bool, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.readySet {
		return clipboardDedupTask{}, false, q.closed
	}
	task := q.ready
	q.ready = clipboardDedupTask{}
	q.readySet = false
	q.working = true
	return task, true, false
}

func (q *clipboardDedupQueue) complete() {
	q.mu.Lock()
	q.working = false
	if q.pendingSet {
		q.ready = q.pending
		q.readySet = true
		q.pending = clipboardDedupTask{}
		q.pendingSet = false
	}
	hasReady := q.readySet
	q.mu.Unlock()
	if hasReady {
		select {
		case q.signal <- struct{}{}:
		default:
		}
	}
}

func (q *clipboardDedupQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

type clipboardProcessor struct {
	cfg    ClipboardConfig
	queue  *clipboardEventQueue
	output chan<- ClipboardChange
	now    func() time.Time

	fingerprints   []clipboardFingerprintEntry
	lastGeneration uint64
	hmacKey        [32]byte
	hmacReady      bool
	activeReads    atomic.Int32
	maxReads       atomic.Int32
	dedupQueue     *clipboardDedupQueue
	dedupDone      chan struct{}
	pixelHash      func([]byte) ([sha256.Size]byte, bool)

	echoMu sync.Mutex
	echo   *clipboardEchoEntry
}

func newClipboardProcessor(cfg ClipboardConfig, output chan<- ClipboardChange) *clipboardProcessor {
	p := &clipboardProcessor{
		cfg:        cfg,
		queue:      newClipboardEventQueue(),
		output:     output,
		now:        time.Now,
		dedupQueue: newClipboardDedupQueue(),
		dedupDone:  make(chan struct{}),
		pixelHash:  clipboardPixelFingerprint,
	}
	if _, err := rand.Read(p.hmacKey[:]); err != nil {
		log.Printf("[CLIPBOARD] 生成调试摘要密钥失败: %v", err)
	} else {
		p.hmacReady = true
	}
	return p
}

func (p *clipboardProcessor) Notify(event clipboardPlatformEvent) {
	p.queue.notify(event)
}

func (p *clipboardProcessor) Run(stop <-chan struct{}) {
	go p.runDedupWorker(stop)
	defer func() {
		p.dedupQueue.close()
		<-p.dedupDone
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-stop
		cancel()
	}()
	defer p.queue.releasePending()
	for {
		select {
		case <-p.queue.signal:
		case <-stop:
			return
		}

		event, merged, ok := p.queue.take()
		if !ok {
			continue
		}
		if delay := p.eventDelay(event); delay > 0 {
			event.Debounce = delay
			event, merged, ok = p.waitForQuietPeriod(stop, event, merged)
			if !ok {
				return
			}
		}
		p.process(ctx, stop, event, merged)
	}
}

func (p *clipboardProcessor) runDedupWorker(stop <-chan struct{}) {
	defer close(p.dedupDone)
	for {
		select {
		case <-p.dedupQueue.signal:
		case <-stop:
			return
		}

		for {
			task, ok, closed := p.dedupQueue.take()
			if !ok {
				if closed {
					return
				}
				break
			}
			kind := clipboardContentKind(task.change.Mime)
			if reason := p.duplicateReason(kind, task.raw, task.change.Content); reason != "" {
				p.logSkip(reason, task.backend, task.change.Mime, len(task.change.Content), task.read, task.merged)
				p.dedupQueue.complete()
				continue
			}

			if debugClipboard {
				log.Printf("[CLIPBOARD] backend=%s mime=%s bytes=%d read=%v merged=%d id=%s maxConcurrentReads=%d",
					task.backend, task.change.Mime, len(task.change.Content), task.read, task.merged,
					p.debugDigest(kind, task.change.Content), p.maxReads.Load())
			} else {
				log.Printf("[CLIPBOARD] backend=%s mime=%s bytes=%d read=%v merged=%d",
					task.backend, task.change.Mime, len(task.change.Content), task.read, task.merged)
			}
			select {
			case p.output <- task.change:
				p.dedupQueue.complete()
			case <-stop:
				return
			}
		}
	}
}

func (p *clipboardProcessor) eventDelay(event clipboardPlatformEvent) time.Duration {
	delay := event.Debounce
	if clipboardContentKind(selectClipboardMIME(event.MIMEs)) == "image" {
		imageDelay := time.Duration(p.cfg.ImageReadDelayMS) * time.Millisecond
		if imageDelay > delay {
			delay = imageDelay
		}
	}
	return delay
}

func (p *clipboardProcessor) waitForQuietPeriod(stop <-chan struct{}, event clipboardPlatformEvent, merged int) (clipboardPlatformEvent, int, bool) {
	timer := time.NewTimer(event.Debounce)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return event, merged, true
		case <-p.queue.signal:
			latest, moreMerged, ok := p.queue.take()
			if !ok {
				continue
			}
			if event.Release != nil {
				event.Release()
			}
			event = latest
			merged += moreMerged + 1
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(event.Debounce)
		case <-stop:
			if event.Release != nil {
				event.Release()
			}
			return clipboardPlatformEvent{}, 0, false
		}
	}
}

func (p *clipboardProcessor) process(parent context.Context, stop <-chan struct{}, event clipboardPlatformEvent, merged int) {
	if event.Release != nil {
		defer event.Release()
	}
	if event.Read == nil {
		return
	}
	if event.Generation != 0 && event.Generation == p.lastGeneration {
		p.logSkip("duplicate-generation", event.Backend, "", 0, 0, merged)
		return
	}
	if event.Generation != 0 {
		p.lastGeneration = event.Generation
	}

	requestedMIME := selectClipboardMIME(event.MIMEs)
	if len(event.MIMEs) > 0 && requestedMIME == "" {
		p.logSkip("unsupported", event.Backend, "", 0, 0, merged)
		return
	}

	timeout := time.Duration(p.cfg.ReadTimeoutMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if clipboardMIMEAvailable(event.MIMEs, clipboardOriginMIME) {
		started := p.now()
		_, marker, err := event.Read(ctx, clipboardOriginMIME, clipboardOriginMaxBytes)
		elapsed := p.now().Sub(started)
		if err == nil && p.consumeOrigin(string(marker), event.Generation) {
			p.logSkip("local-origin", event.Backend, requestedMIME, 0, elapsed, merged)
			return
		}
		if err != nil && debugClipboard {
			log.Printf("[CLIPBOARD] backend=%s origin-marker-read-error=%v", event.Backend, err)
		}
	}

	started := p.now()
	active := p.activeReads.Add(1)
	p.recordMaxReads(active)
	mime, content, err := event.Read(ctx, requestedMIME, p.cfg.MaxContentBytes)
	p.activeReads.Add(-1)
	elapsed := p.now().Sub(started)
	if err != nil {
		reason := "read-error"
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
			reason = "timeout"
		case errors.Is(err, errClipboardContentTooLarge):
			reason = "content-too-large"
		case errors.Is(err, context.Canceled):
			reason = "cancelled"
		}
		p.logSkip(reason, event.Backend, mime, len(content), elapsed, merged)
		if reason == "read-error" {
			log.Printf("[CLIPBOARD] backend=%s 读取失败: %v", event.Backend, err)
		}
		return
	}
	if int64(len(content)) > p.cfg.MaxContentBytes {
		p.logSkip("content-too-large", event.Backend, mime, len(content), elapsed, merged)
		return
	}
	if len(content) == 0 {
		p.logSkip("unsupported", event.Backend, mime, 0, elapsed, merged)
		return
	}
	if mime == "" {
		mime = requestedMIME
	}
	kind := clipboardContentKind(mime)
	if kind == "" {
		p.logSkip("unsupported", event.Backend, mime, len(content), elapsed, merged)
		return
	}
	raw := clipboardRawFingerprint(kind, content)
	if p.consumeEcho(kind, raw, event.Generation) {
		p.logSkip("local-echo", event.Backend, mime, len(content), elapsed, merged)
		return
	}
	change := ClipboardChange{Timestamp: p.now().Unix(), Mime: mime, Content: content, Generation: event.Generation}
	replaced, coalesced := p.dedupQueue.notify(clipboardDedupTask{
		change: change, raw: raw, backend: event.Backend, read: elapsed, merged: merged,
	})
	if coalesced {
		p.logSkip("dedup-queue-coalesced", replaced.backend, replaced.change.Mime,
			len(replaced.change.Content), replaced.read, replaced.merged)
	}
}

func (p *clipboardProcessor) recordMaxReads(active int32) {
	for {
		current := p.maxReads.Load()
		if active <= current || p.maxReads.CompareAndSwap(current, active) {
			return
		}
	}
}

func (p *clipboardProcessor) logSkip(reason, backend, mime string, size int, elapsed time.Duration, merged int) {
	log.Printf("[CLIPBOARD] skip=%s backend=%s mime=%s bytes=%d read=%v merged=%d", reason, backend, mime, size, elapsed, merged)
}

func clipboardContentKind(mime string) string {
	lower := strings.ToLower(strings.TrimSpace(mime))
	if strings.HasPrefix(lower, "image/") {
		return "image"
	}
	if strings.HasPrefix(lower, "text/plain") || lower == "utf8_string" || lower == "string" || lower == "text" {
		return "text"
	}
	return ""
}

func clipboardRawFingerprint(kind string, content []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(kind))
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(content)))
	h.Write(size[:])
	h.Write(content)
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func (p *clipboardProcessor) duplicateReason(kind string, raw [sha256.Size]byte, content []byte) string {
	window := time.Duration(p.cfg.DedupWindowMS) * time.Millisecond
	if window <= 0 {
		return ""
	}
	now := p.now()
	kept := p.fingerprints[:0]
	for _, entry := range p.fingerprints {
		if now.Sub(entry.at) <= window {
			kept = append(kept, entry)
		}
	}
	p.fingerprints = kept
	for i := range p.fingerprints {
		entry := &p.fingerprints[i]
		if entry.kind == kind && hmac.Equal(entry.raw[:], raw[:]) {
			entry.at = now
			return "duplicate-raw"
		}
	}

	newEntry := clipboardFingerprintEntry{kind: kind, raw: raw, at: now}
	if kind == "image" && p.cfg.ImagePixelDedup {
		newEntry.imageKey, newEntry.imageKeyOK = clipboardImageIdentity(content)
		for i := range p.fingerprints {
			entry := &p.fingerprints[i]
			if entry.kind != "image" || !newEntry.imageKeyOK || !entry.imageKeyOK || entry.imageKey != newEntry.imageKey {
				continue
			}
			if !newEntry.pixelTried {
				newEntry.pixel, newEntry.pixelOK = p.pixelHash(content)
				newEntry.pixelTried = true
				if !newEntry.pixelOK {
					break
				}
			}
			if !entry.pixelTried && len(entry.imageData) > 0 {
				entry.pixel, entry.pixelOK = p.pixelHash(entry.imageData)
				entry.pixelTried = true
				entry.imageData = nil
			}
			if entry.pixelOK && hmac.Equal(entry.pixel[:], newEntry.pixel[:]) {
				entry.at = now
				return "duplicate-pixels"
			}
		}
		if !newEntry.pixelTried {
			newEntry.imageData = content
		}
	}
	p.fingerprints = append(p.fingerprints, newEntry)
	if len(p.fingerprints) > 16 {
		p.fingerprints = append([]clipboardFingerprintEntry(nil), p.fingerprints[len(p.fingerprints)-16:]...)
	}
	return ""
}

func clipboardImageIdentity(content []byte) (clipboardImageKey, bool) {
	config, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return clipboardImageKey{}, false
	}
	return clipboardImageKey{format: format, width: config.Width, height: config.Height}, true
}

func clipboardPixelFingerprint(content []byte) ([sha256.Size]byte, bool) {
	img, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return [sha256.Size]byte{}, false
	}
	bounds := img.Bounds()
	h := sha256.New()
	var dimensions [16]byte
	binary.LittleEndian.PutUint64(dimensions[0:8], uint64(bounds.Dx()))
	binary.LittleEndian.PutUint64(dimensions[8:16], uint64(bounds.Dy()))
	h.Write(dimensions[:])
	row := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), 1))
	for y := 0; y < bounds.Dy(); y++ {
		draw.Draw(row, row.Bounds(), img, image.Pt(bounds.Min.X, bounds.Min.Y+y), draw.Src)
		h.Write(row.Pix[:bounds.Dx()*4])
		if y%64 == 63 {
			runtime.Gosched()
		}
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, true
}

func selectClipboardMIME(mimes []string) string {
	for _, mime := range mimes {
		if strings.EqualFold(strings.TrimSpace(mime), "image/png") {
			return mime
		}
	}
	for _, mime := range mimes {
		if supportedClipboardImageMIME(mime) {
			return mime
		}
	}
	for _, mime := range mimes {
		lower := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(mime), " ", ""))
		if strings.HasPrefix(lower, "text/plain") && (strings.Contains(lower, "charset=utf-8") || strings.Contains(lower, "charset=utf8") || !strings.Contains(lower, "charset=")) {
			return mime
		}
	}
	for _, mime := range mimes {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "text/plain") {
			return mime
		}
	}
	for _, preferred := range []string{"UTF8_STRING", "STRING", "TEXT"} {
		for _, mime := range mimes {
			if mime == preferred {
				return mime
			}
		}
	}
	return ""
}

func supportedClipboardImageMIME(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/bmp", "image/tiff", "image/webp":
		return true
	default:
		return false
	}
}

func clipboardMIMEAvailable(mimes []string, wanted string) bool {
	for _, mime := range mimes {
		if strings.EqualFold(strings.TrimSpace(mime), wanted) {
			return true
		}
	}
	return false
}

func firstClipboardOrigin(origins []string) string {
	if len(origins) == 0 {
		return ""
	}
	return origins[0]
}

func (p *clipboardProcessor) registerEcho(token, mime string, content []byte) {
	p.registerEchoAt(token, mime, content, 0)
}

func (p *clipboardProcessor) registerEchoAt(token, mime string, content []byte, startGeneration uint64) {
	kind := clipboardContentKind(mime)
	if kind == "" {
		return
	}
	ttl := time.Duration(p.cfg.DedupWindowMS) * time.Millisecond
	if ttl < 5*time.Second {
		ttl = 5 * time.Second
	}
	p.echoMu.Lock()
	p.echo = &clipboardEchoEntry{
		token:           token,
		kind:            kind,
		raw:             clipboardRawFingerprint(kind, content),
		startGeneration: startGeneration,
		expiresAt:       p.now().Add(ttl),
	}
	p.echoMu.Unlock()
}

func (p *clipboardProcessor) completeEcho(token string, generation uint64, success bool) {
	p.echoMu.Lock()
	defer p.echoMu.Unlock()
	if p.echo == nil || p.echo.token != token {
		return
	}
	if !success {
		p.echo = nil
		return
	}
	p.echo.generation = generation
}

func (p *clipboardProcessor) consumeEcho(kind string, raw [sha256.Size]byte, generation uint64) bool {
	p.echoMu.Lock()
	defer p.echoMu.Unlock()
	if p.echo == nil {
		return false
	}
	if p.now().After(p.echo.expiresAt) {
		p.echo = nil
		return false
	}
	if p.echo.startGeneration != 0 && generation != 0 && generation <= p.echo.startGeneration {
		return false
	}
	if p.echo.kind == kind && hmac.Equal(p.echo.raw[:], raw[:]) {
		return true
	}
	if p.echo.generation == 0 || generation == 0 || generation >= p.echo.generation {
		p.echo = nil
	}
	return false
}

func (p *clipboardProcessor) consumeOrigin(token string, generation uint64) bool {
	p.echoMu.Lock()
	defer p.echoMu.Unlock()
	if p.echo == nil || token == "" || p.echo.token != token {
		return false
	}
	if p.now().After(p.echo.expiresAt) {
		p.echo = nil
		return false
	}
	return p.echo.generation == 0 || generation == 0 || generation >= p.echo.generation
}

func (p *clipboardProcessor) debugDigest(kind string, content []byte) string {
	if !p.hmacReady {
		return "unavailable"
	}
	h := hmac.New(sha256.New, p.hmacKey[:])
	h.Write([]byte(kind))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil)[:6])
}

var activeClipboardProcessor struct {
	sync.RWMutex
	processor *clipboardProcessor
}

var clipboardLifecycle sync.WaitGroup

func startClipboardWorker(run func()) {
	clipboardLifecycle.Add(1)
	go func() {
		defer clipboardLifecycle.Done()
		run()
	}()
}

func waitClipboardWorkers(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		clipboardLifecycle.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func setActiveClipboardProcessor(processor *clipboardProcessor) {
	activeClipboardProcessor.Lock()
	activeClipboardProcessor.processor = processor
	activeClipboardProcessor.Unlock()
}

func registerLocalClipboardWrite(mime string, content []byte) string {
	token := generateUUID()
	startGeneration := currentClipboardGeneration()
	activeClipboardProcessor.RLock()
	processor := activeClipboardProcessor.processor
	activeClipboardProcessor.RUnlock()
	if processor != nil {
		processor.registerEchoAt(token, mime, content, startGeneration)
	}
	return token
}

func completeLocalClipboardWrite(token string, generation uint64, success bool) {
	activeClipboardProcessor.RLock()
	processor := activeClipboardProcessor.processor
	activeClipboardProcessor.RUnlock()
	if processor != nil {
		processor.completeEcho(token, generation, success)
	}
}

func readAllBounded(reader io.Reader, max int64) ([]byte, error) {
	if max < 0 {
		return nil, fmt.Errorf("invalid clipboard content limit: %d", max)
	}
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errClipboardContentTooLarge
	}
	return data, nil
}
