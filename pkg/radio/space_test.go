package radio

import (
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/clock"
)

func TestSpaceImmediateAndScheduled(t *testing.T) {
	clk := clock.NewFakeClock(time.Unix(0, 0))
	sp := NewSpace(Config{Clock: clk, PropagationDelay: 100 * time.Millisecond,
		SynchronousDelivery: true})

	got := make(chan Frame, 4)
	sp.Register("a", func(_ EndpointID, f Frame) { got <- f })
	sp.Register("b", func(_ EndpointID, f Frame) { got <- f })

	sp.Transmit("a", "b", Frame{Payload: []byte{1}})
	clk.Advance(150 * time.Millisecond)
	f := <-got
	if f.Payload[0] != 1 {
		t.Fatal("immediate frame not delivered")
	}
	if !f.ReceivedAt.Equal(f.SentAt.Add(100 * time.Millisecond)) {
		t.Fatalf("propagation time wrong: sent=%v recv=%v", f.SentAt, f.ReceivedAt)
	}

	// Scheduled transmit arrives at t=5s + propagation, not earlier.
	sp.TransmitAt(clk.Now().Add(5*time.Second), "b", "a", Frame{Payload: []byte{2}})
	clk.Advance(4 * time.Second)
	select {
	case <-got:
		t.Fatal("scheduled frame arrived early")
	default:
	}
	clk.Advance(2 * time.Second)
	f2 := <-got
	if f2.Payload[0] != 2 {
		t.Fatal("scheduled frame not delivered")
	}
}

func TestSpaceLossHook(t *testing.T) {
	clk := clock.NewFakeClock(time.Unix(0, 0))
	sp := NewSpace(Config{Clock: clk, PropagationDelay: time.Millisecond,
		SynchronousDelivery: true})
	got := make(chan Frame, 2)
	sp.Register("a", func(_ EndpointID, f Frame) { got <- f })

	sp.SetLossHook(func(from, to EndpointID, f Frame) (bool, bool) {
		return f.Payload[0] != 9, true // drop 9, corrupt everything else
	})

	sp.Transmit("b", "a", Frame{Payload: []byte{9}})
	sp.Transmit("b", "a", Frame{Payload: []byte{3}})
	sp.SetLossHook(nil)
	sp.Transmit("b", "a", Frame{Payload: []byte{4}})
	clk.Advance(20 * time.Millisecond)

	close(got)
	n := 0
	var sawCorrupt, sawClean bool
	for f := range got {
		n++
		if f.Corrupt {
			sawCorrupt = true
		}
		if f.Payload[0] == 4 {
			sawClean = true
		}
	}
	if n != 2 || !sawCorrupt || !sawClean {
		t.Fatalf("loss hook behavior wrong n=%d corrupt=%v clean=%v", n, sawCorrupt, sawClean)
	}
}

func TestSpaceUnknownTarget(t *testing.T) {
	clk := clock.NewFakeClock(time.Unix(0, 0))
	sp := NewSpace(Config{Clock: clk, SynchronousDelivery: true})
	if sp.Transmit("x", "missing", Frame{Payload: []byte{1}}) {
		t.Fatal("transmit to unregistered endpoint reported success")
	}
}
