package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRegistryOutput(t *testing.T) {
	r := New()
	if r.Ready() {
		t.Fatal("new registry should start unready")
	}
	r.SetReady(true)
	r.IncActive()
	r.AddBytesIn(128)
	r.AddBytesOut(256)
	r.ObserveRequest("put_object", 200, "", 20*time.Millisecond)
	r.ObserveRequest("get_object", 404, "NoSuchKey", 5*time.Millisecond)
	r.DecActive()

	var buf bytes.Buffer
	r.WritePrometheus(&buf)
	out := buf.String()

	for _, want := range []string{
		"gostore_ready 1",
		"gostore_active_requests 0",
		`gostore_request_bytes_total{direction="in"} 128`,
		`gostore_request_bytes_total{direction="out"} 256`,
		`gostore_requests_total{op="put_object",status="200"} 1`,
		`gostore_errors_total{op="get_object",code="NoSuchKey"} 1`,
		`gostore_request_duration_seconds_bucket{op="put_object",le="0.025"} 1`,
		`gostore_request_duration_seconds_count{op="put_object"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
