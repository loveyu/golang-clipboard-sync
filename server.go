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
	"io"
	"log"
	"net/http"
)

func startLocalServer() {
	if appConfig == nil {
		return
	}

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

		// Filter out own messages
		if appConfig != nil && msg.DeviceName == appConfig.Device.Name {
			if debugClipboard {
				log.Printf("[DEBUG] Skipping own HTTP message from %s", msg.DeviceName)
			}
			_, _ = w.Write([]byte("ok"))
			return
		}

		log.Printf("Received clipboard via HTTP: device=%s, type=%s, mime=%s", msg.DeviceName, msg.Type, msg.Mime)

		// Handle V2 messages: download content from center and convert to V1
		if IsV2Type(msg.Type) {
			centerID := findCenterForSource("http")
			if centerID != "" {
				center := appConfig.GetCenterByID(centerID)
				if center != nil {
					v1Msg, err := handleV2Receive(msg, center)
					if err != nil {
						log.Printf("[ERROR] V2 receive via HTTP failed: %v", err)
					} else if v1Msg != nil {
						msg = *v1Msg
					}
				}
			}
		}

		// Route through forward engine with "http" as source ID
		if forwardEngine != nil {
			go forwardEngine.DispatchMessage("http", msg)
		}

		_, _ = w.Write([]byte("ok"))
	})

	addr := appConfig.HTTP.Port
	if addr == "" {
		addr = ":9144"
	}

	log.Printf("Starting local HTTP server on %s", addr)
	err := http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatalf("Failed to start the local server: %v", err)
	}
}
