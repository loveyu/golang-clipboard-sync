//go:build linux && !android

package x11clipboard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

func TestPropertyLongLength(t *testing.T) {
	tests := []struct {
		maxBytes int64
		want     uint32
	}{
		{maxBytes: 0, want: 1},
		{maxBytes: 1, want: 2},
		{maxBytes: 4, want: 2},
		{maxBytes: 5, want: 3},
	}
	for _, test := range tests {
		if got := propertyLongLength(test.maxBytes); got != test.want {
			t.Errorf("propertyLongLength(%d) = %d, want %d", test.maxBytes, got, test.want)
		}
	}
}

func TestNativeX11Compatibility(t *testing.T) {
	if os.Getenv("CLIPBOARD_X11_NATIVE_INTEGRATION") != "1" {
		t.Skip("set CLIPBOARD_X11_NATIVE_INTEGRATION=1 under an X11 server")
	}
	if os.Getenv("DISPLAY") == "" {
		t.Fatal("DISPLAY is empty")
	}
	if _, err := exec.LookPath("xclip"); err != nil {
		t.Fatalf("xclip is required for the compatibility test: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	monitor, err := Start(ctx)
	if err != nil {
		t.Fatalf("start native X11 monitor: %v", err)
	}
	defer monitor.Close()

	t.Run("xclip-to-native-small", func(t *testing.T) {
		payload := []byte("native X11 compatibility \xe2\x9c\x93")
		xclipWrite(t, ctx, "UTF8_STRING", payload)
		got, err := waitForSelectionRead(t, ctx, monitor, "UTF8_STRING", int64(len(payload)))
		if err != nil {
			t.Fatalf("read xclip selection: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("read %d bytes, want %d", len(got), len(payload))
		}
	})

	t.Run("native-to-xclip-with-origin", func(t *testing.T) {
		payload := []byte("native owner text")
		origin := []byte("integration-origin")
		if err := monitor.WriteWithOrigin(ctx, "text/plain;charset=utf-8", payload, "application/x-clipboard-sync-origin", origin); err != nil {
			t.Fatalf("write native selection: %v", err)
		}
		if got := xclipRead(t, ctx, "UTF8_STRING"); !bytes.Equal(got, payload) {
			t.Fatalf("xclip text read %d bytes, want %d", len(got), len(payload))
		}
		if got := xclipRead(t, ctx, "application/x-clipboard-sync-origin"); !bytes.Equal(got, origin) {
			t.Fatalf("xclip origin read %d bytes, want %d", len(got), len(origin))
		}
	})

	t.Run("xclip-to-native-incr", func(t *testing.T) {
		payload := bytes.Repeat([]byte("large-x11-payload-"), 64*1024)
		xclipWrite(t, ctx, "UTF8_STRING", payload)
		got, err := waitForSelectionRead(t, ctx, monitor, "UTF8_STRING", int64(len(payload)))
		if err != nil {
			t.Fatalf("read xclip INCR selection: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("read %d INCR bytes, want %d", len(got), len(payload))
		}
	})

	t.Run("native-to-xclip-incr", func(t *testing.T) {
		payload := bytes.Repeat([]byte("native-large-payload-"), 64*1024)
		if err := monitor.Write(ctx, "text/plain;charset=utf-8", payload); err != nil {
			t.Fatalf("write native INCR selection: %v", err)
		}
		if got := xclipRead(t, ctx, "UTF8_STRING"); !bytes.Equal(got, payload) {
			t.Fatalf("xclip INCR read %d bytes, want %d", len(got), len(payload))
		}
	})

	t.Run("bounded-incr-read", func(t *testing.T) {
		payload := bytes.Repeat([]byte("bounded-large-payload-"), 64*1024)
		xclipWrite(t, ctx, "UTF8_STRING", payload)
		_, err := waitForSelectionRead(t, ctx, monitor, "UTF8_STRING", 1024)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("bounded read error = %v, want ErrTooLarge", err)
		}
	})
}

func waitForSelectionRead(t *testing.T, ctx context.Context, monitor *Monitor, target string, maxBytes int64) ([]byte, error) {
	t.Helper()
	for {
		select {
		case selection, ok := <-monitor.Events():
			if !ok {
				t.Fatalf("monitor disconnected: %v", monitor.Err())
			}
			if !slices.Contains(selection.MIMEs, target) {
				selection.Release()
				continue
			}
			data, err := selection.Read(ctx, target, maxBytes)
			selection.Release()
			if errors.Is(err, ErrReleased) {
				continue
			}
			return data, err
		case <-ctx.Done():
			t.Fatalf("wait for %s selection: %v", target, ctx.Err())
		}
	}
}

func xclipWrite(t *testing.T, ctx context.Context, target string, payload []byte) {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open null output: %v", err)
	}
	defer null.Close()
	command := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", target, "-i")
	command.Stdin = bytes.NewReader(payload)
	// xclip forks an owner process that keeps inherited descriptors open. Use
	// real files here so os/exec does not wait forever for a capture pipe EOF.
	command.Stdout = null
	command.Stderr = null
	if err := command.Run(); err != nil {
		t.Fatalf("xclip write %s failed: %v", target, err)
	}
}

func xclipRead(t *testing.T, ctx context.Context, target string) []byte {
	t.Helper()
	command := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", target, "-o")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("xclip read %s failed: %v", target, err)
	}
	return output
}
