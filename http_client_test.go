package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestHTTPForwardCancellationStopsInflightRequest(t *testing.T) {
	started := make(chan struct{})
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-request.Context().Done():
		case <-releaseServer:
		}
	}))
	t.Cleanup(func() {
		close(releaseServer)
		server.Close()
	})

	parsed, err := url.Parse(server.URL + "/update-clipboard")
	if err != nil {
		t.Fatal(err)
	}
	target := &TargetEntry{ID: "http", ParsedURL: parsed}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- syncViaHTTPTargetContext(ctx, target, ClipboardMessage{Type: ContentTypeText, Content: "dGVzdA=="})
	}()

	waitTestSignal(t, started, "HTTP 请求开始")
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("HTTP 取消错误 = %v，期望 context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP 请求取消超时")
	}
}
