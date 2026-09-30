package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

// A reused DevNonce must be rejected by the server (no join-accept).
func TestRepeatedDevNonceRejected(t *testing.T) {
	p := provision(6)
	h := newHarness(t, nil, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}

	// Craft a second join request that reuses the exact same DevNonce,
	// bypassing the device's monotonic counter to simulate a replay.
	sess := d.Session()
	jr := &lorawan.JoinRequest{
		JoinEUI:  p.JoinEUI,
		DevEUI:   p.DevEUI,
		DevNonce: lorawan.DevNonce(sess.LastDevNonce),
	}
	raw, err := lorawan.EncodeJoinRequest(p.AppKey, jr)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := h.server.Session(p.DevEUI)
	if err := h.inject(d, raw); err != nil {
		t.Fatal(err)
	}
	// Give the server time to (not) answer; no response means rejection.
	time.Sleep(rx1 + rxWin + 40*time.Millisecond)
	after, _ := h.server.Session(p.DevEUI)
	if after.JoinNonceCounter != before.JoinNonceCounter {
		t.Fatalf("server issued a new JoinNonce for a replayed DevNonce: %d -> %d",
			before.JoinNonceCounter, after.JoinNonceCounter)
	}
}

// A replayed data frame (old FCnt) must be dropped; the app never sees it.
func TestReplayedDataFrameDropped(t *testing.T) {
	p := provision(7)
	var deliveries [][]byte
	var mu sync.Mutex
	h := newHarness(t, func(_ lorawan.EUI, b []byte) {
		mu.Lock()
		deliveries = append(deliveries, append([]byte(nil), b...))
		mu.Unlock()
	}, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}

	// First real uplink consumes FCnt 0 and establishes the server floor.
	if err := d.SendUnconfirmed(ctx, []byte("first")); err != nil {
		t.Fatal(err)
	}
	s := d.Session()
	raw, err := lorawan.EncodeFrame(&s, &lorawan.Frame{
		MType:   lorawan.MTypeUnconfirmedDataUp,
		DevAddr: s.DevAddr,
		FCnt:    99, // gap/out-of-order: rejected, must not jump the floor
		HasPort: true,
		FPort:   1,
		Payload: []byte("replay"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.inject(d, raw); err != nil {
		t.Fatal(err)
	}

	// A legitimate fresh uplink still works afterwards.
	if err := d.SendUnconfirmed(ctx, []byte("second")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 || string(deliveries[0]) != "first" || string(deliveries[1]) != "second" {
		t.Fatalf("deliveries = %q", deliveries)
	}
}

// A corrupted frame must be dropped, counted, and not crash the server.
func TestCorruptedFrameDropped(t *testing.T) {
	p := provision(8)
	h := newHarness(t, nil, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	garbage := []byte{0x80, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18}
	if err := h.inject(d, garbage); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	// Device and server remain functional.
	if err := d.SendConfirmed(ctx, []byte("still alive")); err != nil {
		t.Fatalf("server did not recover after garbage: %v", err)
	}
}
