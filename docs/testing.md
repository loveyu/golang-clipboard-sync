# 测试指南

## 前置条件

- Docker (用于运行 Mosquitto MQTT broker)
- Go 1.19+
- xclip (Linux 下可选，用于剪贴板写入)

## 快速测试

### 自动测试脚本

```bash
# 给予执行权限
chmod +x test_mqtt.sh

# 运行所有测试
./test_mqtt.sh
```

### 手动测试

#### 1. 启动 MQTT Broker

```bash
# 使用 Docker 运行 Mosquitto
docker run -d --name mqtt-broker -p 1883:1883 eclipse-mosquitto:2 mosquitto -c /mosquitto-no-auth.conf
```

#### 2. 启动 HTTP 测试服务器

```python
# 创建测试服务器
cat > /tmp/test_http_server.py << 'EOF'
from http.server import HTTPServer, BaseHTTPRequestHandler

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        content_length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(content_length)
        print(f"[HTTP] Received: {self.path}")
        print(f"[HTTP] Body: {body.decode()[:200]}...")
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(b'{"status":"ok"}')

    def log_message(self, format, *args):
        pass

server = HTTPServer(('0.0.0.0', 18884), Handler)
print("[HTTP] Server started on :18884")
server.serve_forever()
EOF

python3 /tmp/test_http_server.py &
```

#### 3. 监听 MQTT Topic

```bash
docker exec mqtt-broker mosquitto_sub -t "linux-clipboard/#" -v
```

#### 4. 测试命令

```bash
# 确保已编译
go build -o clipboard-sync .

# MQTT 文本同步
CLIPBOARD_SYNC_URLS='mqtt://localhost:1883/linux-clipboard/{$type}?clientId=test' \
    CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "Hello MQTT"

# MQTT 图片同步
echo "fake-png" > /tmp/test.png
CLIPBOARD_SYNC_URLS='mqtt://localhost:1883/linux-clipboard/{$type}?clientId=test' \
    CLIPBOARD_DEBUG=1 ./clipboard-sync --received-image-file /tmp/test.png

# HTTP 同步
CLIPBOARD_SYNC_URLS='http://localhost:18884/update-clipboard' \
    CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "Hello HTTP"

# MQTT + HTTP 同时同步
CLIPBOARD_SYNC_URLS='mqtt://localhost:1883/linux-clipboard/{$type}?clientId=test;http://localhost:18884/update-clipboard' \
    CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "Hello Both"
```

## 测试场景

### 1. MQTT 单协议文本同步

```bash
docker exec mqtt-broker mosquitto_sub -t "linux-clipboard/#" -v &
./clipboard-sync --received-write-text "Hello"
# 期望: 消息发送到 linux-clipboard/text topic
```

### 2. MQTT 图片同步 (带 {$type} 占位符)

```bash
./clipboard-sync --received-image-file /path/to/image.png
# 期望: 消息发送到 linux-clipboard/image topic (根据 mime 类型解析)
```

### 3. HTTP 同步

```bash
# 启动本地 HTTP 服务器后
./clipboard-sync --received-write-text "Hello HTTP"
# 期望: POST 请求发送到 /update-clipboard
```

### 4. MQTT + HTTP 同时同步

```bash
CLIPBOARD_SYNC_URLS='mqtt://host:1883/linux-clipboard/{$type}?clientId=multi;http://host:18884/update-clipboard' \
    ./clipboard-sync --received-write-text "Multi-protocol"
# 期望: 同时发送到 MQTT topic 和 HTTP endpoint
```

### 5. MQTT 重连功能

```bash
# 正常发送
./clipboard-sync --received-write-text "Before"

# 停止 broker
docker stop mqtt-broker

# 尝试发送 (应超时)
./clipboard-sync --received-write-text "During" || echo "Failed as expected"

# 重启 broker
docker start mqtt-broker

# 发送 (应自动重连成功)
./clipboard-sync --received-write-text "After"
```

### 6. 类型过滤

```bash
# 只允许 text 类型，图片会被跳过
CLIPBOARD_SYNC_URLS='mqtt://host:1883/topic?types=text' \
    ./clipboard-sync --received-image-file /path/to/image.png
# 期望: DEBUG 日志显示 "类型过滤跳过"

# HTTP 类型过滤（只允许 image）
CLIPBOARD_SYNC_URLS='http://host:18884/update?types=image' \
    ./clipboard-sync --received-write-text "Text message"
# 期望: 文本消息被过滤跳过
```

## URL 参数说明

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
| `reconnectMaxInterval` | 最大重连间隔(秒) | 60 |
| `allowInterfaceIps` | 允许的本地网卡IP段 | (空=允许全部) |
| `denyInterfaceIps` | 排除的本地网卡IP段 | (空=不排除) |
| `types` | 允许的内容类型(逗号分隔) | (空=允许全部) |

**Topic 占位符:**

`{$type}` 会根据内容 mime 类型解析:
- `text/plain` → `text`
- `image/png` → `image`

示例: `mqtt://host:1883/linux-clipboard/{$type}` → `linux-clipboard/text` 或 `linux-clipboard/image`

**allowInterfaceIps / denyInterfaceIps 示例:**

```bash
# 只在本地IP符合 192.168.1.0/24 网段时启用
mqtt://host:1883/topic?allowInterfaceIps=192.168.1.0/24

# 只在本地IP符合 10.0.0.0/8 或 192.168.0.0/16 时启用
mqtt://host:1883/topic?allowInterfaceIps=10.0.0.0/8,192.168.0.0/16

# 只在本地IP为 192.168.1.100 时启用
mqtt://host:1883/topic?allowInterfaceIps=192.168.1.100

# 排除 Docker 网段
mqtt://host:1883/topic?denyInterfaceIps=172.17.0.0/16

# allow 和 deny 同时使用 (deny 优先)
mqtt://host:1883/topic?allowInterfaceIps=10.0.0.0/8&denyInterfaceIps=10.0.0.0/24
```

**网卡检查间隔:**
- 有消息发送时: 5秒检查一次
- 空闲时 (1分钟无消息): 30秒检查一次
- 未配置 allowInterfaceIps 和 denyInterfaceIps: 跳过检查

### 环境变量

| 变量 | 说明 |
|------|------|
| `CLIPBOARD_SYNC_URLS` | 同步目标 URL (分号分隔) |
| `CLIPBOARD_DEBUG` | 开启调试日志 (`=1`) |

### .env 文件支持

支持通过 `.env` 文件配置环境变量，优先查找：
1. 当前目录的 `.env` 或 `.env.local`
2. 可执行文件所在目录的 `.env` 或 `.env.local`
3. 通过 `-env-file` 参数指定的自定义路径

```bash
# 示例 .env 文件
CLIPBOARD_SYNC_URLS=mqtt://host:1883/linux-clipboard/{$type}?clientId=test&types=text,image
CLIPBOARD_DEBUG=1

# 使用 -env-file 参数指定配置文件
./clipboard-sync -env-file /path/to/config.env
```

## 清理

```bash
# 停止并删除 MQTT broker
docker stop mqtt-broker && docker rm mqtt-broker

# 停止 HTTP 服务器
pkill -f test_http_server.py

# 停止 MQTT 监听
pkill -f mosquitto_sub
```
