# GoStore Performance Baseline and Tuning Report

## Scope

Measured GoStore end-to-end over HTTP (including Signature V4 authentication, request handling, SHA-256/MD5 hashing, `fsync` + directory sync on blob publish, and metadata journal `fsync`) using [`cmd/bench`](../cmd/bench/main.go):

- **4 KiB objects:** `put_4k`, `get_4k` at concurrency `1`, `8`, `32`
- **1 MiB objects:** `put_1m`, `get_1m` at concurrency `1`, `8`, `32`
- **32 MiB large transfers:** `put_32m`, `get_32m` at concurrency `1`, `4`
- **Mixed reads & writes (75% GET / 25% PUT, 64 KiB):** `mixed_rw` at concurrency `1`, `8`, `32`
- **Prefix listings (`ListObjectsV2`, 2,000 seeded keys across 10 tenants × 10 days):** `list_prefix` at concurrency `1`, `8`, `32`

Every `(scenario, concurrency)` cell ran a 1-op-per-worker warmup followed by a 3-second measurement window (minimum 2 ops/worker for 32 MiB transfers), repeated twice before and twice after the change.

## Test Environment

| Component | Value |
|---|---|
| OS / Arch | `linux/amd64` (WSL 2 kernel `6.18.58.1-microsoft-standard-WSL2`) |
| CPU | 13th Gen Intel Core i7-13700HX (`24` vCPUs exposed to WSL 2) |
| RAM | `7790 MiB` |
| Go | `go1.27.4` |
| Filesystem | `ext4` (`/var/tmp` on `/dev/sdd`, 952 GiB available) |
| Cache & Sync Conditions | Warm OS page cache (1-op warmup per worker); explicit `f.Sync()`, directory `fsync`, and `meta.journal` `Sync()` enabled on every write |

## Reproducible Commands

Run the full benchmark suite (two repeated runs) and capture CPU/heap profiles:

```bash
go run ./cmd/bench \
  -data-dir=/var/tmp \
  -duration=3s \
  -concurrency=1,8,32 \
  -large-concurrency=1,4 \
  -pprof-dir=/var/tmp/gostore-pprof \
  -pprof-scenario=put_4k@8 \
  -json=/var/tmp/bench-run1.json

go run ./cmd/bench \
  -data-dir=/var/tmp \
  -duration=3s \
  -concurrency=1,8,32 \
  -large-concurrency=1,4 \
  -json=/var/tmp/bench-run2.json
```

Inspect profiles:

```bash
go tool pprof -top -nodecount=15 /var/tmp/gostore-pprof/put_4k_8_cpu.pb.gz
go tool pprof -top -cum -nodecount=15 /var/tmp/gostore-pprof/put_4k_8_cpu.pb.gz
```

## Baseline Results (Before Optimization)

Two repeated baseline runs (`Run 1` / `Run 2`). All runs reported `0` errors.

| Scenario | Conc | Ops/s (R1 / R2) | MiB/s (R1 / R2) | p50 ms (R1 / R2) | p95 ms (R1 / R2) | p99 ms (R1 / R2) | CPU % (R1 / R2) | Peak RSS MiB (R1 / R2) | Peak FDs (R1 / R2) | Disk MiB (R1 / R2) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `put_4k` | 1 | 118.6 / 133.3 | 0.46 / 0.52 | 8.12 / 7.22 | 10.88 / 9.94 | 12.17 / 11.77 | 8.7 / 9.3 | 15.8 / 15.7 | 13 / 13 | 1.5 / 1.7 |
| `put_4k` | 8 | 251.5 / 273.3 | 0.98 / 1.07 | 31.35 / 28.94 | 38.93 / 33.94 | 40.91 / 42.63 | 17.9 / 20.5 | 17.8 / 16.6 | 27 / 22 | 4.4 / 4.8 |
| `put_4k` | 32 | 254.9 / 275.2 | 1.00 / 1.07 | 124.33 / 116.65 | 148.34 / 126.72 | 151.72 / 142.58 | 19.8 / 23.5 | 19.3 / 19.3 | 49 / 46 | 7.1 / 7.7 |
| `get_4k` | 1 | 3740.3 / 3781.3 | 14.61 / 14.77 | 0.23 / 0.23 | 0.50 / 0.47 | 0.68 / 0.59 | 120.0 / 122.0 | 20.2 / 20.4 | 45 / 44 | 7.6 / 8.2 |
| `get_4k` | 8 | 9866.3 / 10760.1 | 38.54 / 42.03 | 0.78 / 0.73 | 1.52 / 1.40 | 2.00 / 1.75 | 419.3 / 401.3 | 24.7 / 25.8 | 50 / 50 | 7.6 / 8.2 |
| `get_4k` | 32 | 18914.2 / 19341.3 | 73.88 / 75.55 | 1.24 / 1.22 | 4.75 / 4.58 | 7.08 / 6.86 | 710.5 / 692.2 | 55.6 / 50.8 | 66 / 62 | 7.6 / 8.2 |
| `put_1m` | 1 | 89.0 / 92.4 | 89.01 / 92.44 | 10.94 / 10.55 | 14.05 / 13.21 | 17.62 / 15.29 | 29.9 / 29.9 | 38.9 / 26.8 | 45 / 44 | 135.7 / 136.3 |
| `put_1m` | 8 | 213.2 / 215.4 | 213.19 / 215.43 | 36.66 / 37.05 | 43.76 / 42.22 | 46.31 / 44.84 | 61.2 / 60.5 | 23.0 / 23.2 | 48 / 46 | 135.9 / 136.6 |
| `put_1m` | 32 | 217.2 / 206.6 | 217.24 / 206.55 | 147.31 / 154.22 | 166.67 / 163.96 | 171.81 / 172.74 | 68.4 / 61.8 | 26.3 / 26.5 | 52 / 46 | 136.1 / 136.8 |
| `get_1m` | 1 | 867.9 / 917.2 | 867.93 / 917.16 | 1.09 / 1.05 | 1.54 / 1.42 | 1.81 / 1.70 | 107.3 / 107.0 | 23.1 / 23.4 | 45 / 44 | 168.2 / 168.8 |
| `get_1m` | 8 | 3156.0 / 3557.0 | 3155.99 / 3557.04 | 2.50 / 2.10 | 3.64 / 3.44 | 4.18 / 3.98 | 690.3 / 690.7 | 26.5 / 27.6 | 52 / 52 | 168.2 / 168.8 |
| `get_1m` | 32 | 5102.0 / 4945.1 | 5101.99 / 4945.05 | 4.64 / 4.87 | 14.40 / 14.90 | 19.67 / 20.22 | 1159.3 / 1150.0 | 37.9 / 41.0 | 70 / 73 | 168.2 / 168.8 |
| `put_32m` | 1 | 11.1 / 11.0 | 356.08 / 350.97 | 86.28 / 89.84 | 110.22 / 103.90 | 113.50 / 103.94 | 77.6 / 74.2 | 32.0 / 31.8 | 45 / 45 | 424.2 / 424.8 |
| `put_32m` | 4 | 26.6 / 20.1 | 849.95 / 641.86 | 148.38 / 170.73 | 166.72 / 210.55 | 182.82 / 564.23 | 219.5 / 186.9 | 26.8 / 32.1 | 48 / 48 | 424.2 / 424.8 |
| `get_32m` | 1 | 33.8 / 37.0 | 1081.08 / 1182.53 | 29.20 / 26.79 | 32.86 / 28.56 | 34.58 / 29.16 | 101.0 / 100.5 | 24.6 / 25.7 | 45 / 45 | 552.2 / 552.8 |
| `get_32m` | 4 | 108.7 / 107.6 | 3478.55 / 3441.98 | 36.47 / 36.53 | 38.51 / 40.99 | 40.22 / 42.84 | 400.2 / 399.9 | 25.4 / 26.7 | 48 / 48 | 552.2 / 552.8 |
| `mixed_rw` | 1 | 139.0 / 88.5 | 8.69 / 5.53 | 0.72 / 0.86 | 26.86 / 71.75 | 81.16 / 87.02 | 14.0 / 11.4 | 23.9 / 25.0 | 45 / 45 | 560.3 / 560.9 |
| `mixed_rw` | 8 | 339.3 / 150.8 | 21.21 / 9.42 | 0.38 / 0.95 | 199.33 / 288.97 | 285.83 / 331.96 | 27.7 / 18.4 | 25.1 / 25.7 | 47 / 52 | 560.4 / 560.9 |
| `mixed_rw` | 32 | 178.8 / 201.6 | 11.17 / 12.60 | 0.96 / 0.74 | 927.39 / 815.17 | 998.77 / 1000.50 | 24.1 / 21.7 | 25.2 / 27.4 | 60 / 47 | 560.4 / 561.0 |
| `list_prefix` | 1 | 5345.6 / 5154.2 | 14.87 / 14.34 | 0.18 / 0.18 | 0.26 / 0.27 | 0.44 / 0.46 | 82.7 / 83.7 | 26.7 / 27.5 | 44 / 44 | 561.4 / 562.0 |
| `list_prefix` | 8 | 15944.7 / 16233.4 | 44.36 / 45.17 | 0.43 / 0.42 | 0.99 / 0.97 | 1.25 / 1.24 | 494.6 / 467.6 | 28.0 / 29.4 | 44 / 44 | 561.4 / 562.0 |
| `list_prefix` | 32 | 21940.6 / 22024.2 | 61.04 / 61.27 | 1.14 / 1.17 | 3.80 / 3.72 | 5.34 / 5.28 | 725.4 / 717.0 | 31.3 / 31.2 | 44 / 44 | 561.4 / 562.0 |

## Profiling Analysis and Bottleneck Identification

1. **Write-path saturation (`put_4k`, `put_1m`, `mixed_rw`):**
   Throughput for `put_4k` plateaued around **~255–275 ops/s** from `c=8` to `c=32` while using only **~18–24% of 1 CPU core**, and `put_4k` median latency quadrupled linearly from `~30ms` (`c=8`) to `~120ms` (`c=32`). Similarly, `mixed_rw` `p99` latency jumped to `998.77–1000.50ms` at `c=32` because every 25% write waited behind serialized metadata commits.
   In the baseline `put_4k@8` CPU profile (`360ms` total CPU samples across 3s wall time, `12.00%` of 1 core), `38.89%` (`140ms`) was `Syscall6` and `30.56%` (`110ms`) was `runtime.futex` from workers waking and contending on `meta.Store.writeMu`, because `meta.Store` executed `encode` + `journal.Write` + `journal.Sync()` one record at a time.
2. **Secondary observation (read/allocation path, left untouched in this step):**
   The `get_1m@8` CPU profile (`18.22s` samples over 3s) showed `45.39%` in hardware `sha256.blockSHANI` (mandatory full-read integrity check) and `29.14%` in `Syscall6` / `deadlineWriter.Write`, while `alloc_space` showed `streamVerified` allocating fresh `2 × 64 KiB` buffers per request. We kept this change strictly focused on the dominant write-serialization bottleneck.

## Optimization Implemented: Metadata Journal Group Commit

Implemented leader-follower **group commit** in [`internal/meta/batch.go`](../internal/meta/batch.go), [`internal/meta/store.go`](../internal/meta/store.go), and [`internal/meta/multipart.go`](../internal/meta/multipart.go) (see [ADR 0002](adr/0002-metadata-group-commit.md)):

- Concurrent mutators enqueue onto `pending` under `queueMu`. The leader validates queued ops sequentially through a per-batch `batchOverlay` (handling conflicts such as same-key overwrites or `PutObject` vs `DeleteBucket` inside the same batch), appends all encoded self-checksummed records in a single `journal.Write(batchBuf)`, executes **one** `journal.Sync()`, applies committed changes under `mu.Lock()`, and wakes waiters.
- **Disk syncing semantics are unchanged:** no mutating call returns success before its record is written to `meta.journal` and `Sync()` returns `nil`.

## Post-Optimization Results (After Group Commit)

Two repeated post-change runs (`Run 1` / `Run 2`). All runs reported `0` errors.

| Scenario | Conc | Ops/s (R1 / R2) | MiB/s (R1 / R2) | p50 ms (R1 / R2) | p95 ms (R1 / R2) | p99 ms (R1 / R2) | CPU % (R1 / R2) | Peak RSS MiB (R1 / R2) | Peak FDs (R1 / R2) | Disk MiB (R1 / R2) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `put_4k` | 1 | 132.5 / 163.0 | 0.52 / 0.64 | 6.41 / 5.95 | 8.14 / 7.39 | 13.43 / 8.36 | 6.7 / 7.0 | 15.7 / 15.8 | 13 / 12 | 1.7 / 2.1 |
| `put_4k` | 8 | 682.8 / 705.6 | 2.67 / 2.76 | 11.34 / 11.22 | 14.59 / 13.15 | 17.32 / 13.90 | 39.2 / 37.6 | 19.6 / 18.4 | 28 / 26 | 9.4 / 10.1 |
| `put_4k` | 32 | 2270.4 / 2365.3 | 8.87 / 9.24 | 13.23 / 12.78 | 17.93 / 17.27 | 20.36 / 18.70 | 224.4 / 210.4 | 28.1 / 28.0 | 72 / 71 | 31.9 / 33.5 |
| `get_4k` | 1 | 4941.7 / 5463.2 | 19.30 / 21.34 | 0.19 / 0.17 | 0.36 / 0.29 | 0.48 / 0.43 | 118.0 / 113.3 | 28.6 / 28.7 | 45 / 43 | 32.4 / 34.0 |
| `get_4k` | 8 | 17195.8 / 18759.9 | 67.17 / 73.28 | 0.39 / 0.35 | 0.98 / 0.91 | 1.29 / 1.18 | 489.9 / 474.6 | 35.0 / 34.9 | 51 / 50 | 32.4 / 34.0 |
| `get_4k` | 32 | 17478.1 / 18288.3 | 68.27 / 71.44 | 1.71 / 1.69 | 4.03 / 3.87 | 5.56 / 5.10 | 601.6 / 586.6 | 33.2 / 43.2 | 67 / 63 | 32.4 / 34.0 |
| `put_1m` | 1 | 109.4 / 99.5 | 109.44 / 99.49 | 8.95 / 8.52 | 10.36 / 10.60 | 12.01 / 12.50 | 29.6 / 26.0 | 30.2 / 49.1 | 45 / 43 | 160.5 / 162.1 |
| `put_1m` | 8 | 435.9 / 433.1 | 435.87 / 433.14 | 18.15 / 18.24 | 22.00 / 22.13 | 24.68 / 25.03 | 119.5 / 116.3 | 27.3 / 28.0 | 52 / 50 | 161.0 / 162.6 |
| `put_1m` | 32 | 1020.2 / 1026.1 | 1020.16 / 1026.12 | 30.77 / 30.78 | 40.06 / 39.67 | 45.61 / 43.26 | 399.5 / 399.8 | 31.1 / 31.2 | 76 / 74 | 162.0 / 163.6 |
| `get_1m` | 1 | 995.6 / 995.7 | 995.62 / 995.65 | 0.98 / 0.98 | 1.16 / 1.12 | 1.50 / 1.54 | 106.0 / 105.0 | 28.0 / 28.5 | 45 / 43 | 194.0 / 195.6 |
| `get_1m` | 8 | 5390.8 / 5389.0 | 5390.77 / 5388.98 | 1.38 / 1.39 | 2.27 / 2.24 | 2.67 / 2.69 | 722.1 / 720.0 | 31.9 / 33.8 | 52 / 50 | 194.0 / 195.6 |
| `get_1m` | 32 | 8412.2 / 7992.1 | 8412.21 / 7992.05 | 3.20 / 3.35 | 8.17 / 8.71 | 11.97 / 12.50 | 1321.5 / 1315.6 | 53.9 / 52.0 | 71 / 68 | 194.0 / 195.6 |
| `put_32m` | 1 | 9.4 / 9.2 | 301.17 / 295.52 | 93.57 / 94.67 | 107.95 / 108.87 | 119.52 / 116.41 | 63.3 / 60.7 | 44.4 / 40.6 | 45 / 43 | 450.0 / 451.6 |
| `put_32m` | 4 | 28.5 / 28.1 | 912.61 / 898.42 | 140.68 / 141.03 | 156.00 / 161.85 | 156.51 / 169.18 | 200.9 / 190.4 | 44.9 / 40.6 | 48 / 46 | 450.1 / 451.7 |
| `get_32m` | 1 | 37.0 / 37.8 | 1183.69 / 1210.82 | 26.86 / 26.35 | 28.45 / 27.23 | 29.17 / 27.82 | 100.6 / 100.6 | 29.0 / 30.1 | 45 / 43 | 578.1 / 579.7 |
| `get_32m` | 4 | 141.9 / 145.3 | 4540.71 / 4651.03 | 27.79 / 27.22 | 30.44 / 28.91 | 34.18 / 34.00 | 401.5 / 400.0 | 30.0 / 31.9 | 48 / 46 | 578.1 / 579.7 |
| `mixed_rw` | 1 | 547.1 / 551.8 | 34.19 / 34.49 | 0.27 / 0.27 | 6.80 / 6.83 | 7.76 / 8.11 | 20.3 / 21.0 | 29.2 / 30.1 | 45 / 43 | 586.2 / 587.9 |
| `mixed_rw` | 8 | 2163.2 / 2050.7 | 135.20 / 128.17 | 0.31 / 0.37 | 14.83 / 15.69 | 16.25 / 18.15 | 89.1 / 96.6 | 31.2 / 32.3 | 51 / 49 | 586.8 / 588.4 |
| `mixed_rw` | 32 | 6785.8 / 5466.9 | 424.11 / 341.68 | 1.01 / 1.27 | 17.69 / 18.80 | 20.26 / 21.78 | 400.1 / 384.2 | 36.7 / 36.8 | 75 / 72 | 588.5 / 589.8 |
| `list_prefix` | 1 | 5610.1 / 5423.3 | 15.61 / 15.09 | 0.17 / 0.18 | 0.23 / 0.24 | 0.34 / 0.38 | 81.3 / 83.3 | 31.0 / 32.1 | 44 / 42 | 589.5 / 590.7 |
| `list_prefix` | 8 | 27078.2 / 19724.7 | 75.34 / 54.88 | 0.25 / 0.35 | 0.61 / 0.84 | 0.83 / 1.13 | 483.3 / 490.6 | 33.1 / 34.7 | 44 / 42 | 589.5 / 590.7 |
| `list_prefix` | 32 | 41785.6 / 30610.5 | 116.26 / 85.16 | 0.61 / 0.84 | 1.81 / 2.48 | 2.56 / 3.48 | 885.2 / 899.7 | 38.9 / 42.1 | 44 / 42 | 589.5 / 590.7 |

## Before-and-After Summary

| Workload (Concurrency) | Before (Run 1 / Run 2) | After (Run 1 / Run 2) | Change |
|---|---:|---:|---|
| `put_4k` (`c=8`) | 251.5 / 273.3 ops/s (`p50`: 31.35 / 28.94 ms) | **682.8 / 705.6 ops/s** (`p50`: 11.34 / 11.22 ms) | **~2.6× throughput**, **~2.7× lower p50** |
| `put_4k` (`c=32`) | 254.9 / 275.2 ops/s (`p50`: 124.33 / 116.65 ms) | **2270.4 / 2365.3 ops/s** (`p50`: 13.23 / 12.78 ms) | **~8.7× throughput**, **~9.2× lower p50** |
| `put_1m` (`c=8`) | 213.19 / 215.43 MiB/s (`p50`: 36.66 / 37.05 ms) | **435.87 / 433.14 MiB/s** (`p50`: 18.15 / 18.24 ms) | **~2.0× throughput**, **~2.0× lower p50** |
| `put_1m` (`c=32`) | 217.24 / 206.55 MiB/s (`p50`: 147.31 / 154.22 ms) | **1020.16 / 1026.12 MiB/s** (`p50`: 30.77 / 30.78 ms) | **~4.8× throughput**, **~4.9× lower p50** |
| `mixed_rw` (`c=8`) | 339.3 / 150.8 ops/s (`p99`: 285.83 / 331.96 ms) | **2163.2 / 2050.7 ops/s** (`p99`: 16.25 / 18.15 ms) | **~6.4–13.6× throughput**, **~18× lower p99** |
| `mixed_rw` (`c=32`) | 178.8 / 201.6 ops/s (`p99`: 998.77 / 1000.50 ms) | **6785.8 / 5466.9 ops/s** (`p99`: 20.26 / 21.78 ms) | **~27–38× throughput**, **~47× lower p99** |
| `put_32m` (`c=1`) | 356.08 / 350.97 MiB/s | 301.17 / 295.52 MiB/s | Single large-transfer path remains I/O + hash bound (no significant gain) |
| `put_32m` (`c=4`) | 849.95 / 641.86 MiB/s | 912.61 / 898.42 MiB/s | Marginal gain (~1.1–1.4×); dominated by 32 MiB streaming & blob sync |

In the post-change `put_4k@8` CPU profile, active server CPU rose from `12.00%` (`360ms` in 3s) to `28.99%` (`870ms` in 3s), while `runtime.futex` lock/wakeup overhead dropped from `30.56%` to `10.34%` as worker goroutines overlapped blob writes and `fsync` calls instead of queuing one-by-one behind `meta.Store.writeMu`.
