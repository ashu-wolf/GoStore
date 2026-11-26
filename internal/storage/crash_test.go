//go:build linux

package storage

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/ashu-wolf/gostore/internal/crashpoint"
)

func kill() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // never continue past the crash point
}

// killMidStream sends some data and then kills the process.
type killMidStream struct{ sent bool }

func (k *killMidStream) Read(p []byte) (int, error) {
	if !k.sent {
		k.sent = true
		return copy(p, "new-"), nil
	}
	kill()
	return 0, io.EOF
}

// TestCrashHelper runs only in the child process started by TestCrashRecovery.
func TestCrashHelper(t *testing.T) {
	root, point := os.Getenv("CRASH_ROOT"), os.Getenv("CRASH_POINT")
	if root == "" {
		t.Skip("helper process only")
	}
	e, _ := openEngine(t, root)
	var body io.Reader = &killMidStream{}
	if point != "mid-stream" {
		body = io.LimitReader(repeat('n'), 3)
		crashpoint.Hook = func(name string) {
			if name == point {
				kill()
			}
		}
	}
	_, err := e.PutObject(context.Background(), PutRequest{Bucket: "photos", Key: "a"}, body)
	t.Fatalf("process survived crash point %q (error: %v)", point, err)
}

type repeat byte

func (r repeat) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// A killed process keeps data already written to the operating system, so this
// tests process crashes, not power loss.
func TestCrashRecovery(t *testing.T) {
	tests := []struct{ point, want string }{
		{"mid-stream", "old"},
		{"blob.before-rename", "old"},
		{"blob.after-rename", "old"},
		{"storage.before-commit", "old"},
		{"meta.after-write", "nnn"},
		{"storage.after-commit", "nnn"},
	}
	for _, tt := range tests {
		t.Run(tt.point, func(t *testing.T) {
			root := t.TempDir()
			e, _ := openEngine(t, root)
			if err := e.CreateBucket("photos"); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{"a": "old", "other": "safe"} {
				if _, err := putString(e, key, value); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$")
			cmd.Env = append(os.Environ(), "CRASH_ROOT="+root, "CRASH_POINT="+tt.point)
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil {
				t.Fatal(err)
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child was not killed: %v\n%s", err, out)
			}

			e, report := openEngine(t, root)
			if got := getString(t, e, "a"); got != tt.want {
				t.Errorf("object = %q, want %q", got, tt.want)
			}
			if got := getString(t, e, "other"); got != "safe" {
				t.Errorf("acknowledged object = %q", got)
			}
			if n := blobCount(t, e); n != 2 {
				t.Errorf("blobs = %d, want 2 (report %+v)", n, report)
			}
			if tmp, err := e.blobs.RemoveTemp(); err != nil || tmp != 0 {
				t.Errorf("temporary files remain: %d, %v", tmp, err)
			}
		})
	}
}
