//go:build linux && !android

package waylandclipboard

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestSelectionOfferRequestsOneMIMEAndReadsBoundedData(t *testing.T) {
	monitor, server := newProtocolTestMonitor(t)
	defer monitor.Close()
	defer server.Close()

	sendProtocolEvent(t, server, monitor.deviceID, devEvtDataOffer, appendUint32(nil, 41))
	sendProtocolEvent(t, server, 41, offerEvtOffer, encodeString("text/plain;charset=utf-8"))
	sendProtocolEvent(t, server, 41, offerEvtOffer, encodeString("image/png"))
	sendProtocolEvent(t, server, monitor.deviceID, devEvtSelection, appendUint32(nil, 41))

	selection := waitProtocolSelection(t, monitor.Events())
	defer selection.Release()
	if selection.Generation != 1 || len(selection.MIMEs) != 2 {
		t.Fatalf("selection = generation %d, mimes %v", selection.Generation, selection.MIMEs)
	}

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		objectID, opcode, mime, fd := readReceiveRequest(t, server)
		if objectID != 41 || opcode != offerOpcodeReceive || mime != "image/png" {
			t.Errorf("receive request = object %d opcode %d mime %q", objectID, opcode, mime)
		}
		file := os.NewFile(uintptr(fd), "fake-wayland-writer")
		_, _ = file.Write([]byte("part-1"))
		_, _ = file.Write([]byte("-part-2"))
		_ = file.Close()
	}()

	content, err := selection.Read(context.Background(), "image/png", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "part-1-part-2" {
		t.Fatalf("content = %q", content)
	}
	<-requestDone
}

func TestSelectionReadLimitAndCancellation(t *testing.T) {
	t.Run("limit", func(t *testing.T) {
		monitor, server := newProtocolTestMonitor(t)
		defer monitor.Close()
		defer server.Close()
		selection := protocolTestSelection(monitor, 50)
		go func() {
			_, _, _, fd := readReceiveRequest(t, server)
			file := os.NewFile(uintptr(fd), "fake-wayland-writer")
			_, _ = file.Write([]byte("123456"))
			_ = file.Close()
		}()
		if _, err := selection.Read(context.Background(), "text/plain", 5); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("error = %v，期望 ErrTooLarge", err)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		monitor, server := newProtocolTestMonitor(t)
		defer monitor.Close()
		defer server.Close()
		selection := protocolTestSelection(monitor, 51)
		fdReady := make(chan int, 1)
		go func() {
			_, _, _, fd := readReceiveRequest(t, server)
			fdReady <- fd
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := selection.Read(ctx, "text/plain", 1024)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v，期望 DeadlineExceeded", err)
		}
		_ = syscall.Close(<-fdReady)
	})
}

func TestMonitorWriteServesTextOnSameConnection(t *testing.T) {
	monitor, server := newProtocolTestMonitor(t)
	defer monitor.Close()
	defer server.Close()

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- monitor.Write(context.Background(), "text/plain", []byte("native-write"))
	}()
	objectID, opcode, body := readProtocolRequest(t, server)
	if objectID != monitor.managerID || opcode != mgrOpcodeCreateDataSource || len(body) < 4 {
		t.Fatalf("create source request = object %d opcode %d body %v", objectID, opcode, body)
	}
	sourceID := binary.LittleEndian.Uint32(body[0:4])
	wantMIMEs := []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "STRING", "TEXT"}
	for _, wantMIME := range wantMIMEs {
		objectID, opcode, body = readProtocolRequest(t, server)
		mime, _, err := decodeString(body, 0)
		if err != nil {
			t.Fatal(err)
		}
		if objectID != sourceID || opcode != srcOpcodeOffer || mime != wantMIME {
			t.Fatalf("source offer = object %d opcode %d mime %q，期望 %q", objectID, opcode, mime, wantMIME)
		}
	}
	objectID, opcode, body = readProtocolRequest(t, server)
	if objectID != monitor.deviceID || opcode != devOpcodeSetSelection || binary.LittleEndian.Uint32(body[0:4]) != sourceID {
		t.Fatalf("set selection request = object %d opcode %d body %v", objectID, opcode, body)
	}
	objectID, opcode, body = readProtocolRequest(t, server)
	if objectID != wlDisplayID || opcode != dispOpcodeSync || len(body) < 4 {
		t.Fatalf("sync request = object %d opcode %d body %v", objectID, opcode, body)
	}
	sendProtocolEvent(t, server, binary.LittleEndian.Uint32(body[0:4]), 0, nil)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	message := protocolMessage(sourceID, srcEvtSend, encodeString("text/plain"))
	if _, _, err := server.WriteMsgUnix(message, syscall.UnixRights(int(writer.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	content, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "native-write" {
		t.Fatalf("served content = %q", content)
	}
}

func TestCancelledSourceSendConsumesItsDescriptor(t *testing.T) {
	monitor, server := newProtocolTestMonitor(t)
	defer monitor.Close()
	defer server.Close()

	const cancelledID uint32 = 70
	const currentID uint32 = 71
	textMIME := map[string]struct{}{"text/plain": {}}
	monitor.sourcesMu.Lock()
	monitor.sources[cancelledID] = source{mimes: textMIME, data: []byte("stale")}
	monitor.sources[currentID] = source{mimes: textMIME, data: []byte("current")}
	monitor.sourcesMu.Unlock()

	staleReader, staleWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	currentReader, currentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	body := encodeString("text/plain")
	message := append(protocolMessage(cancelledID, srcEvtCancelled, nil), protocolMessage(cancelledID, srcEvtSend, body)...)
	message = append(message, protocolMessage(currentID, srcEvtSend, body)...)
	if _, _, err := server.WriteMsgUnix(message, syscall.UnixRights(int(staleWriter.Fd()), int(currentWriter.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	_ = staleWriter.Close()
	_ = currentWriter.Close()

	staleContent, err := io.ReadAll(staleReader)
	_ = staleReader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(staleContent) != 0 {
		t.Fatalf("cancelled source content = %q，期望为空", staleContent)
	}
	currentContent, err := io.ReadAll(currentReader)
	_ = currentReader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(currentContent) != "current" {
		t.Fatalf("current source content = %q，期望 current", currentContent)
	}
}

func TestNativeWaylandConnectionIntegration(t *testing.T) {
	if os.Getenv("CLIPBOARD_WAYLAND_NATIVE_INTEGRATION") != "1" {
		t.Skip("设置 CLIPBOARD_WAYLAND_NATIVE_INTEGRATION=1 后运行原生 Wayland 连接测试")
	}
	ctx, cancel := context.WithCancel(context.Background())
	monitor, err := Start(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	info := monitor.Info()
	if info.Interface != "ext_data_control_manager_v1" && info.Interface != "zwlr_data_control_manager_v1" {
		t.Fatalf("unexpected manager: %+v", info)
	}
	cancel()
	monitor.Close()
	if err := monitor.Err(); err != nil {
		t.Fatalf("正常关闭返回错误: %v", err)
	}
}

func TestNativeWaylandWriteIntegration(t *testing.T) {
	if os.Getenv("CLIPBOARD_WAYLAND_NATIVE_INTEGRATION") != "1" {
		t.Skip("设置 CLIPBOARD_WAYLAND_NATIVE_INTEGRATION=1 后运行原生 Wayland 写入测试")
	}
	if _, err := exec.LookPath("wl-paste"); err != nil {
		t.Skip("wl-paste 不可用")
	}
	ctx, cancel := context.WithCancel(context.Background())
	monitor, err := Start(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		monitor.Close()
	}()

	const want = "clipboard-sync-native-write"
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer writeCancel()
	writeErrors := make(chan error, 2)
	for range 2 {
		go func() {
			writeErrors <- monitor.Write(writeCtx, "text/plain;charset=utf-8", []byte(want))
		}()
	}
	for range 2 {
		if err := <-writeErrors; err != nil {
			t.Fatal(err)
		}
	}
	var own Selection
	select {
	case own = <-monitor.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("未收到本连接写入产生的 selection")
	}
	ownContent, err := own.Read(writeCtx, "text/plain;charset=utf-8", 1024)
	own.Release()
	if err != nil {
		t.Fatalf("读取本连接 selection: %v", err)
	}
	if string(ownContent) != want {
		t.Fatalf("本连接 selection = %q，期望 %q", ownContent, want)
	}
	pasteCtx, pasteCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pasteCancel()
	out, err := exec.CommandContext(pasteCtx, "wl-paste", "--no-newline").Output()
	if err != nil {
		t.Fatalf("wl-paste: %v", err)
	}
	if string(out) != want {
		t.Fatalf("wl-paste = %q，期望 %q", out, want)
	}
}

func newProtocolTestMonitor(t *testing.T) (*Monitor, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	clientFile := os.NewFile(uintptr(fds[0]), "wayland-test-client")
	serverFile := os.NewFile(uintptr(fds[1]), "wayland-test-server")
	clientConn, err := net.FileConn(clientFile)
	if err != nil {
		t.Fatal(err)
	}
	serverConn, err := net.FileConn(serverFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = clientFile.Close()
	_ = serverFile.Close()
	client := clientConn.(*net.UnixConn)
	server := serverConn.(*net.UnixConn)
	monitor := &Monitor{
		connection: &conn{socket: client, nextID: 100},
		managerID:  11,
		deviceID:   10,
		events:     make(chan Selection, 1),
		done:       make(chan struct{}),
		sources:    make(map[uint32]source),
		sends:      make(map[*os.File]struct{}),
		sendQueue:  make(chan sourceSend, 64),
		sendDone:   make(chan struct{}),
		syncs:      make(map[uint32]chan syncResult),
	}
	go monitor.serveLoop()
	go monitor.eventLoop(make(map[uint32][]string))
	return monitor, server
}

func protocolTestSelection(monitor *Monitor, offerID uint32) Selection {
	return Selection{
		Generation: 1,
		MIMEs:      []string{"text/plain"},
		state:      &selectionState{monitor: monitor, offerID: offerID},
	}
}

func sendProtocolEvent(t *testing.T, server *net.UnixConn, objectID uint32, opcode uint16, body []byte) {
	t.Helper()
	message := protocolMessage(objectID, opcode, body)
	if _, err := server.Write(message); err != nil {
		t.Fatal(err)
	}
}

func protocolMessage(objectID uint32, opcode uint16, body []byte) []byte {
	message := make([]byte, 8+len(body))
	binary.LittleEndian.PutUint32(message[0:4], objectID)
	binary.LittleEndian.PutUint32(message[4:8], uint32(len(message))<<16|uint32(opcode))
	copy(message[8:], body)
	return message
}

func waitProtocolSelection(t *testing.T, events <-chan Selection) Selection {
	t.Helper()
	select {
	case selection := <-events:
		return selection
	case <-time.After(time.Second):
		t.Fatal("等待 selection 超时")
		return Selection{}
	}
}

func readReceiveRequest(t *testing.T, server *net.UnixConn) (uint32, uint16, string, int) {
	t.Helper()
	var payload [4096]byte
	var control [256]byte
	n, controlN, _, _, err := server.ReadMsgUnix(payload[:], control[:])
	if err != nil {
		t.Error(err)
		return 0, 0, "", -1
	}
	if n < 8 {
		t.Errorf("request too short: %d", n)
		return 0, 0, "", -1
	}
	objectID := binary.LittleEndian.Uint32(payload[0:4])
	header := binary.LittleEndian.Uint32(payload[4:8])
	opcode := uint16(header & 0xffff)
	mime, _, err := decodeString(payload[8:n], 0)
	if err != nil {
		t.Error(err)
	}
	messages, err := syscall.ParseSocketControlMessage(control[:controlN])
	if err != nil {
		t.Error(err)
		return objectID, opcode, mime, -1
	}
	for i := range messages {
		fds, err := syscall.ParseUnixRights(&messages[i])
		if err == nil && len(fds) > 0 {
			return objectID, opcode, mime, fds[0]
		}
	}
	t.Error("receive request did not include an fd")
	return objectID, opcode, mime, -1
}

func readProtocolRequest(t *testing.T, server *net.UnixConn) (uint32, uint16, []byte) {
	t.Helper()
	header := make([]byte, 8)
	if _, err := io.ReadFull(server, header); err != nil {
		t.Fatal(err)
	}
	objectID := binary.LittleEndian.Uint32(header[0:4])
	word := binary.LittleEndian.Uint32(header[4:8])
	size := int(word >> 16)
	body := make([]byte, size-8)
	if _, err := io.ReadFull(server, body); err != nil {
		t.Fatal(err)
	}
	return objectID, uint16(word & 0xffff), body
}
