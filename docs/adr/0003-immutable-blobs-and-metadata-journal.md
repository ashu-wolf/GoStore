# ADR 0003: Immutable Sharded Blobs and In-Memory CRC-32C Metadata Journal

- **Status:** Accepted
- **Date:** 2026-11-26

## Context

GoStore needs crash-safe object persistence with linearizable overwrites/deletes, prefix listing in UTF-8 byte order, and streaming I/O without external database dependencies or path-traversal risks from arbitrary S3 keys.

## Decision

1. **Immutable, randomly addressed blob files (`internal/blob`):** Every upload and multipart assembly writes to `tmp/upload-*`, syncs, and renames to `blobs/<id[:2]>/<id>` using a 128-bit random hex ID. User-supplied bucket and key strings never enter filesystem paths. Published blobs are never modified in place.
2. **In-memory metadata index backed by a CRC-32C append-only journal (`internal/meta`):** Instead of adding an external embedded KV dependency (`bbolt`) in version 1, GoStore holds bucket/object/upload state in memory (including a sorted key slice per bucket for `ListObjectsV2` seeks and prefix-delimiter binary-search skipping) and persists every mutation as a length-prefixed, dual-CRC-32C JSON record synced to `meta.journal` prior to visibility. Startup replays and compacts the journal.
3. **Single-node S3 scope (`internal/server`):** Implement path-style addressing, Signature V4 header authentication (signed, unsigned, and `aws-chunked`), and full-object SHA-256 verification on reads (holding back the final chunk and closing the socket on mismatch).

## Consequences

- **Benefits:** Zero external CGo or database dependencies; fast in-memory reads and prefix/delimiter listings; simple startup recovery (`blobs/` + `meta.journal` form the complete durable state).
- **Trade-offs:** All object metadata must fit in RAM, startup time scales with journal replay, and single-node local storage has no replication, object versioning, presigned URLs, or virtual-hosted-style routing.
