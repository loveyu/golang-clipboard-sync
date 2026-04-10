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
	"strings"
	"time"
)

var (
	isWayland = os.Getenv("XDG_SESSION_TYPE") == "wayland"
	isX11     = os.Getenv("XDG_SESSION_TYPE") == "x11" || os.Getenv("DISPLAY") != ""
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

// ListenClipboardChanges 监听剪贴板返回
func ListenClipboardChanges() <-chan ClipboardChange {
	if isWayland {
		return listenClipboardChangesWayland()
	}
	return listenClipboardChangesX11()
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

func listenClipboardChangesWayland() <-chan ClipboardChange {
	changes := make(chan ClipboardChange)

	cmd := exec.Command("wl-paste", "-w", "date", "+%s")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		defer close(changes)
		defer cmd.Wait()
		defer cmd.Process.Kill()

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
			changes <- ClipboardChange{
				Timestamp: ts,
				Mime:      mime,
				Content:   content,
			}
		}
	}()

	log.Print("开始通过wl-paste监听剪贴板 (Wayland)")
	return changes
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

func listenClipboardChangesX11() <-chan ClipboardChange {
	changes := make(chan ClipboardChange)

	cmd := exec.Command("xsel", "--clipboard", "--watch")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		defer close(changes)
		defer cmd.Wait()
		defer cmd.Process.Kill()

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
			changes <- ClipboardChange{
				Timestamp: time.Now().Unix(),
				Mime:      mime,
				Content:   content,
			}
		}
	}()

	log.Print("开始通过xsel监听剪贴板 (X11)")
	return changes
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
