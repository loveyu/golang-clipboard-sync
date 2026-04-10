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
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"strings"
)

// LoadEnvFile 加载 .env 文件到环境变量
// 忽略空行和以 # 开头的注释行
func LoadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 移除 Windows CRLF 的 \r（TrimSpace 在某些情况下可能未处理）
		line = strings.TrimRight(line, "\r")

		// 解析 key=value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// 移除可能的引号
		value = strings.Trim(value, "\"'\"")

		// 仅当环境变量未设置时才设置
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
			log.Printf("[DEBUG] Loaded env: %s=%s", key, value)
		}
	}

	return scanner.Err()
}

func removeTrailingNewline(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		return data[:len(data)-1]
	}
	return data
}

// DetermineContentType determines the type of content (text, image, file, etc.).
func DetermineContentType(types []string) (string, string) {
	for _, t := range types {
		// Check for image types
		if strings.HasPrefix(t, "image/") {
			return "image", t
		}
		// Check for text types
		if strings.HasPrefix(t, "text/") {
			return "text", t
		}
		// Check for file types
		if t == "text/uri-list" || t == "application/x-kde4-urilist" {
			return "file", t
		}
	}
	return "unknown", "application/unknown"
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// isTypeAllowed 检查内容类型是否在允许列表中，为空则允许全部
func isTypeAllowed(allowedTypes []string, contentType string) bool {
	if len(allowedTypes) == 0 {
		return true // 默认允许全部
	}
	for _, t := range allowedTypes {
		if t == contentType {
			return true
		}
	}
	return false
}

// getDeviceName 获取设备名称，优先使用环境变量 CLIPBOARD_DEVICE_NAME，未设置则使用主机 hostname
func getDeviceName() string {
	deviceName := os.Getenv("CLIPBOARD_DEVICE_NAME")
	if deviceName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatal("无法获取 hostname，请设置 CLIPBOARD_DEVICE_NAME 环境变量")
		}
		deviceName = hostname
		log.Printf("CLIPBOARD_DEVICE_NAME 未设置，使用 hostname: %s", deviceName)
	}
	return deviceName
}

// generateUUID 生成新的 UUID v4
func generateUUID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return ""
	}
	// Set version 4 (random) and variant bits
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
