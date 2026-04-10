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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func syncViaHTTP(t *SyncTarget, msg ClipboardMessage) error {
	jsonData, err := json.Marshal(msg)
	if err != nil {
		if debugClipboard {
			log.Printf("[DEBUG] JSON序列化失败: %v", err)
		}
		return err
	}
	if debugClipboard {
		log.Printf("[DEBUG] 发送请求 - URL: %s, Type: %s, Content前100字符: %s",
			t.RawURL, msg.Type, string(jsonData[:min(len(jsonData), 100)]))
	}

	// 使用预解析的URL，移除query参数和认证信息
	reqURL := *t.ParsedURL // 复制
	reqURL.User = nil      // 清除URL中的认证信息
	reqURL.RawQuery = ""   // 移除query参数（它们是配置选项如allowInterfaceIps等）

	// 替换路径中的{$type}变量
	reqURL.Path = resolveTopic(reqURL.Path, msg.Mime)

	cleanURL := reqURL.String()

	var lastErr error
	maxAttempts := t.Retries + 1 // 首次 + 重试次数

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 重试前延迟
			time.Sleep(time.Duration(t.RetryDelay) * time.Millisecond)
			if debugClipboard {
				log.Printf("[DEBUG] HTTP重试请求 (attempt %d/%d)", attempt, t.Retries)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		req, err := http.NewRequestWithContext(ctx, "POST", cleanURL, bytes.NewBuffer(jsonData))
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		// Set Content-Type header to application/json
		req.Header.Set("Content-Type", "application/json")

		// 设置Basic认证
		if t.Username != "" {
			req.SetBasicAuth(t.Username, t.Password)
		}

		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			if debugClipboard {
				log.Printf("[DEBUG] 请求失败: %s, URL: %s, 错误: %v", msg.Type, t.RawURL, err)
			}
			lastErr = err
			continue
		}

		// Read and log the response
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if debugClipboard {
			log.Printf("[DEBUG] 网络请求 - URL: %s, Type: %s, Content长度: %d, Response状态: %d, Body: %s",
				cleanURL, msg.Type, len(jsonData), resp.StatusCode, string(respBody))
		} else {
			log.Printf("Request: %s, Response from %s: %s\n", msg.Type, cleanURL, respBody)
		}

		// HTTP 2xx 成功
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		lastErr = fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	return lastErr
}
