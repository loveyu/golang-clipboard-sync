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
)

// needsInterfaceFilter checks if any listen or target entry uses interface filtering.
func needsInterfaceFilter() bool {
	if appConfig == nil {
		return false
	}
	for i := range appConfig.Listen {
		if appConfig.Listen[i].AllowInterfaceIPs != "" {
			return true
		}
	}
	for i := range appConfig.Targets {
		if appConfig.Targets[i].AllowInterfaceIPs != "" {
			return true
		}
	}
	return false
}

// isTypeAllowed checks if content type is in the allowed list. Empty list allows all.
func isTypeAllowed(allowedTypes []string, contentType string) bool {
	if len(allowedTypes) == 0 {
		return true
	}
	baseType := BaseContentType(contentType)
	for _, t := range allowedTypes {
		if t == contentType || t == baseType {
			return true
		}
	}
	return false
}

// isInterfaceAllowed checks if local IP matches allowed/denied CIDR ranges.
func isInterfaceAllowed(allowIPs, denyIPs string) bool {
	if allowIPs == "" && denyIPs == "" {
		return true
	}

	allowed := IsAllowedByInterface(strings.Join([]string{allowIPs}, ","), strings.Join([]string{denyIPs}, ","))
	if !allowed && debugClipboard {
		log.Printf("[DEBUG] Interface filter blocked: allow=%s, deny=%s", allowIPs, denyIPs)
	}
	return allowed
}
