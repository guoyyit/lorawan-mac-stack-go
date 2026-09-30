package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

func TestConfirmedUplinkAcked(t *testing.T) {
	p := provision(2)
	var appCount int64
	h := newHarness(t, func(_ lorawan.EUI, _ []byte) { atomic.AddInt64(&appCount, 1) }, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.SendConfirmed(ctx, []byte("ping")); err != nil {
		t.Fatalf("confirmed uplink: %v", err)
	}
	if got := atomic.LoadInt64(&appCount); got != 1 {
		t.Fatalf("app deliveries = %d, want 1", got)
	}
	if got := d.Session().FCntUp; got != 1 {
		t.Fatalf("FCntUp = %d, want 1", got)
	}
}

func TestConfirmedUplinkRetransmitsUntilACK(t *testing.T) {
	p := provision(3)
	var appCount int64
	h := newHarness(t, func(_ lorawan.EUI, _ []byte) { atomic.AddInt64(&appCount, 1) }, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	// First two attempts are lost on the uplink; the third gets through.
	d.Air().DropNext(2)
	if err := d.SendConfirmed(ctx, []byte("retry me")); err != nil {
		t.Fatalf("should eventually be acked: %v", err)
	}
	if got := atomic.LoadInt64(&appCount); got != 1 {
		t.Fatalf("app deliveries = %d, want exactly 1", got)
	}
}

func TestConfirmedUplinkGivesUpAfterRetries(t *testing.T) {
	p := provision(4)
	h := newHarness(t, nil, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	d.Air().DropNext(100)
	if err := d.SendConfirmed(ctx, []byte("nobody home")); err != lorawan.ErrNoACK {
		t.Fatalf("err = %v, want ErrNoACK", err)
	}
}

func TestDuplicateFrameDeliversAppOnce(t *testing.T) {
	p := provision(5)
	var mu sync.Mutex
	var got [][]byte
	h := newHarness(t, func(_ lorawan.EUI, b []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), b...))
		mu.Unlock()
	}, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	// First ACK is lost on the downlink; device re-sends the identical frame.
	h.pipe.ServerEnd().DropNext(1)
	if err := d.SendConfirmed(ctx, []byte("once")); err != nil {
		t.Fatalf("duplicate must still be acked: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || string(got[0]) != "once" {
		t.Fatalf("app got %d payloads: %q", len(got), got)
	}
}
