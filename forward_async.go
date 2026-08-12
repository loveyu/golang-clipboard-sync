package main

import (
	"context"
	"log"
	"sync"
	"time"
)

type targetSendFunc func(context.Context, *TargetEntry, ClipboardMessage, string)

// latestWorkQueue bounds memory to one active item and one latest pending
// item. When producers outrun a slow consumer, stale pending work is replaced
// instead of spawning unbounded goroutines or retaining every clipboard image.
type latestWorkQueue[T any] struct {
	mu         sync.Mutex
	ready      T
	readySet   bool
	working    bool
	pending    T
	pendingSet bool
	closed     bool
	signal     chan struct{}
}

func newLatestWorkQueue[T any]() *latestWorkQueue[T] {
	return &latestWorkQueue[T]{signal: make(chan struct{}, 1)}
}

func (q *latestWorkQueue[T]) notify(item T) (replaced T, didReplace, accepted bool) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return replaced, false, false
	}
	if !q.working && !q.readySet {
		q.ready = item
		q.readySet = true
	} else {
		replaced = q.pending
		didReplace = q.pendingSet
		q.pending = item
		q.pendingSet = true
	}
	q.mu.Unlock()

	select {
	case q.signal <- struct{}{}:
	default:
	}
	return replaced, didReplace, true
}

func (q *latestWorkQueue[T]) take() (item T, ok, drained bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.readySet {
		return item, false, q.closed && !q.working && !q.pendingSet
	}
	item = q.ready
	var zero T
	q.ready = zero
	q.readySet = false
	q.working = true
	return item, true, false
}

func (q *latestWorkQueue[T]) complete() {
	q.mu.Lock()
	q.working = false
	if q.pendingSet {
		q.ready = q.pending
		q.readySet = true
		var zero T
		q.pending = zero
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

func (q *latestWorkQueue[T]) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

type clipboardChangeSender struct {
	engine *ForwardEngine
	queue  *latestWorkQueue[ClipboardChange]
	done   chan struct{}
}

func newClipboardChangeSender(engine *ForwardEngine) *clipboardChangeSender {
	s := &clipboardChangeSender{
		engine: engine,
		queue:  newLatestWorkQueue[ClipboardChange](),
		done:   make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *clipboardChangeSender) run() {
	defer close(s.done)
	for {
		select {
		case <-s.queue.signal:
		case <-s.engine.ctx.Done():
			return
		}

		for {
			change, ok, drained := s.queue.take()
			if !ok {
				if drained {
					return
				}
				break
			}

			msg, err := s.engine.processChange(change)
			if err != nil {
				log.Printf("Error processing clipboard change: %v", err)
				s.queue.complete()
				continue
			}
			if msg != nil {
				msg.SendTime = fractionalUnixNow()
				select {
				case <-s.engine.ctx.Done():
					return
				default:
					s.engine.dispatchMessage("system", *msg, true)
				}
			}
			s.queue.complete()
		}
	}
}

type forwardJob struct {
	sourceID string
	centerID string
	msg      ClipboardMessage
}

type targetSender struct {
	engine *ForwardEngine
	target *TargetEntry
	queue  *latestWorkQueue[forwardJob]
	done   chan struct{}
}

func newTargetSender(engine *ForwardEngine, target *TargetEntry) *targetSender {
	s := &targetSender{
		engine: engine,
		target: target,
		queue:  newLatestWorkQueue[forwardJob](),
		done:   make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *targetSender) run() {
	defer close(s.done)
	for {
		select {
		case <-s.queue.signal:
		case <-s.engine.ctx.Done():
			return
		}

		for {
			job, ok, drained := s.queue.take()
			if !ok {
				if drained {
					return
				}
				break
			}
			s.engine.sendTarget(s.engine.ctx, s.target, job.msg, job.centerID)
			s.queue.complete()
		}
	}
}

// EnqueueClipboardChange moves message construction and Base64 encoding away
// from the clipboard capture loop. It returns immediately and bounds retained
// raw data to one active plus one latest pending change.
func (e *ForwardEngine) EnqueueClipboardChange(change ClipboardChange) bool {
	e.asyncMu.Lock()
	if e.closing {
		e.asyncMu.Unlock()
		return false
	}
	if e.local == nil {
		e.local = newClipboardChangeSender(e)
	}
	local := e.local
	e.asyncMu.Unlock()

	replaced, coalesced, accepted := local.queue.notify(change)
	if coalesced {
		log.Printf("[FORWARD] skip=outbound-queue-coalesced mime=%s bytes=%d generation=%d",
			replaced.Mime, len(replaced.Content), replaced.Generation)
	}
	return accepted
}

// DispatchMessage routes local system writes immediately and enqueues every
// network target independently. Slow targets cannot block each other.
func (e *ForwardEngine) DispatchMessage(sourceID string, msg ClipboardMessage) bool {
	return e.dispatchMessage(sourceID, msg, false)
}

func (e *ForwardEngine) dispatchMessage(sourceID string, msg ClipboardMessage, internal bool) bool {
	e.asyncMu.Lock()
	if (e.closing && !internal) || e.sendersClosed {
		e.asyncMu.Unlock()
		return false
	}
	e.asyncMu.Unlock()

	accepted := false
	writeSystem := false
	for targetID := range e.targetIDs(sourceID) {
		if targetID == "system" {
			writeSystem = true
			accepted = true
			continue
		}
		target := e.cfg.GetTargetByID(targetID)
		if target == nil {
			log.Printf("[WARN] Forward target not found: %s", targetID)
			continue
		}
		sender := e.senderFor(target, internal)
		if sender == nil {
			continue
		}
		job := forwardJob{sourceID: sourceID, centerID: e.findCenterForTarget(sourceID, targetID), msg: msg}
		replaced, coalesced, ok := sender.queue.notify(job)
		if !ok {
			continue
		}
		accepted = true
		if coalesced {
			log.Printf("[FORWARD] skip=target-queue-coalesced target=%s source=%s type=%s bytes=%d uuid=%s",
				targetID, replaced.sourceID, replaced.msg.Type, len(replaced.msg.Content), replaced.msg.UUID)
		}
	}

	if writeSystem {
		setClipboardContent(msg)
	}
	return accepted
}

func (e *ForwardEngine) senderFor(target *TargetEntry, internal bool) *targetSender {
	e.asyncMu.Lock()
	defer e.asyncMu.Unlock()
	if e.sendersClosed || (e.closing && !internal) {
		return nil
	}
	if sender := e.senders[target.ID]; sender != nil {
		return sender
	}
	sender := newTargetSender(e, target)
	e.senders[target.ID] = sender
	return sender
}

// Shutdown first drains message construction, then drains every isolated
// target queue. When the caller deadline expires, in-flight HTTP/MQTT work is
// cancelled through the engine context.
func (e *ForwardEngine) Shutdown(ctx context.Context) error {
	e.asyncMu.Lock()
	e.closing = true
	local := e.local
	e.asyncMu.Unlock()

	var shutdownErr error
	if local != nil {
		local.queue.close()
		if err := waitDone(ctx, local.done); err != nil {
			shutdownErr = err
			e.cancel()
			<-local.done
		}
	}

	e.asyncMu.Lock()
	e.sendersClosed = true
	senders := make([]*targetSender, 0, len(e.senders))
	for _, sender := range e.senders {
		senders = append(senders, sender)
		sender.queue.close()
	}
	e.asyncMu.Unlock()

	if shutdownErr == nil {
		if err := waitTargetSenders(ctx, senders); err != nil {
			shutdownErr = err
			e.cancel()
			waitTargetSendersUnbounded(senders)
		}
	} else {
		waitTargetSendersUnbounded(senders)
	}
	e.cancel()
	return shutdownErr
}

func waitDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitTargetSenders(ctx context.Context, senders []*targetSender) error {
	done := make(chan struct{})
	go func() {
		waitTargetSendersUnbounded(senders)
		close(done)
	}()
	return waitDone(ctx, done)
}

func waitTargetSendersUnbounded(senders []*targetSender) {
	for _, sender := range senders {
		<-sender.done
	}
}

func fractionalUnixNow() float64 {
	now := time.Now()
	return float64(now.Unix()) + float64(now.Nanosecond())/1e9
}
