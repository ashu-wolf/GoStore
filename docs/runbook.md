# GoStore Linux Operations Runbook

## 1. Service Endpoints

| Path | Purpose | Auth Required |
|---|---|---|
| `/-/healthz` | Liveness probe (`200 ok`) | No |
| `/-/readyz` | Readiness probe (`200 ready` while serving, `503 not ready` during shutdown) | No |
| `/-/metrics` | Prometheus metrics (`gostore_ready`, `gostore_active_requests`, `gostore_requests_total`, `gostore_errors_total`, `gostore_request_bytes_total`, `gostore_request_duration_seconds`) | No |
| `/debug/pprof/` | Go pprof profiles on `GOSTORE_PPROF_ADDR` (disabled by default; must bind to loopback) | No (loopback only) |
| `/` and `/{bucket}[/{key}]` | S3 HTTP API | Yes (AWS SigV4) |

> **Important:** Expose only S3 routes externally via your reverse proxy. Restrict `/-/metrics` to internal monitoring scrapers and never expose `GOSTORE_PPROF_ADDR` outside `127.0.0.1`/`::1`.

## 2. TLS via Reverse Proxy

GoStore speaks plain HTTP and expects TLS termination at a reverse proxy on the same host (or private network).

### Critical proxy requirements for S3 + SigV4
1. **Preserve the `Host` header** verbatim; SigV4 signs `host`.
2. **Do not rewrite or normalize URI paths** (`//`, `..`, `%2F`); S3 keys are arbitrary UTF-8 strings.
3. **Disable request and response buffering** so large streaming uploads/downloads do not buffer to proxy disk or delay SHA-256 connection-abort detection.
4. **Allow large bodies** (`client_max_body_size 0` in Nginx).

### Nginx example
```nginx
server {
    listen 443 ssl http2;
    server_name s3.example.com;

    ssl_certificate     /etc/letsencrypt/live/s3.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/s3.example.com/privkey.pem;

    # Block external access to internal operational endpoints.
    location ^~ /-/ {
        return 404;
    }

    location / {
        proxy_pass http://127.0.0.1:9000;
        proxy_http_version 1.1;

        proxy_set_header Host $http_host;
        proxy_set_header Connection "";

        client_max_body_size 0;
        proxy_request_buffering off;
        proxy_buffering off;

        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

### Caddy example
```caddyfile
s3.example.com {
    @ops path /-/*
    respond @ops 404

    reverse_proxy 127.0.0.1:9000 {
        flush_interval -1
        header_up Host {http.request.host}
    }
}
```

## 3. Running with Docker Compose

```bash
export GOSTORE_ACCESS_KEY="replace-with-access-key"
export GOSTORE_SECRET_KEY="replace-with-secret-key"
docker compose up -d
```

- The container runs as non-root UID/GID `10001` (`gostore`) with data persisted in the `gostore_data` volume at `/data`.
- `stop_grace_period` (`35s`) exceeds `GOSTORE_SHUTDOWN_TIMEOUT` (`30s`) so `SIGTERM` drains active requests cleanly before Docker escalates to `SIGKILL`.

## 4. Graceful Shutdown Behavior

On `SIGINT` or `SIGTERM`:
1. `/-/readyz` immediately returns `503 not ready`, and new S3 requests receive `503 ServiceUnavailable`.
2. Active HTTP listeners close and in-flight requests drain up to `GOSTORE_SHUTDOWN_TIMEOUT` (default `30s`).
3. If the deadline expires, active request contexts are canceled and sockets closed.
4. After all request handlers exit, background cleanup workers stop and `meta.journal` and the blob lock are closed.

## 5. Backup and Recovery

GoStore stores object data in `/data/blobs/` and metadata in `/data/meta.journal`.

> **Critical rule:** Back up `meta.journal` and `blobs/` **together as a consistent point-in-time copy with GoStore stopped** (or from a single atomic filesystem snapshot such as LVM/ZFS/EBS). Restoring an older `meta.journal` against newer `blobs/` causes startup recovery to delete newer blobs as orphans; restoring a newer `meta.journal` without its blobs leaves damaged/missing objects.

### Cold backup (Docker Compose)
```bash
docker compose stop gostore
docker run --rm -v gostore_gostore_data:/data:ro -v "$PWD":/backup alpine:3.21 \
  tar -C /data -czf "/backup/gostore-backup-$(date -u +%Y%m%dT%H%M%SZ).tar.gz" meta.journal blobs
docker compose start gostore
```

### Cold backup (Linux host / systemd)
```bash
sudo systemctl stop gostore
tar -C /var/lib/gostore -czf "/var/backups/gostore-$(date -u +%Y%m%dT%H%M%SZ).tar.gz" meta.journal blobs
sudo systemctl start gostore
```

### Restore & verify
```bash
docker compose stop gostore
docker run --rm -v gostore_gostore_data:/data -v "$PWD":/backup alpine:3.21 \
  sh -c 'rm -rf /data/meta.journal /data/blobs /data/tmp && tar -C /data -xzf /backup/gostore-backup.tar.gz && chown -R 10001:10001 /data'
docker compose start gostore
docker compose logs --tail=20 gostore
```
Check the startup log line `"storage recovered"`: verify `"damaged_objects":0` and inspect `orphans_removed` / `temp_removed`. Startup automatically refuses to boot if `meta.journal` is corrupt or missing when blobs are present.
