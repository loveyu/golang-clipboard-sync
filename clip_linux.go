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
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var (
	isWayland = os.Getenv("XDG_SESSION_TYPE") == "wayland"
	isX11     = os.Getenv("XDG_SESSION_TYPE") == "x11" || os.Getenv("DISPLAY") != ""
)

const (
	defaultMaxRuntime = 3600 // 默认最大运行时间（秒）
	envMaxRuntime     = "CLIPBOARD_MAX_RUNTIME"
)

func initClipboard() {
	if isWayland {
		log.Println("Linux剪贴板初始化 (Wayland)")
	} else if isX11 {
		log.Println("Linux剪贴板初始化 (X11)")
	} else {
		log.Println("Linux剪贴板初始化 (未知环境，默认X11)")
	}
}

// getMaxRuntime 读取最大运行时间配置
func getMaxRuntime() time.Duration {
	val := os.Getenv(envMaxRuntime)
	if val == "" {
		return time.Duration(defaultMaxRuntime) * time.Second
	}
	seconds, err := strconv.Atoi(val)
	if err != nil || seconds <= 0 {
		log.Printf("[WARN] 无效的 %s 值 '%s'，使用默认值 %ds", envMaxRuntime, val, defaultMaxRuntime)
		return time.Duration(defaultMaxRuntime) * time.Second
	}
	return time.Duration(seconds) * time.Second
}

// DetectClipboardMime 检测当前剪贴板的MIME类型
func DetectClipboardMime() string {
	if isWayland {
		return detectClipboardMimeWayland()
	}
	return detectClipboardMimeX11()
}

// ReadClipboardContent 依据类型读取剪贴板
func ReadClipboardContent(mime string) ([]byte, error) {
	if isWayland {
		return readClipboardContentWayland(mime)
	}
	return readClipboardContentX11(mime)
}

// SetClipboardContentText 设置剪贴板文本内容
func SetClipboardContentText(content string) error {
	if isWayland {
		return setClipboardTextWayland(content)
	}
	return setClipboardTextX11(content)
}

// SetClipboardContentImage 设置剪贴板图片内容
func SetClipboardContentImage(image []byte) error {
	if isWayland {
		return setClipboardImageWayland(image)
	}
	return setClipboardImageX11(image)
}

// ListenClipboardChanges 监听剪贴板变化，包含自动重启机制
func ListenClipboardChanges() <-chan ClipboardChange {
	changes := make(chan ClipboardChange)
	go func() {
		defer close(changes)

		maxRuntime := getMaxRuntime()
		backoff := 3 * time.Second
		const maxBackoff = 60 * time.Second

		for {
			errCh := startClipboardMonitorPipe(changes)
			timer := time.NewTimer(maxRuntime)

			select {
			case err := <-errCh:
				timer.Stop()
				if err != nil {
					log.Printf("[CLIPBOARD] 监听进程异常退出: %v，%v 后重启...", err, backoff)
				} else {
					log.Printf("[CLIPBOARD] 监听进程退出，%v 后重启...", backoff)
				}
			case <-timer.C:
				log.Printf("[CLIPBOARD] 达到最大运行时间 (%v)，重启监听...", maxRuntime)
				backoff = 3 * time.Second // 正常重启重置退避
				continue
			case <-stopCh:
				timer.Stop()
				return
			}

			select {
			case <-time.After(backoff):
			case <-stopCh:
				return
			}

			// 指数退避
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}()
	return changes
}

// startClipboardMonitorPipe 启动平台相关的剪贴板监听管道
func startClipboardMonitorPipe(changes chan<- ClipboardChange) <-chan error {
	if isWayland {
		return listenClipboardChangesWaylandPipe(changes)
	}
	return listenClipboardChangesX11Pipe(changes)
}

// ==================== Wayland 实现 ====================

func detectClipboardMimeWayland() string {
	cmd := exec.Command("wl-paste", "--list-types")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "text/plain"
	}
	types := strings.Fields(string(output))
	for _, t := range types {
		if t == "text/plain" || t == "text/html" || t == "text/plain;charset=utf-8" {
			return "text/plain"
		}
	}
	for _, t := range types {
		if strings.HasPrefix(t, "image/") {
			return t
		}
	}
	if len(types) > 0 {
		return types[0]
	}
	return "text/plain"
}

func readClipboardContentWayland(mime string) ([]byte, error) {
	cmd := exec.Command("wl-paste", "-t", mime)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	output, err := io.ReadAll(stdout)
	if err != nil {
		return nil, err
	}
	cmd.Wait()
	output = removeTrailingNewline(output)
	return output, nil
}

func setClipboardTextWayland(content string) error {
	cmd := exec.Command("wl-copy", "--type", "text/plain")
	reader := strings.NewReader(content)
	cmd.Stdin = reader
	return cmd.Run()
}

func setClipboardImageWayland(image []byte) error {
	tmpImageFile, err := os.CreateTemp("", "clipboard_image_*.png")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer os.Remove(tmpImageFile.Name())

	if _, err := tmpImageFile.Write(image); err != nil {
		return fmt.Errorf("failed to write image data to temporary file: %v", err)
	}

	tmpImageFile.Close()

	cmd := exec.Command("bash", "-c", "wl-copy --type=image/png < "+tmpImageFile.Name())
	return cmd.Run()
}

// listenClipboardChangesWaylandPipe 启动 wl-paste 监听，通过 error channel 报告异常
func listenClipboardChangesWaylandPipe(changes chan<- ClipboardChange) <-chan error {
	errCh := make(chan error, 1)

	cmd := exec.Command("wl-paste", "-w", "date", "+%s")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		errCh <- fmt.Errorf("创建 wl-paste 管道失败: %w", err)
		return errCh
	}

	if err := cmd.Start(); err != nil {
		errCh <- fmt.Errorf("启动 wl-paste 失败: %w", err)
		return errCh
	}

	go func() {
		defer func() {
			cmd.Process.Kill()
			cmd.Wait()
		}()

		scanner := bufio.NewScanner(stdout)
		loop := -1
		for scanner.Scan() {
			loop++
			if loop == 0 {
				continue
			}
			timestamp := scanner.Text()
			mime := detectClipboardMimeWayland()
			content, _ := readClipboardContentWayland(mime)
			debugLog("剪贴板变更 (Wayland) - MIME: %s, 内容: %s", mime, debugFormatContent(content))
			var ts int64
			fmt.Sscanf(timestamp, "%d", &ts)
			select {
			case changes <- ClipboardChange{
				Timestamp: ts,
				Mime:      mime,
				Content:   content,
			}:
			case <-stopCh:
				errCh <- nil
				return
			}
		}

		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("wl-paste 管道错误: %w", err)
		} else {
			errCh <- fmt.Errorf("wl-paste 进程退出 (EOF)")
		}
	}()

	log.Print("[CLIPBOARD] 开始通过 wl-paste 监听剪贴板 (Wayland)")
	return errCh
}

// ==================== X11 实现 ====================

func detectClipboardMimeX11() string {
	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "text/plain"
	}
	types := strings.Fields(string(output))
	for _, t := range types[1:] {
		if t == "TEXT" || t == "STRING" || t == "UTF8_STRING" {
			return "text/plain"
		}
		if strings.HasPrefix(t, "image/") || t == "image/png" || t == "image/bmp" {
			return "image/png"
		}
	}
	return "text/plain"
}

func readClipboardContentX11(mime string) ([]byte, error) {
	selection := "clipboard"
	if strings.HasPrefix(mime, "image/") {
		cmd := exec.Command("xclip", "-selection", selection, "-t", "image/png", "-o")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, err
		}
		return output, nil
	}
	cmd := exec.Command("xclip", "-selection", selection, "-o")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	output = removeTrailingNewline(output)
	return output, nil
}

func setClipboardTextX11(content string) error {
	cmd := exec.Command("xclip", "-selection", "clipboard")
	cmd.Stdin = strings.NewReader(content)
	return cmd.Run()
}

func setClipboardImageX11(image []byte) error {
	tmpImageFile, err := os.CreateTemp("", "clipboard_image_*.png")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer os.Remove(tmpImageFile.Name())

	if _, err := tmpImageFile.Write(image); err != nil {
		return fmt.Errorf("failed to write image data to temporary file: %v", err)
	}

	tmpImageFile.Close()

	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-i")
	cmd.Stdin, err = os.Open(tmpImageFile.Name())
	if err != nil {
		return err
	}
	return cmd.Run()
}

// listenClipboardChangesX11Pipe 启动 xsel 监听，通过 error channel 报告异常
func listenClipboardChangesX11Pipe(changes chan<- ClipboardChange) <-chan error {
	errCh := make(chan error, 1)

	cmd := exec.Command("xsel", "--clipboard", "--watch")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		errCh <- fmt.Errorf("创建 xsel 管道失败: %w", err)
		return errCh
	}

	if err := cmd.Start(); err != nil {
		errCh <- fmt.Errorf("启动 xsel 失败: %w", err)
		return errCh
	}

	go func() {
		defer func() {
			cmd.Process.Kill()
			cmd.Wait()
		}()

		scanner := bufio.NewScanner(stdout)
		loop := -1
		for scanner.Scan() {
			loop++
			if loop == 0 {
				continue
			}
			mime := detectClipboardMimeX11()
			content, _ := readClipboardContentX11(mime)
			debugLog("剪贴板变更 (X11) - MIME: %s, 内容: %s", mime, debugFormatContent(content))
			select {
			case changes <- ClipboardChange{
				Timestamp: time.Now().Unix(),
				Mime:      mime,
				Content:   content,
			}:
			case <-stopCh:
				errCh <- nil
				return
			}
		}

		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("xsel 管道错误: %w", err)
		} else {
			errCh <- fmt.Errorf("xsel 进程退出 (EOF)")
		}
	}()

	log.Print("[CLIPBOARD] 开始通过 xsel 监听剪贴板 (X11)")
	return errCh
}

// ==================== 辅助函数 ====================

func debugLog(format string, args ...interface{}) {
	if debugClipboard {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func debugFormatContent(content []byte) string {
	displayContent := string(content)
	if len(displayContent) > 100 {
		displayContent = displayContent[:100] + "..."
	}
	return displayContent
}
