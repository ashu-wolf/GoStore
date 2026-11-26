# ADR 0001: Concurrency Conflicts, Commit Ordering, and Resource Limits

- **Status:** Accepted
- **Date:** 2026-11-26

## Context

GoStore streams object bodies to and from disk without holding metadata locks or open transactions during network I/O. Multiple clients can operate concurrently on the same bucket, key, or multipart upload while background expiry runs. We need clear conflict-resolution rules and bounded memory, concurrency, and disk-space usage.

## Decision

1. **Metadata journal serialization (`meta.Store.writeMu`)** is the single linearizability point for all state mutations. Blob writes stream concurrently to unique temp/blob files before acquiring `writeMu`.
2. **Active resource pins (`uploadPins`, `blobPins`)** prevent background expiry and concurrent deletions from unlinking part blobs still being read during multipart assembly (`CompleteMultipartUpload`) or expiring an upload with an in-flight request.
3. **Bounded data concurrency** is enforced at the HTTP layer with a non-blocking semaphore (`GOSTORE_MAX_CONCURRENT_REQUESTS`, default `256`). When full, data requests immediately receive `503 SlowDown` with `Retry-After: 1`. Operational `/-/healthz` bypasses this semaphore.
4. **Idle-transfer deadlines** (`GOSTORE_IDLE_TRANSFER_TIMEOUT`, default `60s`) refresh on each read/write chunk via `http.ResponseController`, timing out stalled transfers without cutting off long, active large uploads/downloads.
5. **Disk space accounting** reserves known upload and multipart assembly sizes against `statfs` available space minus `GOSTORE_MIN_FREE_SPACE` (default `1 GiB`), decrementing remaining reservation as bytes land on disk, and treats `ENOSPC` mid-write as a clean failure that removes the temp file without altering committed state.

## Conflict Matrix and Deciding Transactions

| Conflicting Operations | How Conflict Is Handled | Deciding Transaction / Mechanism |
|---|---|---|
| `PutObject` vs `PutObject` (same key) | Both stream to separate temp/blob files concurrently. | Second `meta.Store.PutObject` (`opPutObject`) under `writeMu` wins; the earlier blob is deleted after the second commit. |
| `PutObject` vs `DeleteObject` (same key) | `PutObject` streams outside locks; delete commits metadata removal. | Order under `meta.Store.writeMu` (`opPutObject` vs `opDeleteObject`) decides final visibility. |
| `OpenObject` / `HeadObject` vs overwrite or `DeleteObject` | On Linux, an already-opened fd stays readable after `unlink`. If unlinked before `Open`/`Size`, `lookupObject` retries `meta.Object`. | `meta.Store.Object` under `mu.RLock` sees either the new version or `ErrNoSuchKey`. |
| `PutObject` vs `DeleteBucket` | `PutObject` checks `HasBucket` early, then re-checks atomically at commit. | `meta.Store.writeMu` (`opPutObject` vs `opDeleteBucket`). If `DeleteBucket` wins first, `PutObject` gets `ErrNoSuchBucket` and deletes its uncommitted blob; if `PutObject` wins first, `DeleteBucket` gets `ErrBucketNotEmpty`. |
| `CreateBucket` vs `CreateBucket` | Both validate name independently. | First `opCreateBucket` under `writeMu` succeeds; second gets `ErrBucketExists`. |
| `UploadPart` vs `UploadPart` (same part number) | Both stream to independent blobs concurrently. | Later `opPutPart` under `writeMu` wins; replaced part blob deletion is deferred if pinned by an assembly. |
| `CompleteMultipartUpload` vs `AbortMultipartUpload` | `Complete` pins existing part blobs under `pinMu` and assembles outside `writeMu`; `Abort` commits immediately and defers deleting pinned blobs. | `opCompleteUpload` vs `opAbortUpload` under `writeMu` decides the winner (`ErrNoSuchUpload` for loser); loser cleans up unused blobs. |
| `CompleteMultipartUpload` vs `CompleteMultipartUpload` | Both may assemble if racing before commit; once committed, a 24h completion record is saved. | First `opCompleteUpload` under `writeMu` commits; second sees the saved `CompletedUpload`, deletes its extra blob, and returns the identical result. |
| `CompleteMultipartUpload` vs `UploadPart` (part replaced during assembly) | Assembly reads the pinned snapshot of parts. | `opCompleteUpload` re-verifies each part's `BlobID` under `writeMu`; if changed, `Complete` fails with `ErrInvalidPart` and deletes its stale assembled blob. |
| `ExpireUploads` vs `UploadPart` / `ListParts` / `CompleteMultipartUpload` | Active requests hold `uploadPins` and `UploadPart` refreshes `LastActiveAt`. | `ExpireUploads` skips pinned uploads and commits `opAbortUpload` under `writeMu` only for inactive uploads past expiry. |
| `ListObjects` vs writes | Seek into sorted key index under `mu.RLock`. | `meta.Store.mu` provides a consistent page snapshot without blocking streaming I/O. |
