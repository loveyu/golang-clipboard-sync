package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigClipboardDefaults(t *testing.T) {
	path := writeClipboardConfigTestFile(t, "device:\n  name: test\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Clipboard != defaultClipboardConfig() {
		t.Fatalf("clipboard 默认配置 = %+v，期望 %+v", cfg.Clipboard, defaultClipboardConfig())
	}
}

func TestLoadConfigClipboardValuesAndEnvironmentOverride(t *testing.T) {
	t.Setenv("CLIPBOARD_BACKEND", "command")
	path := writeClipboardConfigTestFile(t, `device:
  name: test
clipboard:
  backend: native
  dedupWindowMs: 0
  readTimeoutMs: 800
  maxContentBytes: 1048576
  imagePixelDedup: false
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Clipboard.Backend != "command" || cfg.Clipboard.DedupWindowMS != 0 || cfg.Clipboard.ImagePixelDedup {
		t.Fatalf("clipboard 配置解析错误: %+v", cfg.Clipboard)
	}
}

func TestLoadConfigRejectsInvalidClipboardConfig(t *testing.T) {
	t.Setenv("CLIPBOARD_BACKEND", "")
	tests := []string{
		"backend: invalid",
		"dedupWindowMs: 60001",
		"readTimeoutMs: 499",
		"maxContentBytes: 1024",
	}
	for _, value := range tests {
		t.Run(strings.ReplaceAll(value, " ", "_"), func(t *testing.T) {
			path := writeClipboardConfigTestFile(t, "device:\n  name: test\nclipboard:\n  "+value+"\n")
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("非法配置 %q 未返回错误", value)
			}
		})
	}
}

func writeClipboardConfigTestFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
