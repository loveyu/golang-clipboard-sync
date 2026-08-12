# clipboard-sync

[English](README.md) | [中文](README_CN.md)

跨平台剪贴板同步工具，通过 YAML 配置文件灵活管理 MQTT、HTTP 多协议同步。

## 功能特性

- **YAML 配置**: 统一的配置文件管理所有参数，支持远程配置下载
- **多协议支持**: MQTT（发布/订阅）和 HTTP（推送）协议
- **跨平台**: Linux（X11/Wayland）、macOS、Windows
- **原生 Wayland**: 纯 Go data-control 单连接，无 CGO 或 `libwayland` 运行时依赖
- **可靠去重**: 5 秒原始内容去重、后台图片像素精确去重、内容绑定的回声抑制
- **异步转发**: MQTT/HTTP 按目标隔离串行发送，慢目标不阻塞剪贴板采集或其他目标
- **转发规则引擎**: 灵活的消息路由，支持多源多目标、自动去重
- **V2 中继协议**: 大文件通过 HTTP 中心服务器中继，MQTT 仅传输引用
- **证书支持**: 自定义 CA、mTLS 客户端证书
- **网络过滤**: 根据本地 IP 网段和网卡名称过滤
- **连接池**: MQTT 连接按 broker+credentials 共享复用
- **自动恢复**: 原生连接或命令监听器异常后指数退避重连

## 快速开始

### 环境要求

- Go 1.24+
- Linux: X11 需要 xclip/xsel；Wayland 原生 data-control 无外部依赖，不支持时自动回退到 wl-clipboard
- macOS: pbcopy/pbpaste、osascript（系统自带）
- Windows: 无特殊依赖

### 构建

```bash
go build -o clipboard-sync .
```

### 配置

创建 `config.yaml` 配置文件（参考 `config-example.yaml`）：

```yaml
device:
  name: "my-device"
  allowedInterfacePatterns: "eth*,enp*,wlp*"
  maxRuntime: 3600

clipboard:
  backend: auto
  dedupWindowMs: 5000
  readTimeoutMs: 5000
  imageReadDelayMs: 200
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

### 运行

```bash
# 启动服务（默认命令）
./clipboard-sync

# 指定配置文件
./clipboard-sync -config /path/to/config.yaml

# 远程下载配置
./clipboard-sync download-config

# 查看版本
./clipboard-sync version

# 测试模式（直接写入剪贴板并触发同步）
./clipboard-sync --received-write-text "Hello World"
./clipboard-sync --received-image-file /path/to/image.png
```

### 环境变量

| 变量 | 说明 |
|------|------|
| `CLIPBOARD_CONFIG_PATH` | 配置文件路径，默认 `config.yaml` |
| `CLIPBOARD_DEBUG` | 调试模式，设为 `1` 启用 |
| `CLIPBOARD_BACKEND` | 覆盖配置的后端：`auto`、`native` 或 `command` |
| `REMOTE_CONFIG_URL` | 远程配置下载 URL（用于 `download-config` 命令） |

## 配置详解

### device - 设备配置

```yaml
device:
  name: "my-notebook"                   # 设备名称，留空则使用主机名
  allowedInterfacePatterns: "eth*,enp*" # 网卡名称过滤（通配符）
  maxRuntime: 3600                      # 监听器最大运行时间(秒)
```

### clipboard - 采集与去重

```yaml
clipboard:
  backend: auto
  dedupWindowMs: 5000
  readTimeoutMs: 5000
  imageReadDelayMs: 200
  maxContentBytes: 134217728
  imagePixelDedup: true
```

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `backend` | `auto`、`native`、`command`；`native` 仅适用于支持 data-control 的 Wayland | `auto` |
| `dedupWindowMs` | 原始内容和图片像素去重窗口，范围 0～60000ms | `5000` |
| `readTimeoutMs` | 单次读取超时，范围 500～60000ms | `5000` |
| `imageReadDelayMs` | 图片读取错峰延迟，范围 0～2000ms；文本不延迟 | `200` |
| `maxContentBytes` | 单次内容上限，范围 1 MiB～1 GiB | `134217728` |
| `imagePixelDedup` | 对原始编码不同的图片进行像素精确去重 | `true` |

`auto` 在 Linux Wayland 上优先选择 `ext_data_control_manager_v1`，其次选择
`zwlr_data_control_manager_v1`；初始化不可用时回退命令后端。运行中的原生连接断开会按
1、2、4、8、16、30、60 秒退避重连。可临时设置 `CLIPBOARD_BACKEND=command` 一键回滚。
原生后端会在读取图片前等待 `imageReadDelayMs`，使截图工具和 CopyQ 先完成关键路径；
同步写入附带来源标记，匹配的本地回声无需传输和解码图片。完整原始内容 SHA-256
仍在采集后立即计算；只有编码不同且格式、尺寸相同的候选图片才会在有界后台队列中解码并做
像素精确比较，不阻塞后续剪贴板读取。

常驻模式下，消息构建和 Base64 编码也在后台执行。每个 MQTT/HTTP 目标拥有独立的串行发送器，
最多保留一条正在处理和一条最新待处理消息；目标变慢时会合并尚未开始的旧消息，避免大图形成
无界内存积压。退出时先停止接收，再限时排空消息构建及各目标队列，超时则取消在途网络请求。
`--received-write-text` 和 `--received-image-file` 测试模式仍同步等待发送完成。

### http - 本地 HTTP 服务器

```yaml
http:
  port: ":9144"   # 监听端口
```

本地 HTTP 服务器接收 `/update-clipboard` 端点的消息，接收到的消息以 `"http"` 作为来源 ID 进入转发规则。

### certificates - 证书配置

```yaml
certificates:
  - id: "home-ca"
    ca: "/path/to/ca.crt"               # CA 证书
  - id: "mtls"
    ca: "/path/to/ca.crt"
    cert: "/path/to/client.crt"         # 客户端证书 (mTLS)
    key: "/path/to/client.key"          # 客户端密钥 (mTLS)
```

被 `centers` 和 MQTT DSN 的 `certificate` 参数引用。

### centers - 剪贴板中心

```yaml
centers:
  - id: "home"
    url: "http://10.0.0.1:8080"         # 中心 HTTP 地址
    token: "secure-token"               # Bearer token 认证
    textMsgId: "clipboard-text"         # 文本消息 ID
    imageMsgId: "clipboard-image"       # 图片消息 ID
    encoding: base64                    # 存储编码: base64(默认) 或 raw(原始内容)
    # certificate: "home-ca"            # HTTPS 时引用证书
```

中心服务器用于 V2 中继协议。当转发规则配置了 `center` 时，图片或超过 10KB 的文本会先 PUT 到中心，再通过 MQTT 发送引用。

- `encoding: base64`（默认）: 中心存储 Base64 编码的内容，Content-Type 保持原始类型
- `encoding: raw`: 中心存储原始内容，便于外部直接预览

### listen - MQTT 订阅入口

```yaml
listen:
  - id: "home-recv"
    dsn: "mqtt://user:pass@10.0.0.1:1883/clipboard/{$type}/+?maxMessageSize=5MB"
    allowInterfaceIps: "10.0.0.0/24"    # IP 网段过滤（可选）
```

DSN 格式: `mqtt[s]://[user:pass@]host:port/topic[?params]`

`{$type}` 在订阅时自动替换为 MQTT 通配符 `+`。

支持的参数:

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `maxMessageSize` | 最大消息大小（如 `5MB`、`512KB`） | 无限制 |
| `certificate` | 引用 certificates 中的 ID | 无 |
| `clientId` | MQTT 客户端 ID | 自动生成 |
| `connectTimeout` | 连接超时(秒) | 3 |
| `keepAliveInterval` | 心跳间隔(秒) | 60 |
| `automaticReconnect` | 自动重连 | true |
| `qos` | QoS 等级 (0/1/2) | 1 |
| `retain` | 保留消息 | true |

### targets - 发布目标

```yaml
targets:
  - id: "mqtt-pub"
    dsn: "mqtt://user:pass@10.0.0.1:1883/clipboard/{$type}/my-device"
    allowInterfaceIps: "10.0.0.0/24"
  - id: "http-relay"
    dsn: "http://10.0.0.1:9144/update-clipboard"
```

支持 MQTT 和 HTTP 两种协议。`{$type}` 在发布时替换为 `text` 或 `image`。

HTTP 目标额外参数:

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `types` | 允许的内容类型 | 全部 |
| `retries` | 失败重试次数 | 0 |
| `retryDelay` | 重试延迟(ms) | 50 |

### forward - 转发规则

```yaml
forward:
  - from: ["system"]
    to: ["mqtt-pub"]
    center: "home"        # 可选，启用 V2 中继

  - from: ["home-recv"]
    to: ["system"]
    center: "home"

  - from: ["http"]
    to: ["system", "mqtt-pub"]
```

**保留 ID**（不可用于 listen/target/center 的 id）:

| ID | 在 `from` 中 | 在 `to` 中 |
|------|------|------|
| `system` | 本地剪贴板变更 | 设置本地剪贴板 |
| `http` | 通过 HTTP 接收的消息 | — |

- 所有规则都会被匹配，目标自动去重
- `center` 仅对 MQTT 目标生效，HTTP 目标始终发送完整内容
- 同一 broker+credentials 的 MQTT 连接自动共享复用

## 消息格式

同步消息为 JSON 格式：

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

V2 中继消息的 `type` 为 `text-v2` 或 `image-v2`，`content` 为引用格式：

```
deviceName/msgId,centerId:home,sha1:abc123def456,length:12345,encoding:base64
```

`encoding` 字段说明中心存储的编码方式：`base64`（默认）或 `raw`（原始内容）。

### V2 中继协议

**发送端**（当 center 配置且内容为图片或文本 > 10KB）：
1. PUT 数据到中心: `PUT {centerURL}/client/{deviceName}/{msgId}`
2. 发送 MQTT 引用消息: type=`text-v2`/`image-v2`

**接收端**：
1. 解析引用获取 clientId/msgId 和 centerId
2. GET 实际内容: `GET {centerURL}/client/{clientId}/{msgId}`
3. 设置本地剪贴板

## 项目结构

```
.
├── main.go              # 主程序入口、子命令处理
├── config.go            # YAML 配置加载和校验
├── type.go              # 数据结构和 V2 协议
├── forward_engine.go    # 转发规则引擎
├── mqtt_client.go       # MQTT 连接池和订阅
├── http_client.go       # HTTP 客户端
├── relay.go             # V2 中继逻辑（PUT/GET 中心）
├── server.go            # 本地 HTTP 接收服务器
├── sync.go              # 同步辅助函数
├── network.go           # 网络接口监控
├── helper.go            # 工具函数
├── clip_linux.go        # Linux 剪贴板实现
├── clip_darwin.go       # macOS 剪贴板实现
├── clip_windows.go      # Windows 剪贴板实现
├── config-example.yaml  # 示例配置文件
└── tests/
    └── integration/     # 集成测试
```

## 许可证

Apache License 2.0 — 详见 [LICENSE](LICENSE)。
