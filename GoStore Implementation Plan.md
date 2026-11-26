# GoStore Implementation Plan

Nov 26, 2026 · @Ashutosh

This plan builds GoStore, a single-node, S3-compatible object storage service in Go, in 12 core steps plus an optional AI agent. Each step has concrete work and a test that shows it is done.

| Phase | Steps | Result |
| --- | --- | --- |
| Foundation | 1–3 | Running service, validated types, durable blob writes |
| Storage engine | 4–5 | Crash-safe metadata, bucket and object operations |
| S3 compatibility | 6–8 | HTTP API, Signature V4, multipart upload |
| Reliability and operations | 9–10 | Bounded concurrency, metrics, container |
| Evidence and release | 11–12 | Measured performance, reviewable repository |
| Optional extension | 13 | AI agent with tool calling |

## Working rules

- Keep the main branch runnable after every step.
- Write tests with any change to durability, visibility or concurrency.
- Start CI, structured logging and race tests in step 1.
- Record each significant design choice in a short architecture decision record (ADR).
- Measure before optimizing, and save the command behind every result.
- Describe a feature as complete only after its "Done when" checks pass.

## Key design decisions

Settle these before coding, because several steps depend on them.

| Topic | Decision for version 1 |
| --- | --- |
| Metadata | Use bbolt, an embedded transactional database, with its default disk sync left on. A custom journal and snapshots are a later option. |
| Blob files | Every write creates a new file with a unique ID. Published files are never modified or overwritten. |
| Commit point | An object becomes visible only when its metadata transaction commits. |
| Concurrency | Stream data without holding locks or open transactions. bbolt allows one write transaction at a time, which orders metadata changes. |
| Integrity | Store SHA-256 for verification and return S3-style ETags separately. |
| Authentication | AWS Signature V4 in the request header. Presigned URLs are out of scope. |
| Addressing | Path-style URLs only. |
| Scope limits | No S3 object versioning, replication, erasure coding or IAM policies. |

### Write protocol

1. Check the request, then stream the body to a temporary file while calculating its size and hashes.
2. Verify any length, checksum and signature values supplied by the client. On failure, remove the file.
3. `fsync` and close the file.
4. Rename it to its unique blob path on the same filesystem. `fsync` the destination directory. When creating directories, sync their parents too.
5. In one metadata transaction, confirm the bucket exists, save the new record and note the old blob ID.
6. After the commit succeeds, return success and delete the old blob.

A crash before step 5 commits leaves the old object visible and may leave an unused file. A crash after it leaves the new object visible and may leave the old file. Startup cleanup removes both kinds of unused file. If the client gets no response, it can't know whether the write committed.

### Default limits

These are proposed starting values. Make them configurable and test each one with small overrides.

| Limit | Default |
| --- | --- |
| Concurrent data requests | 256 |
| Single upload or multipart part | 1 GiB |
| Completed multipart object | 5 GiB |
| Multipart part size | At least 5 MiB, except the last part |
| Parts per upload | 10,000 |
| Inactive multipart expiry | 24 hours |
| Reserved free disk space | 1 GiB |
| List page size | 1,000 entries |
| Header read timeout | 10 seconds |
| Graceful shutdown timeout | 30 seconds |

## Detailed implementation sequence

Finish each step's checks before starting the next. Until step 7 adds authentication, listen only on localhost.

### Step 1: Repository and service lifecycle

- Create the Go module, `cmd/server`, `internal/` packages, a Makefile and CI for formatting, `go vet`, tests and race tests.
- Add typed configuration from environment variables, structured logs with request IDs, `/healthz` and SIGTERM handling. Reserve operational paths so they can't collide with bucket names, for example under `/-/`.

**Done when:** a fresh clone runs with documented commands, CI passes and SIGTERM exits cleanly.

### Step 2: Domain model and validation

- Define bucket, object, upload, part and continuation token types, plus domain errors that don't depend on HTTP.
- Validate bucket names, key length (up to 1,024 UTF-8 bytes) and metadata size. Treat keys as data, never as file paths.

**Done when:** table tests and fuzz tests cover invalid names, and keys such as `../../etc/passwd` can't affect file paths.

### Step 3: Streaming blob store

- Implement steps 1–4 of the write protocol with fixed-size buffers. Spread blobs across subdirectories by ID prefix.
- Add reads, seeking and deletion. Restrict file permissions and lock the data directory against a second server process.

**Done when:** tests pass for large streams, client disconnects and injected write, sync and rename errors. Memory use stays roughly flat as object size grows.

### Step 4: Metadata, commit and recovery

- Store buckets and objects in bbolt, with a schema version. Order object records by bucket and key.
- Implement steps 5–6 of the write protocol. Keep transactions short.
- Before reporting ready, remove temporary files and unreferenced blobs. Report missing or wrong-sized referenced blobs, and return an error when they're read. Stop startup if the database can't be opened safely.

**Done when:** a test kills the process at each write stage, restarts it and finds either the old or new object. Every acknowledged write remains. This tests process crashes, not power loss.

### Step 5: Bucket and object service

- Add create, list and delete bucket, plus put, get, head and delete object.
- Check that a bucket is empty and delete it in one transaction. Make deleting a missing object succeed.
- If a read finds its blob already removed, look up the metadata again. An opened file stays readable on Linux after deletion.
- Add listing with prefix, delimiter, common prefixes and an opaque continuation token. Sort keys by UTF-8 byte order and seek through the index instead of loading every key.

**Done when:** direct service tests cover overwrite, missing objects, non-empty buckets and page boundaries, with no duplicate or skipped entries in an unchanged bucket.

### Step 6: S3-compatible HTTP API

- Add path-style routing, XML responses, S3-style errors and ListObjectsV2 parameters.
- Preserve content type and user metadata. Return correct length, modified time and ETag headers; a single upload's ETag is its quoted MD5 hex value.
- Support one byte range per request, returning 206 or 416 as appropriate. Reject unsupported operations explicitly, so they aren't mistaken for normal uploads.
- Check SHA-256 on full reads. Hold back the final chunk until it passes; on failure, close the connection and record an error. Range reads aren't verified in version 1.

**Done when:** HTTP tests cover every supported route and error, including suffix and invalid ranges, and a corrupted blob never produces a complete successful download.

### Step 7: Signature V4 and client compatibility

- Verify canonical requests, signed headers, region, service and clock skew. Compare signatures in constant time and never log secrets.
- Handle signed, unsigned and `aws-chunked` payloads used by your chosen client versions, including chunk signatures and checksum trailers where sent.
- Require credentials by default.

**Done when:** published AWS signing examples pass, tampered requests fail, and pinned versions of AWS CLI and AWS SDK for Go v2 complete create, upload, list, download and delete. Run this check in CI.

### Step 8: Multipart upload

- Add initiate, upload part, list parts, complete and abort. Persist upload and part records.
- On completion, check part order, ETags and size limits. Assemble a new blob and sync it as in the write protocol.
- In one transaction, confirm the upload is still active, publish the object, remove upload state and save the completion result. This lets a repeated request return the same result for 24 hours.
- Calculate the multipart ETag from the binary part MD5 digests, followed by the part count.
- Prevent cleanup from removing files that active requests are using. Expire only inactive uploads.

**Done when:** AWS CLI completes a large upload, and tests cover part replacement, abort racing complete, repeated completion and restart. Incomplete uploads survive restart until they expire.

### Step 9: Concurrency and resource limits

- Document which operations conflict and which transaction decides the result.
- Limit active requests with a semaphore and return a retryable 503 when full. Honor cancellation and set idle-transfer timeouts.
- Account for space needed by simultaneous uploads and multipart assembly. Handle a full disk even after the initial check.
- Make cleanup workers bounded and stoppable.

**Done when:** mixed put, get, delete, list, multipart and bucket deletion tests pass repeatedly with `go test -race ./...`. Overload and disk-full tests leave committed objects intact.

### Step 10: Operations and container

- Add readiness, request counts, bytes, errors, latency histograms and active-request metrics. Avoid object keys in metric labels. Keep `pprof` on a separate localhost-only port, disabled by default.
- On shutdown, mark the service unready, stop new requests and drain active ones. At the deadline, cancel them and close connections. Close bbolt only after workers and handlers stop.
- Write a Dockerfile for a multi-stage, non-root image, and a Docker Compose file with a persistent volume. Write a Linux runbook covering TLS through a reverse proxy, backup and recovery.

**Done when:** the container starts, passes the client check, keeps data after replacement and shuts down within the configured timeout plus a small documented margin.

### Step 11: Performance baseline and tuning

- Measure 4 KiB and 1 MiB objects, large transfers, mixed reads and writes, and prefix listings at several concurrency levels.
- Record throughput, errors, p50, p95 and p99 latency, CPU, memory, open files and disk use. State the hardware, filesystem and cache conditions.
- Profile one significant bottleneck and make a focused improvement if the data supports it. Options include bbolt's batched writes or buffer sizes.

**Done when:** repeated runs produce a documented baseline and before-and-after result. Keep disk syncing the same, and report it if an attempted change doesn't help.

### Step 12: Hardening and release

- Run static analysis, vulnerability checks, race, fuzz, failure and container tests.
- Add the architecture diagram, ADRs, supported-operation list, demo script, limitations and test report. Add a license.
- Check that no credentials, data files or unmeasured claims are committed.

**Done when:** a new reviewer can clone, run, test, demonstrate and restart the service using only the repository documentation.

### Step 13: AI storage agent (optional)

This step covers the job description's preferred agentic AI skill. Start it after the core release.

- Add `cmd/agent` with typed tools to list objects, inspect metadata and delete objects through the S3 API.
- Limit tool steps, run time and accessible buckets. Treat object names and metadata as untrusted input. Send no object contents or secrets to the model.
- Require user confirmation in code, showing the exact objects, before deletion. Recheck their ETags afterward, and avoid changing those objects until deletion finishes.

**Done when:** tests with a fake model cover a multi-step task, invalid tool arguments, the step limit, prompt-like object names and refused deletion.

## Fast track: resume-ready version

To finish sooner, build this smaller version first. It takes an estimated 7 days at 4–6 hours a day, with help from an AI coding assistant. It replaces the 10-week schedule for the first release.

| Day | Build | Plan steps |
| --- | --- | --- |
| 1 | Service setup and validation | 1–2 |
| 2 | Streaming blob store | 3 |
| 3 | Metadata and crash recovery | 4 |
| 4 | Bucket, object and listing operations | 5 |
| 5 | S3-compatible HTTP API, without range reads at first | 6 |
| 6 | Signature V4 and AWS CLI check | 7 |
| 7 | Dockerfile, Docker Compose, CI, race tests and README | Parts of 1, 9, 10 and 12 |

The S3 API covers these operations. GoStore stores data on local disk, so it doesn't need Amazon S3 or an AWS account.

| Area | S3 operations |
| --- | --- |
| Buckets | CreateBucket, ListBuckets, DeleteBucket |
| Objects | PutObject, GetObject, HeadObject, DeleteObject |
| Listing | ListObjectsV2 |

The Docker files are `Dockerfile`, `docker-compose.yml` and `.dockerignore`. The fast version is done when `docker compose up` starts the service, AWS CLI can create a bucket and upload, list, download and delete a file, and data survives a container restart. Configure the client to use single uploads. Keep the recovery and authentication tests. Multipart upload, range reads, benchmarks and the AI agent come later.

## Schedule and release points

The core plan takes about 10 weeks, assuming 10–15 hours a week. This is a planning estimate, so review it after week 3.

| Week | Steps | Checkpoint |
| --- | --- | --- |
| 1 | 1–2 | Service skeleton, CI and validation work |
| 2 | 3 | Streaming and failure tests pass |
| 3 | 4 | Process-crash recovery tests pass |
| 4 | 5 | Bucket, object and listing tests pass |
| 5 | 6 | HTTP API works locally |
| 6 | 7 | Standard S3 clients work with authentication |
| 7 | 8 | Multipart upload works across restarts |
| 8 | 9 | Race, overload and disk-full tests pass |
| 9 | 10 | Container and runbook are complete |
| 10 | 11–12 | Measured, reviewable release |
| Extra 1–2 weeks | 13 | Optional AI agent |

If time is short, publish an early version after step 7 and label it as in progress. It still needs a README with setup instructions, supported operations and limitations, plus a check for committed secrets. It won't have multipart upload, so configure test clients to use single uploads. Defer step 13 and extra tuning before cutting recovery or authentication tests.

## Final checklist

- [ ] README covers setup, configuration, API examples, tests, durability, limitations and non-goals.
- [ ] CI passes formatting, tests, race detection, static analysis, client compatibility and container checks.
- [ ] The architecture diagram matches the code and shows the commit point.
- [ ] Crash recovery results are recorded, including what wasn't tested.
- [ ] The benchmark report includes commands, environment, results and interpretation.
- [ ] A clean machine or container can reproduce the demo.
- [ ] Every feature and performance claim matches the released code.
