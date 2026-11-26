#!/usr/bin/env bash
set -euo pipefail

# End-to-end reviewer demo for GoStore:
# builds the server, demonstrates S3 operations and operational endpoints,
# restarts the process against the same data directory to prove persistence,
# and verifies clean shutdown.

for cmd in go aws curl python3 cmp; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "missing required command: $cmd" >&2
    exit 1
  fi
done

TMP_DIR="$(mktemp -d)"
SERVER_PID=""

stop_server() {
  if [[ -n "$SERVER_PID" ]]; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    SERVER_PID=""
  fi
}

cleanup() {
  local status=$?
  stop_server
  if [[ $status -ne 0 && -f "${TMP_DIR}/server.log" ]]; then
    echo "=== Server log ===" >&2
    cat "${TMP_DIR}/server.log" >&2 || true
  fi
  if [[ "${DEMO_KEEP_DATA:-0}" != "1" ]]; then
    rm -rf "$TMP_DIR"
  else
    echo "Kept demo workspace at $TMP_DIR"
  fi
}
trap cleanup EXIT

PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
export GOSTORE_ADDR="127.0.0.1:${PORT}"
export GOSTORE_DATA_DIR="${TMP_DIR}/data"
export GOSTORE_ACCESS_KEY="gostore-demo-access"
export GOSTORE_SECRET_KEY="gostore-demo-secret"
export GOSTORE_REGION="us-east-1"
export GOSTORE_MIN_FREE_SPACE="104857600"

export AWS_ACCESS_KEY_ID="$GOSTORE_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$GOSTORE_SECRET_KEY"
export AWS_DEFAULT_REGION="$GOSTORE_REGION"
export AWS_EC2_METADATA_DISABLED=true
ENDPOINT="http://${GOSTORE_ADDR}"

start_server() {
  "${TMP_DIR}/gostore" >>"${TMP_DIR}/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 50); do
    if curl -fsS "${ENDPOINT}/-/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "server failed to become ready" >&2
  return 1
}

echo "[1/7] Building GoStore server..."
go build -o "${TMP_DIR}/gostore" ./cmd/server

echo "[2/7] Starting GoStore on ${ENDPOINT} (data: ${GOSTORE_DATA_DIR})..."
start_server
echo "  healthz: $(curl -fsS "${ENDPOINT}/-/healthz")"
echo "  readyz:  $(curl -fsS "${ENDPOINT}/-/readyz")"

echo "[3/7] Verifying unauthenticated S3 access is rejected (403)..."
code="$(curl -s -o /dev/null -w "%{http_code}" "${ENDPOINT}/")"
if [[ "$code" != "403" ]]; then
  echo "expected 403, got $code" >&2
  exit 1
fi
echo "  unauthenticated GET / -> HTTP $code"

echo "[4/7] Creating bucket and uploading sample + 11 MiB multipart objects..."
printf 'hello from gostore demo\n' > "${TMP_DIR}/note.txt"
dd if=/dev/urandom of="${TMP_DIR}/archive.bin" bs=1048576 count=11 status=none

aws --endpoint-url "$ENDPOINT" s3 mb s3://demo-bucket
aws --endpoint-url "$ENDPOINT" s3 cp "${TMP_DIR}/note.txt" s3://demo-bucket/docs/note.txt \
  --content-type text/plain --metadata owner=reviewer
aws --endpoint-url "$ENDPOINT" s3 cp "${TMP_DIR}/archive.bin" s3://demo-bucket/backups/archive.bin

echo "[5/7] Listing with prefix and checking metrics..."
aws --endpoint-url "$ENDPOINT" s3api list-objects-v2 --bucket demo-bucket --prefix docs/ \
  --query 'Contents[].{Key:Key,Size:Size}' --output table
curl -fsS "${ENDPOINT}/-/metrics" | grep -E '^(gostore_ready|gostore_requests_total)' | head -n 6

echo "[6/7] Restarting GoStore to verify durable persistence..."
stop_server
start_server
grep '"storage recovered"' "${TMP_DIR}/server.log" | tail -n 1

aws --endpoint-url "$ENDPOINT" s3 cp s3://demo-bucket/docs/note.txt "${TMP_DIR}/note-after.txt"
aws --endpoint-url "$ENDPOINT" s3 cp s3://demo-bucket/backups/archive.bin "${TMP_DIR}/archive-after.bin"
cmp -s "${TMP_DIR}/note.txt" "${TMP_DIR}/note-after.txt"
cmp -s "${TMP_DIR}/archive.bin" "${TMP_DIR}/archive-after.bin"
echo "  verified downloaded files match byte-for-byte after restart"

echo "[7/7] Cleaning up demo objects and bucket..."
aws --endpoint-url "$ENDPOINT" s3 rm s3://demo-bucket/docs/note.txt
aws --endpoint-url "$ENDPOINT" s3 rm s3://demo-bucket/backups/archive.bin
aws --endpoint-url "$ENDPOINT" s3 rb s3://demo-bucket

echo "Demo completed successfully."
