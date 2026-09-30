package clock

import (
	"testing"
	"time"
)

func TestFakeClockTimerOrdering(t *testing.T) {
	fc := NewFakeClock(time.Unix(100, 0))
	var order []int
	fc.AfterFunc(3*time.Second, func() { order = append(order, 3) })
	fc.AfterFunc(time.Second, func() { order = append(order, 1) })
	fc.AfterFunc(2*time.Second, func() { order = append(order, 2) })

	fc.Advance(4 * time.Second)
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("timers fired out of order: %v", order)
	}
}

func TestFakeClockAfterChannel(t *testing.T) {
	fc := NewFakeClock(time.Unix(0, 0))
	ch := fc.After(2 * time.Second)
	select {
	case <-ch:
		t.Fatal("channel fired early")
	default:
	}
	fc.Advance(time.Second)
	select {
	case <-ch:
		t.Fatal("channel fired early at 1s")
	default:
	}
	fc.Advance(time.Second)
	select {
	case <-ch:
	default:
		t.Fatal("channel did not fire at 2s")
	}
}

func TestFakeClockStoppedTimer(t *testing.T) {
	fc := NewFakeClock(time.Unix(0, 0))
	fired := false
	tm := fc.AfterFunc(time.Second, func() { fired = true })
	if !tm.Stop() {
		t.Fatal("Stop returned false for active timer")
	}
	fc.Advance(2 * time.Second)
	if fired {
		t.Fatal("stopped timer fired")
	}
}
