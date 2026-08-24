#!/usr/bin/env bash
# Integration test runner for clipboard-sync
# Requires: go, docker, python3, mosquitto-clients, curl, jq

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
BUILD_DIR="${SCRIPT_DIR}/build"
CONFIG_DIR="${SCRIPT_DIR}/configs"
RESULT_DIR="${SCRIPT_DIR}/results"

MQTT_PORT=1883
CENTER_PORT=9999
CAPTURE_PORT=9998
APP_PORT=9144
DEVICE_NAME="test-device"
TEST_TOKEN="test-integration-token"

# Clipboard environment: "x11" (default), "wayland", "darwin", or "windows"
CLIPBOARD_ENV="${CLIPBOARD_ENV:-x11}"

# Binary extension (Windows Git Bash needs .exe)
case "$(uname -s 2>/dev/null)" in
    MINGW*|CYGWIN*|MSYS*) EXE_EXT=".exe" ;;
    *) EXE_EXT="" ;;
esac
BINARY="${BUILD_DIR}/clipboard-sync${EXE_EXT}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass=0
fail=0
skip=0

log_pass() { echo -e "  ${GREEN}[PASS]${NC} $1"; ((pass++)); }
log_fail() { echo -e "  ${RED}[FAIL]${NC} $1"; ((fail++)); }
log_skip() { echo -e "  ${YELLOW}[SKIP]${NC} $1"; ((skip++)); }
log_info() { echo -e "         $1"; }

MQTT_PID=""

cleanup() {
    jobs -p 2>/dev/null | xargs kill 2>/dev/null || true
    docker rm -f mosquitto-test 2>/dev/null || true
    [ -n "${MQTT_PID}" ] && kill "${MQTT_PID}" 2>/dev/null || true
}
trap cleanup EXIT

# ======================== Setup ========================

echo "=== Setting up test environment ==="

mkdir -p "${BUILD_DIR}" "${CONFIG_DIR}" "${RESULT_DIR}"

# Build
echo "Building..."
cd "${PROJECT_DIR}"
CGO_ENABLED=0 go build -o "${BINARY}" . || { echo "Build failed"; exit 1; }

# Generate test image (small PNG)
python3 -c "
import struct, zlib, sys
def create_png(w, h):
    def chunk(t, d):
        c = t + d
        return struct.pack('>I', len(d)) + c + struct.pack('>I', zlib.crc32(c) & 0xffffffff)
    raw = b''
    for y in range(h):
        raw += b'\x00' + bytes([x % 256 for x in range(w * 3)])
    return b'\x89PNG\r\n\x1a\n' + \
        chunk(b'IHDR', struct.pack('>IIBBBBB', w, h, 8, 2, 0, 0, 0)) + \
        chunk(b'IDAT', zlib.compress(raw)) + chunk(b'IEND', b'')
sys.stdout.buffer.write(create_png(64, 64))
" > "${RESULT_DIR}/test_image.png"
log_info "Test image: $(wc -c < "${RESULT_DIR}/test_image.png") bytes"

# Start Mosquitto
echo "Starting Mosquitto..."
mkdir -p /tmp/mosquitto-test
cat > /tmp/mosquitto-test/mosquitto.conf <<EOF
listener ${MQTT_PORT}
allow_anonymous true
EOF
if [ "$CLIPBOARD_ENV" != "windows" ] && command -v docker >/dev/null 2>&1; then
    docker rm -f mosquitto-test 2>/dev/null || true
    docker run -d --name mosquitto-test -p ${MQTT_PORT}:1883 \
        -v /tmp/mosquitto-test/mosquitto.conf:/mosquitto/config/mosquitto.conf:ro \
        eclipse-mosquitto:2
    for i in $(seq 1 20); do
        if docker exec mosquitto-test mosquitto_pub -h localhost -p 1883 -t "test" -m "ping" 2>/dev/null; then
            break
        fi
        sleep 0.5
    done
else
    mosquitto -c /tmp/mosquitto-test/mosquitto.conf &
    MQTT_PID=$!
    for i in $(seq 1 20); do
        if mosquitto_pub -h localhost -p ${MQTT_PORT} -t "test" -m "ping" 2>/dev/null; then
            break
        fi
        sleep 0.5
    done
fi

# Install mosquitto-clients if needed
if ! command -v mosquitto_pub >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq mosquitto-clients 2>/dev/null || true
fi

# Start mock servers
echo "Starting mock servers..."
python3 "${SCRIPT_DIR}/mock_server.py" ${CENTER_PORT} ${CAPTURE_PORT} &
MOCK_PID=$!
sleep 1
for i in $(seq 1 10); do
    if curl -sf http://localhost:${CENTER_PORT}/_stats >/dev/null 2>&1; then
        break
    fi
    sleep 0.5
done

echo ""
echo "=== Running integration tests ==="
echo ""

# ======================== Helpers ========================

# Start MQTT subscriber, return PID
start_subscriber() {
    local topic="$1"
    local outfile="$2"
    local timeout="${3:-15}"

    mosquitto_sub -h localhost -p ${MQTT_PORT} -t "${topic}" -C 1 -W ${timeout} > "${outfile}" 2>/dev/null &
    local pid=$!
    sleep 1  # Wait for connection
    echo $pid
}

# Reset mock server state
reset_mock() {
    kill ${MOCK_PID} 2>/dev/null || true
    wait ${MOCK_PID} 2>/dev/null || true
    python3 "${SCRIPT_DIR}/mock_server.py" ${CENTER_PORT} ${CAPTURE_PORT} &
    MOCK_PID=$!
    for i in $(seq 1 20); do
        if curl -sf http://localhost:${CENTER_PORT}/_stats >/dev/null 2>&1; then
            break
        fi
        sleep 0.5
    done
}

# ======================== Clipboard env helpers ========================

# Returns 0 if the required clipboard tools for the current env are present.
check_clipboard_env() {
    if [ "$CLIPBOARD_ENV" = "wayland" ]; then
        command -v wl-paste >/dev/null 2>&1 && command -v wl-copy >/dev/null 2>&1
    elif [ "$CLIPBOARD_ENV" = "darwin" ]; then
        command -v pbcopy >/dev/null 2>&1 && command -v pbpaste >/dev/null 2>&1
    elif [ "$CLIPBOARD_ENV" = "windows" ]; then
        return 0
    else
        command -v xvfb-run >/dev/null 2>&1 && command -v xclip >/dev/null 2>&1
    fi
}

# Run a command with the appropriate clipboard environment wrapper.
# Wayland/darwin: runs directly (compositor or native clipboard already available).
# X11: wraps with xvfb-run to provide a virtual display.
run_with_clipboard() {
    if [ "$CLIPBOARD_ENV" = "wayland" ] || [ "$CLIPBOARD_ENV" = "darwin" ] || [ "$CLIPBOARD_ENV" = "windows" ]; then
        "$@"
    else
        xvfb-run -a "$@"
    fi
}

# ======================== Tests ========================

# Test 1: Text V1 via MQTT
test_text_v1_mqtt() {
    echo "--- Test 1: Text V1 via MQTT ---"
    local config="${CONFIG_DIR}/text_v1.yaml"
    local outfile="${RESULT_DIR}/text_v1.json"

    cat > "$config" <<EOF
device:
  name: "${DEVICE_NAME}"
debug: true
targets:
  - id: "mqtt-target"
    dsn: "mqtt://localhost:${MQTT_PORT}/test/text-v1?retain=false&clientId=t1"
forward:
  - from: ["system"]
    to: ["mqtt-target"]
EOF

    local sub_pid=$(start_subscriber "test/text-v1" "${outfile}")
    "${BINARY}" -config "$config" --received-write-text "Hello V1" >/dev/null 2>&1

    wait $sub_pid 2>/dev/null || true

    if [ -s "${outfile}" ]; then
        local type=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('type',''))" "${outfile}")
        local content=$(python3 -c "import json,base64,sys; d=json.load(open(sys.argv[1])); print(base64.b64decode(d['content']).decode())" "${outfile}")
        if [ "$type" = "text" ] && [ "$content" = "Hello V1" ]; then
            log_pass "type=$type content=$content"
        else
            log_fail "expected type=text content='Hello V1', got type=$type content=$content"
        fi
    else
        log_fail "no MQTT message received"
    fi
}

# Test 2: Image V1 via MQTT
test_image_v1_mqtt() {
    echo "--- Test 2: Image V1 via MQTT ---"
    local config="${CONFIG_DIR}/image_v1.yaml"
    local outfile="${RESULT_DIR}/image_v1.json"

    cat > "$config" <<EOF
device:
  name: "${DEVICE_NAME}"
debug: true
targets:
  - id: "mqtt-target"
    dsn: "mqtt://localhost:${MQTT_PORT}/test/image-v1?retain=false&clientId=t2"
forward:
  - from: ["system"]
    to: ["mqtt-target"]
EOF

    local sub_pid=$(start_subscriber "test/image-v1" "${outfile}")
    "${BINARY}" -config "$config" --received-image-file "${RESULT_DIR}/test_image.png" >/dev/null 2>&1

    wait $sub_pid 2>/dev/null || true

    if [ -s "${outfile}" ]; then
        local type=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('type',''))" "${outfile}")
        local mime=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('mime',''))" "${outfile}")
        if [ "$type" = "image" ] && [ "$mime" = "image/png" ]; then
            log_pass "type=$type mime=$mime"
        else
            log_fail "expected type=image mime=image/png, got type=$type mime=$mime"
        fi
    else
        log_fail "no MQTT message received"
    fi
}

# Test 3: Text V2 via MQTT + Center
test_text_v2_mqtt() {
    echo "--- Test 3: Text V2 via MQTT + Center ---"
    reset_mock

    local config="${CONFIG_DIR}/text_v2.yaml"
    local outfile="${RESULT_DIR}/text_v2.json"

    local big_text=$(python3 -c "print('A' * 15000)")

    cat > "$config" <<EOF
device:
  name: "${DEVICE_NAME}"
debug: true
centers:
  - id: "test-center"
    url: "http://localhost:${CENTER_PORT}"
    token: "${TEST_TOKEN}"
    textMsgId: "clipboard-text"
    imageMsgId: "clipboard-image"
targets:
  - id: "mqtt-target"
    dsn: "mqtt://localhost:${MQTT_PORT}/test/text-v2?retain=false&clientId=t3"
forward:
  - from: ["system"]
    to: ["mqtt-target"]
    center: "test-center"
EOF

    local sub_pid=$(start_subscriber "test/text-v2" "${outfile}")
    "${BINARY}" -config "$config" --received-write-text "$big_text" >/dev/null 2>&1

    wait $sub_pid 2>/dev/null || true

    if [ -s "${outfile}" ]; then
        local type=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('type',''))" "${outfile}")
        local content=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('content',''))" "${outfile}")

        if [ "$type" != "text-v2" ]; then
            log_fail "expected type=text-v2, got type=$type"
            return
        fi

        # Verify V2 format: clientId/msgId,centerId:xxx,sha1:xxx,length:nnnn,encoding:base64
        if echo "$content" | grep -qE "^${DEVICE_NAME}/clipboard-text,centerId:test-center,sha1:[a-f0-9]+,length:[0-9]+,encoding:base64$"; then
            log_pass "type=$type format OK"
        else
            log_fail "invalid V2 format: $content"
        fi

        # Verify center PUT
        local stats=$(curl -sf http://localhost:${CENTER_PORT}/_stats)
        local put_ok=$(python3 -c "import json,sys; d=json.loads('''$stats'''); print('ok' if any(p['key']=='${DEVICE_NAME}/clipboard-text' for p in d['puts']) else 'fail')")
        if [ "$put_ok" = "ok" ]; then
            log_info "Center PUT verified"
        else
            log_fail "Center PUT not found"
        fi

        # Verify center content (should be base64 since encoding=base64)
        local center_data=$(curl -sf -H "Authorization: Bearer ${TEST_TOKEN}" "http://localhost:${CENTER_PORT}/client/${DEVICE_NAME}/clipboard-text")
        if [ -n "$center_data" ]; then
            log_info "Center content: $(echo -n "$center_data" | wc -c) bytes"
        else
            log_fail "Center content empty"
        fi
    else
        log_fail "no MQTT message received"
    fi
}

# Test 4: Image V2 via MQTT + Center
test_image_v2_mqtt() {
    echo "--- Test 4: Image V2 via MQTT + Center ---"
    reset_mock

    local config="${CONFIG_DIR}/image_v2.yaml"
    local outfile="${RESULT_DIR}/image_v2.json"

    cat > "$config" <<EOF
device:
  name: "${DEVICE_NAME}"
debug: true
centers:
  - id: "test-center"
    url: "http://localhost:${CENTER_PORT}"
    token: "${TEST_TOKEN}"
    textMsgId: "clipboard-text"
    imageMsgId: "clipboard-image"
targets:
  - id: "mqtt-target"
    dsn: "mqtt://localhost:${MQTT_PORT}/test/image-v2?retain=false&clientId=t4"
forward:
  - from: ["system"]
    to: ["mqtt-target"]
    center: "test-center"
EOF

    local sub_pid=$(start_subscriber "test/image-v2" "${outfile}")
    "${BINARY}" -config "$config" --received-image-file "${RESULT_DIR}/test_image.png" >/dev/null 2>&1

    wait $sub_pid 2>/dev/null || true

    if [ -s "${outfile}" ]; then
        local type=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('type',''))" "${outfile}")
        local content=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('content',''))" "${outfile}")

        if [ "$type" != "image-v2" ]; then
            log_fail "expected type=image-v2, got type=$type"
            return
        fi

        if echo "$content" | grep -qE "^${DEVICE_NAME}/clipboard-image,centerId:test-center,sha1:[a-f0-9]+,length:[0-9]+,encoding:base64$"; then
            log_pass "type=$type format OK"
        else
            log_fail "invalid V2 format: $content"
        fi
    else
        log_fail "no MQTT message received"
    fi
}

# Test 5: HTTP target
test_http_target() {
    echo "--- Test 5: HTTP target ---"
    reset_mock

    local config="${CONFIG_DIR}/http_target.yaml"

    cat > "$config" <<EOF
device:
  name: "${DEVICE_NAME}"
debug: true
targets:
  - id: "http-target"
    dsn: "http://localhost:${CAPTURE_PORT}/update-clipboard"
forward:
  - from: ["system"]
    to: ["http-target"]
EOF

    "${BINARY}" -config "$config" --received-write-text "Hello HTTP" >/dev/null 2>&1
    sleep 1

    local stats=$(curl -sf http://localhost:${CAPTURE_PORT}/_stats)
    local count=$(python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d['count'])" <<< "$stats")

    if [ "$count" -ge 1 ]; then
        local body=$(python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d['messages'][-1]['body'])" <<< "$stats")
        local content=$(python3 -c "import json,base64; d=json.loads('''$body'''); print(base64.b64decode(d['content']).decode())")
        if [ "$content" = "Hello HTTP" ]; then
            log_pass "HTTP target received, content=$content"
        else
            log_fail "content mismatch: $content"
        fi
    else
        log_fail "no HTTP message received"
    fi
}

# Test 6: MQTT receive -> forward to HTTP
test_mqtt_receive_v1() {
    echo "--- Test 6: MQTT receive -> HTTP forward ---"

    if ! check_clipboard_env; then
        log_skip "requires clipboard env tools (x11: xvfb-run+xclip, wayland: wl-paste+wl-copy, darwin: pbcopy+pbpaste)"
        return
    fi

    reset_mock
    local config="${CONFIG_DIR}/mqtt_recv.yaml"

    cat > "$config" <<EOF
device:
  name: "${DEVICE_NAME}"
debug: true
http:
  port: ":${APP_PORT}"
listen:
  - id: "mqtt-recv"
    dsn: "mqtt://localhost:${MQTT_PORT}/test/recv/+?retain=false&clientId=t6"
targets:
  - id: "http-dest"
    dsn: "http://localhost:${CAPTURE_PORT}/update-clipboard"
forward:
  - from: ["mqtt-recv"]
    to: ["system", "http-dest"]
EOF

    run_with_clipboard "${BINARY}" -config "$config" > "${RESULT_DIR}/recv.log" 2>&1 &
    local app_pid=$!
    sleep 3

    # Publish a V1 message from a different device
    python3 -c "
import json, base64, time
msg = {'time': time.time(), 'uuid': 'test-recv-1', 'deviceName': 'remote-device',
       'mime': 'text/plain', 'type': 'text',
       'content': base64.b64encode(b'Hello from MQTT').decode(),
       'sendTime': time.time()}
print(json.dumps(msg))
" > "${RESULT_DIR}/mqtt_send1.json"

    mosquitto_pub -h localhost -p ${MQTT_PORT} -t "test/recv/text" -f "${RESULT_DIR}/mqtt_send1.json"
    sleep 2

    local stats=$(curl -sf http://localhost:${CAPTURE_PORT}/_stats)
    local found=$(python3 -c "
import json, sys
d = json.loads(sys.stdin.read())
msgs = [m for m in d['messages'] if 'remote-device' in m.get('body','')]
print(len(msgs))
" <<< "$stats")

    kill $app_pid 2>/dev/null; wait $app_pid 2>/dev/null || true

    if [ "$found" -ge 1 ]; then
        log_pass "message forwarded to HTTP target"
    else
        log_fail "no forwarded message found"
        log_info "App log (last 10 lines):"
        tail -10 "${RESULT_DIR}/recv.log" | sed 's/^/         /'
    fi
}

# Test 7: Echo prevention
test_echo_prevention() {
    echo "--- Test 7: Echo prevention ---"

    if ! check_clipboard_env; then
        log_skip "requires clipboard env tools (x11: xvfb-run+xclip, wayland: wl-paste+wl-copy, darwin: pbcopy+pbpaste)"
        return
    fi

    reset_mock
    local config="${CONFIG_DIR}/echo.yaml"
    local echo_port=$((APP_PORT + 1))

    cat > "$config" <<EOF
device:
  name: "${DEVICE_NAME}"
debug: true
http:
  port: ":${echo_port}"
targets:
  - id: "http-dest"
    dsn: "http://localhost:${CAPTURE_PORT}/update-clipboard"
forward:
  - from: ["http"]
    to: ["system", "http-dest"]
  - from: ["system"]
    to: ["http-dest"]
EOF

    local before_count=$(python3 -c "
import json, urllib.request
d = json.loads(urllib.request.urlopen('http://localhost:${CAPTURE_PORT}/_stats').read())
print(d['count'])
")

    run_with_clipboard "${BINARY}" -config "$config" > "${RESULT_DIR}/echo.log" 2>&1 &
    local app_pid=$!
    sleep 3

    # Write a remote message locally and forward it once. The clipboard event
    # caused by the local write must be suppressed rather than forwarded again.
    python3 -c "
import json, base64, time
msg = {'time': time.time(), 'uuid': 'echo-test', 'deviceName': 'remote-device',
       'mime': 'text/plain', 'type': 'text',
       'content': base64.b64encode(b'Echo test').decode(),
       'sendTime': time.time()}
print(json.dumps(msg))
" | curl -sf -X POST -H "Content-Type: application/json" -d @- "http://localhost:${echo_port}/update-clipboard" >/dev/null

    # Forwarding is asynchronous. Poll for the direct delivery instead of
    # assuming a loaded CI runner will finish it within one fixed second.
    local after_count=$before_count
    for _ in $(seq 1 20); do
        after_count=$(python3 -c "
import json, urllib.request
d = json.loads(urllib.request.urlopen('http://localhost:${CAPTURE_PORT}/_stats').read())
print(d['count'])
")
        if [ "$after_count" -gt "$before_count" ]; then
            break
        fi
        sleep 0.25
    done

    # Leave enough time for a wrongly unsuppressed clipboard event to arrive,
    # then assert that the count still increased exactly once.
    sleep 2
    after_count=$(python3 -c "
import json, urllib.request
d = json.loads(urllib.request.urlopen('http://localhost:${CAPTURE_PORT}/_stats').read())
print(d['count'])
")

    kill $app_pid 2>/dev/null; wait $app_pid 2>/dev/null || true

    local forwarded_count=$((after_count - before_count))
    if [ "$forwarded_count" = "1" ]; then
        log_pass "local clipboard echo was suppressed (forwarded once)"
    else
        log_fail "expected one direct forward and no clipboard echo, got $forwarded_count (before=$before_count, after=$after_count)"
    fi
}

# Test 8: V2 receive via MQTT + Center
test_mqtt_receive_v2() {
    echo "--- Test 8: V2 receive via MQTT + Center ---"

    if ! check_clipboard_env; then
        log_skip "requires clipboard env tools (x11: xvfb-run+xclip, wayland: wl-paste+wl-copy, darwin: pbcopy+pbpaste)"
        return
    fi

    reset_mock
    local config="${CONFIG_DIR}/v2_recv.yaml"
    local v2_port=$((APP_PORT + 2))

    # Pre-PUT data to center
    curl -sf -X PUT -H "Authorization: Bearer ${TEST_TOKEN}" -H "Content-Type: text/plain" \
        --data-binary "V2 test content from center" \
        "http://localhost:${CENTER_PORT}/client/remote-device/clipboard-text" >/dev/null

    cat > "$config" <<EOF
device:
  name: "${DEVICE_NAME}"
debug: true
http:
  port: ":${v2_port}"
centers:
  - id: "test-center"
    url: "http://localhost:${CENTER_PORT}"
    token: "${TEST_TOKEN}"
    textMsgId: "clipboard-text"
    imageMsgId: "clipboard-image"
listen:
  - id: "mqtt-recv"
    dsn: "mqtt://localhost:${MQTT_PORT}/test/v2-recv/+?retain=false&clientId=t8"
targets:
  - id: "http-dest"
    dsn: "http://localhost:${CAPTURE_PORT}/update-clipboard"
forward:
  - from: ["mqtt-recv"]
    to: ["system", "http-dest"]
    center: "test-center"
EOF

    run_with_clipboard "${BINARY}" -config "$config" > "${RESULT_DIR}/v2_recv.log" 2>&1 &
    local app_pid=$!
    sleep 3

    # Publish V2 message
    python3 -c "
import json, time
msg = {'time': time.time(), 'uuid': 'v2-recv-test', 'deviceName': 'remote-device',
       'mime': 'text/plain', 'type': 'text-v2',
       'content': 'remote-device/clipboard-text,centerId:test-center,sha1:fake,length:28,encoding:base64',
       'sendTime': time.time()}
print(json.dumps(msg))
" > "${RESULT_DIR}/v2_send.json"

    mosquitto_pub -h localhost -p ${MQTT_PORT} -t "test/v2-recv/text" -f "${RESULT_DIR}/v2_send.json"
    sleep 2

    # Check app log for V2 download
    local v2_ok=0
    grep -q "V2 download" "${RESULT_DIR}/v2_recv.log" 2>/dev/null && v2_ok=1

    # Check HTTP capture for forwarded message
    local stats=$(curl -sf http://localhost:${CAPTURE_PORT}/_stats)
    local found=$(python3 -c "
import json, sys
d = json.loads(sys.stdin.read())
msgs = [m for m in d['messages'] if 'remote-device' in m.get('body','')]
print(len(msgs))
" <<< "$stats")

    kill $app_pid 2>/dev/null; wait $app_pid 2>/dev/null || true

    if [ "$v2_ok" -ge 1 ] || [ "$found" -ge 1 ]; then
        log_pass "V2 download OK (log=$v2_ok, forwarded=$found)"
    else
        log_fail "V2 receive failed"
        log_info "App log (last 10 lines):"
        tail -10 "${RESULT_DIR}/v2_recv.log" | sed 's/^/         /'
    fi
}

# ======================== Run ========================

test_text_v1_mqtt
test_image_v1_mqtt
test_text_v2_mqtt
test_image_v2_mqtt
test_http_target
test_mqtt_receive_v1
test_echo_prevention
test_mqtt_receive_v2

# ======================== Summary ========================

echo ""
echo "=========================================="
echo "  Results: ${GREEN}${pass} passed${NC}, ${RED}${fail} failed${NC}, ${YELLOW}${skip} skipped${NC}"
echo "=========================================="

if [ $fail -gt 0 ]; then
    exit 1
fi
