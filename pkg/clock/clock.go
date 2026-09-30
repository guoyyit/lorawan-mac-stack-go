// Package clock provides a pluggable time source so the stack can run with
// wall-clock time in production/demo and with a deterministic virtual clock
// in tests.
package clock

import (
	"container/heap"
	"sync"
	"time"
)

// Timer represents a single scheduled timer callback.
type Timer interface {
	Stop() bool
	C() <-chan time.Time
}

// Clock is the time abstraction used by the MAC stack.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, fn func()) Timer
	After(d time.Duration) <-chan time.Time
}

// NewRealClock returns a clock backed by the wall clock.
func NewRealClock() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, fn func()) Timer {
	return &realTimer{t: time.AfterFunc(d, fn)}
}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type realTimer struct{ t *time.Timer }

func (r *realTimer) Stop() bool          { return r.t.Stop() }
func (r *realTimer) C() <-chan time.Time { return r.t.C }

// FakeClock is a manually advanced virtual clock.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers fakeTimerHeap
	nextID int64
}

// NewFakeClock returns a FakeClock starting at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

// Now returns the virtual current time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules fn after virtual duration d.
func (c *FakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	if d < 0 {
		d = 0
	}
	c.mu.Lock()
	c.nextID++
	ft := &fakeTimer{
		id:  c.nextID,
		at:  c.now.Add(d),
		fn:  fn,
		ch:  make(chan time.Time, 1),
		clk: c,
	}
	heap.Push(&c.timers, ft)
	c.mu.Unlock()
	return ft
}

// After returns a channel that receives the virtual time at which d elapses.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.AfterFunc(d, func() {
		select {
		case ch <- c.Now():
		default:
		}
	})
	return ch
}

// Advance moves the virtual clock by d, firing every due timer.
func (c *FakeClock) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	end := c.Now().Add(d)
	for {
		c.mu.Lock()
		if c.timers.Len() == 0 {
			c.now = end
			c.mu.Unlock()
			return
		}
		next := c.timers[0]
		if next.at.After(end) {
			c.now = end
			c.mu.Unlock()
			return
		}
		heap.Pop(&c.timers)
		c.now = next.at
		c.mu.Unlock()

		if !next.stopped {
			select {
			case next.ch <- next.at:
			default:
			}
			next.fn()
		}
	}
}

// Pending reports whether any timers are queued.
func (c *FakeClock) Pending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timers.Len() > 0
}

// runUntil advances the clock in increments from a test's perspective;
// exported helper: advance while any timer is due within d of current time.

type fakeTimer struct {
	id      int64
	at      time.Time
	fn      func()
	ch      chan time.Time
	clk     *FakeClock
	stopped bool
	index   int
}

func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	wasActive := !t.stopped
	t.stopped = true
	if t.index >= 0 {
		heap.Remove(&t.clk.timers, t.index)
	}
	return wasActive
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

type fakeTimerHeap []*fakeTimer

func (h fakeTimerHeap) Len() int { return len(h) }

func (h fakeTimerHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].id < h[j].id
	}
	return h[i].at.Before(h[j].at)
}

func (h fakeTimerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *fakeTimerHeap) Push(x any) {
	t := x.(*fakeTimer)
	t.index = len(*h)
	*h = append(*h, t)
}

func (h *fakeTimerHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	t.index = -1
	*h = old[:n-1]
	return t
}

var _ Clock = (*FakeClock)(nil)
var _ Clock = realClock{}
