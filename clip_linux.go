//go:build linux && !android

// Copyright 2024 clipboard-sync Authors
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

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clipboard-sync/internal/waylandclipboard"
	"clipboard-sync/internal/x11clipboard"
)

var isWayland, isX11 = detectLinuxSession()

const defaultMaxRuntime = 3600

type linuxClipboardBackend string

const (
	linuxBackendNativeWayland linuxClipboardBackend = "native-wayland"
	linuxBackendNativeX11     linuxClipboardBackend = "native-x11"
	linuxBackendCommand       linuxClipboardBackend = "command"
)

var selectedLinuxBackend = linuxBackendCommand

var waylandMonitorState struct {
	sync.RWMutex
	monitor *waylandclipboard.Monitor
}

var x11MonitorState struct {
	sync.RWMutex
	monitor *x11clipboard.Monitor
}

var linuxClipboardGeneration atomic.Uint64

// clipboardMonitor represents one command-backend watcher. Stop waits for the
// child process and scanner goroutine, preventing overlapping rotations.
type clipboardMonitor struct {
	errCh    <-chan error
	stopFn   func()
	stopOnce sync.Once
}

func (m *clipboardMonitor) Stop() {
	if m == nil || m.stopFn == nil {
		return
	}
	m.stopOnce.Do(m.stopFn)
}

type clipboardMonitorStarter func(func()) *clipboardMonitor

func failedClipboardMonitor(err error) *clipboardMonitor {
	errCh := make(chan error, 1)
	errCh <- err
	return &clipboardMonitor{errCh: errCh}
}

func initClipboard() {
	requested := appConfig.Clipboard.Backend
	if isWayland && requested != "command" {
		info, err := waylandclipboard.Probe()
		if err == nil {
			selectedLinuxBackend = linuxBackendNativeWayland
			log.Printf("[CLIPBOARD] backend=native reason=data-control-available interface=%s version=%d", info.Interface, info.Version)
			return
		}
		if requested == "native" {
			msg := fmt.Sprintf("原生 Wayland data-control 后端初始化失败: %v", err)
			log.Printf("[ERROR] %s", msg)
			showErrorDialog("clipboard-sync - 环境检查失败", msg)
			log.Fatalf("Exiting: %s", msg)
		}
		log.Printf("[CLIPBOARD] backend=command reason=native-unavailable error=%v", err)
	}
	if isX11 {
		info, err := x11clipboard.Probe()
		if err == nil && requested != "command" {
			selectedLinuxBackend = linuxBackendNativeX11
			log.Printf("[CLIPBOARD] backend=native-x11 reason=xfixes-available version=%d.%d", info.XFixesMajor, info.XFixesMinor)
			return
		}
		if err != nil {
			msg := fmt.Sprintf("X11 XFixes 事件监听初始化失败: %v", err)
			log.Printf("[ERROR] %s", msg)
			showErrorDialog("clipboard-sync - 环境检查失败", msg)
			log.Fatalf("Exiting: %s", msg)
		}
	}
	if !isWayland && !isX11 {
		msg := "无法识别 Linux 图形会话，不能启动剪贴板监听"
		log.Printf("[ERROR] %s", msg)
		showErrorDialog("clipboard-sync - 环境检查失败", msg)
		log.Fatalf("Exiting: %s", msg)
	}

	selectedLinuxBackend = linuxBackendCommand
	missing := requiredCommandBackendTools()
	if len(missing) > 0 {
		msg := fmt.Sprintf("缺少必需程序: %s\n\nWayland 命令后端需要: wl-paste, wl-copy\nX11 命令后端需要: xclip", strings.Join(missing, ", "))
		log.Printf("[ERROR] %s", msg)
		showErrorDialog("clipboard-sync - 环境检查失败", msg)
		log.Fatalf("Exiting: missing required programs: %s", strings.Join(missing, ", "))
	}
	reason := "configured"
	if requested == "auto" {
		reason = "automatic-fallback"
	}
	log.Printf("[CLIPBOARD] backend=command reason=%s session=%s", reason, linuxSessionName())
}

func requiredCommandBackendTools() []string {
	var required []string
	if isWayland {
		required = []string{"wl-paste", "wl-copy"}
	} else {
		required = []string{"xclip"}
	}
	var missing []string
	for _, command := range required {
		if !commandExists(command) {
			missing = append(missing, command)
		}
	}
	return missing
}

func detectLinuxSession() (wayland, x11 bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("XDG_SESSION_TYPE"))) {
	case "wayland":
		return true, false
	case "x11":
		return false, true
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return true, false
	}
	return false, os.Getenv("DISPLAY") != ""
}

func linuxSessionName() string {
	if isWayland {
		return "wayland"
	}
	if isX11 {
		return "x11"
	}
	return "unknown-x11"
}

func showErrorDialog(title, message string) {
	if commandExists("notify-send") {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "notify-send", "--urgency=critical", "--app-name=clipboard-sync", title, message).Run()
	}
}

func getMaxRuntime() time.Duration {
	if appConfig != nil && appConfig.Device.MaxRuntime > 0 {
		return time.Duration(appConfig.Device.MaxRuntime) * time.Second
	}
	return defaultMaxRuntime * time.Second
}

func DetectClipboardMime() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Clipboard.ReadTimeoutMS)*time.Millisecond)
	defer cancel()
	var mimes []string
	if isWayland {
		mimes, _ = detectClipboardMIMEsWayland(ctx)
	} else {
		mimes, _ = detectClipboardMIMEsX11(ctx)
	}
	return selectClipboardMIME(mimes)
}

func ReadClipboardContent(mime string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Clipboard.ReadTimeoutMS)*time.Millisecond)
	defer cancel()
	if selectedLinuxBackend == linuxBackendNativeWayland {
		waylandMonitorState.RLock()
		monitor := waylandMonitorState.monitor
		waylandMonitorState.RUnlock()
		if monitor == nil {
			return nil, waylandclipboard.ErrUnavailable
		}
		return nil, errors.New("native direct read requires a selection event")
	}
	if selectedLinuxBackend == linuxBackendNativeX11 {
		return nil, errors.New("native X11 direct read requires a selection event")
	}
	if isWayland {
		return runCommandOutputBounded(ctx, appConfig.Clipboard.MaxContentBytes, "wl-paste", "-n", "-t", mime)
	}
	return readClipboardContentX11(ctx, mime, appConfig.Clipboard.MaxContentBytes)
}

func SetClipboardContentText(content string, origin ...string) error {
	if selectedLinuxBackend == linuxBackendNativeWayland {
		return writeNativeClipboard("text/plain;charset=utf-8", []byte(content), firstClipboardOrigin(origin))
	}
	if selectedLinuxBackend == linuxBackendNativeX11 {
		return writeNativeX11Clipboard("text/plain;charset=utf-8", []byte(content), firstClipboardOrigin(origin))
	}
	if isWayland {
		return runClipboardWriteCommand("wl-copy", []string{"--type", "text/plain;charset=utf-8"}, []byte(content))
	}
	return runClipboardWriteCommand("xclip", []string{"-selection", "clipboard", "-t", "UTF8_STRING", "-i"}, []byte(content))
}

func SetClipboardContentImage(image []byte, origin ...string) error {
	if selectedLinuxBackend == linuxBackendNativeWayland {
		return writeNativeClipboard("image/png", image, firstClipboardOrigin(origin))
	}
	if selectedLinuxBackend == linuxBackendNativeX11 {
		return writeNativeX11Clipboard("image/png", image, firstClipboardOrigin(origin))
	}
	if isWayland {
		return runClipboardWriteCommand("wl-copy", []string{"--type", "image/png"}, image)
	}
	return runClipboardWriteCommand("xclip", []string{"-selection", "clipboard", "-t", "image/png", "-i"}, image)
}

func writeNativeClipboard(mime string, content []byte, origin string) error {
	waylandMonitorState.RLock()
	monitor := waylandMonitorState.monitor
	waylandMonitorState.RUnlock()
	if monitor == nil {
		return waylandclipboard.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Clipboard.ReadTimeoutMS)*time.Millisecond)
	defer cancel()
	if origin != "" {
		return monitor.WriteWithOrigin(ctx, mime, content, clipboardOriginMIME, []byte(origin))
	}
	return monitor.Write(ctx, mime, content)
}

func writeNativeX11Clipboard(mime string, content []byte, origin string) error {
	x11MonitorState.RLock()
	monitor := x11MonitorState.monitor
	x11MonitorState.RUnlock()
	if monitor == nil {
		return x11clipboard.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Clipboard.ReadTimeoutMS)*time.Millisecond)
	defer cancel()
	if origin != "" {
		return monitor.WriteWithOrigin(ctx, mime, content, clipboardOriginMIME, []byte(origin))
	}
	return monitor.Write(ctx, mime, content)
}

func runClipboardWriteCommand(name string, args []string, content []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Clipboard.ReadTimeoutMS)*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = bytes.NewReader(content)
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func currentClipboardGeneration() uint64 {
	return linuxClipboardGeneration.Load()
}

func ListenClipboardChanges() <-chan ClipboardChange {
	changes := make(chan ClipboardChange, 1)
	processor := newClipboardProcessor(appConfig.Clipboard, changes)
	setActiveClipboardProcessor(processor)
	startClipboardWorker(func() {
		processor.Run(stopCh)
		setActiveClipboardProcessor(nil)
		close(changes)
	})
	switch selectedLinuxBackend {
	case linuxBackendNativeWayland:
		monitor, err := waylandclipboard.Start(context.Background())
		if err != nil {
			if appConfig.Clipboard.Backend == "auto" && len(requiredCommandBackendTools()) == 0 {
				selectedLinuxBackend = linuxBackendCommand
				log.Printf("[CLIPBOARD] backend=command reason=native-start-failed error=%v", err)
				startClipboardWorker(func() { runCommandClipboardSource(processor, stopCh, getMaxRuntime(), startClipboardMonitorPipe) })
				return changes
			}
			log.Fatalf("[CLIPBOARD] native backend start failed: %v", err)
		}
		waylandMonitorState.Lock()
		waylandMonitorState.monitor = monitor
		waylandMonitorState.Unlock()
		startClipboardWorker(func() { runNativeWaylandClipboardSource(processor, stopCh, monitor) })
	case linuxBackendNativeX11:
		monitor, err := x11clipboard.Start(context.Background())
		if err != nil {
			log.Fatalf("[CLIPBOARD] native X11 backend start failed: %v", err)
		}
		x11MonitorState.Lock()
		x11MonitorState.monitor = monitor
		x11MonitorState.Unlock()
		startClipboardWorker(func() { runX11ClipboardSource(processor, stopCh, monitor, true) })
	case linuxBackendCommand:
		if isWayland {
			startClipboardWorker(func() { runCommandClipboardSource(processor, stopCh, getMaxRuntime(), startClipboardMonitorPipe) })
			break
		}
		monitor, err := x11clipboard.Start(context.Background())
		if err != nil {
			log.Fatalf("[CLIPBOARD] X11 event monitor start failed: %v", err)
		}
		startClipboardWorker(func() { runX11ClipboardSource(processor, stopCh, monitor, false) })
	}
	return changes
}

func runNativeWaylandClipboardSource(processor *clipboardProcessor, stop <-chan struct{}, initial *waylandclipboard.Monitor) {
	backoff := time.Second
	monitor := initial
	for {
		if monitor == nil {
			var err error
			monitor, err = waylandclipboard.Start(context.Background())
			if err != nil {
				log.Printf("[CLIPBOARD] backend=native start-error=%v reconnect=%v", err, backoff)
				if !waitClipboardBackoff(stop, backoff) {
					return
				}
				backoff = nextClipboardBackoff(backoff)
				continue
			}
			waylandMonitorState.Lock()
			waylandMonitorState.monitor = monitor
			waylandMonitorState.Unlock()
		}
		info := monitor.Info()
		log.Printf("[CLIPBOARD] backend=native started interface=%s version=%d", info.Interface, info.Version)

		running := true
		for running {
			select {
			case selection, ok := <-monitor.Events():
				if !ok {
					running = false
					break
				}
				generation := linuxClipboardGeneration.Add(1)
				backoff = time.Second
				selected := selection
				processor.Notify(clipboardPlatformEvent{
					Generation: generation,
					MIMEs:      append([]string(nil), selected.MIMEs...),
					Backend:    "native-wayland",
					Read: func(ctx context.Context, mime string, maxBytes int64) (string, []byte, error) {
						content, err := selected.Read(ctx, mime, maxBytes)
						if errors.Is(err, waylandclipboard.ErrTooLarge) {
							err = errClipboardContentTooLarge
						}
						return mime, content, err
					},
					Release: selected.Release,
				})
			case <-stop:
				running = false
			}
		}
		monitor.Close()
		disconnectErr := monitor.Err()
		waylandMonitorState.Lock()
		if waylandMonitorState.monitor == monitor {
			waylandMonitorState.monitor = nil
		}
		waylandMonitorState.Unlock()
		monitor = nil
		select {
		case <-stop:
			log.Print("[CLIPBOARD] backend=native stopped")
			return
		default:
		}
		log.Printf("[CLIPBOARD] backend=native disconnected error=%v reconnect=%v", disconnectErr, backoff)
		if !waitClipboardBackoff(stop, backoff) {
			return
		}
		backoff = nextClipboardBackoff(backoff)
	}
}

func runX11ClipboardSource(processor *clipboardProcessor, stop <-chan struct{}, initial *x11clipboard.Monitor, native bool) {
	backoff := time.Second
	monitor := initial
	backend := "native-x11"
	if !native {
		backend = "command-x11"
	}
	for {
		if monitor == nil {
			var err error
			monitor, err = x11clipboard.Start(context.Background())
			if err != nil {
				log.Printf("[CLIPBOARD] backend=%s start-error=%v reconnect=%v", backend, err, backoff)
				if !waitClipboardBackoff(stop, backoff) {
					return
				}
				backoff = nextClipboardBackoff(backoff)
				continue
			}
			if native {
				x11MonitorState.Lock()
				x11MonitorState.monitor = monitor
				x11MonitorState.Unlock()
			}
		}
		info := monitor.Info()
		log.Printf("[CLIPBOARD] backend=%s started xfixes=%d.%d", backend, info.XFixesMajor, info.XFixesMinor)

		running := true
		for running {
			select {
			case selection, ok := <-monitor.Events():
				if !ok {
					running = false
					break
				}
				generation := linuxClipboardGeneration.Add(1)
				backoff = time.Second
				selected := selection
				processor.Notify(clipboardPlatformEvent{
					Generation: generation,
					MIMEs:      append([]string(nil), selected.MIMEs...),
					Backend:    backend,
					Read: func(ctx context.Context, mime string, maxBytes int64) (string, []byte, error) {
						var content []byte
						var err error
						if native {
							content, err = selected.Read(ctx, mime, maxBytes)
						} else {
							content, err = readClipboardContentX11(ctx, mime, maxBytes)
						}
						if errors.Is(err, x11clipboard.ErrTooLarge) {
							err = errClipboardContentTooLarge
						}
						if clipboardContentKind(mime) == "text" && mime != clipboardOriginMIME {
							content = convertToUTF8(content, mime)
							mime = "text/plain;charset=utf-8"
						}
						return mime, content, err
					},
					Release: selected.Release,
				})
			case <-stop:
				running = false
			}
		}
		monitor.Close()
		disconnectErr := monitor.Err()
		if native {
			x11MonitorState.Lock()
			if x11MonitorState.monitor == monitor {
				x11MonitorState.monitor = nil
			}
			x11MonitorState.Unlock()
		}
		monitor = nil
		select {
		case <-stop:
			log.Printf("[CLIPBOARD] backend=%s stopped", backend)
			return
		default:
		}
		log.Printf("[CLIPBOARD] backend=%s disconnected error=%v reconnect=%v", backend, disconnectErr, backoff)
		if !waitClipboardBackoff(stop, backoff) {
			return
		}
		backoff = nextClipboardBackoff(backoff)
	}
}

func nextClipboardBackoff(current time.Duration) time.Duration {
	if current < 16*time.Second {
		return current * 2
	}
	if current < 30*time.Second {
		return 30 * time.Second
	}
	return 60 * time.Second
}

func waitClipboardBackoff(stop <-chan struct{}, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-stop:
		return false
	}
}

func runCommandClipboardSource(processor *clipboardProcessor, stop <-chan struct{}, maxRuntime time.Duration, start clipboardMonitorStarter) {
	backoff := 3 * time.Second
	for {
		notify := func() {
			generation := linuxClipboardGeneration.Add(1)
			processor.Notify(commandClipboardEvent(generation))
		}
		monitor := start(notify)
		timer := time.NewTimer(maxRuntime)
		var restart bool
		select {
		case err := <-monitor.errCh:
			timer.Stop()
			monitor.Stop()
			log.Printf("[CLIPBOARD] backend=command watcher-exit error=%v restart=%v", err, backoff)
			restart = true
		case <-timer.C:
			log.Printf("[CLIPBOARD] backend=command rotate=%v", maxRuntime)
			monitor.Stop()
			backoff = 3 * time.Second
		case <-stop:
			timer.Stop()
			monitor.Stop()
			log.Print("[CLIPBOARD] backend=command stopped")
			return
		}
		if restart {
			if !waitClipboardBackoff(stop, backoff) {
				return
			}
			if backoff < 60*time.Second {
				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			}
		}
	}
}

func commandClipboardEvent(generation uint64) clipboardPlatformEvent {
	return clipboardPlatformEvent{
		Generation: generation,
		Backend:    "command-" + linuxSessionName(),
		Debounce:   clipboardEventDebounce,
		Read: func(ctx context.Context, _ string, maxBytes int64) (string, []byte, error) {
			if isWayland {
				return readCommandSelectionWayland(ctx, maxBytes)
			}
			return readCommandSelectionX11(ctx, maxBytes)
		},
	}
}

func readCommandSelectionWayland(ctx context.Context, maxBytes int64) (string, []byte, error) {
	mimes, err := detectClipboardMIMEsWayland(ctx)
	if err != nil {
		return "", nil, err
	}
	mime := selectClipboardMIME(mimes)
	if mime == "" {
		return "", nil, nil
	}
	content, err := runCommandOutputBounded(ctx, maxBytes, "wl-paste", "-n", "-t", mime)
	if clipboardContentKind(mime) == "text" {
		content = convertToUTF8(content, mime)
	}
	return mime, content, err
}

func readCommandSelectionX11(ctx context.Context, maxBytes int64) (string, []byte, error) {
	mimes, err := detectClipboardMIMEsX11(ctx)
	if err != nil {
		return "", nil, err
	}
	mime := selectClipboardMIME(mimes)
	if mime == "" {
		return "", nil, nil
	}
	content, err := readClipboardContentX11(ctx, mime, maxBytes)
	if clipboardContentKind(mime) == "text" {
		content = convertToUTF8(content, mime)
	}
	return mime, content, err
}

func detectClipboardMIMEsWayland(ctx context.Context) ([]string, error) {
	output, err := runCommandOutputBounded(ctx, 1024*1024, "wl-paste", "--list-types")
	if err != nil {
		return nil, err
	}
	var mimes []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		if mime := strings.TrimSpace(scanner.Text()); mime != "" {
			mimes = append(mimes, mime)
		}
	}
	return mimes, scanner.Err()
}

func detectClipboardMIMEsX11(ctx context.Context) ([]string, error) {
	output, err := runCommandOutputBounded(ctx, 1024*1024, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(output)), nil
}

func readClipboardContentX11(ctx context.Context, mime string, maxBytes int64) ([]byte, error) {
	args := []string{"-selection", "clipboard", "-t", mime, "-o"}
	if mime == "UTF8_STRING" || mime == "STRING" || mime == "TEXT" {
		args = []string{"-selection", "clipboard", "-o"}
	}
	return runCommandOutputBounded(ctx, maxBytes, "xclip", args...)
}

func runCommandOutputBounded(ctx context.Context, maxBytes int64, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	data, readErr := readAllBounded(stdout, maxBytes)
	if readErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, waitErr
	}
	return data, nil
}

func startClipboardMonitorPipe(notify func()) *clipboardMonitor {
	return startCommandWatcher("wl-paste", []string{"-w", "date", "+%s"}, notify, true)
}

func startCommandWatcher(name string, args []string, notify func(), ignoreFirst bool) *clipboardMonitor {
	errCh := make(chan error, 1)
	command := exec.Command(name, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return failedClipboardMonitor(fmt.Errorf("创建 %s 管道失败: %w", name, err))
	}
	if err := command.Start(); err != nil {
		return failedClipboardMonitor(fmt.Errorf("启动 %s 失败: %w", name, err))
	}
	cancel := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdout)
		first := true
		for scanner.Scan() {
			if first && ignoreFirst {
				first = false
				continue
			}
			first = false
			notify()
		}
		waitErr := command.Wait()
		monitorErr := waitErr
		if scannerErr := scanner.Err(); scannerErr != nil {
			monitorErr = scannerErr
		}
		if monitorErr == nil {
			monitorErr = fmt.Errorf("%s watcher reached EOF", name)
		}
		select {
		case errCh <- monitorErr:
		case <-cancel:
		case <-stopCh:
		}
	}()
	log.Printf("[CLIPBOARD] backend=command watcher=%s started", name)
	return &clipboardMonitor{
		errCh: errCh,
		stopFn: func() {
			close(cancel)
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			<-done
		},
	}
}
