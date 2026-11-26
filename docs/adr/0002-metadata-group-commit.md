# ADR 0002: Metadata Journal Group Commit

- **Status:** Accepted
- **Date:** 2026-11-26

## Context

Baseline performance measurements (see [benchmarks.md](../benchmarks.md)) showed that concurrent write workloads (`put_4k`, `put_1m`, and `mixed_rw`) plateaued around ~255–275 commits/s at concurrency 8–32 while leaving CPU mostly idle (`~18–24%` of one core). Profiling confirmed contention and serialization on `meta.Store.writeMu`, because each commit appended one JSON record to `meta.journal` and called `Sync()` serially while blocking all other writers.

## Decision

Batch concurrent metadata mutations in [`internal/meta/batch.go`](../../internal/meta/batch.go) using a leader-follower group commit queue:

1. Callers enqueue requests onto `pending` under `queueMu`. One active leader drains the queued batch, hand-off transferring leadership only after its own batch completes so the leader never absorbs a second batch's latency.
2. The leader evaluates every queued mutation sequentially against a per-batch `batchOverlay`, preserving exact linearizability for conflicting operations (e.g., same-key overwrites, `PutObject` vs `DeleteBucket`, `CompleteUpload` vs `AbortUpload`) without exposing uncommitted state to `RLock` readers.
3. Failed pre-validations return their domain errors without writing to disk. Valid encoded records are written in one `journal.Write(batchBuf)`, synced with **one** `journal.Sync()`, and applied to in-memory state under `mu.Lock()` before signaling callers.
4. No caller returns success until `journal.Sync()` succeeds. Each record retains its own length and CRC-32C checksums, so partial writes on crash remain recoverable via existing `replay()` logic.

## Consequences

- Preserves GoStore's strict durability contract (every acknowledged write is `fsync`-ed before returning) and keeps single-writer (`c=1`) latency overhead minimal.
- Scales `put_4k@32` from ~265 ops/s to ~2,300 ops/s, `put_1m@32` from ~212 MiB/s to ~1,023 MiB/s, and cuts `mixed_rw@32` `p99` latency from ~1,000ms to ~21ms.
