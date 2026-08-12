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
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	lock           = sync.Mutex{}
	debugClipboard bool

	stopping bool
	stopCh   = make(chan struct{})

	forwardEngine *ForwardEngine
)

func setClipboardContent(msg ClipboardMessage) {
	lock.Lock()
	defer lock.Unlock()

	decoded, err := base64.StdEncoding.DecodeString(msg.Content)
	if err != nil {
		log.Printf("base64解码失败: %s", err)
		return
	}

	var mime string
	switch BaseContentType(msg.Type) {
	case "text":
		mime = "text/plain"
	case "image":
		mime = "image/png"
	default:
		log.Printf("Unsupported clipboard content type: %s", msg.Type)
		return
	}
	token := registerLocalClipboardWrite(mime, decoded)
	switch BaseContentType(msg.Type) {
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
	}
	completeLocalClipboardWrite(token, currentClipboardGeneration(), err == nil)
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

	base64Content := base64.StdEncoding.EncodeToString(change.Content)

	return &ClipboardMessage{
		Time:       float64(change.Timestamp) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix()),
		UUID:       generateUUID(),
		DeviceName: appConfig.Device.Name,
		Mime:       mime,
		Type:       contentType,
		Content:    base64Content,
	}, nil
}

func changeEvent(change ClipboardChange) {
	if stopping {
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

	msg.SendTime = float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix())

	// Route through forward engine
	if forwardEngine != nil {
		forwardEngine.ProcessMessage("system", *msg)
	}
}

func main() {
	var configPath string
	var versionFlag bool
	var receivedWriteText string
	var receivedImageFile string

	flag.Usage = printHelp
	flag.StringVar(&configPath, "config", "", "Path to config file (overrides CLIPBOARD_CONFIG_PATH)")
	flag.BoolVar(&versionFlag, "version", false, "Print version information")
	flag.BoolVar(&versionFlag, "v", false, "Print version information (shorthand)")
	flag.StringVar(&receivedWriteText, "received-write-text", "", "Write text to clipboard (for testing)")
	flag.StringVar(&receivedImageFile, "received-image-file", "", "Write image file to clipboard (for testing)")
	flag.Parse()

	if versionFlag {
		printVersion()
		return
	}

	// Subcommand handling
	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "start":
			runStart(configPath, receivedWriteText, receivedImageFile)
		case "init-config":
			runInitConfig(configPath)
		case "show-example-config":
			os.Stdout.Write(configExample)
		case "download-config":
			runDownloadConfig(configPath)
		case "help":
			printHelp()
		case "version", "--version", "-v":
			printVersion()
		default:
			fmt.Fprintf(os.Stderr, "Unknown command: %s\nUse 'help' for usage.\n", args[0])
			os.Exit(1)
		}
		return
	}

	// Default: start (backward compatible with direct flags)
	runStart(configPath, receivedWriteText, receivedImageFile)
}

func runStart(configPath, receivedWriteText, receivedImageFile string) {
	// Load config
	path := configPath
	if path == "" {
		path = ConfigPath()
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", path, err)
	}
	appConfig = cfg

	debugClipboard = IsDebug()
	log.Printf("Config loaded from: %s, device: %s", path, cfg.Device.Name)

	// Test mode: write to clipboard and sync
	if receivedWriteText != "" {
		msg := ClipboardMessage{
			Time:       float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix()),
			UUID:       generateUUID(),
			DeviceName: cfg.Device.Name,
			Mime:       "text/plain",
			Type:       "text",
			Content:    base64.StdEncoding.EncodeToString([]byte(receivedWriteText)),
		}
		msg.SendTime = float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix())
		setClipboardContent(msg)

		forwardEngine = NewForwardEngine(cfg)
		forwardEngine.ProcessMessage("system", msg)
		log.Printf("Wrote text to clipboard and synced: %s", receivedWriteText)
		return
	}

	if receivedImageFile != "" {
		data, err := os.ReadFile(receivedImageFile)
		if err != nil {
			log.Fatalf("Failed to read image file: %v", err)
		}
		msg := ClipboardMessage{
			Time:       float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix()),
			UUID:       generateUUID(),
			DeviceName: cfg.Device.Name,
			Mime:       "image/png",
			Type:       "image",
			Content:    base64.StdEncoding.EncodeToString(data),
		}
		msg.SendTime = float64(time.Now().Unix()) + float64(time.Now().UnixNano())/1e9 - float64(time.Now().Unix())
		setClipboardContent(msg)

		forwardEngine = NewForwardEngine(cfg)
		forwardEngine.ProcessMessage("system", msg)
		log.Printf("Wrote image file to clipboard and synced: %s", receivedImageFile)
		return
	}

	// Initialize forward engine
	forwardEngine = NewForwardEngine(cfg)
	initClipboard()
	clipboardChanges := ListenClipboardChanges()

	// Initialize network monitor if needed
	if needsInterfaceFilter() {
		InitNetworkMonitor()
		defer CloseNetworkMonitor()
	}

	// Subscribe to MQTT listen entries
	SubscribeAllListeners()

	// Start local HTTP server
	go startLocalServer()

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case status, ok := <-clipboardChanges:
			if !ok {
				log.Print("[CLIPBOARD] processor stopped")
				return
			}
			changeEvent(status)
		case sig := <-sigCh:
			log.Printf("Received signal: %v, shutting down...", sig)
			stopping = true
			close(stopCh)
			if !waitClipboardWorkers(5 * time.Second) {
				log.Print("[CLIPBOARD] worker shutdown exceeded 5 seconds")
			}
			log.Println("Closing all MQTT connections...")
			CloseAllMQTTClients()
			log.Println("Shutdown complete.")
			return
		}
	}
}

func runInitConfig(configPath string) {
	path := configPath
	if path == "" {
		path = ConfigPath()
	}

	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "Error: config file already exists: %s\n", path)
		fmt.Fprintf(os.Stderr, "Remove it first or use -config to specify a different path.\n")
		os.Exit(1)
	}

	if err := os.WriteFile(path, configExample, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to write config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Example config written to: %s\n", path)
	fmt.Println("Edit the file and run: clipboard-sync start")
}

func runDownloadConfig(configPath string) {
	remoteURL := os.Getenv("REMOTE_CONFIG_URL")
	if remoteURL == "" && appConfig != nil {
		remoteURL = appConfig.RemoteConfig
	}
	if remoteURL == "" {
		log.Fatal("REMOTE_CONFIG_URL is not set and no remoteConfig in config")
	}

	path := configPath
	if path == "" {
		path = ConfigPath()
	}

	log.Printf("Downloading config from %s to %s", remoteURL, path)

	resp, err := http.Get(remoteURL)
	if err != nil {
		log.Fatalf("Download config failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("Download config failed: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Read config failed: %v", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Fatalf("Write config failed: %v", err)
	}

	log.Printf("Config saved to %s (%d bytes)", path, len(data))
}

func printHelp() {
	fmt.Print(`clipboard-sync - 跨设备剪贴板同步工具

用法:
  clipboard-sync [command] [flags]

命令:
  start               启动服务（默认，省略命令时等同于 start）
  init-config         将内置示例配置写入配置文件（文件已存在时报错）
  show-example-config 打印内置示例配置到标准输出
  download-config     从远程URL下载配置文件（需配置 remoteConfig 或 REMOTE_CONFIG_URL）
  help                显示此帮助信息
  version             显示版本信息

标志:
  -config string                配置文件路径（覆盖 CLIPBOARD_CONFIG_PATH 环境变量）
  -v, -version                  显示版本信息
  -received-write-text string   将文本写入本地剪贴板并通过 forward 规则同步（测试用）
  -received-image-file string   将图片文件写入本地剪贴板并通过 forward 规则同步（测试用）

环境变量:
  CLIPBOARD_DEBUG=1      启用调试日志（等同于配置 debug: true）
  CLIPBOARD_CONFIG_PATH  配置文件路径（默认: config.yaml）
  REMOTE_CONFIG_URL      download-config 命令使用的远程配置URL

DSN参数（listen 条目，格式: mqtt[s]://user:pass@host:port/topic?params）:
  maxMessageSize       最大消息大小（如 5MB, 512KB），超过则丢弃
  clientId             MQTT客户端ID（默认自动生成）
  connectTimeout       连接超时（秒，默认 3）
  keepAliveInterval    心跳间隔（秒，默认 60）
  automaticReconnect   自动重连（true/false，默认 true）
  reconnectInterval    重连基础间隔（秒，默认 5）
  reconnectMaxInterval 最大重连间隔（秒，默认 60）
  qos                  QoS等级（0/1/2，默认 1）
  certificate          引用 certificates 中的ID（mqtts时使用）

DSN参数（targets MQTT条目，含上述参数外还有）:
  retain               发布时是否保留消息（true/false，默认 true）
  types                过滤内容类型，逗号分隔（如 "text,image"，为空则不过滤）
  retries              失败重试次数（默认 1）
  retryDelay           重试间隔（毫秒，默认 50）

DSN参数（targets HTTP条目，格式: http[s]://user:pass@host:port/path?params）:
  types                过滤内容类型，逗号分隔（如 "text,image"，为空则不过滤）
  retries              失败重试次数（默认 0）
  retryDelay           重试间隔（毫秒，默认 50）

保留 ID（不可用于 listen/target/center 的 id 字段）:
  system  本地剪贴板（forward.from = 监听本地变更，forward.to = 写入本地）
  http    HTTP接收端点（forward.from 中使用）

HTTP 端点:
  POST /update-clipboard  接收 ClipboardMessage JSON，通过 forward 规则路由

消息协议:
  V1  内容直接内嵌于MQTT消息（Base64编码）
  V2  内容上传至中继服务器，MQTT消息只传引用（图片或文本>10KB时自动使用）

配置文件:
  clipboard-sync show-example-config          # 查看示例配置
  clipboard-sync init-config                  # 生成 config.yaml（文件已存在时报错）
  clipboard-sync init-config -config foo.yaml # 生成指定路径的配置文件

`)
}
