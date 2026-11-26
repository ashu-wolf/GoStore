#!/usr/bin/env bash
set -euo pipefail

TMP_DIR="$(mktemp -d)"
cleanup() {
  local status=$?
  if [[ $status -ne 0 ]]; then
    docker compose logs gostore >&2 || true
  fi
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

export GOSTORE_ACCESS_KEY="gostore-container-access"
export GOSTORE_SECRET_KEY="gostore-container-secret"
export GOSTORE_REGION="us-east-1"
export GOSTORE_SHUTDOWN_TIMEOUT="2s"

docker compose build
docker compose up -d

for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:9000/-/readyz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done
curl -fsS "http://127.0.0.1:9000/-/readyz" | grep -qx "ready"
curl -fsS "http://127.0.0.1:9000/-/healthz" | grep -qx "ok"

export AWS_ACCESS_KEY_ID="$GOSTORE_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$GOSTORE_SECRET_KEY"
export AWS_DEFAULT_REGION="$GOSTORE_REGION"
export AWS_EC2_METADATA_DISABLED=true
ENDPOINT="http://127.0.0.1:9000"

dd if=/dev/urandom of="${TMP_DIR}/payload.bin" bs=1024 count=256 status=none

aws --endpoint-url "$ENDPOINT" s3 mb s3://container-bucket
aws --endpoint-url "$ENDPOINT" s3 cp "${TMP_DIR}/payload.bin" s3://container-bucket/obj.bin
aws --endpoint-url "$ENDPOINT" s3 ls s3://container-bucket/ | grep -q "obj.bin"

# Verify metrics endpoint reports requests without exposing object keys.
curl -fsS "http://127.0.0.1:9000/-/metrics" | grep -q 'gostore_requests_total{op="put_object",status="200"}'
if curl -fsS "http://127.0.0.1:9000/-/metrics" | grep -q "obj.bin"; then
  echo "metrics leaked object key"
  exit 1
fi

# Replace container and confirm data persists in volume.
docker compose up -d --force-recreate
for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:9000/-/readyz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done
aws --endpoint-url "$ENDPOINT" s3 cp s3://container-bucket/obj.bin "${TMP_DIR}/downloaded.bin"
cmp -s "${TMP_DIR}/payload.bin" "${TMP_DIR}/downloaded.bin"

# Verify graceful shutdown completes within GOSTORE_SHUTDOWN_TIMEOUT (2s) + 3s margin.
start_ns=$(date +%s%N)
docker compose stop gostore
elapsed_ms=$(( ($(date +%s%N) - start_ns) / 1000000 ))
echo "Container stopped in ${elapsed_ms}ms"
if [[ $elapsed_ms -gt 5000 ]]; then
  echo "container shutdown exceeded timeout + margin (${elapsed_ms}ms > 5000ms)"
  exit 1
fi

echo "Container integration check passed"
