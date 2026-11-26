// Package metrics tracks request, error, latency, and byte metrics for
// Prometheus scraping without putting object keys in labels.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var latencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1,
	0.25, 0.5, 1, 2.5, 5, 10, 30,
}

type histogram struct {
	mu      sync.Mutex
	buckets []uint64
	count   uint64
	sum     float64
}

func newHistogram() *histogram {
	return &histogram{buckets: make([]uint64, len(latencyBuckets))}
}

func (h *histogram) observe(sec float64) {
	h.mu.Lock()
	for i, bound := range latencyBuckets {
		if sec <= bound {
			h.buckets[i]++
		}
	}
	h.count++
	h.sum += sec
	h.mu.Unlock()
}

type reqKey struct {
	op     string
	status string
}

type errKey struct {
	op   string
	code string
}

// Registry records operational metrics in memory. It is safe for concurrent use.
type Registry struct {
	ready          atomic.Bool
	activeRequests atomic.Int64
	bytesIn        atomic.Int64
	bytesOut       atomic.Int64

	mu        sync.Mutex
	requests  map[reqKey]uint64
	errors    map[errKey]uint64
	latencies map[string]*histogram
}

// New creates an initialized Registry (starting unready until SetReady(true)).
func New() *Registry {
	return &Registry{
		requests:  make(map[reqKey]uint64),
		errors:    make(map[errKey]uint64),
		latencies: make(map[string]*histogram),
	}
}

// SetReady sets the readiness state exposed on /-/readyz and gostore_ready.
func (r *Registry) SetReady(ready bool) {
	r.ready.Store(ready)
}

// Ready reports whether the service is ready to serve requests.
func (r *Registry) Ready() bool {
	return r.ready.Load()
}

// IncActive increments the active data request gauge.
func (r *Registry) IncActive() {
	r.activeRequests.Add(1)
}

// DecActive decrements the active data request gauge.
func (r *Registry) DecActive() {
	r.activeRequests.Add(-1)
}

// AddBytesIn records uploaded payload bytes.
func (r *Registry) AddBytesIn(n int64) {
	if n > 0 {
		r.bytesIn.Add(n)
	}
}

// AddBytesOut records downloaded payload bytes.
func (r *Registry) AddBytesOut(n int64) {
	if n > 0 {
		r.bytesOut.Add(n)
	}
}

// ObserveRequest records a completed request's operation, HTTP status, error
// code (if any), and duration.
func (r *Registry) ObserveRequest(op string, status int, errCode string, d time.Duration) {
	st := strconv.Itoa(status)
	r.mu.Lock()
	r.requests[reqKey{op: op, status: st}]++
	if errCode != "" {
		r.errors[errKey{op: op, code: errCode}]++
	}
	h := r.latencies[op]
	if h == nil {
		h = newHistogram()
		r.latencies[op] = h
	}
	r.mu.Unlock()

	h.observe(d.Seconds())
}

// WritePrometheus writes metrics in Prometheus text exposition format.
func (r *Registry) WritePrometheus(w io.Writer) {
	readyVal := 0
	if r.ready.Load() {
		readyVal = 1
	}
	_, _ = fmt.Fprintln(w, "# HELP gostore_ready Whether GoStore is ready to accept traffic (1 = ready, 0 = not ready).")
	_, _ = fmt.Fprintln(w, "# TYPE gostore_ready gauge")
	_, _ = fmt.Fprintf(w, "gostore_ready %d\n", readyVal)

	_, _ = fmt.Fprintln(w, "# HELP gostore_active_requests Current number of active S3 requests.")
	_, _ = fmt.Fprintln(w, "# TYPE gostore_active_requests gauge")
	_, _ = fmt.Fprintf(w, "gostore_active_requests %d\n", r.activeRequests.Load())

	_, _ = fmt.Fprintln(w, "# HELP gostore_request_bytes_total Total object payload bytes transferred.")
	_, _ = fmt.Fprintln(w, "# TYPE gostore_request_bytes_total counter")
	_, _ = fmt.Fprintf(w, "gostore_request_bytes_total{direction=\"in\"} %d\n", r.bytesIn.Load())
	_, _ = fmt.Fprintf(w, "gostore_request_bytes_total{direction=\"out\"} %d\n", r.bytesOut.Load())

	r.mu.Lock()
	reqs := maps.Clone(r.requests)
	errs := maps.Clone(r.errors)
	lats := maps.Clone(r.latencies)
	r.mu.Unlock()

	_, _ = fmt.Fprintln(w, "# HELP gostore_requests_total Total number of S3 HTTP requests.")
	_, _ = fmt.Fprintln(w, "# TYPE gostore_requests_total counter")
	reqKeys := slices.SortedFunc(maps.Keys(reqs), func(a, b reqKey) int {
		if a.op != b.op {
			if a.op < b.op {
				return -1
			}
			return 1
		}
		if a.status < b.status {
			return -1
		}
		if a.status > b.status {
			return 1
		}
		return 0
	})
	for _, k := range reqKeys {
		_, _ = fmt.Fprintf(w, "gostore_requests_total{op=%q,status=%q} %d\n", k.op, k.status, reqs[k])
	}

	_, _ = fmt.Fprintln(w, "# HELP gostore_errors_total Total number of S3 error responses.")
	_, _ = fmt.Fprintln(w, "# TYPE gostore_errors_total counter")
	errKeys := slices.SortedFunc(maps.Keys(errs), func(a, b errKey) int {
		if a.op != b.op {
			if a.op < b.op {
				return -1
			}
			return 1
		}
		if a.code < b.code {
			return -1
		}
		if a.code > b.code {
			return 1
		}
		return 0
	})
	for _, k := range errKeys {
		_, _ = fmt.Fprintf(w, "gostore_errors_total{op=%q,code=%q} %d\n", k.op, k.code, errs[k])
	}

	_, _ = fmt.Fprintln(w, "# HELP gostore_request_duration_seconds S3 request latency distribution in seconds.")
	_, _ = fmt.Fprintln(w, "# TYPE gostore_request_duration_seconds histogram")
	for _, op := range slices.Sorted(maps.Keys(lats)) {
		h := lats[op]
		h.mu.Lock()
		for i, bound := range latencyBuckets {
			_, _ = fmt.Fprintf(w, "gostore_request_duration_seconds_bucket{op=%q,le=\"%g\"} %d\n", op, bound, h.buckets[i])
		}
		_, _ = fmt.Fprintf(w, "gostore_request_duration_seconds_bucket{op=%q,le=\"+Inf\"} %d\n", op, h.count)
		_, _ = fmt.Fprintf(w, "gostore_request_duration_seconds_sum{op=%q} %g\n", op, h.sum)
		_, _ = fmt.Fprintf(w, "gostore_request_duration_seconds_count{op=%q} %d\n", op, h.count)
		h.mu.Unlock()
	}
}
