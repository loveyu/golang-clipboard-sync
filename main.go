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
	"bytes"
	"encoding/base64"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// initEnv 初始化环境变量，加载 .env 文件
func initEnv(envFile string) {
	var envPaths []string

	// 优先使用指定的 env 文件
	if envFile != "" {
		envPaths = []string{envFile}
	} else {
		// 默认查找路径
		envPaths = []string{".env", ".env.local"}

		// 获取可执行文件所在目录
		exePath, err := os.Executable()
		if err == nil {
			exeDir := filepath.Dir(exePath)
			envPaths = append(envPaths, filepath.Join(exeDir, ".env"))
			envPaths = append(envPaths, filepath.Join(exeDir, ".env.local"))
		}
	}

	for _, envPath := range envPaths {
		if err := LoadEnvFile(envPath); err == nil {
			log.Printf("[CONFIG] Loaded environment from: %s", envPath)
			break
		}
	}
}

var (
	enableClipboard bool
	enableTimestamp time.Time
	lock            = sync.Mutex{}
	debugClipboard  bool

	lastChangeContent []byte
	lastChangeTime    time.Time

	stopping bool
	stopCh   = make(chan struct{})
)

const disableDuration = 2 * time.Second

// 设置开始时间
var startTime = time.Now()

func setClipboardContent(msg ClipboardMessage) {
	lock.Lock()
	defer lock.Unlock()

	// 设置忽略读取剪贴板的信号
	enableClipboard = false

	// content 始终是 base64 编码，需要解码
	decoded, err := base64.StdEncoding.DecodeString(msg.Content)
	if err != nil {
		log.Printf("base64解码失败: %s", err)
		enableClipboard = true
		enableTimestamp = time.Now()
		return
	}

	switch msg.Type {
	case "text":
		err = SetClipboardContentText(string(decoded))
		if err != nil {
			log.Printf("写入文本到剪贴板失败: %s", err)
		}
	case "image":
		err = SetClipboardContentImage(decoded)
		if err != nil {
			log.Printf("写入图片到剪贴板失败: %s", err)
		}
	default:
		log.Printf("Unsupported clipboard content type: %s", msg.Type)
	}

	enableClipboard = true
	enableTimestamp = time.Now()
}

// ProcessClipboardChange processes the clipboard change and returns ClipboardMessage.
func ProcessClipboardChange(change ClipboardChange) (*ClipboardMessage, error) {
	lock.Lock()
	defer lock.Unlock()

	types := []string{change.Mime}
	contentType, mime := DetermineContentType(types)

	if contentType == "unknown" {
		log.Printf("Ignoring clipboard change with unknown content, mime: %s", change.Mime)
		return nil, nil
	}

	// content 始终为 base64 编码
	base64Content := base64.StdEncoding.EncodeToString(change.Content)

	return &ClipboardMessage{
		Time:       float64(change.Timestamp) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix()),
		UUID:       generateUUID(),
		DeviceName: getDeviceName(),
		Mime:       mime,
		Type:       contentType,
		Content:    base64Content,
	}, nil
}

func changeEvent(change ClipboardChange) {
	// 如果正在停机，忽略新消息
	if stopping {
		return
	}

	// 如果距离开始时间小于1s忽略
	if time.Since(startTime) < time.Second {
		log.Println("Ignoring clipboard change within the first second.")
		return
	}

	// 去重：相同内容在短时间内不重复发送（修复Wayland重复触发）
	if bytes.Equal(change.Content, lastChangeContent) && time.Since(lastChangeTime) < time.Second {
		debugLog("忽略重复剪贴板变更")
		return
	}
	lastChangeContent = change.Content
	lastChangeTime = time.Now()

	// 如果当前为禁止读取剪贴板，忽略
	if !enableClipboard {
		log.Println("Clipboard reading is disabled.")
		return
	}

	if time.Since(enableTimestamp) < disableDuration/4 {
		// 0.5秒内，忽略
		return
	}

	// 如果当前为恢复剪贴板 2 秒内，忽略
	if time.Since(enableTimestamp) < disableDuration {
		//log.Printf("Clipboard reading is temporarily disabled, since: %dms\n", time.Since(enableTimestamp).Milliseconds())
		return
	}

	msg, err := ProcessClipboardChange(change)
	if err != nil {
		log.Printf("Error processing clipboard change: %v", err)
		return
	}
	if msg == nil {
		return
	}

	// 设置发送时间
	msg.SendTime = float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix())

	syncClipboardContent(*msg)
}

func main() {
	envFile := flag.String("env-file", "", "Path to .env file")
	receivedWriteText := flag.String("received-write-text", "", "Write text to clipboard (for testing)")
	receivedImageFile := flag.String("received-image-file", "", "Write image file to clipboard (for testing)")
	flag.Parse()

	// 初始化环境变量（必须在 flag.Parse 之后，以便处理 -env-file 参数）
	initEnv(*envFile)

	// 在 .env 加载之后再读取 debug 标志
	debugClipboard = os.Getenv("CLIPBOARD_DEBUG") == "1"

	// 验证设备名称（必需）
	_ = getDeviceName()

	// 测试模式：不启动HTTP服务器，直接写入剪贴板并触发同步
	if *receivedWriteText != "" {
		msg := ClipboardMessage{
			Time:       float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix()),
			UUID:       generateUUID(),
			DeviceName: getDeviceName(),
			Mime:       "text/plain",
			Type:       "text",
			Content:    base64.StdEncoding.EncodeToString([]byte(*receivedWriteText)),
		}
		msg.SendTime = float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix())
		setClipboardContent(msg)
		syncClipboardContent(msg) // 触发同步推送
		log.Printf("Wrote text to clipboard and synced: %s", *receivedWriteText)
		return
	}

	if *receivedImageFile != "" {
		data, err := ioutil.ReadFile(*receivedImageFile)
		if err != nil {
			log.Fatalf("Failed to read image file: %v", err)
		}
		msg := ClipboardMessage{
			Time:       float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix()),
			UUID:       generateUUID(),
			DeviceName: getDeviceName(),
			Mime:       "image/png",
			Type:       "image",
			Content:    base64.StdEncoding.EncodeToString(data),
		}
		msg.SendTime = float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix())
		setClipboardContent(msg)
		syncClipboardContent(msg) // 触发同步推送
		log.Printf("Wrote image file to clipboard and synced: %s", *receivedImageFile)
		return
	}

	// 正常模式：启动监听和同步
	if needsInterfaceFilter() {
		InitNetworkMonitor()
		defer CloseNetworkMonitor()
	}

	clipboardChanges := ListenClipboardChanges()

	go startLocalServer()

	enableClipboard = true

	initClipboard()

	// 监听系统信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case status := <-clipboardChanges:
			changeEvent(status)
		case sig := <-sigCh:
			log.Printf("Received signal: %v, shutting down...", sig)
			stopping = true
			log.Println("Closing all MQTT connections...")
			CloseAllMQTTClients()
			log.Println("Shutdown complete.")
			close(stopCh)
			return
		}
	}
}
