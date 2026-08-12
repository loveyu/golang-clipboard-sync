// Copyright 2026 The golang.design Initiative Authors.
// Copyright 2026 clipboard-sync Authors.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

//go:build linux && !android

// Package waylandclipboard implements the minimum Wayland data-control client
// needed by clipboard-sync. The wire codec and discovery flow are adapted from
// golang.design/x/clipboard v0.8.0. Unlike its public multi-format Watch API,
// this package exposes one selection offer with all MIME types and receives
// exactly one chosen MIME through the same persistent connection.
package waylandclipboard

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	ErrUnavailable = errors.New("wayland data-control is unavailable")
	ErrTooLarge    = errors.New("wayland clipboard content exceeds configured limit")
	ErrReleased    = errors.New("wayland clipboard offer has been released")
)

const wlDisplayID = 1

const (
	regOpcodeBind = 0

	dispOpcodeSync        = 0
	dispOpcodeGetRegistry = 1

	mgrOpcodeCreateDataSource = 0
	mgrOpcodeGetDataDevice    = 1

	devOpcodeSetSelection = 0
	devEvtDataOffer       = 0
	devEvtSelection       = 1

	offerOpcodeReceive = 0
	offerOpcodeDestroy = 1
	offerEvtOffer      = 0

	srcOpcodeOffer  = 0
	srcEvtSend      = 0
	srcEvtCancelled = 1
)

var managerInterfaces = []string{
	"ext_data_control_manager_v1",
	"zwlr_data_control_manager_v1",
}

type global struct {
	name    uint32
	version uint32
}

// Info describes the selected data-control protocol.
type Info struct {
	Interface string
	Version   uint32
}

type conn struct {
	socket *net.UnixConn

	idMu      sync.Mutex
	nextID    uint32
	writeMu   sync.Mutex
	closeOnce sync.Once

	fdMu sync.Mutex
	rbuf []byte
	fds  []int
}

func socketPath() string {
	display := os.Getenv("WAYLAND_DISPLAY")
	if display == "" {
		return ""
	}
	if filepath.IsAbs(display) {
		return display
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		return ""
	}
	return filepath.Join(runtimeDir, display)
}

func connect() (*conn, error) {
	path := socketPath()
	if path == "" {
		return nil, ErrUnavailable
	}
	socket, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	return &conn{socket: socket, nextID: wlDisplayID + 1}, nil
}

func (c *conn) close() {
	c.closeOnce.Do(func() {
		_ = c.socket.Close()
		c.fdMu.Lock()
		for _, fd := range c.fds {
			_ = syscall.Close(fd)
		}
		c.fds = nil
		c.fdMu.Unlock()
	})
}

func (c *conn) newID() uint32 {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	id := c.nextID
	c.nextID++
	return id
}

func (c *conn) request(objectID uint32, opcode uint16, payload []byte) error {
	size := 8 + len(payload)
	if size > 0xffff {
		return fmt.Errorf("wayland message too large: %d bytes", size)
	}
	message := make([]byte, size)
	binary.LittleEndian.PutUint32(message[0:4], objectID)
	binary.LittleEndian.PutUint32(message[4:8], uint32(size)<<16|uint32(opcode))
	copy(message[8:], payload)
	c.writeMu.Lock()
	_, err := c.socket.Write(message)
	c.writeMu.Unlock()
	return err
}

func (c *conn) requestFD(objectID uint32, opcode uint16, payload []byte, fd int) error {
	size := 8 + len(payload)
	message := make([]byte, size)
	binary.LittleEndian.PutUint32(message[0:4], objectID)
	binary.LittleEndian.PutUint32(message[4:8], uint32(size)<<16|uint32(opcode))
	copy(message[8:], payload)
	c.writeMu.Lock()
	_, _, err := c.socket.WriteMsgUnix(message, syscall.UnixRights(fd), nil)
	c.writeMu.Unlock()
	return err
}

func (c *conn) fill() error {
	var payload [4096]byte
	var control [256]byte
	n, controlN, _, _, err := c.socket.ReadMsgUnix(payload[:], control[:])
	if err != nil {
		return err
	}
	if n > 0 {
		c.rbuf = append(c.rbuf, payload[:n]...)
	}
	if controlN > 0 {
		messages, parseErr := syscall.ParseSocketControlMessage(control[:controlN])
		if parseErr != nil {
			return parseErr
		}
		for i := range messages {
			fds, rightsErr := syscall.ParseUnixRights(&messages[i])
			if rightsErr == nil {
				c.fdMu.Lock()
				c.fds = append(c.fds, fds...)
				c.fdMu.Unlock()
			}
		}
	}
	return nil
}

func (c *conn) readEvent() (objectID uint32, opcode uint16, body []byte, err error) {
	for len(c.rbuf) < 8 {
		if err := c.fill(); err != nil {
			return 0, 0, nil, err
		}
	}
	objectID = binary.LittleEndian.Uint32(c.rbuf[0:4])
	header := binary.LittleEndian.Uint32(c.rbuf[4:8])
	size := int(header >> 16)
	opcode = uint16(header & 0xffff)
	if size < 8 {
		return 0, 0, nil, fmt.Errorf("invalid Wayland message size: %d", size)
	}
	for len(c.rbuf) < size {
		if err := c.fill(); err != nil {
			return 0, 0, nil, err
		}
	}
	body = append([]byte(nil), c.rbuf[8:size]...)
	c.rbuf = append(c.rbuf[:0], c.rbuf[size:]...)
	return objectID, opcode, body, nil
}

func (c *conn) nextFD() (int, bool) {
	c.fdMu.Lock()
	defer c.fdMu.Unlock()
	if len(c.fds) == 0 {
		return -1, false
	}
	fd := c.fds[0]
	c.fds = c.fds[1:]
	return fd, true
}

func (c *conn) bind(registryID, name uint32, iface string, version uint32) (uint32, error) {
	id := c.newID()
	payload := make([]byte, 0, 16+len(iface))
	payload = appendUint32(payload, name)
	payload = append(payload, encodeString(iface)...)
	payload = appendUint32(payload, version)
	payload = appendUint32(payload, id)
	return id, c.request(registryID, regOpcodeBind, payload)
}

func (c *conn) sync() (uint32, error) {
	id := c.newID()
	return id, c.request(wlDisplayID, dispOpcodeSync, appendUint32(nil, id))
}

func appendUint32(dst []byte, value uint32) []byte {
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], value)
	return append(dst, encoded[:]...)
}

func encodeString(value string) []byte {
	length := len(value) + 1
	padded := (length + 3) &^ 3
	encoded := make([]byte, 4+padded)
	binary.LittleEndian.PutUint32(encoded[0:4], uint32(length))
	copy(encoded[4:], value)
	return encoded
}

func decodeString(body []byte, offset int) (string, int, error) {
	if offset+4 > len(body) {
		return "", 0, io.ErrUnexpectedEOF
	}
	length := int(binary.LittleEndian.Uint32(body[offset : offset+4]))
	offset += 4
	padded := (length + 3) &^ 3
	if length == 0 || offset+padded > len(body) {
		return "", 0, io.ErrUnexpectedEOF
	}
	return string(body[offset : offset+length-1]), offset + padded, nil
}

func displayError(body []byte) error {
	if len(body) < 8 {
		return errors.New("Wayland protocol error")
	}
	code := binary.LittleEndian.Uint32(body[4:8])
	message, _, err := decodeString(body, 8)
	if err != nil {
		return fmt.Errorf("Wayland protocol error (code %d)", code)
	}
	return fmt.Errorf("Wayland protocol error (code %d): %s", code, message)
}

func preferredManager(globals map[string]global) (string, global, bool) {
	for _, iface := range managerInterfaces {
		if value, ok := globals[iface]; ok {
			return iface, value, true
		}
	}
	return "", global{}, false
}

func connectDevice() (*conn, uint32, uint32, Info, error) {
	c, err := connect()
	if err != nil {
		return nil, 0, 0, Info{}, err
	}
	_ = c.socket.SetDeadline(time.Now().Add(5 * time.Second))
	fail := func(err error) (*conn, uint32, uint32, Info, error) {
		c.close()
		return nil, 0, 0, Info{}, err
	}

	registryID := c.newID()
	if err := c.request(wlDisplayID, dispOpcodeGetRegistry, appendUint32(nil, registryID)); err != nil {
		return fail(err)
	}
	barrier, err := c.sync()
	if err != nil {
		return fail(err)
	}
	globals := make(map[string]global)
	for {
		objectID, opcode, body, err := c.readEvent()
		if err != nil {
			return fail(err)
		}
		if objectID == wlDisplayID && opcode == 0 {
			return fail(displayError(body))
		}
		if objectID == registryID && opcode == 0 && len(body) >= 4 {
			name := binary.LittleEndian.Uint32(body[0:4])
			iface, offset, decodeErr := decodeString(body, 4)
			if decodeErr == nil && offset+4 <= len(body) {
				globals[iface] = global{name: name, version: binary.LittleEndian.Uint32(body[offset : offset+4])}
			}
		}
		if objectID == barrier && opcode == 0 {
			break
		}
	}

	managerIface, managerGlobal, ok := preferredManager(globals)
	if !ok {
		return fail(ErrUnavailable)
	}
	seatGlobal, ok := globals["wl_seat"]
	if !ok {
		return fail(ErrUnavailable)
	}
	managerID, err := c.bind(registryID, managerGlobal.name, managerIface, 1)
	if err != nil {
		return fail(err)
	}
	seatID, err := c.bind(registryID, seatGlobal.name, "wl_seat", 1)
	if err != nil {
		return fail(err)
	}
	deviceID := c.newID()
	payload := appendUint32(nil, deviceID)
	payload = appendUint32(payload, seatID)
	if err := c.request(managerID, mgrOpcodeGetDataDevice, payload); err != nil {
		return fail(err)
	}
	_ = c.socket.SetDeadline(time.Time{})
	return c, managerID, deviceID, Info{Interface: managerIface, Version: managerGlobal.version}, nil
}

// Probe verifies that a Wayland data-control manager and a seat can be bound.
func Probe() (Info, error) {
	c, _, _, info, err := connectDevice()
	if err != nil {
		return Info{}, err
	}
	c.close()
	return info, nil
}

type selectionState struct {
	monitor  *Monitor
	offerID  uint32
	once     sync.Once
	released atomic.Bool
}

// Selection is a single data-control selection offer. Call Release after Read
// or when the offer is skipped.
type Selection struct {
	Generation uint64
	MIMEs      []string
	state      *selectionState
}

func (s Selection) Read(ctx context.Context, mime string, maxBytes int64) ([]byte, error) {
	if s.state == nil || s.state.released.Load() {
		return nil, ErrReleased
	}
	return s.state.monitor.receive(ctx, s.state.offerID, mime, maxBytes)
}

func (s Selection) Release() {
	if s.state == nil {
		return
	}
	s.state.once.Do(func() {
		s.state.released.Store(true)
		_ = s.state.monitor.connection.request(s.state.offerID, offerOpcodeDestroy, nil)
	})
}

type source struct {
	mimes      map[string]struct{}
	data       []byte
	originMIME string
	originData []byte
}

type sourceSend struct {
	file *os.File
	data []byte
}

type syncResult struct {
	err error
}

// Monitor owns one persistent data-control connection for selection events,
// receives and local writes.
type Monitor struct {
	connection *conn
	managerID  uint32
	deviceID   uint32
	info       Info

	events     chan Selection
	done       chan struct{}
	closeOnce  sync.Once
	closing    atomic.Bool
	generation atomic.Uint64

	sourcesMu sync.RWMutex
	sources   map[uint32]source
	sourceIDs []uint32
	writeMu   sync.Mutex
	sendsMu   sync.Mutex
	sends     map[*os.File]struct{}
	sendQueue chan sourceSend
	sendDone  chan struct{}
	syncMu    sync.Mutex
	syncs     map[uint32]chan syncResult
	errMu     sync.RWMutex
	err       error
}

// Start binds one data-control device, consumes the initial selection as a
// baseline, then begins publishing only subsequent changes.
func Start(ctx context.Context) (*Monitor, error) {
	connection, managerID, deviceID, info, err := connectDevice()
	if err != nil {
		return nil, err
	}
	monitor := &Monitor{
		connection: connection,
		managerID:  managerID,
		deviceID:   deviceID,
		info:       info,
		events:     make(chan Selection, 1),
		done:       make(chan struct{}),
		sources:    make(map[uint32]source),
		sends:      make(map[*os.File]struct{}),
		sendQueue:  make(chan sourceSend, 64),
		sendDone:   make(chan struct{}),
		syncs:      make(map[uint32]chan syncResult),
	}

	offers := make(map[uint32][]string)
	barrier, err := connection.sync()
	if err != nil {
		connection.close()
		return nil, err
	}
	for {
		objectID, opcode, body, err := connection.readEvent()
		if err != nil {
			connection.close()
			return nil, err
		}
		if objectID == wlDisplayID && opcode == 0 {
			connection.close()
			return nil, displayError(body)
		}
		monitor.collectOfferEvent(offers, objectID, opcode, body)
		if objectID == barrier && opcode == 0 {
			break
		}
	}
	for offerID := range offers {
		_ = connection.request(offerID, offerOpcodeDestroy, nil)
	}

	go monitor.serveLoop()
	go monitor.eventLoop(make(map[uint32][]string))
	go func() {
		select {
		case <-ctx.Done():
			monitor.Close()
		case <-monitor.done:
		}
	}()
	return monitor, nil
}

func (m *Monitor) Info() Info { return m.info }

func (m *Monitor) Events() <-chan Selection { return m.events }

func (m *Monitor) Err() error {
	m.errMu.RLock()
	defer m.errMu.RUnlock()
	return m.err
}

func (m *Monitor) setErr(err error) {
	m.errMu.Lock()
	m.err = err
	m.errMu.Unlock()
}

func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		m.closing.Store(true)
		m.connection.close()
		m.sendsMu.Lock()
		for file := range m.sends {
			_ = file.Close()
		}
		m.sendsMu.Unlock()
		<-m.done
		close(m.sendQueue)
		<-m.sendDone
	})
}

func (m *Monitor) eventLoop(offers map[uint32][]string) {
	defer m.failSyncs(net.ErrClosed)
	defer close(m.done)
	defer close(m.events)
	for {
		objectID, opcode, body, err := m.connection.readEvent()
		if err != nil {
			if !m.closing.Load() {
				m.setErr(err)
			}
			return
		}
		switch {
		case objectID == wlDisplayID && opcode == 0:
			m.setErr(displayError(body))
			return
		case opcode == 0 && m.completeSync(objectID):
		case objectID == m.deviceID && opcode == devEvtSelection && len(body) >= 4:
			offerID := binary.LittleEndian.Uint32(body[0:4])
			generation := m.generation.Add(1)
			if offerID != 0 {
				selection := Selection{
					Generation: generation,
					MIMEs:      append([]string(nil), offers[offerID]...),
					state:      &selectionState{monitor: m, offerID: offerID},
				}
				m.publish(selection)
			}
			for id := range offers {
				if id != offerID {
					delete(offers, id)
				}
			}
		case objectID == m.deviceID && opcode == devEvtDataOffer && len(body) >= 4:
			offers[binary.LittleEndian.Uint32(body[0:4])] = nil
		case opcode == srcEvtSend && m.hasSource(objectID):
			m.serveSource(objectID, body)
		case opcode == srcEvtCancelled && m.hasSource(objectID):
			// Keep a bounded history of cancelled sources. Send requests that
			// were already queued by a clipboard manager may arrive after the
			// cancelled event. A tombstone makes us consume and close their
			// SCM_RIGHTS descriptor in order without retaining clipboard bytes.
			m.tombstoneSource(objectID)
		case opcode == offerEvtOffer && hasOffer(offers, objectID):
			if _, ok := offers[objectID]; ok {
				if mime, _, decodeErr := decodeString(body, 0); decodeErr == nil {
					offers[objectID] = append(offers[objectID], mime)
				}
			}
		}
	}
}

func (m *Monitor) completeSync(objectID uint32) bool {
	m.syncMu.Lock()
	waiter, ok := m.syncs[objectID]
	if ok {
		delete(m.syncs, objectID)
	}
	m.syncMu.Unlock()
	if ok {
		waiter <- syncResult{}
	}
	return ok
}

func (m *Monitor) failSyncs(err error) {
	m.syncMu.Lock()
	waiters := m.syncs
	m.syncs = make(map[uint32]chan syncResult)
	m.syncMu.Unlock()
	for _, waiter := range waiters {
		waiter <- syncResult{err: err}
	}
}

func (m *Monitor) confirm(ctx context.Context) error {
	id := m.connection.newID()
	waiter := make(chan syncResult, 1)
	m.syncMu.Lock()
	m.syncs[id] = waiter
	m.syncMu.Unlock()
	if err := m.connection.request(wlDisplayID, dispOpcodeSync, appendUint32(nil, id)); err != nil {
		m.syncMu.Lock()
		delete(m.syncs, id)
		m.syncMu.Unlock()
		return err
	}
	select {
	case result := <-waiter:
		return result.err
	case <-ctx.Done():
		m.syncMu.Lock()
		delete(m.syncs, id)
		m.syncMu.Unlock()
		return ctx.Err()
	}
}

func hasOffer(offers map[uint32][]string, offerID uint32) bool {
	_, ok := offers[offerID]
	return ok
}

func (m *Monitor) collectOfferEvent(offers map[uint32][]string, objectID uint32, opcode uint16, body []byte) {
	switch {
	case objectID == m.deviceID && opcode == devEvtDataOffer && len(body) >= 4:
		offers[binary.LittleEndian.Uint32(body[0:4])] = nil
	case opcode == offerEvtOffer:
		if _, ok := offers[objectID]; ok {
			if mime, _, err := decodeString(body, 0); err == nil {
				offers[objectID] = append(offers[objectID], mime)
			}
		}
	}
}

func (m *Monitor) publish(selection Selection) {
	select {
	case m.events <- selection:
		return
	default:
	}
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

func (m *Monitor) receive(ctx context.Context, offerID uint32, mime string, maxBytes int64) ([]byte, error) {
	if mime == "" {
		return nil, ErrUnavailable
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if err := m.connection.requestFD(offerID, offerOpcodeReceive, encodeString(mime), int(writer.Fd())); err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}
	writer.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = reader.Close() })
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	stopClose()
	reader.Close()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

func (m *Monitor) hasSource(sourceID uint32) bool {
	m.sourcesMu.RLock()
	_, ok := m.sources[sourceID]
	m.sourcesMu.RUnlock()
	return ok
}

func (m *Monitor) tombstoneSource(sourceID uint32) {
	m.sourcesMu.Lock()
	value, ok := m.sources[sourceID]
	if ok {
		value.data = nil
		value.originData = nil
		m.sources[sourceID] = value
	}
	m.sourcesMu.Unlock()
}

func (m *Monitor) serveSource(sourceID uint32, body []byte) {
	mime, _, _ := decodeString(body, 0)
	fd, ok := m.connection.nextFD()
	if !ok {
		return
	}
	m.sourcesMu.RLock()
	value, exists := m.sources[sourceID]
	m.sourcesMu.RUnlock()
	_, mimeOffered := value.mimes[mime]
	if !exists || (mime != "" && !mimeOffered) {
		_ = syscall.Close(fd)
		return
	}
	data := value.data
	if mime == value.originMIME {
		data = value.originData
	}
	file := os.NewFile(uintptr(fd), "clipboard-sync-wayland-send")
	if file == nil {
		_ = syscall.Close(fd)
		return
	}
	m.sendsMu.Lock()
	if m.closing.Load() {
		m.sendsMu.Unlock()
		_ = file.Close()
		return
	}
	m.sends[file] = struct{}{}
	m.sendsMu.Unlock()
	// Keep source transfers in protocol order. Clipboard managers commonly
	// request several advertised text types at once; completing those writes
	// out of order can make them accept an empty/broken-pipe transfer.
	m.sendQueue <- sourceSend{file: file, data: data}
}

func (m *Monitor) serveLoop() {
	defer close(m.sendDone)
	for send := range m.sendQueue {
		if !m.closing.Load() {
			remaining := send.data
			for len(remaining) > 0 {
				n, err := send.file.Write(remaining)
				if err != nil || n == 0 {
					break
				}
				remaining = remaining[n:]
			}
		}
		_ = send.file.Close()
		m.sendsMu.Lock()
		delete(m.sends, send.file)
		m.sendsMu.Unlock()
	}
}

// Write replaces the regular selection and serves it through this monitor's
// existing data-control connection.
func (m *Monitor) Write(ctx context.Context, mime string, data []byte) error {
	return m.WriteWithOrigin(ctx, mime, data, "", nil)
}

// WriteWithOrigin replaces the regular selection and optionally advertises a
// small origin marker beside the regular content. Consumers can read the
// marker without transferring a large image payload.
func (m *Monitor) WriteWithOrigin(ctx context.Context, mime string, data []byte, originMIME string, originData []byte) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closing.Load() {
		return net.ErrClosed
	}
	mimes := []string{mime}
	if strings.HasPrefix(strings.ToLower(mime), "text/") {
		mimes = []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "STRING", "TEXT"}
	} else if strings.HasPrefix(strings.ToLower(mime), "image/") {
		mimes = []string{"image/png"}
	}
	if originMIME != "" && len(originData) > 0 {
		mimes = append(mimes, originMIME)
	}
	offered := make(map[string]struct{}, len(mimes))
	for _, offeredMIME := range mimes {
		offered[offeredMIME] = struct{}{}
	}
	sourceID := m.connection.newID()
	m.sourcesMu.Lock()
	m.sources[sourceID] = source{
		mimes:      offered,
		data:       append([]byte(nil), data...),
		originMIME: originMIME,
		originData: append([]byte(nil), originData...),
	}
	m.sourceIDs = append(m.sourceIDs, sourceID)
	if len(m.sourceIDs) > 64 {
		staleID := m.sourceIDs[0]
		m.sourceIDs = m.sourceIDs[1:]
		delete(m.sources, staleID)
	}
	m.sourcesMu.Unlock()
	fail := func(err error) error {
		m.tombstoneSource(sourceID)
		return err
	}
	if err := m.connection.request(m.managerID, mgrOpcodeCreateDataSource, appendUint32(nil, sourceID)); err != nil {
		return fail(err)
	}
	for _, offeredMIME := range mimes {
		if err := m.connection.request(sourceID, srcOpcodeOffer, encodeString(offeredMIME)); err != nil {
			return fail(err)
		}
	}
	if err := m.connection.request(m.deviceID, devOpcodeSetSelection, appendUint32(nil, sourceID)); err != nil {
		return fail(err)
	}
	if err := m.confirm(ctx); err != nil {
		return fail(err)
	}
	return nil
}
