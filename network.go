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
	"fmt"
	"log"
	"net"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// NetworkMonitor 监听本地网卡IP变化
type NetworkMonitor struct {
	mu           sync.RWMutex
	currentIPs   []net.IP // 当前本地IP列表
	stopCh       chan struct{}
	lastActivity time.Time // 最近活动时间
	ticker       *time.Ticker
	tickerMu     sync.Mutex
}

var networkMonitor = &NetworkMonitor{
	stopCh: make(chan struct{}),
}

const (
	activeInterval = 5 * time.Second  // 活跃期检查间隔
	idleInterval   = 30 * time.Second // 空闲期检查间隔
	activeWindow   = 1 * time.Minute  // 活跃期窗口
)

// InitNetworkMonitor 初始化网络监控
func InitNetworkMonitor() {
	networkMonitor.mu.Lock()
	defer networkMonitor.mu.Unlock()

	// 初始扫描
	networkMonitor.currentIPs = getLocalIPs()
	networkMonitor.stopCh = make(chan struct{})
	networkMonitor.lastActivity = time.Now()
	networkMonitor.ticker = time.NewTicker(idleInterval)

	// 启动后台监控
	go networkMonitor.monitor()

	log.Printf("[NET] Network monitor started, local IPs: %v", networkMonitor.currentIPs)
}

// CloseNetworkMonitor 关闭网络监控
func CloseNetworkMonitor() {
	networkMonitor.mu.Lock()
	defer networkMonitor.mu.Unlock()

	networkMonitor.tickerMu.Lock()
	defer networkMonitor.tickerMu.Unlock()

	select {
	case <-networkMonitor.stopCh:
		// 已经关闭
	default:
		close(networkMonitor.stopCh)
	}
	if networkMonitor.ticker != nil {
		networkMonitor.ticker.Stop()
	}
}

// RecordActivity 记录最近有消息，用于加速检查间隔
func RecordActivity() {
	networkMonitor.mu.Lock()
	defer networkMonitor.mu.Unlock()

	wasIdle := time.Since(networkMonitor.lastActivity) > activeWindow
	networkMonitor.lastActivity = time.Now()

	// 如果之前是空闲状态，切换到快速检查
	if wasIdle {
		networkMonitor.tickerMu.Lock()
		defer networkMonitor.tickerMu.Unlock()

		if networkMonitor.ticker != nil {
			networkMonitor.ticker.Stop()
		}
		networkMonitor.ticker = time.NewTicker(activeInterval)
		log.Printf("[NET] Network check interval: 5s (active)")
	}
}

// monitor 后台监控网卡IP变化
func (nm *NetworkMonitor) monitor() {
	networkMonitor.tickerMu.Lock()
	ticker := networkMonitor.ticker
	networkMonitor.tickerMu.Unlock()

	for {
		select {
		case <-nm.stopCh:
			return
		case <-ticker.C:
			nm.checkAndAdjustInterval()
			nm.checkIPChanges()
		}
	}
}

// checkAndAdjustInterval 检查并调整检查间隔
func (nm *NetworkMonitor) checkAndAdjustInterval() {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	timeSinceActivity := time.Since(nm.lastActivity)
	isActive := timeSinceActivity <= activeWindow

	nm.tickerMu.Lock()
	defer nm.tickerMu.Unlock()

	var newInterval time.Duration
	if isActive {
		newInterval = activeInterval
	} else {
		newInterval = idleInterval
	}

	// 检查当前ticker间隔
	currentInterval := tickerInterval(nm.ticker)
	if currentInterval != newInterval {
		nm.ticker.Stop()
		nm.ticker = time.NewTicker(newInterval)
		if isActive {
			log.Printf("[NET] Network check interval: 5s (active)")
		} else {
			log.Printf("[NET] Network check interval: 30s (idle)")
		}
	}
}

// tickerInterval 获取ticker的间隔
func tickerInterval(t *time.Ticker) time.Duration {
	if t == nil {
		return 0
	}
	// time.Ticker 不提供获取间隔的公共方法，通过尝试读取 C channel 非阻塞判断
	return idleInterval // 默认
}

// checkIPChanges 检查IP是否有变化
func (nm *NetworkMonitor) checkIPChanges() {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	newIPs := getLocalIPs()
	oldIPs := nm.currentIPs

	// 比较是否有变化
	if !ipListsEqual(oldIPs, newIPs) {
		nm.currentIPs = newIPs
		log.Printf("[NET] Local IPs changed: %v", newIPs)
	}
}

// getLocalIPs 获取所有本地物理网卡的IP地址（排除loopback）
func getLocalIPs() []net.IP {
	var ips []net.IP

	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("[NET] Failed to get interfaces: %v", err)
		return ips
	}

	// 获取默认网卡名称模式（根据操作系统）
	var defaultPatterns string
	switch runtime.GOOS {
	case "linux":
		defaultPatterns = "eth*,enp*,wlp*"
	case "windows":
		defaultPatterns = "以太网*,Wi-Fi*,本地连接*,WLAN*,Ethernet*"
	default:
		defaultPatterns = ""
	}

	// 检查是否配置了自定义模式
	allowedPatterns := getEnv("CLIPBOARD_ALLOWED_INTERFACE_PATTERNS", defaultPatterns)

	for _, iface := range ifaces {
		// 跳过loopback
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// 跳过down的接口
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		// 跳过虚拟接口
		if isVirtualInterface(iface.Name) {
			continue
		}

		// 检查是否匹配允许的网卡名称模式（所有平台）
		if allowedPatterns != "" && !isInterfaceNameMatched(iface.Name, allowedPatterns) {
			if debugClipboard {
				log.Printf("[DEBUG] 网卡名称不符合模式，跳过: %s (patterns: %s)", iface.Name, allowedPatterns)
			}
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				// 只处理IPv4
				if ipNet.IP.To4() != nil {
					ips = append(ips, ipNet.IP)
				}
			}
		}
	}

	return ips
}

// isInterfaceNameMatched 检查网卡名称是否匹配给定的模式列表
// 模式格式: "eth*,enp*,wlp*" (逗号分隔，支持通配符 *)
func isInterfaceNameMatched(name, patterns string) bool {
	if patterns == "" {
		return true
	}

	patternList := strings.Split(patterns, ",")
	for _, pattern := range patternList {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}

		// 将通配符模式转换为正则
		regexPattern := "^" + strings.ReplaceAll(pattern, "*", ".*") + "$"
		matched, err := regexp.MatchString(regexPattern, name)
		if err == nil && matched {
			return true
		}
	}
	return false
}

// isVirtualInterface 判断是否为虚拟接口
func isVirtualInterface(name string) bool {
	if runtime.GOOS == "windows" {
		// Windows 虚拟接口前缀
		virtualPrefixes := []string{
			"docker", "br-", "veth", "vmnet", "virbr",
			"VMware", "Virtual", "Loopback",
			"Humble", "npipe", "Local",
		}
		for _, prefix := range virtualPrefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		// Windows 上以数字开头的可能是虚拟接口 (如 "10.0.0.1" 这样的描述)
		// 但实际的网卡名通常不是纯数字，所以这里保守判断
		return false
	}

	// Linux/Unix 虚拟接口前缀
	virtualPrefixes := []string{
		"docker", "br-", "veth", "vmnet", "virbr",
		"wlx", "wwan", "ap",
		"neta", "netvm",
	}

	for _, prefix := range virtualPrefixes {
		if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// ipListsEqual 比较两个IP列表是否相等
func ipListsEqual(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]bool)
	for _, ip := range a {
		aMap[ip.String()] = true
	}

	for _, ip := range b {
		if !aMap[ip.String()] {
			return false
		}
	}

	return true
}

// IsAllowedByInterface 检查本地IP是否符合允许的网段
// allowInterfaceIps 格式: "192.168.1.0/24,10.0.0.0/8" 或 "192.168.1.100"（单个IP）
// denyInterfaceIps 格式同上，排除这些网段
func IsAllowedByInterface(allowInterfaceIps, denyInterfaceIps string) bool {
	// 获取本地IP（优先使用监控中的IP，否则立即扫描）
	networkMonitor.mu.RLock()
	localIPs := networkMonitor.currentIPs
	networkMonitor.mu.RUnlock()

	// 如果IP列表为空，立即扫描一次
	if len(localIPs) == 0 {
		localIPs = getLocalIPs()
	}

	// 先检查是否在排除列表中
	if denyInterfaceIps != "" {
		if isIPMatchedByCIDRs(localIPs, denyInterfaceIps) {
			if debugClipboard {
				log.Printf("[DEBUG] Local IP matched deny list: %s", denyInterfaceIps)
			}
			return false
		}
	}

	// 如果没有配置 allowInterfaceIps，直接允许
	if allowInterfaceIps == "" {
		return true
	}

	// 记录最近有消息，触发快速检查
	RecordActivity()

	if debugClipboard {
		log.Printf("[DEBUG] Local IPs for interface check: %v, allowed: %s", localIPs, allowInterfaceIps)
	}

	// 检查是否匹配允许列表
	return isIPMatchedByCIDRs(localIPs, allowInterfaceIps)
}

// isIPMatchedByCIDRs 检查本地IP是否匹配任一CIDR
func isIPMatchedByCIDRs(localIPs []net.IP, cidrList string) bool {
	cidrs := strings.Split(cidrList, ",")
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}

		_, allowedNet, err := net.ParseCIDR(cidr)
		if err != nil {
			// 尝试作为单个IP解析
			ip := net.ParseIP(cidr)
			if ip == nil {
				log.Printf("[NET] Invalid CIDR or IP: %s", cidr)
				continue
			}
			// 单个IP，创建/32网络
			allowedNet = &net.IPNet{IP: ip, Mask: net.IPv4Mask(255, 255, 255, 255)}
		}

		// 检查是否有本地IP匹配
		for _, localIP := range localIPs {
			if allowedNet.Contains(localIP) {
				return true
			}
		}
	}
	return false
}

// ParseCIDR 解析CIDR字符串，支持单个IP
func ParseCIDR(cidrStr string) (*net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(cidrStr)
	if err == nil {
		return ipNet, nil
	}

	// 尝试作为单个IP
	ip := net.ParseIP(cidrStr)
	if ip != nil {
		if ip.To4() != nil {
			return &net.IPNet{IP: ip, Mask: net.IPv4Mask(255, 255, 255, 255)}, nil
		}
		// IPv6
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
	}

	return nil, fmt.Errorf("invalid CIDR or IP: %s", cidrStr)
}
