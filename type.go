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
	"strconv"
	"strings"
)

// Content type constants
const (
	ContentTypeText    = "text"
	ContentTypeImage   = "image"
	ContentTypeTextV2  = "text-v2"
	ContentTypeImageV2 = "image-v2"

	V2RelayThreshold = 10 * 1024 // 10KB threshold for V2 relay
)

// IsV2Type returns true for text-v2 and image-v2 content types.
func IsV2Type(t string) bool {
	return t == ContentTypeTextV2 || t == ContentTypeImageV2
}

// BaseContentType returns the base type (text/image) from a V1 or V2 type.
func BaseContentType(t string) string {
	switch t {
	case ContentTypeText, ContentTypeTextV2:
		return ContentTypeText
	case ContentTypeImage, ContentTypeImageV2:
		return ContentTypeImage
	default:
		return t
	}
}

// V2ContentType returns the V2 version of a base type.
func V2ContentType(t string) string {
	switch t {
	case ContentTypeText:
		return ContentTypeTextV2
	case ContentTypeImage:
		return ContentTypeImageV2
	default:
		return t
	}
}

// V2Content represents a parsed V2 relay content string.
// Format: "clientId/msgId,centerId:xxx,sha1:xxxxx,length:1111"
type V2Content struct {
	ClientID string
	MsgID    string
	CenterID string
	SHA1     string
	Length   int
}

// ParseV2Content parses a V2 relay content string.
func ParseV2Content(s string) (*V2Content, error) {
	parts := strings.Split(s, ",")
	if len(parts) < 1 {
		return nil, fmt.Errorf("empty V2 content")
	}

	v2 := &V2Content{}

	// First part: clientId/msgId
	pathParts := strings.SplitN(parts[0], "/", 2)
	if len(pathParts) != 2 {
		return nil, fmt.Errorf("invalid clientId/msgId format: %s", parts[0])
	}
	v2.ClientID = pathParts[0]
	v2.MsgID = pathParts[1]

	// Remaining parts: key:value pairs
	for _, part := range parts[1:] {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "centerId":
			v2.CenterID = kv[1]
		case "sha1":
			v2.SHA1 = kv[1]
		case "length":
			if n, err := strconv.Atoi(kv[1]); err == nil {
				v2.Length = n
			}
		}
	}

	return v2, nil
}

// BuildV2Content builds a V2 relay content string.
func BuildV2Content(clientID, msgID, centerID, sha1 string, length int) string {
	return fmt.Sprintf("%s/%s,centerId:%s,sha1:%s,length:%d",
		clientID, msgID, centerID, sha1, length)
}

// ClipboardMessage 剪贴板同步消息格式
type ClipboardMessage struct {
	Time          float64 `json:"time"`                    // 时间戳（Unix seconds）
	UUID          string  `json:"uuid"`                    // 消息唯一标识
	DeviceName    string  `json:"deviceName"`              // 设备名称
	Mime          string  `json:"mime"`                    // MIME类型
	Type          string  `json:"type"`                    // 内容类型（text/image/text-v2/image-v2）
	Content       string  `json:"content"`                 // Base64编码的内容 或 V2引用字符串
	SendTime      float64 `json:"sendTime"`                // 发送时间（Unix seconds）
	ForwardSource string  `json:"forwardSource,omitempty"` // 转发来源设备名称
}

// ClipboardChange represents a clipboard change event with timestamp and content
type ClipboardChange struct {
	Timestamp int64  `json:"timestamp"`
	Mime      string `json:"mime"`
	Content   []byte `json:"content"`
}
