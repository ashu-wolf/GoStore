# GoStore

An S3-compatible object storage service in Go. **Work in progress.**

Done so far: server setup and health check, core types and validation, a streaming blob store,
a metadata journal, startup recovery, bucket/object operations including listing, and the
path-style S3 HTTP API (without Signature V4 authentication or multipart upload yet).

## Requirements

Linux, or WSL 2 on Windows. Storage relies on Linux filesystem behavior.

## Run

    make run
    curl http://127.0.0.1:9000/-/healthz

## Check

    make check

## Configuration

| Variable | Default |
|---|---|
| `GOSTORE_ADDR` | `127.0.0.1:9000` |
| `GOSTORE_DATA_DIR` | `./data` |
| `GOSTORE_SHUTDOWN_TIMEOUT` | `30s` |
| `GOSTORE_MAX_OBJECT_SIZE` | `1073741824` (1 GiB) |
| `GOSTORE_LIST_PAGE_SIZE` | `1000` |

## Durability

An upload is synced to a new blob file first. It becomes visible when its record is appended to
the metadata journal and synced. At startup, unfinished uploads and unused blobs are removed.
An unfinished record at the end of the journal is discarded. A complete record that fails its
checks stops startup, as does a missing journal when blob files exist.

Recovery tests kill the process at points in the write path. Power loss is not tested.

## Limitations

All metadata is held in memory, and metadata writes are synced one at a time.
Back up the journal and blob files together, with the server stopped. Restoring an older journal
makes newer blobs look unused, so startup removes them.
