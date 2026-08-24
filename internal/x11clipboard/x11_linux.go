//go:build linux && !android

// Copyright 2026 clipboard-sync Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package x11clipboard implements an event-driven X11 CLIPBOARD owner and
// requestor. It uses XFixes for selection-owner notifications and speaks the
// selection protocol directly, including bounded INCR reads and writes.
package x11clipboard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

var (
	ErrUnavailable = errors.New("X11 clipboard unavailable")
	ErrTooLarge    = errors.New("X11 clipboard content exceeds configured limit")
	ErrReleased    = errors.New("X11 clipboard selection is no longer current")
)

const (
	discoveryTimeout = 5 * time.Second
	incrChunkBytes   = 64 * 1024
	incrWriteTimeout = 30 * time.Second
)

type Info struct {
	XFixesMajor uint32
	XFixesMinor uint32
}

type atomSet struct {
	clipboard xproto.Atom
	targets   xproto.Atom
	timestamp xproto.Atom
	incr      xproto.Atom
	property  xproto.Atom
}

type selectionChange struct {
	generation uint64
	owner      xproto.Window
}

type selectionRead struct {
	owner    xproto.Window
	target   xproto.Atom
	property xproto.Atom
	notify   chan xproto.SelectionNotifyEvent
	changed  chan xproto.PropertyNotifyEvent
}

type transferKey struct {
	window   xproto.Window
	property xproto.Atom
}

type source struct {
	data    map[xproto.Atom][]byte
	offered []xproto.Atom
	stamp   xproto.Timestamp
}

type Selection struct {
	Generation uint64
	MIMEs      []string
	state      *selectionState
}

type selectionState struct {
	monitor *Monitor
	owner   xproto.Window
	current atomic.Bool
}

func (s Selection) Read(ctx context.Context, mime string, maxBytes int64) ([]byte, error) {
	if s.state == nil || !s.state.current.Load() {
		return nil, ErrReleased
	}
	return s.state.monitor.readSelection(ctx, s.state.owner, mime, maxBytes)
}

func (s Selection) Release() {
	if s.state != nil {
		s.state.current.Store(false)
	}
}

type Monitor struct {
	conn  *xgb.Conn
	win   xproto.Window
	atoms atomSet
	info  Info

	events       chan Selection
	changes      chan selectionChange
	connectionOK chan struct{}
	discoveryOK  chan struct{}
	done         chan struct{}
	closeOnce    sync.Once
	closing      atomic.Bool
	generation   atomic.Uint64

	atomMu     sync.RWMutex
	atomByName map[string]xproto.Atom
	nameByAtom map[xproto.Atom]string

	readMu    sync.Mutex
	pendingMu sync.RWMutex
	pending   *selectionRead

	sourceMu sync.RWMutex
	source   source

	transferMu sync.RWMutex
	transfers  map[transferKey]chan struct{}

	errMu sync.RWMutex
	err   error
}

func Probe() (Info, error) {
	conn, info, err := connect()
	if err != nil {
		return Info{}, err
	}
	conn.Close()
	return info, nil
}

func connect() (*xgb.Conn, Info, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, Info{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	fail := func(err error) (*xgb.Conn, Info, error) {
		conn.Close()
		return nil, Info{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := xfixes.Init(conn); err != nil {
		return fail(err)
	}
	version, err := xfixes.QueryVersion(conn, 5, 0).Reply()
	if err != nil {
		return fail(err)
	}
	if version == nil || version.MajorVersion < 2 {
		return fail(fmt.Errorf("XFixes 2.0 or newer is required"))
	}
	return conn, Info{XFixesMajor: version.MajorVersion, XFixesMinor: version.MinorVersion}, nil
}

func Start(ctx context.Context) (*Monitor, error) {
	conn, info, err := connect()
	if err != nil {
		return nil, err
	}
	screen := xproto.Setup(conn).DefaultScreen(conn)
	if screen == nil {
		conn.Close()
		return nil, fmt.Errorf("%w: default screen is missing", ErrUnavailable)
	}
	win, err := xproto.NewWindowId(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: allocate window: %v", ErrUnavailable, err)
	}
	if err := xproto.CreateWindowChecked(
		conn,
		screen.RootDepth,
		win,
		screen.Root,
		0, 0, 1, 1, 0,
		xproto.WindowClassInputOutput,
		screen.RootVisual,
		xproto.CwEventMask,
		[]uint32{xproto.EventMaskPropertyChange},
	).Check(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: create event window: %v", ErrUnavailable, err)
	}

	m := &Monitor{
		conn:         conn,
		win:          win,
		info:         info,
		events:       make(chan Selection, 1),
		changes:      make(chan selectionChange, 1),
		connectionOK: make(chan struct{}),
		discoveryOK:  make(chan struct{}),
		done:         make(chan struct{}),
		atomByName:   make(map[string]xproto.Atom),
		nameByAtom:   make(map[xproto.Atom]string),
		transfers:    make(map[transferKey]chan struct{}),
	}
	for _, name := range []string{
		"CLIPBOARD", "TARGETS", "TIMESTAMP", "INCR", "CLIPBOARD_SYNC_DATA",
	} {
		if _, err := m.intern(name); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%w: intern %s: %v", ErrUnavailable, name, err)
		}
	}
	m.atoms = atomSet{
		clipboard: m.atomByName["CLIPBOARD"],
		targets:   m.atomByName["TARGETS"],
		timestamp: m.atomByName["TIMESTAMP"],
		incr:      m.atomByName["INCR"],
		property:  m.atomByName["CLIPBOARD_SYNC_DATA"],
	}
	mask := uint32(xfixes.SelectionEventMaskSetSelectionOwner |
		xfixes.SelectionEventMaskSelectionWindowDestroy |
		xfixes.SelectionEventMaskSelectionClientClose)
	if err := xfixes.SelectSelectionInputChecked(conn, win, m.atoms.clipboard, mask).Check(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: select XFixes clipboard events: %v", ErrUnavailable, err)
	}

	go m.eventLoop()
	go m.discoveryLoop()
	go func() {
		<-m.connectionOK
		<-m.discoveryOK
		close(m.events)
		close(m.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			m.Close()
		case <-m.done:
		}
	}()
	return m, nil
}

func (m *Monitor) Info() Info { return m.info }

func (m *Monitor) Events() <-chan Selection { return m.events }

func (m *Monitor) Err() error {
	m.errMu.RLock()
	defer m.errMu.RUnlock()
	return m.err
}

func (m *Monitor) setErr(err error) {
	if err == nil || m.closing.Load() {
		return
	}
	m.errMu.Lock()
	if m.err == nil {
		m.err = err
	}
	m.errMu.Unlock()
}

func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		m.closing.Store(true)
		m.conn.Close()
		<-m.done
	})
}

func (m *Monitor) eventLoop() {
	defer close(m.connectionOK)
	for {
		event, xerr := m.conn.WaitForEvent()
		if event == nil && xerr == nil {
			return
		}
		if xerr != nil {
			m.setErr(xerr)
			return
		}
		switch value := event.(type) {
		case xfixes.SelectionNotifyEvent:
			if value.Selection != m.atoms.clipboard {
				continue
			}
			if value.Owner == m.win {
				m.sourceMu.Lock()
				if m.source.data != nil {
					m.source.stamp = value.SelectionTimestamp
				}
				m.sourceMu.Unlock()
			}
			generation := m.generation.Add(1)
			m.queueChange(selectionChange{generation: generation, owner: value.Owner})
		case xproto.SelectionNotifyEvent:
			m.routeSelectionNotify(value)
		case xproto.PropertyNotifyEvent:
			m.routePropertyNotify(value)
		case xproto.SelectionRequestEvent:
			go m.handleSelectionRequest(value)
		case xproto.SelectionClearEvent:
			if value.Selection == m.atoms.clipboard {
				m.sourceMu.Lock()
				m.source = source{}
				m.sourceMu.Unlock()
			}
		}
	}
}

func (m *Monitor) queueChange(change selectionChange) {
	select {
	case m.changes <- change:
	default:
		select {
		case <-m.changes:
		default:
		}
		select {
		case m.changes <- change:
		default:
		}
	}
}

func (m *Monitor) discoveryLoop() {
	defer close(m.discoveryOK)
	for {
		select {
		case change := <-m.changes:
			if change.owner == xproto.WindowNone {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
			mimes, err := m.discoverTargets(ctx, change.owner)
			cancel()
			if err != nil || len(mimes) == 0 {
				continue
			}
			state := &selectionState{monitor: m, owner: change.owner}
			state.current.Store(true)
			selection := Selection{Generation: change.generation, MIMEs: mimes, state: state}
			select {
			case m.events <- selection:
			default:
				select {
				case stale := <-m.events:
					stale.Release()
				default:
				}
				select {
				case m.events <- selection:
				default:
					selection.Release()
				}
			}
		case <-m.connectionOK:
			return
		}
	}
}

func (m *Monitor) discoverTargets(ctx context.Context, owner xproto.Window) ([]string, error) {
	data, valueType, format, err := m.readRaw(ctx, owner, m.atoms.targets, 1024*1024)
	if err != nil {
		return nil, err
	}
	if valueType != xproto.AtomAtom || format != 32 {
		return nil, ErrUnavailable
	}
	currentOwner, err := xproto.GetSelectionOwner(m.conn, m.atoms.clipboard).Reply()
	if err != nil || currentOwner == nil || currentOwner.Owner != owner {
		return nil, ErrReleased
	}
	names := make([]string, 0, len(data)/4)
	seen := make(map[string]struct{}, len(data)/4)
	for offset := 0; offset+4 <= len(data); offset += 4 {
		atom := xproto.Atom(xgb.Get32(data[offset:]))
		name, err := m.atomName(atom)
		if err != nil || name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

func (m *Monitor) readSelection(ctx context.Context, owner xproto.Window, mime string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 || mime == "" {
		return nil, ErrUnavailable
	}
	currentOwner, err := xproto.GetSelectionOwner(m.conn, m.atoms.clipboard).Reply()
	if err != nil || currentOwner == nil || currentOwner.Owner != owner {
		return nil, ErrReleased
	}
	target, err := m.intern(mime)
	if err != nil {
		return nil, err
	}
	data, _, _, err := m.readRaw(ctx, owner, target, maxBytes)
	return data, err
}

func (m *Monitor) readRaw(ctx context.Context, owner xproto.Window, target xproto.Atom, maxBytes int64) ([]byte, xproto.Atom, byte, error) {
	m.readMu.Lock()
	defer m.readMu.Unlock()
	if m.closing.Load() {
		return nil, 0, 0, ErrUnavailable
	}
	if maxBytes < 0 {
		return nil, 0, 0, ErrTooLarge
	}
	_ = xproto.DeletePropertyChecked(m.conn, m.win, m.atoms.property).Check()
	pending := &selectionRead{
		owner: owner, target: target, property: m.atoms.property,
		notify:  make(chan xproto.SelectionNotifyEvent, 1),
		changed: make(chan xproto.PropertyNotifyEvent, 32),
	}
	m.pendingMu.Lock()
	m.pending = pending
	m.pendingMu.Unlock()
	defer func() {
		m.pendingMu.Lock()
		if m.pending == pending {
			m.pending = nil
		}
		m.pendingMu.Unlock()
	}()
	if err := xproto.ConvertSelectionChecked(
		m.conn, m.win, m.atoms.clipboard, target, m.atoms.property, xproto.TimeCurrentTime,
	).Check(); err != nil {
		return nil, 0, 0, err
	}
	select {
	case notify := <-pending.notify:
		if notify.Property == xproto.AtomNone {
			return nil, 0, 0, ErrUnavailable
		}
	case <-ctx.Done():
		return nil, 0, 0, ctx.Err()
	case <-m.connectionOK:
		return nil, 0, 0, ErrUnavailable
	}
	// The owner sets the property before sending SelectionNotify, so the
	// corresponding PropertyNewValue has already been queued. Consume that
	// notification before reading (and deleting) the initial value. For INCR,
	// only later PropertyNewValue events represent actual data chunks.
	drainPropertyChanges(pending.changed)

	length := propertyLongLength(maxBytes)
	reply, err := xproto.GetProperty(
		m.conn, true, m.win, m.atoms.property, xproto.AtomAny, 0, length,
	).Reply()
	if err != nil || reply == nil {
		return nil, 0, 0, coalesceError(err)
	}
	if reply.Type == m.atoms.incr {
		return m.readINCR(ctx, pending, maxBytes)
	}
	if int64(len(reply.Value)) > maxBytes || reply.BytesAfter > 0 {
		_ = xproto.DeletePropertyChecked(m.conn, m.win, m.atoms.property).Check()
		return nil, reply.Type, reply.Format, ErrTooLarge
	}
	return append([]byte(nil), reply.Value...), reply.Type, reply.Format, nil
}

func drainPropertyChanges(changes <-chan xproto.PropertyNotifyEvent) {
	for {
		select {
		case <-changes:
		default:
			return
		}
	}
}

func propertyLongLength(maxBytes int64) uint32 {
	words := uint64(maxBytes+3)/4 + 1
	if words > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(words)
}

func (m *Monitor) readINCR(ctx context.Context, pending *selectionRead, maxBytes int64) ([]byte, xproto.Atom, byte, error) {
	data := make([]byte, 0)
	var valueType xproto.Atom
	var format byte
	tooLarge := false
	for {
		select {
		case <-pending.changed:
			reply, err := xproto.GetProperty(
				m.conn, true, m.win, pending.property, xproto.AtomAny, 0, propertyLongLength(maxBytes),
			).Reply()
			if err != nil || reply == nil {
				return nil, 0, 0, coalesceError(err)
			}
			if len(reply.Value) == 0 {
				if tooLarge {
					return nil, valueType, format, ErrTooLarge
				}
				return data, valueType, format, nil
			}
			if valueType == 0 {
				valueType, format = reply.Type, reply.Format
			}
			if int64(len(data))+int64(len(reply.Value)) > maxBytes || reply.BytesAfter > 0 {
				tooLarge = true
				_ = xproto.DeletePropertyChecked(m.conn, m.win, pending.property).Check()
				continue
			}
			if !tooLarge {
				data = append(data, reply.Value...)
			}
		case <-ctx.Done():
			return nil, 0, 0, ctx.Err()
		case <-m.connectionOK:
			return nil, 0, 0, ErrUnavailable
		}
	}
}

func coalesceError(err error) error {
	if err != nil {
		return err
	}
	return ErrUnavailable
}

func (m *Monitor) routeSelectionNotify(event xproto.SelectionNotifyEvent) {
	m.pendingMu.RLock()
	pending := m.pending
	m.pendingMu.RUnlock()
	if pending == nil || event.Requestor != m.win || event.Target != pending.target {
		return
	}
	select {
	case pending.notify <- event:
	default:
	}
}

func (m *Monitor) routePropertyNotify(event xproto.PropertyNotifyEvent) {
	if event.State == xproto.PropertyNewValue {
		m.pendingMu.RLock()
		pending := m.pending
		m.pendingMu.RUnlock()
		if pending != nil && event.Window == m.win && event.Atom == pending.property {
			select {
			case pending.changed <- event:
			default:
			}
		}
		return
	}
	if event.State != xproto.PropertyDelete {
		return
	}
	key := transferKey{window: event.Window, property: event.Atom}
	m.transferMu.RLock()
	ready := m.transfers[key]
	m.transferMu.RUnlock()
	if ready != nil {
		select {
		case ready <- struct{}{}:
		default:
		}
	}
}

func (m *Monitor) intern(name string) (xproto.Atom, error) {
	m.atomMu.RLock()
	atom, ok := m.atomByName[name]
	m.atomMu.RUnlock()
	if ok {
		return atom, nil
	}
	reply, err := xproto.InternAtom(m.conn, false, uint16(len(name)), name).Reply()
	if err != nil || reply == nil {
		return 0, coalesceError(err)
	}
	m.atomMu.Lock()
	m.atomByName[name] = reply.Atom
	m.nameByAtom[reply.Atom] = name
	m.atomMu.Unlock()
	return reply.Atom, nil
}

func (m *Monitor) atomName(atom xproto.Atom) (string, error) {
	m.atomMu.RLock()
	name, ok := m.nameByAtom[atom]
	m.atomMu.RUnlock()
	if ok {
		return name, nil
	}
	reply, err := xproto.GetAtomName(m.conn, atom).Reply()
	if err != nil || reply == nil {
		return "", coalesceError(err)
	}
	m.atomMu.Lock()
	m.nameByAtom[atom] = reply.Name
	m.atomByName[reply.Name] = atom
	m.atomMu.Unlock()
	return reply.Name, nil
}

func (m *Monitor) Write(ctx context.Context, mime string, data []byte) error {
	return m.WriteWithOrigin(ctx, mime, data, "", nil)
}

func (m *Monitor) WriteWithOrigin(ctx context.Context, mime string, data []byte, originMIME string, originData []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closing.Load() || mime == "" {
		return ErrUnavailable
	}
	offers := []string{mime}
	if strings.HasPrefix(strings.ToLower(mime), "text/") {
		offers = []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "STRING", "TEXT"}
	} else if strings.HasPrefix(strings.ToLower(mime), "image/") {
		offers = []string{"image/png"}
	}
	if originMIME != "" && len(originData) > 0 {
		offers = append(offers, originMIME)
	}
	values := make(map[xproto.Atom][]byte, len(offers))
	atoms := make([]xproto.Atom, 0, len(offers))
	for _, offered := range offers {
		atom, err := m.intern(offered)
		if err != nil {
			return err
		}
		payload := data
		if offered == originMIME {
			payload = originData
		}
		values[atom] = append([]byte(nil), payload...)
		atoms = append(atoms, atom)
	}

	m.sourceMu.Lock()
	m.source = source{data: values, offered: atoms, stamp: xproto.TimeCurrentTime}
	m.sourceMu.Unlock()
	if err := xproto.SetSelectionOwnerChecked(
		m.conn, m.win, m.atoms.clipboard, xproto.TimeCurrentTime,
	).Check(); err != nil {
		return err
	}
	reply, err := xproto.GetSelectionOwner(m.conn, m.atoms.clipboard).Reply()
	if err != nil || reply == nil || reply.Owner != m.win {
		return ErrUnavailable
	}
	return nil
}

func (m *Monitor) handleSelectionRequest(request xproto.SelectionRequestEvent) {
	if request.Selection != m.atoms.clipboard {
		m.sendSelectionNotify(request, xproto.AtomNone)
		return
	}
	property := request.Property
	if property == xproto.AtomNone {
		property = request.Target
	}
	m.sourceMu.RLock()
	current := m.source
	m.sourceMu.RUnlock()
	if current.data == nil {
		m.sendSelectionNotify(request, xproto.AtomNone)
		return
	}

	if request.Target == m.atoms.targets {
		targets := make([]xproto.Atom, 0, len(current.offered)+2)
		targets = append(targets, m.atoms.targets, m.atoms.timestamp)
		targets = append(targets, current.offered...)
		payload := atomBytes(targets)
		if err := xproto.ChangePropertyChecked(
			m.conn, xproto.PropModeReplace, request.Requestor, property,
			xproto.AtomAtom, 32, uint32(len(targets)), payload,
		).Check(); err != nil {
			m.sendSelectionNotify(request, xproto.AtomNone)
			return
		}
		m.sendSelectionNotify(request, property)
		return
	}
	if request.Target == m.atoms.timestamp {
		payload := make([]byte, 4)
		xgb.Put32(payload, uint32(current.stamp))
		if err := xproto.ChangePropertyChecked(
			m.conn, xproto.PropModeReplace, request.Requestor, property,
			xproto.AtomInteger, 32, 1, payload,
		).Check(); err != nil {
			m.sendSelectionNotify(request, xproto.AtomNone)
			return
		}
		m.sendSelectionNotify(request, property)
		return
	}
	payload, ok := current.data[request.Target]
	if !ok {
		m.sendSelectionNotify(request, xproto.AtomNone)
		return
	}
	if len(payload) <= incrChunkBytes {
		if err := xproto.ChangePropertyChecked(
			m.conn, xproto.PropModeReplace, request.Requestor, property,
			request.Target, 8, uint32(len(payload)), payload,
		).Check(); err != nil {
			m.sendSelectionNotify(request, xproto.AtomNone)
			return
		}
		m.sendSelectionNotify(request, property)
		return
	}
	m.startINCRWrite(request, property, payload)
}

func atomBytes(atoms []xproto.Atom) []byte {
	data := make([]byte, len(atoms)*4)
	for index, atom := range atoms {
		xgb.Put32(data[index*4:], uint32(atom))
	}
	return data
}

func (m *Monitor) sendSelectionNotify(request xproto.SelectionRequestEvent, property xproto.Atom) {
	event := xproto.SelectionNotifyEvent{
		Time: request.Time, Requestor: request.Requestor, Selection: request.Selection,
		Target: request.Target, Property: property,
	}
	_ = xproto.SendEventChecked(m.conn, false, request.Requestor, 0, string(event.Bytes())).Check()
}

func (m *Monitor) startINCRWrite(request xproto.SelectionRequestEvent, property xproto.Atom, payload []byte) {
	key := transferKey{window: request.Requestor, property: property}
	ready := make(chan struct{}, 1)
	m.transferMu.Lock()
	if _, exists := m.transfers[key]; exists {
		m.transferMu.Unlock()
		m.sendSelectionNotify(request, xproto.AtomNone)
		return
	}
	m.transfers[key] = ready
	m.transferMu.Unlock()
	fail := func() {
		m.transferMu.Lock()
		delete(m.transfers, key)
		m.transferMu.Unlock()
		m.sendSelectionNotify(request, xproto.AtomNone)
	}
	if err := xproto.ChangeWindowAttributesChecked(
		m.conn, request.Requestor, xproto.CwEventMask, []uint32{xproto.EventMaskPropertyChange},
	).Check(); err != nil {
		fail()
		return
	}
	size := make([]byte, 4)
	xgb.Put32(size, uint32(len(payload)))
	if err := xproto.ChangePropertyChecked(
		m.conn, xproto.PropModeReplace, request.Requestor, property,
		m.atoms.incr, 32, 1, size,
	).Check(); err != nil {
		fail()
		return
	}
	m.sendSelectionNotify(request, property)
	data := append([]byte(nil), payload...)
	go m.serveINCR(key, ready, request.Target, data)
}

func (m *Monitor) serveINCR(key transferKey, ready <-chan struct{}, target xproto.Atom, data []byte) {
	defer func() {
		m.transferMu.Lock()
		delete(m.transfers, key)
		m.transferMu.Unlock()
	}()
	offset := 0
	for {
		timer := time.NewTimer(incrWriteTimeout)
		select {
		case <-ready:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			return
		case <-m.connectionOK:
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
		end := offset + incrChunkBytes
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]
		if err := xproto.ChangePropertyChecked(
			m.conn, xproto.PropModeReplace, key.window, key.property,
			target, 8, uint32(len(chunk)), chunk,
		).Check(); err != nil {
			return
		}
		offset = end
		if len(chunk) == 0 {
			return
		}
		if offset == len(data) {
			data = data[:0]
			offset = 0
		}
	}
}
