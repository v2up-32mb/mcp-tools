#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP_DIR=$(mktemp -d)
PORT=${MCP_DEMO_PORT:-18080}
TOKEN=${MCP_DEMO_TOKEN:-demo-token}
SERVER_LOG="$TMP_DIR/server.log"
STREAM_LOG="$TMP_DIR/stream.log"

cleanup() {
  if [[ -n "${STREAM_PID:-}" ]]; then
    kill "$STREAM_PID" >/dev/null 2>&1 || true
    wait "$STREAM_PID" 2>/dev/null || true
  fi
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

cat <<MSG
[demo] root dir: $ROOT_DIR
[demo] temp dir: $TMP_DIR
[demo] port: $PORT
MSG

(
  cd "$ROOT_DIR"
  MCP_BEARER_TOKEN="$TOKEN" \
  MCP_LISTEN_ADDR="127.0.0.1:$PORT" \
  MCP_ALLOWED_ROOTS="$ROOT_DIR" \
  go run ./cmd/mcp-tools >"$SERVER_LOG" 2>&1
) &
SERVER_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

INIT_HEADERS="$TMP_DIR/init.headers"
INIT_BODY="$TMP_DIR/init.body"
curl -fsS -D "$INIT_HEADERS" \
  -o "$INIT_BODY" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"demo-script","version":"1.0.0"}}}' \
  "http://127.0.0.1:$PORT/mcp"

SESSION_ID=$(awk -F': ' 'BEGIN{IGNORECASE=1} $1=="Mcp-Session-Id" {gsub("\r", "", $2); print $2}' "$INIT_HEADERS")
if [[ -z "$SESSION_ID" ]]; then
  echo '[demo] failed to extract session id' >&2
  cat "$INIT_HEADERS" >&2
  exit 1
fi

echo "[demo] session id: $SESSION_ID"

touch "$STREAM_LOG"
curl -NsS \
  -H "Authorization: Bearer $TOKEN" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H 'Accept: text/event-stream' \
  "http://127.0.0.1:$PORT/mcp" >"$STREAM_LOG" &
STREAM_PID=$!

for _ in $(seq 1 50); do
  if grep -q 'stream opened' "$STREAM_LOG" 2>/dev/null; then
    break
  fi
  sleep 0.2
done

echo '[demo] opened SSE stream'

echo '[demo] POST tools/list asynchronously into stream'
curl -fsS -i \
  -H "Authorization: Bearer $TOKEN" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}' \
  "http://127.0.0.1:$PORT/mcp"

sleep 1

echo '--- stream output ---'
cat "$STREAM_LOG"
