# GoStore

An S3-compatible, single-node object storage service written in Go for Linux.

GoStore streams object data directly to immutable, prefix-sharded files on disk, serializes metadata commits through a group-committed CRC-32C journal, authenticates requests with AWS Signature V4, verifies full-object reads with SHA-256, and includes Prometheus metrics, graceful shutdown draining, container packaging, and a reproducible benchmark suite.

## Requirements

- **Linux** (or **WSL 2** on Windows): storage uses Linux `flock` and atomic POSIX rename/dirsync semantics.
- **Go `1.27+`** (for building/testing from source).
- **AWS CLI v2**, `curl`, `python3` (optional, for running the demo and CLI compatibility scripts).
- **Docker + Docker Compose** (optional, for running the container image and container persistence checks).

## Quick Start

### Run from source (WSL 2 / Linux)

```bash
export GOSTORE_ACCESS_KEY=gostore-dev-access
export GOSTORE_SECRET_KEY=gostore-dev-secret
make run
```

Verify health, readiness, and metrics:

```bash
curl http://127.0.0.1:9000/-/healthz
curl http://127.0.0.1:9000/-/readyz
curl http://127.0.0.1:9000/-/metrics
```

### Run the end-to-end demo

```bash
bash ./scripts/demo.sh
```

[demo.sh](scripts/demo.sh) builds the server, checks operational endpoints and unauthenticated `403` rejection, creates a bucket, uploads single-part and 11 MiB multipart objects via AWS CLI v2, lists with prefix filtering, restarts GoStore on the same data directory, and verifies objects match byte-for-byte after restart.

### Run with Docker Compose

```bash
export GOSTORE_ACCESS_KEY=replace-with-access-key
export GOSTORE_SECRET_KEY=replace-with-secret-key
docker compose up -d
curl http://127.0.0.1:9000/-/readyz
```

For production TLS termination (Nginx/Caddy), reverse-proxy settings required by SigV4, cold backups, and restore procedures, see [docs/runbook.md](docs/runbook.md).

## Configuration

| Variable | Default | Description |
|---|---|---|
| `GOSTORE_ADDR` | `127.0.0.1:9000` | HTTP listen address |
| `GOSTORE_PPROF_ADDR` | *(disabled)* | Optional loopback-only `pprof` listener (e.g. `127.0.0.1:6060`) |
| `GOSTORE_DATA_DIR` | `./data` | Storage root (`meta.journal`, `blobs/`, `tmp/`) |
| `GOSTORE_SHUTDOWN_TIMEOUT` | `30s` | Graceful drain timeout on `SIGINT`/`SIGTERM` |
| `GOSTORE_IDLE_TRANSFER_TIMEOUT` | `60s` | Per-chunk idle read/write deadline for transfers |
| `GOSTORE_MAX_CONCURRENT_REQUESTS` | `256` | Active S3 concurrency cap (excess returns `503 SlowDown`) |
| `GOSTORE_MAX_OBJECT_SIZE` | `1073741824` (1 GiB) | Max single `PutObject` or `UploadPart` size |
| `GOSTORE_MAX_MULTIPART_SIZE` | `5368709120` (5 GiB) | Max completed multipart object size |
| `GOSTORE_MIN_PART_SIZE` | `5242880` (5 MiB) | Minimum multipart part size (except last part) |
| `GOSTORE_MAX_UPLOAD_PARTS` | `10000` | Maximum parts per multipart upload |
| `GOSTORE_UPLOAD_EXPIRY` | `24h` | Inactive multipart upload expiration & completion retention |
| `GOSTORE_MIN_FREE_SPACE` | `1073741824` (1 GiB) | Reserved disk headroom (`0` disables space checks) |
| `GOSTORE_LIST_PAGE_SIZE` | `1000` | Maximum keys returned per `ListObjectsV2` page |
| `GOSTORE_ACCESS_KEY` | *(required)* | SigV4 access key ID |
| `GOSTORE_SECRET_KEY` | *(required)* | SigV4 secret access key |
| `GOSTORE_REGION` | `us-east-1` | Expected SigV4 signing region |

## Supported Operations

GoStore uses **path-style** addressing (`http://host/{bucket}/{key}`) and requires **AWS Signature V4** (`Authorization` header) on all S3 routes.

### S3 API

| Area | Supported Operations | Notes |
|---|---|---|
| Service | `ListBuckets` (`GET /`) | Sorted bucket list |
| Buckets | `CreateBucket`, `DeleteBucket`, `ListObjectsV2` (`list-type=2`) | Empty bucket check is atomic; `prefix`, `delimiter`, `max-keys`, `continuation-token`, `start-after`, `encoding-type` supported |
| Objects | `PutObject`, `GetObject`, `HeadObject`, `DeleteObject` | Supports `Content-MD5`, `X-Amz-Content-Sha256` (signed, unsigned, `aws-chunked` + trailers), user `X-Amz-Meta-*`, single `Range` (`206`/`416`), and full-read SHA-256 verification |
| Multipart | `CreateMultipartUpload`, `UploadPart`, `ListParts`, `CompleteMultipartUpload`, `AbortMultipartUpload` | Idempotent completion retention (24h default); inactive uploads expire automatically |

### Explicitly Rejected (`501 NotImplemented` / `405` / `403`)

- Virtual-hosted-style bucket routing and presigned URL query authentication
- `ListObjects` v1 (`list-type=1` or omitted)
- Copy (`X-Amz-Copy-Source`), multi-range `Range` requests, ACLs, policies, versioning, tagging, lifecycle, CORS, and bucket `POST` uploads

### Operational Endpoints

| Path | Purpose | Auth |
|---|---|---|
| `/-/healthz` | Liveness check (`200 ok`) | None |
| `/-/readyz` | Readiness probe (`200 ready` / `503 not ready`) | None |
| `/-/metrics` | Prometheus metrics (`gostore_ready`, `gostore_active_requests`, `gostore_requests_total`, `gostore_errors_total`, `gostore_request_bytes_total`, `gostore_request_duration_seconds`) | None |

*(Bucket names cannot be `-`, preventing path collisions with `/-/`.)*

## API Examples (AWS CLI v2)

```bash
export AWS_ACCESS_KEY_ID=gostore-dev-access
export AWS_SECRET_ACCESS_KEY=gostore-dev-secret
export AWS_DEFAULT_REGION=us-east-1
export AWS_EC2_METADATA_DISABLED=true
ENDPOINT="http://127.0.0.1:9000"

aws --endpoint-url "$ENDPOINT" s3 mb s3://photos
aws --endpoint-url "$ENDPOINT" s3 cp ./local.jpg s3://photos/2026/local.jpg
aws --endpoint-url "$ENDPOINT" s3api list-objects-v2 --bucket photos --prefix 2026/
aws --endpoint-url "$ENDPOINT" s3 cp s3://photos/2026/local.jpg ./downloaded.jpg
aws --endpoint-url "$ENDPOINT" s3 rm s3://photos/2026/local.jpg
aws --endpoint-url "$ENDPOINT" s3 rb s3://photos
```

## Testing & Benchmarks

Run formatting, `go vet`, and full race-detector tests (in WSL 2 / Linux):

```bash
make check
```

Run static analysis and vulnerability scanning:

```bash
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
```

Run integration and container checks:

```bash
bash ./scripts/aws_cli_check.sh
bash ./scripts/container_check.sh
```

Run the benchmark suite on native Linux `ext4`:

```bash
make bench
```

## Architecture, Durability & Reports

- [docs/system-and-code-guide.md](docs/system-and-code-guide.md) — full system architecture, component breakdown, and package-by-package code walkthrough
- [docs/architecture.md](docs/architecture.md) — system diagram, write protocol & commit point, read integrity check, and startup recovery
- [docs/runbook.md](docs/runbook.md) — reverse-proxy TLS, Docker Compose operations, graceful shutdown, and backup/restore
- [docs/benchmarks.md](docs/benchmarks.md) — baseline and post-group-commit benchmark methodology, environment, and measured results
- [docs/test-report.md](docs/test-report.md) — static analysis, vulnerability scan, race, fuzz, crash-recovery, and integration test results
- [docs/adr/0001-concurrency-and-resource-limits.md](docs/adr/0001-concurrency-and-resource-limits.md) — concurrency conflicts, pins, and overload/space limits
- [docs/adr/0002-metadata-group-commit.md](docs/adr/0002-metadata-group-commit.md) — leader-follower metadata journal group commit
- [docs/adr/0003-immutable-blobs-and-metadata-journal.md](docs/adr/0003-immutable-blobs-and-metadata-journal.md) — immutable sharded blobs and CRC-32C metadata journal
- [docs/architecture-guide.md](docs/architecture-guide.md) — visual architecture guide and component walkthrough
- [docs/function-reference.md](docs/function-reference.md) — reference of Go functions and methods across packages

## Limitations & Non-Goals

- **Single-node only:** No replication, erasure coding, or multi-node consensus.
- **In-memory metadata:** All bucket, object, and active multipart metadata is held in RAM; startup replays `meta.journal`.
- **Single-host backups:** `meta.journal` and `blobs/` must be backed up together with the server stopped (or from an atomic filesystem snapshot). Restoring an older journal causes startup recovery to delete newer unreferenced blobs.
- **S3 scope limits:** No object versioning, IAM policies/multi-user auth, presigned URLs, virtual-hosted bucket addressing, or server-side encryption. Range `GET` requests (`206`) are not SHA-256 verified end-to-end, unlike full-object `GET` requests.

## License

MIT — see [LICENSE](LICENSE).
