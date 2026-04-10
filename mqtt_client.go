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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// MQTTClientPool 维护MQTT客户端连接池，通过URL hash作为key实现连接复用
type MQTTClientPool struct {
	clients map[string]mqtt.Client
	mu      sync.RWMutex
}

var mqttPool = &MQTTClientPool{
	clients: make(map[string]mqtt.Client),
}

// getMQTTClient 获取或创建MQTT客户端，实现连接复用
func getMQTTClient(mqttURL string) (mqtt.Client, error) {
	hash := hashURL(mqttURL)

	mqttPool.mu.RLock()
	client, exists := mqttPool.clients[hash]
	mqttPool.mu.RUnlock()

	if exists && client.IsConnected() {
		return client, nil
	}

	// 需要创建新连接
	mqttPool.mu.Lock()
	defer mqttPool.mu.Unlock()

	// 双重检查
	if client, exists = mqttPool.clients[hash]; exists && client.IsConnected() {
		return client, nil
	}

	client, err := createMQTTClient(mqttURL)
	if err != nil {
		return nil, err
	}

	mqttPool.clients[hash] = client
	return client, nil
}

// hashURL 生成URL的hash作为连接池的key
func hashURL(rawURL string) string {
	hash := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(hash[:16]) // 使用前16字节，足够唯一
}

// MQTTConfig MQTT连接配置
type MQTTConfig struct {
	Broker               string
	ClientID             string
	Username             string
	Password             string
	Topic                string
	ConnectTimeout       int // 秒
	KeepAliveInterval    int // 秒
	AutoReconnect        bool
	ReconnectInterval    int // 秒
	ReconnectMaxInterval int // 秒
	TLS                  bool
	QoS                  byte   // QoS等级(0或1)
	Retain               bool   // 是否保留消息
	AllowInterfaceIps    string // 允许的网卡IP段，如 "192.168.1.0/24,10.0.0.0/8"
	Retries              int    // 发布失败重试次数，默认1
	RetryDelay           int    // 重试延迟时间(毫秒)，默认50
}

// parseMQTTURL 解析MQTT URL
// 格式: mqtt[s]://[username:[password]@]host[:port]/topic[?query_params]
func parseMQTTURL(rawURL string) (*MQTTConfig, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid MQTT URL: %w", err)
	}

	if u.Scheme != "mqtt" && u.Scheme != "mqtts" {
		return nil, fmt.Errorf("invalid scheme: %s, expected mqtt or mqtts", u.Scheme)
	}

	cfg := &MQTTConfig{
		Broker: u.Host,
		TLS:    u.Scheme == "mqtts",
		Topic:  strings.TrimPrefix(u.Path, "/"),
	}

	// 解析用户名密码
	if u.User != nil {
		cfg.Username = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}

	// 解析query参数
	q := u.Query()

	if v := q.Get("clientId"); v != "" {
		cfg.ClientID = v
	} else {
		cfg.ClientID = fmt.Sprintf("clipboard-sync-%s", hashURL(rawURL)[:8])
	}

	cfg.ConnectTimeout = 3 // 默认3秒
	if v := q.Get("connectTimeout"); v != "" {
		if t, err := time.ParseDuration(v + "s"); err == nil {
			cfg.ConnectTimeout = int(t.Seconds())
		}
	}

	cfg.KeepAliveInterval = 60 // 默认60秒
	if v := q.Get("keepAliveInterval"); v != "" {
		if t, err := time.ParseDuration(v + "s"); err == nil {
			cfg.KeepAliveInterval = int(t.Seconds())
		}
	}

	cfg.AutoReconnect = true // 默认启用
	if v := q.Get("automaticReconnect"); v != "" {
		cfg.AutoReconnect = v == "true"
	}

	cfg.ReconnectInterval = 5 // 默认5秒
	if v := q.Get("reconnectInterval"); v != "" {
		if t, err := time.ParseDuration(v + "s"); err == nil {
			cfg.ReconnectInterval = int(t.Seconds())
		}
	}

	cfg.ReconnectMaxInterval = 60 // 默认60秒
	if v := q.Get("reconnectMaxInterval"); v != "" {
		if t, err := time.ParseDuration(v + "s"); err == nil {
			cfg.ReconnectMaxInterval = int(t.Seconds())
		}
	}

	cfg.QoS = 1 // 默认QoS 1
	if v := q.Get("qos"); v == "0" {
		cfg.QoS = 0
	}

	cfg.Retain = true // 默认保留消息
	if v := q.Get("retain"); v != "" {
		cfg.Retain = v == "true"
	}

	// 解析 allowInterfaceIps 参数
	cfg.AllowInterfaceIps = q.Get("allowInterfaceIps")

	// 解析 retries 参数（发布失败重试次数，默认1）
	cfg.Retries = 1
	if v := q.Get("retries"); v != "" {
		if r, err := fmt.Sscanf(v, "%d", &cfg.Retries); err == nil && r > 0 {
			cfg.Retries = r
		}
	}

	// 解析 retryDelay 参数（重试延迟时间毫秒，默认50）
	cfg.RetryDelay = 50
	if v := q.Get("retryDelay"); v != "" {
		if d, err := fmt.Sscanf(v, "%d", &cfg.RetryDelay); err == nil && d > 0 {
			cfg.RetryDelay = d
		}
	}

	return cfg, nil
}

// createMQTTClient 创建MQTT客户端
func createMQTTClient(rawURL string) (mqtt.Client, error) {
	cfg, err := parseMQTTURL(rawURL)
	if err != nil {
		return nil, err
	}

	opts := mqtt.NewClientOptions()

	broker := cfg.Broker
	if cfg.TLS {
		broker = "ssl://" + broker
	} else {
		broker = "tcp://" + broker
	}

	opts.AddBroker(broker)
	opts.SetClientID(cfg.ClientID)
	opts.SetUsername(cfg.Username)
	opts.SetPassword(cfg.Password)
	opts.SetConnectTimeout(time.Duration(cfg.ConnectTimeout) * time.Second)
	opts.SetKeepAlive(time.Duration(cfg.KeepAliveInterval) * time.Second)
	opts.SetAutoReconnect(cfg.AutoReconnect)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(time.Duration(cfg.ReconnectInterval) * time.Second)
	opts.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		if debugClipboard {
			log.Printf("[DEBUG] MQTT received on %s: %s", msg.Topic(), string(msg.Payload()))
		}
	})

	// 设置连接丢失处理
	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		log.Printf("MQTT connection lost: %v", err)
		if debugClipboard {
			log.Printf("[DEBUG] MQTT connection lost, will auto reconnect")
		}
	})

	// 设置连接成功处理
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		log.Printf("MQTT reconnected successfully")
		if debugClipboard {
			log.Printf("[DEBUG] MQTT connection established")
		}
	})

	// 设置CleanSession为false以支持更好的重连
	opts.SetCleanSession(false)

	client := mqtt.NewClient(opts)

	token := client.Connect()
	if token.WaitTimeout(time.Duration(cfg.ConnectTimeout) * time.Second) {
		if err := token.Error(); err != nil {
			return nil, fmt.Errorf("MQTT connection failed: %w", err)
		}
	} else {
		return nil, fmt.Errorf("MQTT connection timeout")
	}

	if debugClipboard {
		log.Printf("[DEBUG] MQTT connected to %s, topic: %s", broker, cfg.Topic)
	}

	return client, nil
}

// resolveTopic 解析topic中的{$type}占位符，type为mime类型的小写前缀
func resolveTopic(topic string, mime string) string {
	if mime == "" {
		return topic
	}
	// 获取mime类型的前缀（如 "text/plain" -> "text", "image/png" -> "image"）
	parts := strings.SplitN(mime, "/", 2)
	contentType := strings.ToLower(parts[0])
	return strings.ReplaceAll(topic, "{$type}", contentType)
}

// syncViaMQTT 通过MQTT发送剪贴板内容
func syncViaMQTT(t *SyncTarget, msg ClipboardMessage) error {
	cfg, err := parseMQTTURL(t.RawURL)
	if err != nil {
		return err
	}

	// 解析topic中的{$type}占位符
	topic := resolveTopic(cfg.Topic, msg.Mime)

	jsonData, err := json.Marshal(msg)
	if err != nil {
		if debugClipboard {
			log.Printf("[DEBUG] MQTT JSON序列化失败: %v", err)
		}
		return err
	}

	if debugClipboard {
		log.Printf("[DEBUG] MQTT发送 - Topic: %s, Type: %s, Content长度: %d",
			topic, msg.Type, len(jsonData))
	}

	var lastErr error
	maxAttempts := cfg.Retries + 1 // 首次 + 重试次数

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 重试前延迟
			time.Sleep(time.Duration(cfg.RetryDelay) * time.Millisecond)
			if debugClipboard {
				log.Printf("[DEBUG] MQTT重试发布 (attempt %d/%d)", attempt, cfg.Retries)
			}
		}

		// 获取或创建连接
		client, err := getMQTTClient(t.RawURL)
		if err != nil {
			lastErr = fmt.Errorf("failed to get MQTT client: %w", err)
			continue
		}

		// 检查连接状态
		if !client.IsConnected() {
			// 等待自动重连（paho库会在后台自动重连）
			for i := 0; i < 30; i++ { // 最多等待30秒
				time.Sleep(time.Second)
				if client.IsConnected() {
					break
				}
			}
			if !client.IsConnected() {
				lastErr = fmt.Errorf("MQTT client disconnected and reconnection timeout")
				continue
			}
		}

		// 发布消息
		token := client.Publish(topic, cfg.QoS, cfg.Retain, jsonData)
		if token.WaitTimeout(10 * time.Second) {
			if err := token.Error(); err != nil {
				if debugClipboard {
					log.Printf("[DEBUG] MQTT发布失败: %v", err)
				}
				lastErr = fmt.Errorf("MQTT publish failed: %w", err)

				// 发布失败，需要重试
				if attempt < cfg.Retries {
					// 关闭连接并重连
					CloseMQTTClient(t.RawURL) // 会销毁连接，下次getMQTTClient会创建新连接
					if debugClipboard {
						log.Printf("[DEBUG] MQTT连接已关闭，等待重连...")
					}
					continue
				}
			} else {
				// 发布成功
				log.Printf("MQTT published to %s: %s\n", topic, msg.Type)
				return nil
			}
		} else {
			lastErr = fmt.Errorf("MQTT publish timeout")
			// 超时也需要重试
			if attempt < cfg.Retries {
				CloseMQTTClient(t.RawURL)
				continue
			}
		}
	}

	return lastErr
}

// CloseMQTTClient 关闭指定URL的MQTT连接
func CloseMQTTClient(rawURL string) {
	hash := hashURL(rawURL)
	mqttPool.mu.Lock()
	defer mqttPool.mu.Unlock()

	if client, exists := mqttPool.clients[hash]; exists {
		client.Disconnect(0)
		delete(mqttPool.clients, hash)
	}
}

// CloseAllMQTTClients 关闭所有MQTT连接
func CloseAllMQTTClients() {
	mqttPool.mu.Lock()
	defer mqttPool.mu.Unlock()

	for hash, client := range mqttPool.clients {
		client.Disconnect(0)
		delete(mqttPool.clients, hash)
	}
}
