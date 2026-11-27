# Function reference

This tab describes the 226 functions and methods in the non-test Go files of the latest reviewed zip. Test functions and unnamed inline functions aren't listed. Names such as `Store.Put` mean the `Put` method on `Store`.

## Startup, configuration, domain and blob files

### cmd/server/main.go

| Function | What it does |
| --- | --- |
| `main` | Creates the JSON logger, calls `run`, and exits with code 1 on error |
| `run` | Loads settings, listens for SIGINT and SIGTERM, and calls `runServer` |
| `runServer` | Opens storage, builds the handler, serves requests and shuts down in order |
| `startPprof` | Starts the profiling routes on a separate server and returns it |

### internal/config/config.go

| Function | What it does |
| --- | --- |
| `Load` | Returns the settings from environment variables with defaults, or a validation error |
| `env` | Returns a variable's value, or the fallback if empty |
| `isLocalhostAddr` | Reports whether a host and port use `localhost` or a loopback IP |

### internal/domain and internal/crashpoint

| Function | What it does |
| --- | --- |
| `ListToken.Encode` | Converts a list position to URL-safe Base64 JSON |
| `DecodeListToken` | Parses a token and checks that it matches the bucket, prefix and delimiter |
| `ValidateBucketName` | Checks bucket naming rules |
| `isLowerOrDigit` | Reports whether a byte is a lowercase letter or digit |
| `ValidateKey` | Rejects empty, too long, non-UTF-8 or null-containing keys |
| `ValidateMetadata` | Checks metadata names, values and total size |
| `validMetadataName` | Checks that a name is a lowercase HTTP header token |
| `Hit` | Calls the test hook at a named crash point, if one is set |

### internal/blob/store.go

| Function | What it does |
| --- | --- |
| `osOps` | Returns the real create, rename and directory sync operations |
| `Open` | Prepares directories, locks the data folder and returns a `Store` |
| `Store.Close` | Closes the lock file, which releases the lock |
| `Store.Put` | Streams, hashes, verifies, syncs and publishes a file; returns its ID, size and hashes |
| `Store.Open` | Opens a published file for reading and seeking |
| `Store.Delete` | Removes a file; a missing file counts as success |
| `Store.path` | Builds a file path from an ID |
| `copyStream` | Copies with a 64 KiB buffer, enforcing cancellation and size limits |
| `wrapDiskErr` | Adds context to a disk error and marks a full disk as `ErrNoSpace` |
| `verify` | Compares size, MD5 and SHA-256 with expected values |
| `newID` | Creates a random 32-character hexadecimal ID |
| `validID` | Accepts only that ID format |
| `lockDir` | Takes an exclusive, non-waiting lock on the data folder |
| `mkdirAllSynced` | Creates missing directories and syncs their parents |
| `syncDir` | Syncs a directory to disk |

### internal/blob/recovery.go

| Function | What it does |
| --- | --- |
| `Store.RemoveTemp` | Deletes leftover temporary files and returns the count |
| `Store.IDs` | Returns the IDs of correctly placed published files |
| `Store.Size` | Returns a file's size or `ErrNotFound` |

## Metadata (internal/meta)

### store.go

| Function | What it does |
| --- | --- |
| `bucketState.putObject` | Saves an object in memory and adds a new key in sorted position |
| `Open` | Replays the journal, compacts or trims it if needed, and returns the store and a repair report |
| `Store.Close` | Closes the journal |
| `Store.apply` | Applies a record to memory and rejects invalid state changes |
| `Store.snapshot` | Builds the records needed to recreate current state |
| `Store.CreateBucket` | Commits a new bucket unless the name exists |
| `Store.DeleteBucket` | Commits deletion of an existing, empty bucket |
| `Store.PutObject` | Commits an object and returns the replaced file ID |
| `Store.DeleteObject` | Commits deletion and returns the file ID; empty if the key is missing |
| `Store.HasBucket` | Reports whether a bucket exists |
| `Store.Object` | Returns a copy of one object record |
| `Store.AllObjects` | Returns copies of all object records for recovery |
| `Store.ListBuckets` | Returns buckets sorted by name |
| `Store.ListObjects` | Returns one page of objects and common prefixes |
| `startAt` | Finds the index where a list page begins |
| `nextPrefix` | Finds the byte string used to skip past all keys with a prefix |

### journal.go

| Function | What it does |
| --- | --- |
| `encode` | Converts a record to JSON with its length and checksums |
| `replay` | Checks and applies records; returns the last good position and record count |
| `allZero` | Reports whether all remaining bytes are zero |
| `writeJournal` | Atomically replaces the journal using a synced temporary file |
| `syncDir` | Syncs the journal's directory |

### batch.go

| Function | What it does |
| --- | --- |
| `Store.submit` | Queues a change, takes or awaits leadership, and returns the result |
| `Store.flushBatch` | Validates a batch, writes and syncs it once, applies it and wakes callers |
| `Store.finishBatchFailed` | Sends results after a failed batch. **Has the open bug:** some callers don't receive the disk error |
| `newBatchOverlay` | Creates the temporary view for one batch |
| `batchOverlay.hasBucket` | Checks for a bucket, including earlier batch changes |
| `batchOverlay.bucketObjCount` | Counts a bucket's objects, including batch changes |
| `batchOverlay.getObject` | Returns whether an object exists and its file ID |
| `batchOverlay.getUpload` | Returns the batch's copy of an upload, creating it if needed |
| `batchOverlay.getCompletion` | Returns a saved or staged completion result |
| `batchOverlay.allUploadIDs` | Returns saved and staged upload IDs, which may include ones removed in the batch |
| `batchOverlay.allCompletionIDs` | Returns saved and staged completion IDs, which may include removed ones |
| `batchOverlay.stage` | Records a change in the temporary view |

### multipart.go

| Function | What it does |
| --- | --- |
| `Store.applyMultipart` | Applies multipart and completion records to memory |
| `Store.snapshotMultipart` | Builds records for uploads, parts and completion results |
| `Store.CreateUpload` | Commits a new upload |
| `Store.HasUpload` | Checks that an upload exists for the bucket and key |
| `Store.PutPart` | Commits a part and returns any replaced file ID |
| `Store.UploadSnapshot` | Returns an upload and sorted parts, calling the pin callback under the read lock |
| `Store.ListParts` | Returns one page of parts |
| `Store.AbortUpload` | Commits an abort and returns the part file IDs |
| `Store.Completion` | Returns a matching result still within its retention time |
| `Store.CompleteUpload` | Rechecks parts, publishes the object, removes the upload and saves the result |
| `Store.ExpireUploads` | Aborts up to 100 old, inactive uploads and purges old results |
| `Store.AllPartBlobIDs` | Returns file IDs used by active uploads |

## Storage engine (internal/storage)

### engine.go

| Function | What it does |
| --- | --- |
| `Open` | Opens an engine with default options |
| `OpenWithOptions` | Applies options, runs recovery and starts the expiry worker unless disabled |
| `Engine.Close` | Once only: stops the worker and closes metadata and file storage |
| `Engine.CreateBucket` | Validates the name and creates the bucket |
| `Engine.DeleteBucket` | Deletes an empty bucket through metadata |
| `Engine.ListBuckets` | Returns all buckets |
| `Engine.PutObject` | Validates, reserves space, writes data, commits and cleans up unused data |
| `Engine.HeadObject` | Returns metadata after checking file existence and size |
| `Engine.OpenObject` | Returns metadata and an open reader that the caller must close |
| `lookupObject` | Looks up a record and accesses its file, retrying if the record changed |
| `Engine.ListObjects` | Validates list options and handles continuation tokens |
| `checkSize` | Checks a reader's size, then returns to its start if correct |
| `Engine.DeleteObject` | Removes metadata, then the old file |

### multipart.go

| Function | What it does |
| --- | --- |
| `newUploadID` | Creates a random upload ID |
| `Engine.InitiateMultipartUpload` | Validates and saves a new upload |
| `Engine.UploadPart` | Writes one part, commits it and deletes any replaced file |
| `Engine.ListParts` | Validates options and returns a page of parts |
| `Engine.AbortMultipartUpload` | Aborts an upload and deletes, or defers deleting, part files |
| `Engine.CompleteMultipartUpload` | Checks parts, joins them, publishes the object and returns the result |
| `Engine.ExpireUploads` | Expires old uploads and results, and deletes expired part files |
| `Engine.startExpiryWorker` | Starts periodic expiry that stops when the engine closes |
| `Engine.pinUpload` | Marks an upload as in use |
| `Engine.unpinUpload` | Releases one upload pin |
| `Engine.isUploadActive` | Reports whether an upload is pinned |
| `Engine.unpinBlobs` | Releases file pins and performs deferred deletions |
| `Engine.deleteBlob` | Deletes a file, or defers deletion while pinned |
| `Engine.deleteBlobs` | Calls `deleteBlob` for each ID |
| `multiBlobReader.Read` | Reads part files in order, keeping at most one open |
| `multiBlobReader.Close` | Closes the current part file |

### space.go

| Function | What it does |
| --- | --- |
| `statfsAvail` | Returns available disk bytes |
| `Engine.reserveSpace` | Checks free space and reserves the expected bytes |
| `spaceReservation.consume` | Reduces the reservation as data is written; rechecks space for unknown sizes |
| `spaceReservation.release` | Releases any unused reservation |
| `mapBlobErr` | Converts a full-disk error to `ErrInsufficientStorage` |

## HTTP server and metrics

### server/server.go

| Function | What it does |
| --- | --- |
| `New` | Creates a ready handler with default options filled in |
| `Handler.SetReady` | Changes readiness |
| `Handler.Ready` | Returns readiness |
| `Handler.Wait` | Waits for active S3 handlers to finish |
| `Handler.Metrics` | Returns the metrics registry |
| `Handler.ServeHTTP` | Applies readiness, request limit and authentication, then routes requests |
| `Handler.serveOperational` | Serves health, readiness and metrics routes |
| `Handler.logRequest` | Logs request ID, method and duration |
| `statusRecorder.WriteHeader` | Records the first status code and forwards the call |
| `statusRecorder.Write` | Marks headers as sent and writes the data |
| `statusRecorder.Unwrap` | Exposes the original response writer |
| `setOp` | Sets the operation name used in metrics |
| `setErrCode` | Sets the error code used in metrics |
| `requestID` | Gets the request ID from context |
| `authFromContext` | Gets the authentication result from context |
| `newRequestID` | Creates a random request ID |

### server/s3.go

| Function | What it does |
| --- | --- |
| `Handler.handleService` | Handles ListBuckets |
| `Handler.handleBucket` | Routes bucket creation, deletion and listing |
| `Handler.handleListObjectsV2` | Parses list options and writes the XML result |
| `Handler.handleObject` | Routes object and multipart requests |
| `Handler.prepareUploadBody` | Builds the upload reader, checks and deadline cleanup |
| `Handler.handlePutObject` | Stores an object and returns its ETag |
| `Handler.handleCreateMultipartUpload` | Starts a multipart upload |
| `Handler.handleUploadPart` | Checks parameters and stores a part |
| `Handler.handleListParts` | Parses paging options and returns parts |
| `Handler.handleCompleteMultipartUpload` | Reads up to 4 MiB of XML and completes the upload |
| `Handler.handleAbortMultipartUpload` | Aborts an upload |
| `Handler.handleGetOrHeadObject` | Serves headers, full downloads or one byte range |
| `streamVerified` | Streams data, withholding the last chunk until verification passes |
| `abortConnection` | Closes the connection, or panics with `http.ErrAbortHandler` if it can't |
| `deadlineReader.Read` | Refreshes the read deadline, then reads |
| `deadlineWriter.Write` | Refreshes the write deadline, then writes |
| `writeObjectHeaders` | Writes object type, length, time, ETag and metadata headers |
| `extractMetadata` | Collects `x-amz-meta-` request headers |
| `hasUnsupportedQuery` | Finds query parameters that aren't allowed |
| `Handler.writeNotImplemented` | Sends a 501 error |
| `Handler.writeMethodNotAllowed` | Sends a 405 error |
| `Handler.writeMappedError` | Maps internal errors to S3 errors |
| `Handler.writeError` | Records the error code and sends the error response |

### server/sigv4.go

| Function | What it does |
| --- | --- |
| `verifySigV4` | Verifies request authentication and returns signing details |
| `parseAuthorization` | Extracts credential, signed headers and signature |
| `parseRequestDate` | Reads the signing date from `X-Amz-Date` or `Date` |
| `validateSignedHeaders` | Checks header order and required signed headers |
| `classifyPayloadHeader` | Identifies signed, unsigned or chunked payload mode |
| `buildCanonicalRequest` | Builds the standard request text used for signing |
| `headerValues` | Gets header values without regard to letter case |
| `collapseWhitespace` | Trims and merges spaces and tabs |
| `canonicalURI` | Encodes the request path for signing |
| `canonicalQueryString` | Encodes and sorts query parameters |
| `awsURIEncode` | Applies AWS percent encoding |
| `deriveSigningKey` | Derives the request signing key |
| `hmacSHA256` | Calculates HMAC-SHA256 |
| `sha256Hex` | Returns SHA-256 as hexadecimal text |
| `constantTimeStringEqual` | Compares strings using Go's constant-time byte comparison |

### server/chunked.go

| Function | What it does |
| --- | --- |
| `newChecksumTracker` | Creates hashers for requested checksums |
| `newChecksumHash` | Selects a supported checksum algorithm |
| `checksumTracker.Write` | Adds data to each checksum |
| `checksumTracker.verify` | Compares calculated and supplied checksums |
| `collectHeaderChecksums` | Collects checksum request headers |
| `wrapHeaderChecksums` | Wraps a reader if checksum headers exist |
| `headerChecksumReader.Read` | Hashes data and verifies it at end of input |
| `newAWSChunkedReader` | Creates an AWS chunk decoder |
| `awsChunkedReader.Read` | Returns decoded object bytes |
| `awsChunkedReader.nextChunkHeader` | Parses the next chunk header and handles the final chunk |
| `awsChunkedReader.finishChunk` | Checks chunk ending and signature |
| `awsChunkedReader.verifyChunkSignature` | Verifies one link in the chunk signature chain |
| `awsChunkedReader.consumeTrailers` | Reads and verifies trailing headers |
| `awsChunkedReader.verifyTrailerSignature` | Verifies the signature of the trailers |
| `readCRLFLine` | Reads a length-limited line ending in CRLF |
| `parseExpectedTrailers` | Reads expected names from `X-Amz-Trailer` |

### server/range.go and server/xml.go

| Function | What it does |
| --- | --- |
| `byteRange.length` | Returns the number of bytes in a range |
| `parseByteRange` | Parses one byte range against the object size |
| `parseNonNegInt` | Parses a number containing digits only |
| `writeXML` | Encodes and sends an XML response |

### metrics/metrics.go

| Function | What it does |
| --- | --- |
| `newHistogram` | Creates a latency histogram |
| `histogram.observe` | Adds one latency value |
| `New` | Creates an unready registry |
| `Registry.SetReady` | Stores readiness |
| `Registry.Ready` | Returns readiness |
| `Registry.IncActive` | Increases the active request count |
| `Registry.DecActive` | Decreases the active request count |
| `Registry.AddBytesIn` | Adds positive uploaded byte counts |
| `Registry.AddBytesOut` | Adds positive downloaded byte counts |
| `Registry.ObserveRequest` | Records request status, error and duration |
| `Registry.WritePrometheus` | Writes metrics in Prometheus text format |

## Benchmark tool (cmd/bench/main.go)

| Function | What it does |
| --- | --- |
| `main` | Calls `run` and exits with code 1 on error |
| `run` | Parses options, starts a server, runs scenarios and writes reports |
| `runCell` | Runs one scenario at one concurrency level and measures it |
| `captureProfiles` | Saves CPU and heap profiles when requests succeed |
| `percentile` | Picks a percentile from sorted values |
| `readProcCPUTicks` | Reads a process's CPU time from `/proc` |
| `readProcRSSKB` | Reads resident memory in KiB |
| `countProcFDs` | Counts open file descriptors |
| `dirSize` | Adds file sizes under a directory |
| `collectEnv` | Collects operating system, hardware, Go and filesystem details |
| `detectFS` | Identifies the filesystem type |
| `printEnvBanner` | Prints the test environment |
| `freeLocalAddr` | Finds an available local port |
| `waitReady` | Waits up to 15 seconds for readiness, or detects early server exit |
| `parseIntList` | Parses comma-separated positive numbers |
| `parseScenarioSet` | Builds the selected scenario set; nil means all |
| `makePayload` | Creates repeatable test data of a given size |
| `newBenchClient` | Creates the benchmark HTTP client |
| `benchClient.signingKey` | Derives and caches the signing key for a date |
| `benchClient.sign` | Adds authentication headers using an unsigned payload |
| `benchClient.createBucket` | Creates a bucket and requires success |
| `benchClient.createBucketIfNeeded` | Creates a bucket, accepting an existing-bucket conflict |
| `benchClient.putObject` | Uploads an object |
| `benchClient.getObject` | Downloads and discards an object, returning its byte count |
| `benchClient.listObjects` | Sends a list request and returns the response size |
| `hmacSHA256` | Calculates HMAC-SHA256 |
| `benchCanonURI` | Prepares a path for signing |
| `benchCanonQuery` | Encodes and sorts query parameters |
| `awsEncode` | Applies AWS percent encoding |
