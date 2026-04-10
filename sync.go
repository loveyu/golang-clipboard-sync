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
	"log"
	"strings"
	"sync"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func syncClipboardContent(msg ClipboardMessage) {
	targets := getTargetsLazy()
	if len(targets) == 0 {
		return
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10) // Allow 10 concurrent requests

	for _, target := range targets {
		if target != nil {
			wg.Add(1)
			go func(t *SyncTarget) {
				defer wg.Done()
				semaphore <- struct{}{}        // Acquire semaphore
				defer func() { <-semaphore }() // Release semaphore
				err := syncToTarget(t, msg)
				if err != nil {
					log.Printf("Failed to sync clipboard content to %s: %v", t.RawURL, err)
				}
			}(target)
		}
	}

	wg.Wait()
}

// forwardClipboardContent 转发收到的剪贴板消息到转发目标
func forwardClipboardContent(msg ClipboardMessage) {
	targets := getForwardTargetsLazy()
	if len(targets) == 0 {
		return
	}

	// 设置转发来源
	msg.ForwardSource = getDeviceName()

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10)

	for _, target := range targets {
		if target != nil {
			wg.Add(1)
			go func(t *SyncTarget) {
				defer wg.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()
				err := syncToTarget(t, msg)
				if err != nil {
					log.Printf("Failed to forward clipboard content to %s: %v", t.RawURL, err)
				}
			}(target)
		}
	}

	wg.Wait()
}

// needsInterfaceFilter 检查所有URL中是否有任何一个使用了 allowInterfaceIps 或 denyInterfaceIps
func needsInterfaceFilter() bool {
	for _, t := range getTargetsLazy() {
		if len(t.AllowInterfaceIps) > 0 || len(t.DenyInterfaceIps) > 0 {
			return true
		}
	}
	for _, t := range getForwardTargetsLazy() {
		if len(t.AllowInterfaceIps) > 0 || len(t.DenyInterfaceIps) > 0 {
			return true
		}
	}
	return false
}

// isInterfaceAllowed 检查本地网卡IP是否符合允许/拒绝的网段
func isInterfaceAllowed(t *SyncTarget) bool {
	allowIPs := t.AllowInterfaceIps
	denyIPs := t.DenyInterfaceIps

	// 如果都没有配置，则允许
	if len(allowIPs) == 0 && len(denyIPs) == 0 {
		return true
	}

	allowed := IsAllowedByInterface(strings.Join(allowIPs, ","), strings.Join(denyIPs, ","))
	if !allowed && debugClipboard {
		if len(denyIPs) > 0 && !IsAllowedByInterface("", strings.Join(denyIPs, ",")) {
			log.Printf("[DEBUG] 网卡IP过滤跳过 (命中拒绝列表) - URL: %s, 拒绝网段: %v", t.RawURL, denyIPs)
		} else {
			log.Printf("[DEBUG] 网卡IP过滤跳过 - URL: %s, 允许网段: %v", t.RawURL, allowIPs)
		}
	}
	return allowed
}

func syncToTarget(t *SyncTarget, msg ClipboardMessage) error {
	// 检查网卡IP过滤
	if !isInterfaceAllowed(t) {
		if debugClipboard {
			log.Printf("[DEBUG] 网卡IP过滤跳过 - URL: %s", t.RawURL)
		}
		return nil
	}

	// 检测协议类型并应用类型过滤
	if !isTypeAllowed(t.Types, msg.Type) {
		if debugClipboard {
			log.Printf("[DEBUG] 类型过滤跳过 - URL: %s, Type: %s, AllowedTypes: %v", t.RawURL, msg.Type, t.Types)
		}
		return nil
	}

	if t.IsMQTT {
		return syncViaMQTT(t, msg)
	}
	return syncViaHTTP(t, msg)
}
