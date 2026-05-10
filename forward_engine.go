package main

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"
)

// ForwardEngine routes clipboard messages based on forward rules.
type ForwardEngine struct {
	cfg *Config
}

// NewForwardEngine creates a new forward engine.
func NewForwardEngine(cfg *Config) *ForwardEngine {
	return &ForwardEngine{cfg: cfg}
}

// ProcessMessage routes a clipboard message from a source to all matching targets.
// sourceID is "system" for local clipboard changes, or a listen entry ID.
func (e *ForwardEngine) ProcessMessage(sourceID string, msg ClipboardMessage) {
	// Collect all target IDs from matching forward rules
	targetIDSet := make(map[string]bool)

	for i := range e.cfg.Forward {
		rule := &e.cfg.Forward[i]
		matched := false
		for _, from := range rule.From {
			if from == sourceID {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		for _, to := range rule.To {
			targetIDSet[to] = true
		}
	}

	if len(targetIDSet) == 0 {
		return
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10)

	for targetID := range targetIDSet {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if id == "system" {
				setClipboardContent(msg)
				return
			}

			target := e.cfg.GetTargetByID(id)
			if target == nil {
				log.Printf("[WARN] Forward target not found: %s", id)
				return
			}

			center := e.findCenterForTarget(sourceID, id)
			e.forwardToTarget(target, msg, center)
		}(targetID)
	}

	wg.Wait()
}

// findCenterForTarget finds the center ID from a forward rule matching source->target.
func (e *ForwardEngine) findCenterForTarget(sourceID, targetID string) string {
	for i := range e.cfg.Forward {
		rule := &e.cfg.Forward[i]
		fromMatch := false
		for _, from := range rule.From {
			if from == sourceID {
				fromMatch = true
				break
			}
		}
		if !fromMatch {
			continue
		}
		for _, to := range rule.To {
			if to == targetID && rule.Center != "" {
				return rule.Center
			}
		}
	}
	return ""
}

// forwardToTarget sends a message to a specific target.
func (e *ForwardEngine) forwardToTarget(target *TargetEntry, msg ClipboardMessage, centerID string) {
	// Check network condition
	if target.AllowInterfaceIPs != "" {
		if !isInterfaceAllowed(target.AllowInterfaceIPs, "") {
			if debugClipboard {
				log.Printf("[DEBUG] Interface filter skip - Target: %s", target.ID)
			}
			return
		}
	}

	// Check type filter
	if len(target.Types) > 0 {
		baseType := BaseContentType(msg.Type)
		if !isTypeAllowed(target.Types, baseType) {
			if debugClipboard {
				log.Printf("[DEBUG] Type filter skip - Target: %s, Type: %s", target.ID, msg.Type)
			}
			return
		}
	}

	if target.IsMQTT {
		e.forwardViaMQTT(target, msg, centerID)
	} else {
		e.forwardViaHTTP(target, msg)
	}
}

// forwardViaMQTT sends a message via MQTT. If centerID is set and content qualifies,
// uses V2 relay mode.
func (e *ForwardEngine) forwardViaMQTT(target *TargetEntry, msg ClipboardMessage, centerID string) {
	useV2 := false
	if centerID != "" && needsRelay(msg.Type, msg.Content) {
		center := e.cfg.GetCenterByID(centerID)
		if center != nil {
			useV2 = true
		}
	}

	var publishMsg ClipboardMessage
	if useV2 {
		centerCfg := e.cfg.GetCenterByID(centerID)

		// Decode content
		decoded, err := base64.StdEncoding.DecodeString(msg.Content)
		if err != nil {
			log.Printf("[ERROR] Failed to decode content for V2 relay: %v", err)
			return
		}

		// Determine msgId
		msgID := centerCfg.TextMsgID
		contentType := "text/plain"
		if msg.Type == ContentTypeImage {
			msgID = centerCfg.ImageMsgID
			contentType = msg.Mime
		}
		if msgID == "" {
			msgID = "clipboard-" + BaseContentType(msg.Type)
		}

		// PUT to center
		deviceName := e.cfg.Device.Name
		if err := putToCenter(centerCfg, deviceName, msgID, decoded, contentType); err != nil {
			log.Printf("[ERROR] Failed to PUT to center %s: %v", centerID, err)
			return
		}

		// Build V2 message
		sha1Hash := computeSHA1(decoded)
		v2Content := BuildV2Content(deviceName, msgID, centerID, sha1Hash, len(decoded))

		publishMsg = ClipboardMessage{
			Time:       msg.Time,
			UUID:       msg.UUID,
			DeviceName: msg.DeviceName,
			Mime:       msg.Mime,
			Type:       V2ContentType(msg.Type),
			Content:    v2Content,
			SendTime:   msg.SendTime,
		}

		if debugClipboard {
			log.Printf("[DEBUG] V2 relay: PUT %d bytes to center %s, MQTT type=%s",
				len(decoded), centerID, publishMsg.Type)
		}
	} else {
		publishMsg = msg
	}

	// Publish via MQTT
	if err := publishMQTTMessage(target, publishMsg); err != nil {
		log.Printf("[ERROR] MQTT publish to %s failed: %v", target.ID, err)
	}
}

// forwardViaHTTP sends a message via HTTP POST.
func (e *ForwardEngine) forwardViaHTTP(target *TargetEntry, msg ClipboardMessage) {
	if err := syncViaHTTPTarget(target, msg); err != nil {
		log.Printf("[ERROR] HTTP forward to %s failed: %v", target.ID, err)
	}
}

// publishMQTTMessage publishes a ClipboardMessage to an MQTT target.
func publishMQTTMessage(target *TargetEntry, msg ClipboardMessage) error {
	cfg := target.MQTTConfig
	topic := resolveTopic(cfg.Topic, msg.Mime)

	jsonData, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	if debugClipboard {
		log.Printf("[DEBUG] MQTT publish - Target: %s, Topic: %s, Type: %s, Size: %d",
			target.ID, topic, msg.Type, len(jsonData))
	}

	var lastErr error
	maxAttempts := cfg.Retries + 1

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(target.RetryDelay) * time.Millisecond)
			if debugClipboard {
				log.Printf("[DEBUG] MQTT retry (attempt %d/%d) for %s", attempt, cfg.Retries, target.ID)
			}
		}

		client, err := getMQTTClientForTarget(target)
		if err != nil {
			lastErr = err
			continue
		}

		if !client.IsConnected() {
			for i := 0; i < 30; i++ {
				time.Sleep(time.Second)
				if client.IsConnected() {
					break
				}
			}
			if !client.IsConnected() {
				lastErr = errDisconnected
				continue
			}
		}

		token := client.Publish(topic, cfg.QoS, cfg.Retain, jsonData)
		if token.WaitTimeout(10 * time.Second) {
			if err := token.Error(); err != nil {
				lastErr = err
				if attempt < cfg.Retries {
					closeMQTTClientByConfig(cfg)
				}
				continue
			}
			log.Printf("MQTT published to %s: %s", topic, msg.Type)
			return nil
		}
		lastErr = errPublishTimeout
		if attempt < cfg.Retries {
			closeMQTTClientByConfig(cfg)
		}
	}

	return lastErr
}

// resolveTopic resolves {$type} placeholder in topic.
func resolveTopic(topic, mime string) string {
	if mime == "" {
		return topic
	}
	parts := strings.SplitN(mime, "/", 2)
	contentType := strings.ToLower(parts[0])
	return strings.ReplaceAll(topic, "{$type}", contentType)
}

var (
	errDisconnected   = &publishError{"MQTT client disconnected"}
	errPublishTimeout = &publishError{"MQTT publish timeout"}
)

type publishError struct {
	msg string
}

func (e *publishError) Error() string { return e.msg }
