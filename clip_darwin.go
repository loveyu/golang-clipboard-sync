//go:build darwin

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
	"os"
	"os/exec"
	"strings"
	"time"
)

const darwinPollInterval = 500 * time.Millisecond

func initClipboard() {
	// Check required tools
	var missing []string
	for _, cmd := range []string{"pbcopy", "pbpaste", "osascript"} {
		if !commandExists(cmd) {
			missing = append(missing, cmd)
		}
	}

	if len(missing) > 0 {
		msg := fmt.Sprintf("缺少必需程序: %s", strings.Join(missing, ", "))
		log.Printf("[ERROR] %s", msg)
		showErrorDialog("clipboard-sync - 环境检查失败", msg)
		log.Fatalf("Exiting: missing required programs: %s", strings.Join(missing, ", "))
	}

	log.Println("macOS clipboard initialized (pbcopy/pbpaste + osascript)")
}

// showErrorDialog shows a graphical error dialog on macOS via osascript.
func showErrorDialog(title, message string) {
	script := fmt.Sprintf(`display alert "%s" message "%s" as critical`, title, message)
	exec.Command("osascript", "-e", script).Run()
}

// getClipboardChangeCount returns the NSPasteboard changeCount via JXA.
// Only reads an integer, does not read clipboard content.
func getClipboardChangeCount() int64 {
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e",
		`ObjC.import("AppKit"); $.NSPasteboard.generalPasteboard.changeCount`).Output()
	if err != nil {
		return -1
	}
	var count int64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count)
	return count
}

// getClipboardTypes checks what types are currently on the clipboard via JXA.
// Returns "image" if image data exists, otherwise "text".
func getClipboardTypes() string {
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e",
		`ObjC.import("AppKit"); var pb = $.NSPasteboard.generalPasteboard; var types = pb.types; var hasImage = false; for (var i = 0; i < types.count; i++) { var t = types.objectAtIndex(i).js; if (t.indexOf("image") >= 0 || t.indexOf("PNG") >= 0 || t.indexOf("TIFF") >= 0) { hasImage = true; break; } } hasImage ? "image" : "text"`).Output()
	if err != nil {
		return "text"
	}
	result := strings.TrimSpace(string(out))
	if strings.Contains(result, "image") {
		return "image"
	}
	return "text"
}

// ReadClipboardContent reads clipboard content by MIME type.
func ReadClipboardContent(mime string) ([]byte, error) {
	if strings.HasPrefix(mime, "image/") {
		return readClipboardImageDarwin()
	}
	return readClipboardTextDarwin()
}

func readClipboardTextDarwin() ([]byte, error) {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return nil, fmt.Errorf("pbpaste failed: %w", err)
	}
	return out, nil
}

func readClipboardImageDarwin() ([]byte, error) {
	tmpFile, err := os.CreateTemp("", "clipboard_image_*.png")
	if err != nil {
		return nil, err
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// Use JXA to extract image data from NSPasteboard and write to temp file
	script := fmt.Sprintf(
		`ObjC.import("AppKit"); var pb = $.NSPasteboard.generalPasteboard; var data = pb.dataForType("public.png"); if (data) { data.writeToFileAtomically("%s", true); "ok" } else { "empty" }`,
		tmpPath)
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e", script).Output()
	if err != nil {
		return nil, fmt.Errorf("osascript image read failed: %w", err)
	}

	if strings.TrimSpace(string(out)) != "ok" {
		// Try TIFF as fallback
		script = fmt.Sprintf(
			`ObjC.import("AppKit"); var pb = $.NSPasteboard.generalPasteboard; var data = pb.dataForType("public.tiff"); if (data) { var imgRep = $.NSBitmapImageRep.alloc.initWithData(data); var pngData = imgRep.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $()); pngData.writeToFileAtomically("%s", true); "ok" } else { "empty" }`,
			tmpPath)
		out, err = exec.Command("osascript", "-l", "JavaScript", "-e", script).Output()
		if err != nil {
			return nil, fmt.Errorf("osascript TIFF read failed: %w", err)
		}
		if strings.TrimSpace(string(out)) != "ok" {
			return nil, fmt.Errorf("no image data on clipboard")
		}
	}

	return os.ReadFile(tmpPath)
}

// SetClipboardContentText sets text content to clipboard via pbcopy.
func SetClipboardContentText(content string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(content)
	return cmd.Run()
}

// SetClipboardContentImage sets image content to clipboard via JXA.
func SetClipboardContentImage(image []byte) error {
	tmpFile, err := os.CreateTemp("", "clipboard_image_*.png")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(image); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp image: %w", err)
	}
	tmpFile.Close()

	// Use JXA to read the PNG file and set it on the clipboard
	script := fmt.Sprintf(
		`ObjC.import("AppKit"); var data = $.NSData.dataWithContentsOfFile("%s"); if (data) { var pb = $.NSPasteboard.generalPasteboard; pb.clearContents; pb.setDataForType(data, "public.png"); "ok" } else { "error" }`,
		tmpPath)
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e", script).Output()
	if err != nil {
		return fmt.Errorf("osascript image write failed: %w", err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return fmt.Errorf("failed to set image clipboard")
	}
	return nil
}

// ListenClipboardChanges monitors clipboard changes by polling NSPasteboard changeCount.
// Only reads full content when the count actually changes.
func ListenClipboardChanges() <-chan ClipboardChange {
	changes := make(chan ClipboardChange, 1)

	go func() {
		defer close(changes)

		var lastCount int64 = getClipboardChangeCount()

		ticker := time.NewTicker(darwinPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				count := getClipboardChangeCount()
				if count == lastCount || count < 0 {
					continue
				}
				lastCount = count

				// ChangeCount changed — read the actual content
				clipType := getClipboardTypes()
				var mime string
				var content []byte
				var err error

				if clipType == "image" {
					mime = "image/png"
					content, err = readClipboardImageDarwin()
				} else {
					mime = "text/plain"
					content, err = readClipboardTextDarwin()
				}

				if err != nil || len(content) == 0 {
					if debugClipboard && err != nil {
						log.Printf("[DEBUG] macOS clipboard read error: %v", err)
					}
					continue
				}

				debugLog("clipboard change (macOS) - MIME: %s, length: %d", mime, len(content))

				select {
				case changes <- ClipboardChange{
					Timestamp: time.Now().Unix(),
					Mime:      mime,
					Content:   content,
				}:
				case <-stopCh:
					return
				}
			case <-stopCh:
				return
			}
		}
	}()

	log.Println("Started clipboard monitoring (macOS changeCount polling)")
	return changes
}

func debugLog(format string, args ...interface{}) {
	if debugClipboard {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func debugFormatContent(content []byte) string {
	displayContent := string(content)
	if len(displayContent) > 100 {
		displayContent = displayContent[:100] + "..."
	}
	return displayContent
}
