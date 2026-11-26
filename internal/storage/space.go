//go:build linux

package storage

import (
	"errors"
	"syscall"

	"github.com/ashu-wolf/gostore/internal/blob"
	"github.com/ashu-wolf/gostore/internal/domain"
)

const defaultMinFreeSpace = int64(1 << 30) // 1 GiB

func statfsAvail(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

type spaceReservation struct {
	engine       *Engine
	remaining    int64
	checkDynamic bool
}

func (e *Engine) reserveSpace(need int64, checkDynamic bool) (*spaceReservation, error) {
	if e.opts.DisableSpaceCheck {
		return &spaceReservation{engine: e}, nil
	}
	avail, err := e.statfs(e.root)
	if err != nil {
		return nil, err
	}

	e.spaceMu.Lock()
	defer e.spaceMu.Unlock()

	headroom := avail - e.reservedBytes - e.opts.MinFreeSpace
	if headroom <= 0 || need > headroom {
		return nil, domain.ErrInsufficientStorage
	}
	if need > 0 {
		e.reservedBytes += need
	}
	return &spaceReservation{engine: e, remaining: need, checkDynamic: checkDynamic}, nil
}

// consume subtracts bytes written to disk from the pending reservation so they
// are not double-counted against subsequent statfs checks. For unknown-size
// uploads (checkDynamic == true), it also re-checks free space mid-stream.
func (r *spaceReservation) consume(n int64) error {
	if r == nil || n <= 0 {
		return nil
	}
	if r.checkDynamic && !r.engine.opts.DisableSpaceCheck {
		avail, err := r.engine.statfs(r.engine.root)
		if err != nil {
			return err
		}
		r.engine.spaceMu.Lock()
		headroom := avail - r.engine.reservedBytes - r.engine.opts.MinFreeSpace
		r.engine.spaceMu.Unlock()
		if headroom < 0 {
			return domain.ErrInsufficientStorage
		}
	}
	r.engine.spaceMu.Lock()
	if r.remaining > 0 {
		drop := min(n, r.remaining)
		r.remaining -= drop
		r.engine.reservedBytes -= drop
	}
	r.engine.spaceMu.Unlock()
	return nil
}

func (r *spaceReservation) release() {
	if r == nil {
		return
	}
	r.engine.spaceMu.Lock()
	if r.remaining > 0 {
		r.engine.reservedBytes -= r.remaining
		r.remaining = 0
	}
	r.engine.spaceMu.Unlock()
}

func mapBlobErr(err error) error {
	if errors.Is(err, blob.ErrNoSpace) || errors.Is(err, syscall.ENOSPC) {
		return domain.ErrInsufficientStorage
	}
	return err
}
