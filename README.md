# clipboard-sync

跨平台剪贴板同步工具，支持通过 MQTT 和 HTTP 协议同步剪贴板内容。

## 功能特性

- **多协议支持**: 同时支持 MQTT 和 HTTP 协议同步
- **跨平台**: 支持 Linux 和 Windows 系统
- **内容类型**: 支持文本和图片（Base64 编码传输）
- **网络过滤**: 支持根据本地网卡 IP 网段和网卡名称进行过滤
- **类型过滤**: 支持按内容类型（text/image）过滤
- **连接池**: MQTT 连接复用，支持自动重连
- **重试机制**: MQTT 和 HTTP 客户端均支持失败重试
- **优雅停机**: 支持 SIGTERM/SIGINT 信号平滑退出
- **消息转发**: 支持将收到的消息转发到其他目标（中继模式）
- **配置灵活**: 支持环境变量和 .env 文件配置

## 快速开始

### 环境要求

- Go 1.19+
- Linux: xclip（可选）
- Windows: 无特殊依赖

### 构建

```bash
# Linux AMD64
./build_linux.sh

# Linux ARM64
./build_linux.sh arm64

# Windows（在 Linux 上交叉编译）
./build_windows.sh
```

### 配置

创建 `.env` 文件或设置环境变量：

```bash
# 设备名称（必需）
CLIPBOARD_DEVICE_NAME=22081212C

# MQTT 配置
CLIPBOARD_SYNC_URLS=mqtt://localhost:1883/linux-clipboard/{$type}?clientId=client1

# HTTP 配置
CLIPBOARD_SYNC_URLS=http://localhost:18884/update-clipboard

# 同时使用多个协议（分号分隔）
CLIPBOARD_SYNC_URLS=mqtt://host:1883/linux-clipboard/{$type};http://host:18884/update

# 类型过滤（可选，在URL中指定types参数）
CLIPBOARD_SYNC_URLS=mqtt://host:1883/linux-clipboard/{$type}?types=text,image;http://host:18884/update?types=text

# 调试模式
CLIPBOARD_DEBUG=1

# 消息转发（收到消息后转发到其他目标，格式与 CLIPBOARD_SYNC_URLS 一致）
CLIPBOARD_FORWARD_URLS=mqtt://other-host:1883/forward-topic/{$type};http://other-host:18884/update-clipboard

# 本地 HTTP 服务器端口（默认 :9144）
LOCAL_SERVER_PORT=:9144
```

## 消息格式

同步消息为 JSON 格式：

```json
{
    "time": 1757312216.368652,
    "uuid": "2ed24be9-953d-47c4-91c5-ec38559bb848",
    "deviceName": "22081212C",
    "mime": "text/plain",
    "type": "text",
    "content": "MjU2MTM0NTMzMw==",
    "sendTime": 1757312216.575504
}
```

| 字段 | 说明 |
|------|------|
| `time` | 本地时间戳（Unix seconds） |
| `uuid` | 消息唯一标识 |
| `deviceName` | 设备名称（来自 `CLIPBOARD_DEVICE_NAME`） |
| `mime` | MIME 类型 |
| `type` | 内容类型（text/image） |
| `content` | Base64 编码的内容 |
| `sendTime` | 发送时间（Unix seconds） |
| `forwardSource` | 转发来源设备名称（仅转发消息存在） |

### 运行

```bash
# 正常模式（监听剪贴板变化并同步）
./clipboard-sync

# 指定配置文件
./clipboard-sync -env-file /path/to/config.env

# 测试模式（直接写入剪贴板并触发同步）
./clipboard-sync --received-write-text "Hello World"
./clipboard-sync --received-image-file /path/to/image.png
```

## URL 格式

### MQTT URL

```
mqtt[s]://[username:[password]@]host[:port]/topic[?query_params]
```

**Query 参数:**

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `clientId` | MQTT 客户端 ID | `clipboard-sync-{hash}` |
| `connectTimeout` | 连接超时(秒) | 3 |
| `keepAliveInterval` | 心跳间隔(秒) | 60 |
| `automaticReconnect` | 自动重连 | true |
| `reconnectInterval` | 重连间隔(秒) | 5 |
| `qos` | QoS等级(0或1) | 1 |
| `retain` | 是否保留消息(retain) | true |
| `types` | 允许的内容类型(逗号分隔) | (空=允许全部) |
| `allowInterfaceIps` | 允许的本地网卡IP段 | (空=允许全部) |
| `denyInterfaceIps` | 排除的本地网卡IP段 | (空=不排除) |
| `retries` | 发布失败重试次数 | 1 |
| `retryDelay` | 重试延迟时间(毫秒) | 50 |

**Topic 占位符 `{$type}`**:
- `text/plain` → `text`
- `image/png` → `image`

示例: `mqtt://host:1883/linux-clipboard/{$type}` → `linux-clipboard/text` 或 `linux-clipboard/image`

### HTTP URL

直接使用标准 HTTP POST URL，JSON 格式发送。

**Query 参数:**

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `types` | 允许的内容类型(逗号分隔) | (空=允许全部) |
| `allowInterfaceIps` | 允许的本地网卡IP段 | (空=允许全部) |
| `denyInterfaceIps` | 排除的本地网卡IP段 | (空=不排除) |
| `retries` | 请求失败重试次数 | 0（不重试） |
| `retryDelay` | 重试延迟时间(毫秒) | 50 |

## 网络接口过滤

### IP 网段过滤

支持通过 `allowInterfaceIps` 和 `denyInterfaceIps` 参数限制只在特定网段下同步：

```bash
# 只在本地IP符合 192.168.1.0/24 网段时启用
mqtt://host:1883/topic?allowInterfaceIps=192.168.1.0/24

# 只同步文本到 MQTT，图片和文本都同步到 HTTP
mqtt://host:1883/topic?types=text;http://host:18884/update?types=text,image

# 排除 Docker 网段
mqtt://host:1883/topic?denyInterfaceIps=172.17.0.0/16

# allow 和 deny 同时使用（deny 优先）
mqtt://host:1883/topic?allowInterfaceIps=10.0.0.0/8&denyInterfaceIps=10.0.0.0/24
```

### 网卡名称过滤

支持通过环境变量 `CLIPBOARD_ALLOWED_INTERFACE_PATTERNS` 限制只使用特定网卡（支持通配符 `*`）：

```bash
# Linux 默认
CLIPBOARD_ALLOWED_INTERFACE_PATTERNS=eth*,enp*,wlp*

# Windows 默认
CLIPBOARD_ALLOWED_INTERFACE_PATTERNS=以太网*,Wi-Fi*,本地连接*,WLAN*,Ethernet*

# 自定义（所有平台通用）
CLIPBOARD_ALLOWED_INTERFACE_PATTERNS=eth*,enp*,wlp*,以太网*
```

## 重试机制

MQTT 和 HTTP 客户端均支持失败重试：

```bash
# MQTT：发布失败重试3次，每次间隔100ms
mqtt://host:1883/topic?retries=3&retryDelay=100

# HTTP：请求失败重试2次，每次间隔200ms
http://host:18884/update?retries=2&retryDelay=200
```

- **MQTT 默认**: 重试1次，间隔50ms
- **HTTP 默认**: 不重试

## 优雅停机

程序支持接收 SIGTERM/SIGINT 信号平滑退出：

```bash
# 启动后按 Ctrl+C 或 kill 会优雅关闭所有 MQTT 连接
./clipboard-sync
```

退出时会自动关闭所有 MQTT 连接，确保消息发送完成。

## 项目结构

```
.
├── main.go           # 主程序入口，剪贴板变化处理
├── type.go           # 数据结构定义
├── sync.go           # 同步逻辑（MQTT/HTTP）
├── mqtt_client.go     # MQTT 客户端和连接池
├── http_client.go     # HTTP 客户端（含重试机制）
├── network.go        # 网络接口监控
├── server.go         # 本地 HTTP 服务器
├── helper.go         # 辅助函数
├── clip_linux.go     # Linux 剪贴板实现
├── clip_windows.go   # Windows 剪贴板实现
├── build_linux.sh    # Linux 构建脚本
├── build_windows.sh   # Windows 交叉编译脚本
├── test_mqtt.sh      # MQTT 测试脚本
└── docs/
    └── testing.md    # 测试指南
```

## License

Apache License 2.0 - see [LICENSE](LICENSE) file for details.
