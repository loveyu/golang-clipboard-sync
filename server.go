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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// SyncTarget 预解析的同步目标，避免重复解析URL
type SyncTarget struct {
	RawURL            string
	ParsedURL         *url.URL
	IsMQTT            bool
	AllowInterfaceIps []string
	DenyInterfaceIps  []string
	Username          string
	Password          string
	Types             []string // 允许的内容类型，如 ["text","image"]，为空则允许全部
	Retries           int      // 发布失败重试次数，MQTT默认1，HTTP默认0
	RetryDelay        int      // 重试延迟时间(毫秒)，默认50
}

// 延迟初始化的变量
var (
	clipboardSyncURLsInit     string
	targetsInit               []*SyncTarget
	localServerPortInit       string
	targetsInitialized        bool
	clipboardForwardURLsInit  string
	forwardTargetsInit        []*SyncTarget
	forwardTargetsInitialized bool
)

// parseSyncTarget 解析单个URL为SyncTarget
func parseSyncTarget(rawURL string) *SyncTarget {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		log.Printf("[WARN] 解析URL失败: %s, error: %v", rawURL, err)
		return nil
	}

	// 解析逗号分隔的 query 参数为数组
	q := u.Query()
	parseList := func(key string) []string {
		var result []string
		if v := q.Get(key); v != "" {
			for _, s := range strings.Split(v, ",") {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					result = append(result, trimmed)
				}
			}
		}
		return result
	}

	t := &SyncTarget{
		RawURL:            rawURL,
		ParsedURL:         u,
		IsMQTT:            u.Scheme == "mqtt" || u.Scheme == "mqtts",
		AllowInterfaceIps: parseList("allowInterfaceIps"),
		DenyInterfaceIps:  parseList("denyInterfaceIps"),
		Types:             parseList("types"),
		Retries:           0,  // HTTP默认不重试
		RetryDelay:        50, // 默认50ms
	}

	// MQTT默认重试1次
	if t.IsMQTT {
		t.Retries = 1
	}

	// 解析retries参数
	if v := q.Get("retries"); v != "" {
		var retries int
		if _, err := fmt.Sscanf(v, "%d", &retries); err == nil && retries >= 0 {
			t.Retries = retries
		}
	}

	// 解析retryDelay参数
	if v := q.Get("retryDelay"); v != "" {
		var delay int
		if _, err := fmt.Sscanf(v, "%d", &delay); err == nil && delay >= 0 {
			t.RetryDelay = delay
		}
	}

	if u.User != nil {
		t.Username = u.User.Username()
		t.Password, _ = u.User.Password()
	}

	return t
}

// initTargets 延迟初始化并解析所有同步目标
func initTargets() {
	if targetsInitialized {
		return
	}
	clipboardSyncURLsInit = getEnv("CLIPBOARD_SYNC_URLS", "")
	rawURLs := strings.Split(clipboardSyncURLsInit, ";")
	for _, raw := range rawURLs {
		t := parseSyncTarget(raw)
		if t != nil {
			targetsInit = append(targetsInit, t)
		}
	}
	targetsInitialized = true
}

// initForwardTargets 延迟初始化并解析所有转发目标
func initForwardTargets() {
	if forwardTargetsInitialized {
		return
	}
	clipboardForwardURLsInit = getEnv("CLIPBOARD_FORWARD_URLS", "")
	rawURLs := strings.Split(clipboardForwardURLsInit, ";")
	for _, raw := range rawURLs {
		t := parseSyncTarget(raw)
		if t != nil {
			forwardTargetsInit = append(forwardTargetsInit, t)
		}
	}
	forwardTargetsInitialized = true
}

// getForwardTargetsLazy 获取预解析的转发目标列表
func getForwardTargetsLazy() []*SyncTarget {
	initForwardTargets()
	return forwardTargetsInit
}

// getTargetsLazy 获取预解析的同步目标列表
func getTargetsLazy() []*SyncTarget {
	initTargets()
	return targetsInit
}

// getLocalServerPort 获取本地服务器端口
func getLocalServerPort() string {
	if localServerPortInit == "" {
		localServerPortInit = getEnv("LOCAL_SERVER_PORT", ":9144")
	}
	return localServerPortInit
}

func startLocalServer() {
	http.HandleFunc("/update-clipboard", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}

		var msg ClipboardMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, "Invalid JSON format", http.StatusBadRequest)
			return
		}

		log.Printf("Received clipboard: device=%s, type=%s, mime=%s", msg.DeviceName, msg.Type, msg.Mime)
		setClipboardContent(msg)
		go forwardClipboardContent(msg)

		_, _ = w.Write([]byte("ok"))
	})

	err := http.ListenAndServe(getLocalServerPort(), nil)
	if err != nil {
		log.Fatalf("Failed to start the local server: %v", err)
	}
}
