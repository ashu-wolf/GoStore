#!/usr/bin/env bash
set -euo pipefail

TMP_DIR="$(mktemp -d)"
SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"

export GOSTORE_ADDR="127.0.0.1:${PORT}"
export GOSTORE_DATA_DIR="${TMP_DIR}/data"
export GOSTORE_ACCESS_KEY="gostore-ci-access"
export GOSTORE_SECRET_KEY="gostore-ci-secret"
export GOSTORE_REGION="us-east-1"

go build -o "${TMP_DIR}/gostore" ./cmd/server
"${TMP_DIR}/gostore" >"${TMP_DIR}/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS "http://${GOSTORE_ADDR}/-/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -fsS "http://${GOSTORE_ADDR}/-/healthz" | grep -qx "ok"

export AWS_ACCESS_KEY_ID="$GOSTORE_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$GOSTORE_SECRET_KEY"
export AWS_DEFAULT_REGION="$GOSTORE_REGION"
export AWS_EC2_METADATA_DISABLED=true
ENDPOINT="http://${GOSTORE_ADDR}"

# Unauthenticated request must be rejected.
if curl -s -o /dev/null -w "%{http_code}" "${ENDPOINT}/" | grep -qv "403"; then
  echo "expected 403 for unauthenticated request"
  exit 1
fi

printf 'hello from aws cli v2\n' > "${TMP_DIR}/upload.txt"
dd if=/dev/urandom of="${TMP_DIR}/large.bin" bs=1024 count=200 status=none

aws --endpoint-url "$ENDPOINT" s3api create-bucket --bucket cli-bucket
aws --endpoint-url "$ENDPOINT" s3api put-object --bucket cli-bucket --key docs/hello.txt --body "${TMP_DIR}/upload.txt"
aws --endpoint-url "$ENDPOINT" s3 cp "${TMP_DIR}/large.bin" s3://cli-bucket/docs/large.bin

aws --endpoint-url "$ENDPOINT" s3 ls s3://cli-bucket/docs/ | grep -q "hello.txt"
aws --endpoint-url "$ENDPOINT" s3api list-objects-v2 --bucket cli-bucket --prefix docs/ | grep -q "docs/large.bin"

aws --endpoint-url "$ENDPOINT" s3api get-object --bucket cli-bucket --key docs/hello.txt "${TMP_DIR}/dl.txt" >/dev/null
cmp -s "${TMP_DIR}/upload.txt" "${TMP_DIR}/dl.txt"

aws --endpoint-url "$ENDPOINT" s3 cp s3://cli-bucket/docs/large.bin "${TMP_DIR}/dl-large.bin"
cmp -s "${TMP_DIR}/large.bin" "${TMP_DIR}/dl-large.bin"

aws --endpoint-url "$ENDPOINT" s3api delete-object --bucket cli-bucket --key docs/hello.txt
aws --endpoint-url "$ENDPOINT" s3 rm s3://cli-bucket/docs/large.bin
aws --endpoint-url "$ENDPOINT" s3api delete-bucket --bucket cli-bucket

echo "AWS CLI integration check passed"
