#!/bin/bash
# MQTT/HTTP 同步功能测试脚本

set -e

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 配置
MQTT_PORT=${MQTT_PORT:-18883}
HTTP_PORT=${HTTP_PORT:-18884}
MQTT_BROKER="localhost:${MQTT_PORT}"
HTTP_SERVER="localhost:${HTTP_PORT}"

# 确保容器运行
ensure_mqtt() {
    if docker ps --format '{{.Names}}' | grep -q mqtt-broker; then
        docker start mqtt-broker 2>/dev/null || true
    else
        docker run -d --name mqtt-broker -p ${MQTT_PORT}:1883 eclipse-mosquitto:2 mosquitto -c /mosquitto-no-auth.conf
    fi
    sleep 1
}

# 启动 HTTP 测试服务器
start_http_server() {
    cat > /tmp/http_server.py << 'EOFSCRIPT'
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

server = HTTPServer(('0.0.0.0', PORT), Handler)
print(f"[HTTP] Server started on :{PORT}")
server.serve_forever()
EOFSCRIPT

    # 使用 sed 替换端口
    sed -i "s/PORT/${HTTP_PORT}/" /tmp/http_server.py
    python3 /tmp/http_server.py &
    HTTP_PID=$!
    sleep 1
    echo $HTTP_PID
}

# 停止 HTTP 服务器
stop_http_server() {
    if [ -n "$HTTP_PID" ]; then
        kill $HTTP_PID 2>/dev/null || true
    fi
}

# 清理
cleanup() {
    echo -e "${YELLOW}清理资源...${NC}"
    stop_http_server
    docker stop mqtt-broker 2>/dev/null || true
}

trap cleanup EXIT

# 测试函数
test_single_mqtt() {
    echo -e "\n${YELLOW}=== 测试 1: MQTT 单协议文本同步 ===${NC}"
    ensure_mqtt
    docker exec mqtt-broker mosquitto_sub -t "linux-clipboard/#" -v &
    MOSQ_PID=$!
    sleep 1

    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-single" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "Hello MQTT"

    sleep 2
    kill $MOSQ_PID 2>/dev/null || true
    echo -e "${GREEN}✓ MQTT 文本同步测试完成${NC}"
}

test_single_mqtt_image() {
    echo -e "\n${YELLOW}=== 测试 2: MQTT 单协议图片同步 ===${NC}"
    ensure_mqtt
    docker exec mqtt-broker mosquitto_sub -t "linux-clipboard/#" -v &
    MOSQ_PID=$!
    sleep 1

    echo "fake-png" > /tmp/test_image.png
    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-single" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-image-file /tmp/test_image.png

    sleep 2
    kill $MOSQ_PID 2>/dev/null || true
    echo -e "${GREEN}✓ MQTT 图片同步测试完成${NC}"
}

test_http_only() {
    echo -e "\n${YELLOW}=== 测试 3: HTTP 单协议同步 ===${NC}"
    HTTP_PID=$(start_http_server)

    CLIPBOARD_SYNC_URLS="http://${HTTP_SERVER}/update-clipboard" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "Hello HTTP"

    sleep 2
    stop_http_server
    echo -e "${GREEN}✓ HTTP 同步测试完成${NC}"
}

test_mqtt_http_combined() {
    echo -e "\n${YELLOW}=== 测试 4: MQTT + HTTP 同时同步 ===${NC}"
    ensure_mqtt
    HTTP_PID=$(start_http_server)

    docker exec mqtt-broker mosquitto_sub -t "linux-clipboard/#" -v &
    MOSQ_PID=$!
    sleep 1

    echo -e "${YELLOW}发送文本 (应同时到达 MQTT 和 HTTP)...${NC}"
    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-multi;http://${HTTP_SERVER}/update-clipboard" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "Multi-protocol text"

    sleep 2

    echo -e "${YELLOW}发送图片 (应同时到达 MQTT 和 HTTP)...${NC}"
    echo "fake-png" > /tmp/test_image.png
    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-multi;http://${HTTP_SERVER}/update-clipboard" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-image-file /tmp/test_image.png

    sleep 2

    kill $MOSQ_PID 2>/dev/null || true
    stop_http_server
    echo -e "${GREEN}✓ MQTT + HTTP 同时同步测试完成${NC}"
}

test_reconnection() {
    echo -e "\n${YELLOW}=== 测试 5: MQTT 重连功能 ===${NC}"
    ensure_mqtt
    docker exec mqtt-broker mosquitto_sub -t "linux-clipboard/#" -v &
    MOSQ_PID=$!
    sleep 1

    # 正常连接
    echo -e "${YELLOW}Step 1: 正常连接...${NC}"
    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-reconnect&connectTimeout=5" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "Before disconnect"
    sleep 2

    # 停止 broker
    echo -e "${YELLOW}Step 2: 停止 MQTT broker...${NC}"
    docker stop mqtt-broker
    sleep 1

    # 尝试发送 (应失败)
    echo -e "${YELLOW}Step 3: Broker 停止时发送 (应超时)...${NC}"
    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-reconnect&connectTimeout=5" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "During disconnect" || true
    sleep 3

    # 重启 broker
    echo -e "${YELLOW}Step 4: 重启 MQTT broker...${NC}"
    docker start mqtt-broker
    sleep 3

    # 发送 (应自动重连)
    echo -e "${YELLOW}Step 5: Broker 重启后发送 (应自动重连)...${NC}"
    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-reconnect&connectTimeout=5" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "After reconnect"

    sleep 2
    kill $MOSQ_PID 2>/dev/null || true
    echo -e "${GREEN}✓ MQTT 重连测试完成${NC}"
}

test_type_filter() {
    echo -e "\n${YELLOW}=== 测试 6: 类型过滤 ===${NC}"
    ensure_mqtt
    docker exec mqtt-broker mosquitto_sub -t "linux-clipboard/#" -v &
    MOSQ_PID=$!
    sleep 1

    # 只允许 text 类型
    echo -e "${YELLOW}MQTT 只允许 text 类型，发送图片...${NC}"
    echo "fake-png" > /tmp/test_image.png
    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-filter&types=text" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-image-file /tmp/test_image.png

    sleep 1
    echo -e "${YELLOW}MQTT 只允许 text 类型，发送文本...${NC}"
    CLIPBOARD_SYNC_URLS="mqtt://${MQTT_BROKER}/linux-clipboard/{\$type}?clientId=test-filter&types=text" \
        CLIPBOARD_DEBUG=1 ./clipboard-sync --received-write-text "Filtered text"

    sleep 2
    kill $MOSQ_PID 2>/dev/null || true
    echo -e "${GREEN}✓ 类型过滤测试完成${NC}"
}

# 主函数
main() {
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}  Clipboard Sync MQTT/HTTP 测试${NC}"
    echo -e "${GREEN}========================================${NC}"

    # 检查编译
    if [ ! -f "./clipboard-sync" ]; then
        echo -e "${YELLOW}编译项目...${NC}"
        go build -o clipboard-sync .
    fi

    echo -e "\n${YELLOW}前置条件:${NC}"
    echo "  - Docker (用于运行 Mosquitto MQTT broker)"
    echo "  - Go 1.19+"
    echo ""

    # 运行测试
    test_single_mqtt
    test_single_mqtt_image
    test_http_only
    test_mqtt_http_combined
    test_reconnection
    test_type_filter

    echo -e "\n${GREEN}========================================${NC}"
    echo -e "${GREEN}  所有测试完成!${NC}"
    echo -e "${GREEN}========================================${NC}"
}

main "$@"
