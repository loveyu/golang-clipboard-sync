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
)

var (
	isWayland = os.Getenv("XDG_SESSION_TYPE") == "wayland" || os.Getenv("WAYLAND_DISPLAY") != ""
	isX11     = os.Getenv("XDG_SESSION_TYPE") == "x11" || os.Getenv("DISPLAY") != ""
)

const defaultMaxRuntime = 3600

type linuxClipboardBackend string

const (
	linuxBackendNative  linuxClipboardBackend = "native"
	linuxBackendCommand linuxClipboardBackend = "command"
)

var selectedLinuxBackend = linuxBackendCommand

var nativeMonitorState struct {
	sync.RWMutex
	monitor *waylandclipboard.Monitor
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
			selectedLinuxBackend = linuxBackendNative
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

	selectedLinuxBackend = linuxBackendCommand
	missing := requiredCommandBackendTools()
	if len(missing) > 0 {
		msg := fmt.Sprintf("缺少必需程序: %s\n\nWayland 命令后端需要: wl-paste, wl-copy\nX11 需要: xclip, xsel", strings.Join(missing, ", "))
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
		required = []string{"xclip", "xsel"}
	}
	var missing []string
	for _, command := range required {
		if !commandExists(command) {
			missing = append(missing, command)
		}
	}
	return missing
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
	attempts := []struct {
		name string
		args []string
	}{
		{name: "zenity", args: []string{"--error", "--title", title, "--text", message, "--no-markup"}},
		{name: "kdialog", args: []string{"--error", message, "--title", title}},
		{name: "xmessage", args: []string{"-center", "-title", title, message}},
	}
	for _, attempt := range attempts {
		if commandExists(attempt.name) {
			_ = exec.Command(attempt.name, attempt.args...).Run()
			return
		}
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
	if selectedLinuxBackend == linuxBackendNative {
		nativeMonitorState.RLock()
		monitor := nativeMonitorState.monitor
		nativeMonitorState.RUnlock()
		if monitor == nil {
			return nil, waylandclipboard.ErrUnavailable
		}
		return nil, errors.New("native direct read requires a selection event")
	}
	if isWayland {
		return runCommandOutputBounded(ctx, appConfig.Clipboard.MaxContentBytes, "wl-paste", "-n", "-t", mime)
	}
	return readClipboardContentX11(ctx, mime, appConfig.Clipboard.MaxContentBytes)
}

func SetClipboardContentText(content string) error {
	if selectedLinuxBackend == linuxBackendNative {
		return writeNativeClipboard("text/plain;charset=utf-8", []byte(content))
	}
	if isWayland {
		return runClipboardWriteCommand("wl-copy", []string{"--type", "text/plain;charset=utf-8"}, []byte(content))
	}
	return runClipboardWriteCommand("xclip", []string{"-selection", "clipboard", "-t", "UTF8_STRING", "-i"}, []byte(content))
}

func SetClipboardContentImage(image []byte) error {
	if selectedLinuxBackend == linuxBackendNative {
		return writeNativeClipboard("image/png", image)
	}
	if isWayland {
		return runClipboardWriteCommand("wl-copy", []string{"--type", "image/png"}, image)
	}
	return runClipboardWriteCommand("xclip", []string{"-selection", "clipboard", "-t", "image/png", "-i"}, image)
}

func writeNativeClipboard(mime string, content []byte) error {
	nativeMonitorState.RLock()
	monitor := nativeMonitorState.monitor
	nativeMonitorState.RUnlock()
	if monitor == nil {
		return waylandclipboard.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Clipboard.ReadTimeoutMS)*time.Millisecond)
	defer cancel()
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
	if selectedLinuxBackend == linuxBackendNative {
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
		nativeMonitorState.Lock()
		nativeMonitorState.monitor = monitor
		nativeMonitorState.Unlock()
		startClipboardWorker(func() { runNativeClipboardSource(processor, stopCh, monitor) })
	} else {
		startClipboardWorker(func() { runCommandClipboardSource(processor, stopCh, getMaxRuntime(), startClipboardMonitorPipe) })
	}
	return changes
}

func runNativeClipboardSource(processor *clipboardProcessor, stop <-chan struct{}, initial *waylandclipboard.Monitor) {
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
			nativeMonitorState.Lock()
			nativeMonitorState.monitor = monitor
			nativeMonitorState.Unlock()
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
		nativeMonitorState.Lock()
		if nativeMonitorState.monitor == monitor {
			nativeMonitorState.monitor = nil
		}
		nativeMonitorState.Unlock()
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
	if isWayland {
		return startCommandWatcher("wl-paste", []string{"-w", "date", "+%s"}, notify, true)
	}
	return startCommandWatcher("xsel", []string{"--clipboard", "--watch"}, notify, true)
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
