# clipboard-sync

[English](README.md) | [中文](README_CN.md)

Cross-platform clipboard synchronization tool with flexible MQTT and HTTP multi-protocol support via YAML configuration.

## Features

- **YAML Configuration**: Unified config file for all parameters, supports remote config download
- **Multi-protocol**: MQTT (pub/sub) and HTTP (push) protocols
- **Cross-platform**: Linux (X11/Wayland), macOS, Windows
- **Native Wayland**: Pure Go, single data-control connection with no Cgo or runtime `libwayland`
- **Reliable deduplication**: Raw-content and pixel-exact image deduplication plus content-bound echo suppression
- **Forward Engine**: Flexible message routing with multi-source/multi-target and automatic deduplication
- **V2 Relay Protocol**: Large files relayed via HTTP center server; MQTT only transmits a reference
- **Certificate Support**: Custom CA and mTLS client certificates
- **Network Filtering**: Filter by local IP subnet or interface name patterns
- **Connection Pooling**: MQTT connections shared by broker+credentials
- **Automatic recovery**: Exponential back-off reconnect for native connections and command listeners

## Quick Start

### Requirements

- Go 1.24+
- Linux: xclip/xsel for X11; native Wayland has no external dependency and falls back to wl-clipboard when data-control is unavailable
- macOS: pbcopy/pbpaste, osascript (bundled with macOS)
- Windows: no extra dependencies

### Build

```bash
go build -o clipboard-sync .
```

### Configuration

Create `config.yaml` (refer to `config-example.yaml`):

```yaml
device:
  name: "my-device"
  allowedInterfacePatterns: "eth*,enp*,wlp*"
  maxRuntime: 3600

clipboard:
  backend: auto
  dedupWindowMs: 5000
  readTimeoutMs: 5000
  maxContentBytes: 134217728
  imagePixelDedup: true

debug: false

http:
  port: ":9144"

targets:
  - id: "mqtt-pub"
    dsn: "mqtt://user:pass@broker:1883/clipboard/{$type}/my-device"

forward:
  - from: ["system"]
    to: ["mqtt-pub"]
```

### Run

```bash
# Start (default command)
./clipboard-sync

# Specify config file
./clipboard-sync -config /path/to/config.yaml

# Download remote config
./clipboard-sync download-config

# Print version
./clipboard-sync version

# Test mode: write to clipboard and trigger sync
./clipboard-sync --received-write-text "Hello World"
./clipboard-sync --received-image-file /path/to/image.png
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `CLIPBOARD_CONFIG_PATH` | Config file path, default `config.yaml` |
| `CLIPBOARD_DEBUG` | Debug mode, set to `1` to enable |
| `CLIPBOARD_BACKEND` | Override the backend with `auto`, `native`, or `command` |
| `REMOTE_CONFIG_URL` | Remote config URL (for `download-config` command) |

## Configuration Reference

### device

```yaml
device:
  name: "my-notebook"                   # Device name; uses hostname if empty
  allowedInterfacePatterns: "eth*,enp*" # Interface name filter (wildcards)
  maxRuntime: 3600                      # Max listener runtime in seconds before auto-restart
```

### clipboard

```yaml
clipboard:
  backend: auto
  dedupWindowMs: 5000
  readTimeoutMs: 5000
  maxContentBytes: 134217728
  imagePixelDedup: true
```

| Parameter | Description | Default |
|-----------|-------------|---------|
| `backend` | `auto`, `native`, or `command`; native applies to Wayland data-control | `auto` |
| `dedupWindowMs` | Raw and pixel-exact deduplication window (0–60000 ms) | `5000` |
| `readTimeoutMs` | Per-selection read timeout (500–60000 ms) | `5000` |
| `maxContentBytes` | Per-selection limit (1 MiB–1 GiB) | `134217728` |
| `imagePixelDedup` | Compare exact decoded pixels when image encodings differ | `true` |

On Linux Wayland, `auto` prefers `ext_data_control_manager_v1`, then
`zwlr_data_control_manager_v1`, and falls back to the command backend if native initialization is unavailable.
Runtime disconnects reconnect at 1, 2, 4, 8, 16, 30, then at most 60 seconds.
Set `CLIPBOARD_BACKEND=command` for an immediate rollback.

### http — Local HTTP Server

```yaml
http:
  port: ":9144"   # Listen port
```

The local HTTP server accepts messages at `/update-clipboard`. Received messages enter the forward rules with source ID `"http"`.

### certificates

```yaml
certificates:
  - id: "home-ca"
    ca: "/path/to/ca.crt"               # CA certificate
  - id: "mtls"
    ca: "/path/to/ca.crt"
    cert: "/path/to/client.crt"         # Client certificate (mTLS)
    key: "/path/to/client.key"          # Client key (mTLS)
```

Referenced by `centers` and by the `certificate` DSN parameter.

### centers — Clipboard Center

```yaml
centers:
  - id: "home"
    url: "http://10.0.0.1:8080"         # Center HTTP address
    token: "secure-token"               # Bearer token authentication
    textMsgId: "clipboard-text"         # Text message ID
    imageMsgId: "clipboard-image"       # Image message ID
    encoding: base64                    # Storage encoding: base64 (default) or raw
    # certificate: "home-ca"            # Reference certificate for HTTPS
```

The center server is used by the V2 relay protocol. When a forward rule has a `center`, images or text > 10 KB are PUT to the center first, then an MQTT reference is sent.

- `encoding: base64` (default): Center stores Base64-encoded content; Content-Type stays unchanged
- `encoding: raw`: Center stores raw bytes, convenient for external preview

### listen — MQTT Subscription

```yaml
listen:
  - id: "home-recv"
    dsn: "mqtt://user:pass@10.0.0.1:1883/clipboard/{$type}/+?maxMessageSize=5MB"
    allowInterfaceIps: "10.0.0.0/24"    # IP subnet filter (optional)
```

DSN format: `mqtt[s]://[user:pass@]host:port/topic[?params]`

`{$type}` is replaced with the MQTT wildcard `+` when subscribing.

Supported DSN parameters:

| Parameter | Description | Default |
|-----------|-------------|---------|
| `maxMessageSize` | Max message size (e.g. `5MB`, `512KB`) | unlimited |
| `certificate` | References a certificate ID | — |
| `clientId` | MQTT client ID | auto-generated |
| `connectTimeout` | Connection timeout (s) | 3 |
| `keepAliveInterval` | Keep-alive interval (s) | 60 |
| `automaticReconnect` | Auto-reconnect | true |
| `qos` | QoS level (0/1/2) | 1 |
| `retain` | Retain messages | true |

### targets — Publish Targets

```yaml
targets:
  - id: "mqtt-pub"
    dsn: "mqtt://user:pass@10.0.0.1:1883/clipboard/{$type}/my-device"
    allowInterfaceIps: "10.0.0.0/24"
  - id: "http-relay"
    dsn: "http://10.0.0.1:9144/update-clipboard"
```

Supports MQTT and HTTP. `{$type}` is replaced with `text` or `image` when publishing.

HTTP target extra parameters:

| Parameter | Description | Default |
|-----------|-------------|---------|
| `types` | Allowed content types | all |
| `retries` | Retry attempts on failure | 0 |
| `retryDelay` | Retry delay (ms) | 50 |

### forward — Forward Rules

```yaml
forward:
  - from: ["system"]
    to: ["mqtt-pub"]
    center: "home"        # Optional: enable V2 relay

  - from: ["home-recv"]
    to: ["system"]
    center: "home"

  - from: ["http"]
    to: ["system", "mqtt-pub"]
```

**Reserved IDs** (cannot be used as listen/target/center ids):

| ID | As `from` | As `to` |
|----|-----------|---------|
| `system` | Local clipboard change | Write to local clipboard |
| `http` | Message received via HTTP | — |

- All rules are evaluated; destinations are deduplicated automatically
- `center` only applies to MQTT targets; HTTP targets always send full content
- MQTT connections to the same broker+credentials are automatically shared

## Message Format

Sync messages are JSON:

```json
{
    "time": 1757312216.368,
    "uuid": "2ed24be9-953d-47c4-91c5-ec38559bb848",
    "deviceName": "my-device",
    "mime": "text/plain",
    "type": "text",
    "content": "SGVsbG8gV29ybGQ=",
    "sendTime": 1757312216.575
}
```

V2 relay messages have `type` of `text-v2` or `image-v2`, and `content` is a reference string:

```
deviceName/msgId,centerId:home,sha1:abc123def456,length:12345,encoding:base64
```

The `encoding` field indicates how the center stores the content: `base64` (default) or `raw`.

### V2 Relay Protocol

**Sender** (when center is configured and content is an image or text > 10 KB):
1. PUT data to center: `PUT {centerURL}/client/{deviceName}/{msgId}`
2. Send MQTT reference message with type `text-v2`/`image-v2`

**Receiver**:
1. Parse reference to get clientId/msgId and centerId
2. GET actual content: `GET {centerURL}/client/{clientId}/{msgId}`
3. Set local clipboard

## Project Structure

```
.
├── main.go              # Entry point, sub-command handling
├── config.go            # YAML config loading and validation
├── type.go              # Data structures and V2 protocol
├── forward_engine.go    # Forward rules engine
├── mqtt_client.go       # MQTT connection pool and subscriptions
├── http_client.go       # HTTP client
├── relay.go             # V2 relay logic (PUT/GET center)
├── server.go            # Local HTTP receive server
├── sync.go              # Sync helper functions
├── network.go           # Network interface monitoring
├── helper.go            # Utility functions
├── clip_linux.go        # Linux clipboard implementation
├── clip_darwin.go       # macOS clipboard implementation
├── clip_windows.go      # Windows clipboard implementation
├── config-example.yaml  # Example configuration file
└── tests/
    └── integration/     # Integration tests
```

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
