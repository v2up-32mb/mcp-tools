#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP_DIR=$(mktemp -d)
PORT=${MCP_DEMO_PORT:-18081}
TOKEN=${MCP_DEMO_TOKEN:-demo-token}
SERVER_LOG="$TMP_DIR/server.log"

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

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
curl -fsS -D "$INIT_HEADERS" \
  -o /dev/null \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"cap-demo","version":"1.0.0"}}}' \
  "http://127.0.0.1:$PORT/mcp"
SESSION_ID=$(awk -F': ' 'BEGIN{IGNORECASE=1} $1=="Mcp-Session-Id" {gsub("\r", "", $2); print $2}' "$INIT_HEADERS")
if [[ -z "$SESSION_ID" ]]; then
  echo '[demo] failed to extract session id' >&2
  cat "$INIT_HEADERS" >&2
  exit 1
fi

echo "[demo] session id: $SESSION_ID"
echo '[demo] resources/templates/list'
curl -fsS \
  -H "Authorization: Bearer $TOKEN" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"resources/templates/list","params":{}}' \
  "http://127.0.0.1:$PORT/mcp"

echo
echo '[demo] resources/read (allowed root)'
ROOT_URI="file://$ROOT_DIR"
JSON_PAYLOAD=$(printf '{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"%s"}}' "$ROOT_URI")
curl -fsS \
  -H "Authorization: Bearer $TOKEN" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H 'Content-Type: application/json' \
  -d "$JSON_PAYLOAD" \
  "http://127.0.0.1:$PORT/mcp"

echo
echo '[demo] resources/read (README.md file with metadata)'
README_URI="file://$ROOT_DIR/README.md"
JSON_PAYLOAD=$(printf '{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"%s"}}' "$README_URI")
curl -fsS \
  -H "Authorization: Bearer $TOKEN" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H 'Content-Type: application/json' \
  -d "$JSON_PAYLOAD" \
  "http://127.0.0.1:$PORT/mcp"

echo
echo '[demo] prompts/get safe_file_edit'
curl -fsS \
  -H "Authorization: Bearer $TOKEN" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"safe_file_edit","arguments":{"task":"update readme","path":"README.md"}}}' \
  "http://127.0.0.1:$PORT/mcp"

echo
echo '[demo] prompts/get go_dev_loop'
curl -fsS \
  -H "Authorization: Bearer $TOKEN" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":6,"method":"prompts/get","params":{"name":"go_dev_loop","arguments":{"goal":"run go test and fix failures","workdir":".","test_target":"./internal/httpapi","run_vet":false}}}' \
  "http://127.0.0.1:$PORT/mcp"
