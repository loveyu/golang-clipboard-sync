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

// ClipboardMessage 剪贴板同步消息格式
type ClipboardMessage struct {
	Time          float64 `json:"time"`                    // 时间戳（Unix seconds）
	UUID          string  `json:"uuid"`                    // 消息唯一标识
	DeviceName    string  `json:"deviceName"`              // 设备名称
	Mime          string  `json:"mime"`                    // MIME类型
	Type          string  `json:"type"`                    // 内容类型（text/image）
	Content       string  `json:"content"`                 // Base64编码的内容
	SendTime      float64 `json:"sendTime"`                // 发送时间（Unix seconds）
	ForwardSource string  `json:"forwardSource,omitempty"` // 转发来源设备名称
}

// ClipboardChange represents a clipboard change event with timestamp and content
type ClipboardChange struct {
	Timestamp int64  `json:"timestamp"`
	Mime      string `json:"mime"`
	Content   []byte `json:"content"`
}
