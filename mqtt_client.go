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
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

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
	QoS                  byte
	Retain               bool
	AllowInterfaceIps    string
	Retries              int
	RetryDelay           int
}

// MQTTClientPool maintains MQTT connections shared by broker+credentials.
type MQTTClientPool struct {
	clients map[string]mqtt.Client
	mu      sync.RWMutex
}

var mqttPool = &MQTTClientPool{
	clients: make(map[string]mqtt.Client),
}

// hashURL generates a hash for use as a pool key.
func hashURL(rawURL string) string {
	hash := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(hash[:16])
}

// getMQTTClientForTarget gets or creates an MQTT client for a target entry.
func getMQTTClientForTarget(target *TargetEntry) (mqtt.Client, error) {
	return getOrCreateMQTTClient(target.MQTTConfig, target.Certificate)
}

// getOrCreateMQTTClient gets or creates an MQTT client using broker+credentials as key.
func getOrCreateMQTTClient(cfg *MQTTConfig, cert *Certificate) (mqtt.Client, error) {
	key := ConnectionPoolKey(cfg)

	mqttPool.mu.RLock()
	client, exists := mqttPool.clients[key]
	mqttPool.mu.RUnlock()

	if exists && client.IsConnected() {
		return client, nil
	}

	mqttPool.mu.Lock()
	defer mqttPool.mu.Unlock()

	if client, exists = mqttPool.clients[key]; exists && client.IsConnected() {
		return client, nil
	}

	client, err := createMQTTClient(cfg, cert)
	if err != nil {
		return nil, err
	}

	mqttPool.clients[key] = client
	return client, nil
}

// createMQTTClient creates a new MQTT client.
func createMQTTClient(cfg *MQTTConfig, cert *Certificate) (mqtt.Client, error) {
	opts := BuildMQTTClientOptions(cfg, cert)

	opts.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		if debugClipboard {
			log.Printf("[DEBUG] MQTT received on %s: %d bytes", msg.Topic(), len(msg.Payload()))
		}
	})

	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		log.Printf("MQTT connection lost (%s): %v", cfg.Broker, err)
	})

	opts.SetOnConnectHandler(func(client mqtt.Client) {
		log.Printf("MQTT reconnected: %s", cfg.Broker)
		// Re-subscribe all listen entries that use this connection
		resubscribeForBroker(client, cfg)
	})

	client := mqtt.NewClient(opts)

	token := client.Connect()
	if token.WaitTimeout(time.Duration(cfg.ConnectTimeout) * time.Second) {
		if err := token.Error(); err != nil {
			return nil, fmt.Errorf("MQTT connection failed: %w", err)
		}
	} else {
		return nil, fmt.Errorf("MQTT connection timeout: %s", cfg.Broker)
	}

	if debugClipboard {
		log.Printf("[DEBUG] MQTT connected to %s, topic: %s", cfg.GetBrokerAddress(), cfg.Topic)
	}

	return client, nil
}

// closeMQTTClientByConfig closes an MQTT client by its config.
func closeMQTTClientByConfig(cfg *MQTTConfig) {
	key := ConnectionPoolKey(cfg)
	mqttPool.mu.Lock()
	defer mqttPool.mu.Unlock()

	if client, exists := mqttPool.clients[key]; exists {
		client.Disconnect(0)
		delete(mqttPool.clients, key)
	}
}

// CloseAllMQTTClients closes all MQTT connections.
func CloseAllMQTTClients() {
	mqttPool.mu.Lock()
	defer mqttPool.mu.Unlock()

	for key, client := range mqttPool.clients {
		client.Disconnect(0)
		delete(mqttPool.clients, key)
	}
}

// SubscribeAllListeners subscribes to all listen entry topics.
// Called at startup and on reconnection.
func SubscribeAllListeners() {
	if appConfig == nil {
		return
	}

	for i := range appConfig.Listen {
		entry := &appConfig.Listen[i]
		cfg := entry.MQTTConfig
		if cfg == nil {
			continue
		}

		client, err := getOrCreateMQTTClient(cfg, entry.Certificate)
		if err != nil {
			log.Printf("[WARN] Failed to get MQTT client for listen %s: %v", entry.ID, err)
			continue
		}

		subscribeTopic := cfg.SubscribeTopic()
		if debugClipboard {
			log.Printf("[DEBUG] Subscribing to %s (listen: %s)", subscribeTopic, entry.ID)
		}

		entryCopy := entry
		token := client.Subscribe(subscribeTopic, cfg.QoS, func(_ mqtt.Client, msg mqtt.Message) {
			handleMQTTMessage(entryCopy, msg.Topic(), msg.Payload())
		})
		token.Wait()
		if token.Error() != nil {
			log.Printf("[WARN] Subscribe failed for %s: %v", entry.ID, token.Error())
		}
	}
}

// resubscribeForBroker re-subscribes listen entries that use a specific broker connection.
func resubscribeForBroker(client mqtt.Client, reconnectedCfg *MQTTConfig) {
	if appConfig == nil {
		return
	}

	for i := range appConfig.Listen {
		entry := &appConfig.Listen[i]
		cfg := entry.MQTTConfig
		if cfg == nil {
			continue
		}

		// Check if this listen entry uses the same broker
		if ConnectionPoolKey(cfg) != ConnectionPoolKey(reconnectedCfg) {
			continue
		}

		subscribeTopic := cfg.SubscribeTopic()
		entryCopy := entry
		token := client.Subscribe(subscribeTopic, cfg.QoS, func(_ mqtt.Client, msg mqtt.Message) {
			handleMQTTMessage(entryCopy, msg.Topic(), msg.Payload())
		})
		token.Wait()
		if token.Error() != nil {
			log.Printf("[WARN] Re-subscribe failed for %s: %v", entry.ID, token.Error())
		} else if debugClipboard {
			log.Printf("[DEBUG] Re-subscribed %s", entry.ID)
		}
	}
}

// handleMQTTMessage processes a received MQTT message from a listen entry.
func handleMQTTMessage(entry *ListenEntry, topic string, payload []byte) {
	// Check message size limit
	if entry.MaxMessageSize > 0 && int64(len(payload)) > entry.MaxMessageSize {
		log.Printf("[WARN] MQTT message discarded: %d bytes exceeds maxMessageSize %d (listen: %s, topic: %s)",
			len(payload), entry.MaxMessageSize, entry.ID, topic)
		return
	}

	// Check interface filter
	if entry.AllowInterfaceIPs != "" {
		if !IsAllowedByInterface(entry.AllowInterfaceIPs, "") {
			if debugClipboard {
				log.Printf("[DEBUG] Interface filter skip - Listen: %s", entry.ID)
			}
			return
		}
	}

	// Parse message
	var msg ClipboardMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		log.Printf("[WARN] Failed to parse MQTT message from %s: %v", entry.ID, err)
		return
	}

	// Filter out own messages (echo prevention)
	if appConfig != nil && msg.DeviceName == appConfig.Device.Name {
		if debugClipboard {
			log.Printf("[DEBUG] Skipping own message from %s", msg.DeviceName)
		}
		return
	}

	log.Printf("Received clipboard via MQTT: device=%s, type=%s, listen=%s", msg.DeviceName, msg.Type, entry.ID)

	// Handle V2 messages
	if IsV2Type(msg.Type) {
		// Find center from forward rules
		centerID := findCenterForListen(entry.ID)
		if centerID != "" {
			center := appConfig.GetCenterByID(centerID)
			if center != nil {
				if err := handleV2Receive(msg, center); err != nil {
					log.Printf("[ERROR] V2 receive failed: %v", err)
				}
				// Continue to forward engine
				go forwardEngine.ProcessMessage(entry.ID, msg)
				return
			}
		}
		log.Printf("[WARN] V2 message received but no center configured for listen %s", entry.ID)
		return
	}

	// V1 message: set clipboard and trigger forward engine
	if forwardEngine != nil {
		// Check if any forward rule routes to "system" for this listen entry
		if shouldSetLocalClipboard(entry.ID) {
			setClipboardContent(msg)
		}
		go forwardEngine.ProcessMessage(entry.ID, msg)
	}
}

// findCenterForListen finds the center ID from a forward rule matching a listen entry.
func findCenterForListen(listenID string) string {
	if appConfig == nil {
		return ""
	}
	for i := range appConfig.Forward {
		rule := &appConfig.Forward[i]
		for _, from := range rule.From {
			if from == listenID && rule.Center != "" {
				return rule.Center
			}
		}
	}
	return ""
}

// shouldSetLocalClipboard checks if any forward rule routes from a listen entry to "system".
func shouldSetLocalClipboard(listenID string) bool {
	if appConfig == nil {
		return true
	}
	for i := range appConfig.Forward {
		rule := &appConfig.Forward[i]
		fromMatch := false
		for _, from := range rule.From {
			if from == listenID {
				fromMatch = true
				break
			}
		}
		if !fromMatch {
			continue
		}
		for _, to := range rule.To {
			if to == "system" {
				return true
			}
		}
	}
	return false
}
