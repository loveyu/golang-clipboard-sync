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

// syncViaHTTPTarget sends a clipboard message to an HTTP target.
func syncViaHTTPTarget(target *TargetEntry, msg ClipboardMessage) error {
	return syncViaHTTPTargetContext(context.Background(), target, msg)
}

func syncViaHTTPTargetContext(ctx context.Context, target *TargetEntry, msg ClipboardMessage) error {
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	if debugClipboard {
		log.Printf("[DEBUG] HTTP request - Target: %s, Type: %s, Size: %d",
			target.ID, msg.Type, len(jsonData))
	}

	// Build clean URL (strip auth and config params from query)
	reqURL := *target.ParsedURL
	reqURL.User = nil
	reqURL.RawQuery = ""
	reqURL.Path = resolveTopic(reqURL.Path, msg.Mime)
	cleanURL := reqURL.String()

	// Build HTTP client with optional TLS
	client, err := httpClientWithCert(target.Certificate)
	if err != nil {
		return fmt.Errorf("build HTTP client: %w", err)
	}

	var lastErr error
	maxAttempts := target.Retries + 1

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := waitForContext(ctx, time.Duration(target.RetryDelay)*time.Millisecond); err != nil {
				return err
			}
			if debugClipboard {
				log.Printf("[DEBUG] HTTP retry (attempt %d/%d) for %s", attempt, target.Retries, target.ID)
			}
		}

		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(requestCtx, "POST", cleanURL, bytes.NewBuffer(jsonData))
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		if target.Username != "" {
			req.SetBasicAuth(target.Username, target.Password)
		}

		resp, err := client.Do(req)
		cancel()
		if err != nil {
			if debugClipboard {
				log.Printf("[DEBUG] HTTP request failed: %s, error: %v", target.ID, err)
			}
			lastErr = err
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if debugClipboard {
			log.Printf("[DEBUG] HTTP response - Target: %s, Status: %d, Body: %s",
				target.ID, resp.StatusCode, string(respBody))
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Printf("HTTP sent to %s: %s", cleanURL, msg.Type)
			return nil
		}

		lastErr = fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	return lastErr
}
