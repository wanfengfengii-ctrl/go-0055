// Package clock provides an injectable time source so that schedulers,
// recovery coordinators and tests can advance time deterministically
// without sleeping.
package clock

import (
	"sync"
	"time"
)

// Clock abstracts the reading of the current time.
type Clock interface {
	// Now returns the current logical time.
	Now() time.Time
}

// Wall is the real-time implementation of Clock.
type Wall struct{}

// Now reports the real wall-clock time.
func (Wall) Now() time.Time { return time.Now() }

// Fixed is a deterministic, manually-advanceable clock. It is safe for
// concurrent use: advancing the clock is serialised so that concurrent
// readers observe a monotonically non-decreasing value.
type Fixed struct {
	mu sync.Mutex
	t  time.Time
}

// NewFixed constructs a Fixed clock anchored at t.
func NewFixed(t time.Time) *Fixed {
	return &Fixed{t: t}
}

// Now returns the current logical time.
func (f *Fixed) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance moves the logical time forward by d. A non-positive duration is
// a no-op; the clock never moves backwards.
func (f *Fixed) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// Set positions the clock at t, but only if t is ahead of the current time.
func (f *Fixed) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.After(f.t) {
		f.t = t
	}
}
