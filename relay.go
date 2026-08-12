package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// buildTLSConfig creates a *tls.Config from a Certificate entry.
// If cert is nil, returns nil (use default transport).
// Custom CA is merged with system cert pool.
func buildTLSConfig(cert *Certificate) (*tls.Config, error) {
	if cert == nil {
		return nil, nil
	}

	tlsCfg := &tls.Config{}

	// Load custom CA if specified
	if cert.CA != "" {
		caData, err := os.ReadFile(cert.CA)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %s: %w", cert.CA, err)
		}

		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caData) {
			return nil, fmt.Errorf("no certs added from %s", cert.CA)
		}
		tlsCfg.RootCAs = pool
	}

	// Load client certificate (mTLS) if specified
	if cert.Cert != "" && cert.Key != "" {
		certPair, err := tls.LoadX509KeyPair(cert.Cert, cert.Key)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{certPair}
	}

	return tlsCfg, nil
}

// httpClientWithCert creates an *http.Client with optional TLS config.
func httpClientWithCert(cert *Certificate) (*http.Client, error) {
	tlsCfg, err := buildTLSConfig(cert)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{}
	if tlsCfg != nil {
		transport.TLSClientConfig = tlsCfg
	}

	return &http.Client{Transport: transport}, nil
}

// putToCenter uploads data to a clipboard center.
// PUT {centerURL}/client/{clientName}/{msgID} with Bearer token auth.
func putToCenter(center *Center, clientName, msgID string, data []byte, contentType string) error {
	return putToCenterContext(context.Background(), center, clientName, msgID, data, contentType)
}

func putToCenterContext(ctx context.Context, center *Center, clientName, msgID string, data []byte, contentType string) error {
	cert := resolveCertForCenter(center)

	client, err := httpClientWithCert(cert)
	if err != nil {
		return fmt.Errorf("build HTTP client: %w", err)
	}

	u := strings.TrimSuffix(center.URL, "/") + "/client/" + clientName + "/" + msgID

	req, err := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if center.Token != "" {
		req.Header.Set("Authorization", "Bearer "+center.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT failed: HTTP %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// getFromCenter downloads data from a clipboard center.
// GET {centerURL}/client/{clientName}/{msgID} with Bearer token auth.
func getFromCenter(center *Center, clientName, msgID string) ([]byte, string, error) {
	cert := resolveCertForCenter(center)

	client, err := httpClientWithCert(cert)
	if err != nil {
		return nil, "", fmt.Errorf("build HTTP client: %w", err)
	}

	u := strings.TrimSuffix(center.URL, "/") + "/client/" + clientName + "/" + msgID

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}

	if center.Token != "" {
		req.Header.Set("Authorization", "Bearer "+center.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("GET request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("GET failed: HTTP %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	return data, ct, nil
}

// resolveCertForCenter resolves the certificate for a center.
func resolveCertForCenter(center *Center) *Certificate {
	if center.Certificate == "" || appConfig == nil {
		return nil
	}
	return appConfig.GetCertificateByID(center.Certificate)
}

// computeSHA1 computes the SHA1 hash of data and returns hex string.
func computeSHA1(data []byte) string {
	h := sha1.Sum(data)
	return fmt.Sprintf("%x", h)
}

// needsRelay determines if content should use V2 relay mode.
// Relay is needed for images or text content > 10KB.
func needsRelay(msgType string, base64Content string) bool {
	if msgType == ContentTypeImage {
		return true
	}
	if msgType == ContentTypeText {
		decoded, err := base64.StdEncoding.DecodeString(base64Content)
		if err != nil {
			return len(base64Content) > V2RelayThreshold
		}
		return len(decoded) > V2RelayThreshold
	}
	return false
}

// handleV2Receive processes a V2 message: download content from center and return V1 message.
func handleV2Receive(msg ClipboardMessage, center *Center) (*ClipboardMessage, error) {
	v2, err := ParseV2Content(msg.Content)
	if err != nil {
		return nil, fmt.Errorf("parse V2 content: %w", err)
	}

	data, ct, err := getFromCenter(center, v2.ClientID, v2.MsgID)
	if err != nil {
		return nil, fmt.Errorf("get from center: %w", err)
	}

	if debugClipboard {
		log.Printf("[DEBUG] V2 download: %d bytes, content-type: %s", len(data), ct)
	}

	// Decode based on encoding from V2 message (default: base64)
	var decoded []byte
	if v2.Encoding == "raw" {
		decoded = data
	} else {
		var err2 error
		decoded, err2 = base64.StdEncoding.DecodeString(string(data))
		if err2 != nil {
			log.Printf("[WARN] V2 base64 decode failed, using raw data: %v", err2)
			decoded = data
		}
	}

	// Reconstruct a V1 message
	base64Content := base64.StdEncoding.EncodeToString(decoded)
	v1Msg := &ClipboardMessage{
		Time:       msg.Time,
		UUID:       msg.UUID,
		DeviceName: msg.DeviceName,
		Mime:       msg.Mime,
		Type:       BaseContentType(msg.Type),
		Content:    base64Content,
		SendTime:   msg.SendTime,
	}
	return v1Msg, nil
}
