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
	enableClipboard bool
	enableTimestamp time.Time
	lock            = sync.Mutex{}
	debugClipboard  bool

	lastChangeContent []byte
	lastChangeTime    time.Time

	stopping bool
	stopCh   = make(chan struct{})

	forwardEngine *ForwardEngine
)

const disableDuration = 2 * time.Second

var startTime = time.Now()

func setClipboardContent(msg ClipboardMessage) {
	lock.Lock()
	defer lock.Unlock()

	enableClipboard = false

	decoded, err := base64.StdEncoding.DecodeString(msg.Content)
	if err != nil {
		log.Printf("base64解码失败: %s", err)
		enableClipboard = true
		enableTimestamp = time.Now()
		return
	}

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

	if time.Since(startTime) < time.Second {
		log.Println("Ignoring clipboard change within the first second.")
		return
	}

	// Dedup
	if appConfig != nil && appConfig.Device.Name != "" {
		// Use global dedup
	}
	if lastChangeContent != nil && lastChangeTime.IsZero() == false {
		if bytesEqual(change.Content, lastChangeContent) && time.Since(lastChangeTime) < time.Second {
			debugLog("忽略重复剪贴板变更")
			return
		}
	}
	lastChangeContent = change.Content
	lastChangeTime = time.Now()

	if !enableClipboard {
		log.Println("Clipboard reading is disabled.")
		return
	}

	if time.Since(enableTimestamp) < disableDuration/4 {
		return
	}

	if time.Since(enableTimestamp) < disableDuration {
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

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func main() {
	var configPath string
	var versionFlag bool
	var receivedWriteText string
	var receivedImageFile string

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
		case "download-config":
			runDownloadConfig(configPath)
		case "help", "--help":
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

	// Initialize network monitor if needed
	if needsInterfaceFilter() {
		InitNetworkMonitor()
		defer CloseNetworkMonitor()
	}

	// Subscribe to MQTT listen entries
	SubscribeAllListeners()

	// Start local HTTP server
	go startLocalServer()

	enableClipboard = true
	initClipboard()

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	clipboardChanges := ListenClipboardChanges()

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
	fmt.Print(`clipboard-sync - Clipboard synchronization tool

Usage:
  clipboard-sync [command]

Commands:
  start            Start the service (default)
  download-config  Download remote config from REMOTE_CONFIG_URL
  help             Show this help message
  version          Show version

Flags:
  -config string              Path to config file (overrides CLIPBOARD_CONFIG_PATH)
  -received-write-text string Write text to clipboard (for testing)
  -received-image-file string Write image file to clipboard (for testing)
  -v, -version                Print version

Environment Variables:
  CLIPBOARD_DEBUG       Enable debug logging (set to "1")
  CLIPBOARD_CONFIG_PATH Config file path (default: config.yaml)
  REMOTE_CONFIG_URL     URL for download-config command

`)
}
