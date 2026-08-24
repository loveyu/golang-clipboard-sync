package main

import (
	"bytes"
	"testing"
)

func TestDetermineContentTypeSupportsX11TextTargets(t *testing.T) {
	for _, target := range []string{"UTF8_STRING", "STRING", "TEXT", "utf8_string"} {
		kind, mime := DetermineContentType([]string{target})
		if kind != "text" || mime != "text/plain;charset=utf-8" {
			t.Errorf("DetermineContentType(%q) = (%q, %q), want canonical UTF-8 text", target, kind, mime)
		}
	}
}

func TestConvertX11StringFromLatin1(t *testing.T) {
	got := convertToUTF8([]byte{'c', 'a', 'f', 0xe9}, "STRING")
	if !bytes.Equal(got, []byte("caf\xc3\xa9")) {
		t.Fatalf("convertToUTF8(STRING) = %q, want UTF-8 text", got)
	}
}
