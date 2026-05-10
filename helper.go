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
	"crypto/rand"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// commandExists checks whether a command is available in PATH.
func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func removeTrailingNewline(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		return data[:len(data)-1]
	}
	return data
}

// DetermineContentType determines the type of content (text, image, file, etc.).
func DetermineContentType(types []string) (string, string) {
	for _, t := range types {
		// Check for image types
		if strings.HasPrefix(t, "image/") {
			return "image", t
		}
		// Check for text types
		if strings.HasPrefix(t, "text/") {
			return "text", t
		}
		// Check for file types
		if t == "text/uri-list" || t == "application/x-kde4-urilist" {
			return "file", t
		}
	}
	return "unknown", "application/unknown"
}

// extractCharset parses the charset parameter from a MIME type like "text/plain;charset=utf-8".
func extractCharset(mime string) string {
	for _, part := range strings.Split(mime, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "charset=") {
			return strings.ToLower(strings.TrimPrefix(strings.ToLower(part), "charset="))
		}
	}
	return ""
}

// isUTF8Charset reports whether a charset name is a UTF-8 variant.
func isUTF8Charset(cs string) bool {
	switch cs {
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return true
	}
	return false
}

// convertToUTF8 converts data from the charset encoded in the MIME type to UTF-8.
// Returns the original data unchanged if already UTF-8 or charset is unknown.
func convertToUTF8(data []byte, mime string) []byte {
	charset := extractCharset(mime)
	switch charset {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return data
	case "utf-16", "unicode":
		// Detect BOM or default to little-endian
		dec := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM)
		out, _, err := transform.Bytes(dec.NewDecoder(), data)
		if err == nil {
			return out
		}
		return decodeUTF16LE(data)
	case "utf-16le", "utf-16-le":
		out, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder(), data)
		if err == nil {
			return out
		}
		return decodeUTF16LE(data)
	case "utf-16be", "utf-16-be":
		out, _, err := transform.Bytes(unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder(), data)
		if err == nil {
			return out
		}
		return data
	case "iso-8859-1", "latin-1", "latin1":
		out, _, err := transform.Bytes(charmap.ISO8859_1.NewDecoder(), data)
		if err == nil {
			return out
		}
		return data
	default:
		if utf8.Valid(data) {
			return data
		}
		return data
	}
}

// decodeUTF16LE decodes UTF-16 little-endian bytes to UTF-8.
func decodeUTF16LE(b []byte) []byte {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	runes := utf16.Decode(u16)
	var buf bytes.Buffer
	for _, r := range runes {
		buf.WriteRune(r)
	}
	return buf.Bytes()
}

// generateUUID 生成新的 UUID v4
func generateUUID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return ""
	}
	// Set version 4 (random) and variant bits
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
