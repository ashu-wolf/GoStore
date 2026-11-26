# GoStore

An S3-compatible object storage service in Go. **Work in progress:** only the server setup and health check exist so far.

## Run (Windows PowerShell)

    go run ./cmd/server

In a second terminal:

    curl.exe -i http://127.0.0.1:9000/-/healthz

## Check

    powershell -ExecutionPolicy Bypass -File .\check.ps1

## Configuration

| Variable | Default |
|---|---|
| `GOSTORE_ADDR` | `127.0.0.1:9000` |
| `GOSTORE_DATA_DIR` | `./data` |
| `GOSTORE_SHUTDOWN_TIMEOUT` | `30s` |
