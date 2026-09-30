package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

// Queued Class A downlink is delivered in RX1 after the next uplink.
func TestQueuedDownlinkDeliveredInWindow(t *testing.T) {
	p := provision(9)
	h := newHarness(t, nil, p)
	var mu sync.Mutex
	got := [][]byte{}
	d := h.newDevice(t, p, func(b []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), b...))
		mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}

	if err := h.server.EnqueueDown(p.DevEUI, []byte("hello-device")); err != nil {
		t.Fatal(err)
	}
	// No uplink yet -> nothing can be received (Class A).
	time.Sleep(rx1 + rx2 + 2*rxWin)
	mu.Lock()
	if len(got) != 0 {
		mu.Unlock()
		t.Fatalf("downlink arrived without an uplink: %q", got)
	}
	mu.Unlock()

	// An uplink opens the windows; the queued downlink goes in RX1.
	if err := d.SendUnconfirmed(ctx, []byte("poke")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || string(got[0]) != "hello-device" {
		t.Fatalf("downlink = %q", got)
	}
}

// Server LinkADR command changes device DR/power without losing the session.
func TestLinkADRChangesRateAndKeepsSession(t *testing.T) {
	p := provision(10)
	h := newHarness(t, nil, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	addrBefore := d.Session().DevAddr
	fcntBefore := d.Session().FCntUp

	if err := h.server.SetLinkADR(p.DevEUI, 5, 3); err != nil {
		t.Fatal(err)
	}
	// The command rides in the ACK of the next confirmed uplink.
	if err := d.SendConfirmed(ctx, []byte("change rate")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	sess := d.Session()
	if sess.DataRate != 5 || sess.TXPower != 3 {
		t.Fatalf("dr/power = %d/%d", sess.DataRate, sess.TXPower)
	}
	if sess.DevAddr != addrBefore {
		t.Fatal("ADR change altered DevAddr")
	}
	if sess.FCntUp <= fcntBefore {
		t.Fatal("ADR change reset frame counters")
	}
	// Communication continues at the new rate settings.
	if err := d.SendConfirmed(ctx, []byte("still joined")); err != nil {
		t.Fatalf("communication broke after ADR: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	acks, ok := h.server.ADRACKCount(p.DevEUI)
	if !ok || acks < 1 {
		t.Fatalf("server did not receive LinkADRAns (acks=%d)", acks)
	}
}

// Bad-MIC downlink is dropped by the device and does not poison counters.
func TestBadMICDownlinkDropped(t *testing.T) {
	p := provision(11)
	h := newHarness(t, nil, p)
	d := h.newDevice(t, p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	s := d.Session()
	raw, err := lorawan.EncodeFrame(&s, &lorawan.Frame{
		MType:   lorawan.MTypeUnconfirmedDataDown,
		DevAddr: s.DevAddr,
		FCnt:    0,
		HasPort: true,
		FPort:   1,
		Payload: []byte("forged"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tamper with MIC, then deliver during a receive window.
	raw[len(raw)-1] ^= 0xff
	go func() {
		time.Sleep(rx1 + 5*time.Millisecond)
		_ = h.pipe.ServerEnd().Send(ctx, raw)
	}()
	if err := d.SendUnconfirmed(ctx, []byte("open again")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	// Device still works; down counter untouched (no legitimate content down).
	if got := d.Session().FCntDown; got != 0 {
		t.Fatalf("FCntDown = %d, forged frames must not advance it", got)
	}
}
