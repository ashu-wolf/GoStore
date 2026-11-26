# GoStore: System Architecture and Code Guide

Nov 26, 2026 · @Ashutosh

## Overview

GoStore is a single-node, S3-compatible object storage server written in Go for Linux. Object bytes are stored as immutable files, and metadata is held in memory and recorded in an append-only journal. This guide describes the code in the latest zip, reviewed on 26 November 2026.

There are two key ideas:

- **Data and names are separate.** A file is named with a random ID. The metadata record maps a bucket and key to that ID.
- **Metadata decides what is visible.** A new object appears only after its journal record is synced and applied in memory.

## Architecture

&#91;embedded content: GoStore architecture\]

Requests flow downward. `internal/domain` holds shared types and validation, `internal/config` loads settings, and `cmd/server` connects everything.

## Components

Each package has one main job.

| Component | Responsibility | Main files |
| --- | --- | --- |
| `cmd/server` | Starts and stops the service, and runs the optional pprof listener | `main.go` |
| `cmd/bench` | Starts a real server, sends signed load and records performance | `main.go` |
| `internal/config` | Reads environment variables, applies defaults and checks values | `config.go` |
| `internal/domain` | Shared types, errors, name validation and list tokens | `types.go`, `errors.go`, `validate.go`, `token.go` |
| `internal/server` | S3 routes, authentication, upload decoding, ranges, XML and request limits | `server.go`, `s3.go`, `sigv4.go`, `chunked.go`, `range.go`, `xml.go` |
| `internal/storage` | Coordinates file data and metadata | `engine.go`, `multipart.go`, `space.go` |
| `internal/blob` | Writes, reads and deletes data files, and locks the data directory | `store.go`, `recovery.go` |
| `internal/meta` | Metadata index, journal, group commit and listing | `store.go`, `journal.go`, `batch.go`, `multipart.go` |
| `internal/metrics` | Request, error, byte and latency metrics in Prometheus format | `metrics.go` |
| `internal/crashpoint` | Named points where crash tests kill the process | `crashpoint.go` |

### Data model

| Type | What it holds |
| --- | --- |
| `Bucket` | Name and creation time |
| `Object` | Bucket, key, blob ID, size, ETag, SHA-256, content type, user metadata and modified time |
| `Upload` | An unfinished multipart upload, including its last activity time |
| `Part` | Upload ID, part number, blob ID, size, ETag and modified time |
| `CompletedUpload` | The saved result of a completed upload, used to answer retries |

### Disk layout

```text
<data directory>/
  LOCK              prevents a second server process
  meta.journal      metadata history
  tmp/upload-*      unfinished uploads
  blobs/00 ... ff/  published data files, grouped by the first 2 ID characters
```

## How requests work

Every S3 request passes the same gates before reaching storage.

1. `ServeHTTP` creates a request ID. Paths under `/-/` (health, readiness and metrics) are handled at once, without authentication or the request limit.
2. An S3 request is rejected with 503 if the server is shutting down or all request slots are in use.
3. `verifySigV4` checks the signature.
4. The path is split into bucket and key, and the request goes to `handleService`, `handleBucket` or `handleObject`.
5. Errors are turned into S3 XML responses by `writeMappedError` and `writeError`.

### Upload (PutObject)

&#91;embedded content: PutObject write path\]

No metadata lock is held while the body streams. The commit checks again that the bucket still exists.

### Download (GetObject and HeadObject)

1. The engine looks up the metadata and opens the file. It checks the file size.
2. If the file has just been removed, `lookupObject` reads the metadata again. If the record still points to the same missing file, it returns `ErrDamaged`.
3. For a full download, `streamVerified` calculates SHA-256 while sending 64 KiB reads, always holding one chunk back.
4. The last chunk is sent only if the size and hash match. On a mismatch, the server returns an error or closes the connection.
5. A single byte range returns 206, or 416 if invalid. Range reads aren't hash-checked. HEAD sends headers and checks only that the file exists with the right size.

### Delete and listing

- **Delete:** the metadata record is removed first, then the file. Deleting a missing key succeeds.
- **ListObjectsV2:** the engine searches a sorted key list for each bucket. With a delimiter, it skips over each grouped prefix using binary search. The continuation token is Base64-encoded JSON tied to the bucket, prefix and delimiter. It isn't signed and isn't used for access control.

### Multipart upload

| Stage | What happens |
| --- | --- |
| Create | A random upload ID and an `Upload` record are saved |
| Upload part | Each part becomes its own file. Uploading the same number again replaces it |
| List parts | Returns part records in number order, one page at a time |
| Complete | Checks order, ETags and sizes, joins the parts into a new file, then publishes the object, removes the upload and saves the result in one journal record |
| Abort | Removes the upload record and deletes its part files |
| Expiry | A background worker runs every minute and aborts up to 100 inactive uploads per run. It also removes old completion results |

During completion, `multiBlobReader` reads one part file at a time. The multipart ETag is the MD5 of the binary part MD5 values, followed by `-` and the part count.

## Durability, recovery and concurrency

A successful change is reported only after the required syncs succeed. The exception to this found in review is listed under known issues. File deletions aren't synced, because startup cleanup can remove leftover files.

### Journal format

The journal starts with the header `GOSTORE-JOURNAL-1`. Each record has a 4-byte payload length, a 4-byte CRC-32C of that length, a 4-byte CRC-32C of the payload, and a JSON payload of up to 64 KiB. The separate length checksum stops a damaged length from looking like an unfinished record.

| Operation group | Record types |
| --- | --- |
| Buckets and objects | `create_bucket`, `delete_bucket`, `put_object`, `delete_object` |
| Multipart | `create_upload`, `put_part`, `abort_upload`, `complete_upload` |
| Completion results | `save_completion`, `purge_completion` |

### Group commit

Group commit lets concurrent changes share one disk sync.

1. Each caller adds a request to a queue. The first becomes the **leader**.
2. The leader takes the queued requests and checks them in order against a temporary view called `batchOverlay`. Later requests can see earlier ones, but readers can't.
3. It writes all valid records together and syncs once.
4. Only then does it update the real index and return results.
5. If more requests arrived, leadership passes to the next waiting caller.

If the write or sync fails, the store refuses further changes until restart, because it can't know what reached disk.

### Startup recovery

1. Lock the data directory.
2. List published files and replay the journal. An unfinished last record is discarded. A complete damaged record stops startup.
3. Create a new journal only if there are no files. This prevents a lost journal from causing all data to be deleted.
4. Compact the journal if its record count exceeds twice the live count plus 1,000.
5. Remove temporary files.
6. Report objects with missing or wrong-sized files.
7. Delete files that no object or upload part references.

| Crash happens | State after restart |
| --- | --- |
| Before the journal record is written | The old object remains |
| After a complete record is written but before sync, process crash only | The new object can appear, although the client got no success reply |
| After the sync | The new object remains |

These results cover process crashes. Power loss wasn't tested.

### Locks and pins

| Mechanism | Purpose |
| --- | --- |
| `queueMu` | Protects the group commit queue |
| `writeMu` | Allows one batch flush at a time, and guards closing |
| `mu` (read/write) | Protects the in-memory index. It isn't held during the disk sync |
| `spaceMu` | Protects reserved disk space |
| `pinMu` | Protects pins and deferred deletions |
| Upload pins | Stop expiry while a request is using an upload |
| File pins | Delay deleting part files while completion reads them |
| Request semaphore | Limits active S3 requests, 256 by default |

When two changes conflict, their order inside the metadata commit decides the result. For example, if a bucket deletion comes first, a competing upload fails with no such bucket. If the upload comes first, the deletion fails because the bucket isn't empty.

### Disk space and shutdown

- Known upload sizes are reserved in advance. Unknown sizes are checked as data arrives. A full disk returns HTTP 507.
- On shutdown, the server marks itself unready and drains requests for up to 30 seconds by default. It then cancels contexts and closes connections, waits for handlers and closes storage.

## Code walkthrough

Read the packages in the order below to follow the project from startup to disk. Most Go files that touch storage or the server are marked for Linux builds only.

### cmd/server: startup and shutdown

- `main` creates a JSON logger and calls `run`.
- `run` loads settings and creates a context that ends on SIGINT or SIGTERM.
- `runServer` opens storage, which completes recovery **before** the server listens. It then builds the handler, sets a 10-second header timeout and 60-second idle timeout, and manages shutdown.
- `startPprof` runs profiling routes on a separate server. The configuration accepts only a loopback address for it.

### internal/config and internal/domain: settings and rules

- `config.Load` requires an access key and secret key, and rejects invalid values.
- `ValidateBucketName` enforces length, allowed characters and reserved forms.
- `ValidateKey` allows up to 1,024 bytes of valid UTF-8 with no null byte. A key such as `../../etc/passwd` is accepted because it never becomes a file path.
- `ValidateMetadata` checks header names, printable ASCII values and a 2,048-byte total.
- `ListToken.Encode` and `DecodeListToken` handle continuation tokens.
- Domain errors such as `ErrNoSuchKey` don't depend on HTTP. Callers compare them with `errors.Is`.

### internal/blob: file data

| Function | What it does |
| --- | --- |
| `Open` (package) | Creates directories, takes the `flock` lock and creates all 256 subdirectories |
| `Put` | Creates a temporary file, streams, verifies, syncs, renames and syncs the directory |
| `Open` (method), `Delete` | Read or remove a file after validating its ID |
| `copyStream` | Copies through a 64 KiB buffer, checking cancellation and size limits between reads |
| `newID`, `validID` | Create and accept only 32-character hexadecimal IDs, which blocks path traversal |
| `RemoveTemp`, `IDs`, `Size` | Support startup recovery and size checks |

The `fileOps` structure lets tests simulate failures when creating, renaming or syncing.

### internal/meta: metadata

`Store` holds three maps: buckets, active uploads and completion results. Each bucket also keeps its keys in sorted order.

| Function | What it does |
| --- | --- |
| `Open` | Replays the journal, then compacts it or trims an unfinished record |
| `apply`, `applyMultipart` | Apply one record to memory, used during replay and after commit |
| `submit` | Queues a change and elects a leader |
| `flushBatch` | Validates, writes, syncs and applies a batch |
| `batchOverlay.stage` | Records a change in the temporary view |
| `ListObjects`, `startAt`, `nextPrefix` | Page through sorted keys and common prefixes |
| `CompleteUpload` | Checks that selected parts still match, then publishes in one record |
| `ExpireUploads` | Creates records that abort old uploads and purge old results |
| `encode`, `replay`, `writeJournal` | Encode and read records, and replace the journal atomically |

Each change is passed to `submit` as a `prepare` function. It checks the current state and returns the records to write. This core pattern appears in `CreateBucket`:

```go
return s.submit(func(ov *batchOverlay) ([]record, error) {
	if ov.hasBucket(b.Name) {
		return nil, domain.ErrBucketExists
	}
	return []record{{Op: opCreateBucket, Bucket: &b}}, nil
})
```

### internal/storage: the coordinator

- `OpenWithOptions` applies defaults, runs recovery and starts the expiry worker.
- `PutObject` validates, reserves space, writes the file and commits metadata. If a commit fails normally, it deletes the new file. If the result is `ErrCommitUnknown`, it keeps the file because the record may be on disk.
- `lookupObject` is a generic helper shared by `HeadObject` and `OpenObject`. It handles a file removed between the metadata read and file access.
- `ListObjects` validates options and creates continuation tokens.
- `reserveSpace`, `consume` and `release` track space needed by uploads in progress.
- `UploadPart` and `CompleteMultipartUpload` manage multipart data and pins.
- `deleteBlob` postpones deletion of a pinned file. `unpinBlobs` deletes it when the last pin is released.
- `Close` stops the worker, then closes the journal and releases the lock.

### internal/server: HTTP and S3

**`server.go` and `s3.go`**

- `ServeHTTP` reads the escaped path, so unusual keys aren't rewritten by path cleaning.
- `handleService`, `handleBucket` and `handleObject` choose the operation. They reject unsupported query options.
- `prepareUploadBody` adds the idle read deadline, size checks, checksum checks and chunk decoding. Both normal and part uploads use it.
- `streamVerified` protects full downloads. `deadlineReader` and `deadlineWriter` refresh idle deadlines on each read or write.
- `writeMappedError` converts internal errors into HTTP statuses and S3 error codes.
- `parseByteRange` in `range.go` handles normal, open-ended and suffix ranges.

**`sigv4.go`: authentication**

1. Parse the `Authorization` header and check the access key, region and service.
2. Allow at most 15 minutes of clock difference by default, and check the required signed headers.
3. Build a standard form of the request, called the canonical request.
4. Derive a key from the secret, date, region and service using HMAC-SHA256.
5. Compare signatures using `hmac.Equal`, which resists timing attacks.

**`chunked.go`: streamed uploads**

`awsChunkedReader` removes AWS chunk framing and passes on only object bytes. In signed modes, each signature depends on the previous one. It also checks trailing CRC-32, CRC-32C, CRC-64/NVME, SHA-1 or SHA-256 checksums. A chunk is limited to 16 MiB. Its signature is checked when the chunk ends, and the temporary file is removed if it fails.

### internal/metrics and internal/crashpoint

- `metrics.Registry` keeps readiness, active requests, bytes in and out, request and error counts, and latency histograms. Labels use operation names, status and error codes, never object keys.
- `crashpoint.Hit` does nothing in normal operation. Tests set `Hook` to kill a process at a named stage.

### cmd/bench: load testing

The tool starts a separate server and runs eight scenarios: 4 KiB, 1 MiB and 32 MiB uploads and downloads, mixed 64 KiB traffic, and prefix listing. It records latency, throughput, errors, CPU, memory, open files and disk use through Linux `/proc` and the data directory. It can also save CPU and heap profiles.

## Configuration, running and testing

Only the access key and secret key are required. All setting names below start with `GOSTORE_`.

| Setting | Default | Controls |
| --- | --- | --- |
| `ACCESS_KEY`, `SECRET_KEY` | Required | Request credentials |
| `ADDR` | `127.0.0.1:9000` | Listen address |
| `DATA_DIR` | `./data` | Storage folder |
| `REGION` | `us-east-1` | Signature region |
| `PPROF_ADDR` | Off | Local profiling address |
| `SHUTDOWN_TIMEOUT` | 30s | Time to drain requests |
| `IDLE_TRANSFER_TIMEOUT` | 60s | Stalled transfer limit |
| `MAX_CONCURRENT_REQUESTS` | 256 | Active S3 requests |
| `MAX_OBJECT_SIZE` | 1 GiB | Single upload or part, in bytes |
| `MAX_MULTIPART_SIZE` | 5 GiB | Completed object, in bytes |
| `MIN_PART_SIZE` | 5 MiB | All parts except the last, in bytes |
| `MAX_UPLOAD_PARTS` | 10,000 | Highest part number allowed |
| `UPLOAD_EXPIRY` | 24h | Inactive upload lifetime |
| `MIN_FREE_SPACE` | 1 GiB | Reserved disk space in bytes; 0 turns checks off |
| `LIST_PAGE_SIZE` | 1,000 | Maximum list page size |

Run these from the project folder on Linux or WSL 2. Use your own credentials outside local testing.

```bash
export GOSTORE_ACCESS_KEY=gostore-dev-access
export GOSTORE_SECRET_KEY=gostore-dev-secret
make run                        # start the server
make check                      # format, vet and race tests
make bench                      # benchmarks
bash ./scripts/demo.sh          # end-to-end demo
docker compose up -d            # run in a container
```

### Supported S3 operations

| Area | Operations |
| --- | --- |
| Buckets | ListBuckets, CreateBucket, DeleteBucket, ListObjectsV2 |
| Objects | PutObject, GetObject, HeadObject, DeleteObject |
| Multipart | Create, UploadPart, ListParts, Complete, Abort |

### Deployment and tests

- The Dockerfile builds in two stages and runs as non-root user 10001. Compose publishes port 9000 on localhost only and keeps data in a volume.
- GoStore serves plain HTTP. The runbook describes TLS through Nginx or Caddy and backups with the server stopped.
- The repository has tests for storage, crashes, concurrency, HTTP, authentication and metrics. Scripts check AWS CLI use and container persistence.

During the 26 November review, race tests passed in `cmd/bench`, `blob`, `config`, `domain`, `meta`, `metrics`, `storage` and `server`. This used Go 1.24.7 rather than 1.27.4. `sigv4_test.go` and the `cmd/server` tests were excluded because the AWS SDK wasn't available. AWS CLI, container, benchmark and vulnerability checks weren't run.

## Limitations and known issues

One correctness bug is still open, so step 11 isn't complete.

### Open issues in the reviewed code

| Issue | Effect | Suggested action |
| --- | --- | --- |
| `finishBatchFailed` can return success or a result based on unsaved changes | A second delete in a failed batch reported success while the object remained. This was reproduced with a forced sync failure | Send the disk error to every request in the batch and add a test |
| Readiness doesn't change after a journal commit failure | Later changes fail, but `/-/readyz` can still report ready | Connect the failed store state to readiness |
| `UPLOAD_EXPIRY` doesn't set completion retention | Completion results use the 24-hour default. The README says the setting controls both | Pass `CompletionRetention`, or correct the README |
| Benchmark comparison is affected by other differences | Reads and single-client tests also changed, so exact speedups aren't reliable | Alternate old and new versions under the same conditions |
| Some documentation is out of date | A `store.go` comment says writes sync one at a time. ADR 0001 mentions only health checks as bypassing the request limit, but all `/-/` paths do | Update the text |

The readiness, retention and bypass findings come from reading the code. They weren't tested while running.

### Design limitations

- It runs on one node, with no replication or erasure coding.
- All metadata must fit in memory.
- The journal is compacted only at startup, so it grows while the server runs.
- The implementation uses a custom journal, while the original plan chose bbolt. ADR 0003 records this change.
- There is no object versioning, IAM, presigned URL, virtual-hosted addressing or built-in encryption.
- Copy, ListObjects version 1, multiple byte ranges and listing active multipart uploads aren't supported.
- Range downloads aren't checked against SHA-256.
- Restoring a journal and files from different times can lose data: startup deletes files the journal doesn't reference.
