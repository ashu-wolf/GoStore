# GoStore Test and Verification Report

This report records the verification checks, crash-recovery matrix, fuzz runs, and known testing boundaries for the current GoStore codebase.

## 1. Automated Verification Summary

| Check | Command | Scope & Outcome |
|---|---|---|
| Formatting | `test -z "$(gofmt -l .)"` | Pass (`0` unformatted files) |
| `go vet` | `go vet ./...` | Pass across all packages |
| Static analysis | `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...` | Pass (`0` findings) |
| Vulnerability scan | `go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...` | Pass (`No vulnerabilities found`) |
| Unit & integration tests (`-race`) | `go test -race ./...` | Pass across all 8 tested packages (`cmd/bench`, `cmd/server`, `internal/blob`, `internal/config`, `internal/domain`, `internal/meta`, `internal/metrics`, `internal/server`, `internal/storage`; `internal/crashpoint` has no test files) |
| Repeated concurrency / stress checks | `go test -race -count=5 ./internal/meta ./internal/storage` and `go test -race -count=10 ./internal/server ./cmd/server` | Pass (`TestMixedConcurrencyStress`, `TestConcurrentServiceOperations`, `TestSemaphoreOverloadReturns503`, `TestRunServerGracefulDrainAndPprof`, `TestGroupCommitCoalescesConcurrentSyncs`) |
| Fuzzing (`-fuzztime=3s` each, 24 workers) | `go test -fuzz=<target> -fuzztime=3s ...` | Pass (`FuzzValidateBucketName`: 1,005,881 execs; `FuzzValidateKey`: 132,785 execs; `FuzzDecodeListToken`: 425,212 execs; `FuzzReplay`: 334,978 execs) |
| AWS CLI v2 compatibility | `bash ./scripts/aws_cli_check.sh` | Verified locally and in CI (`ubuntu-latest`, pinned AWS CLI v2 `2.42.4`) |
| Container & persistence | `bash ./scripts/container_check.sh` | Verified locally (`Container stopped in 385ms`, data preserved across `--force-recreate`) and in CI (`ubuntu-latest`) |
| End-to-end reviewer demo | `bash ./scripts/demo.sh` | Verified locally (single + 11 MiB multipart upload, prefix listing, metrics, restart persistence) |

## 2. Crash Recovery & Failure Injection Coverage

### Process Crash Recovery (`internal/storage/crash_test.go`)

`TestCrashRecovery` spawns real child processes that `SIGKILL` (`syscall.SIGKILL`) themselves mid-overwrite of key `"a"` (`"old"` -> `"nnn"`, alongside acknowledged key `"other"` = `"safe"`), then reopens the `Engine` and checks consistency:

| Crash Point | Killed When | Expected & Verified State After Reopen | Temp/Orphan Cleanup |
|---|---|---|---|
| `mid-stream` | Mid-read while streaming request body to temp file | `"a"` stays `"old"`, `"other"` stays `"safe"` | `TempRemoved`, `2` live blobs |
| `blob.before-rename` | After `f.Sync()` + `f.Close()`, before `os.Rename` | `"a"` stays `"old"`, `"other"` stays `"safe"` | Uncommitted temp file removed |
| `blob.after-rename` | After `os.Rename` to `blobs/xx/<id>`, before `syncDir` & metadata commit | `"a"` stays `"old"`, `"other"` stays `"safe"` | Unreferenced blob removed |
| `storage.before-commit` | After blob is synced & renamed, before `meta.journal` write | `"a"` stays `"old"`, `"other"` stays `"safe"` | Unreferenced blob removed |
| `meta.after-write` | After writing complete CRC-32C journal record, before `journal.Sync()` | `"a"` recovers as `"nnn"` (complete valid record preserved), `"other"` stays `"safe"` | Replaced `"old"` blob removed as orphan |
| `storage.after-commit` | After `meta.journal` commit & sync, before deleting `"old"` blob | `"a"` recovers as `"nnn"`, `"other"` stays `"safe"` | Replaced `"old"` blob removed as orphan |

### Additional Failure & Integrity Tests

- **Journal tail & corruption (`internal/meta/store_test.go`):**
  - `TestUnfinishedTailAtEveryByte`: truncates the final journal record at every byte boundary; startup discards only the partial tail while preserving prior records and allowing new writes.
  - `TestZeroFilledTail`: discards trailing NUL-allocated bytes safely.
  - `TestCorruptionStopsStartup`: flips bits in the file header, mid-file length/payload, last length/payload, and payload CRC-32C; confirms `Open` fails with `ErrCorrupt` without modifying the file.
  - `TestSyncFailureStopsFurtherWrites`: injected `Sync()` failure returns `ErrCommitUnknown`, leaves uncommitted record invisible, and blocks subsequent writes until restart.
- **Disk & blob failures (`internal/blob/store_test.go`, `internal/storage/concurrency_test.go`):**
  - `TestPutDiskFailures`: injected temp creation, rename, and directory sync failures clean up temporary files without leaving partial state.
  - `TestPutENOSPCLeavesNoTempFiles` & `TestDiskSpaceAccountingAndFullDiskProtection`: enforces space reservations and handles `ENOSPC` mid-write cleanly without disturbing committed objects.
- **Network / read integrity (`internal/server/s3_test.go`, `limits_test.go`):**
  - `TestCorruptedBlobAbortsFullDownload`: corrupting a stored blob causes `streamVerified` to withhold the final chunk and abort the connection before sending a complete `200 OK` body.
  - `TestSemaphoreOverloadReturns503` and `TestIdleTransferTimeoutAndCancellation`: enforce `503 SlowDown` overload protection and idle socket timeouts.

## 3. What Was Not Tested

- **Physical power loss / storage hardware cache faults:** `TestCrashRecovery` sends `SIGKILL` after OS writes; it tests process crash safety, not sudden power cut or storage devices that ignore `fsync` write-barriers.
- **Long multi-hour soak fuzzing:** Fuzz targets ran for `3s` per target locally (hundreds of thousands of executions each) plus seed corpora in every CI `go test -race ./...` run.
- **External TLS termination:** GoStore serves plain HTTP; TLS behavior is delegated to Nginx/Caddy (documented in [runbook.md](runbook.md)) rather than terminated inside Go.
- **Multi-node clustering or replication:** Out of scope for version 1.
