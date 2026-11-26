# GoStore

An S3-compatible object storage service in Go. **Work in progress.**

Done so far: server setup and health check, core types and validation, and a streaming blob store.
The blob store is not connected to the HTTP API yet, and there are no S3 operations yet.

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
