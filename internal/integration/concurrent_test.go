package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/server"
)

// Many devices join and exchange confirmed traffic concurrently; each app
// handler receives exactly its own device's payloads.
func TestMultipleDevicesConcurrently(t *testing.T) {
	const n = 6
	all := make([]lorawan.EUI, 0, n)
	provs := make([]server.DeviceProvision, 0, n)
	for i := byte(40); i < 40+n; i++ {
		p := provision(i)
		provs = append(provs, p)
		all = append(all, p.DevEUI)
	}

	var counts [n]int64
	h := newHarness(t, func(id lorawan.EUI, _ []byte) {
		for i := range all {
			if all[i] == id {
				atomic.AddInt64(&counts[i], 1)
			}
		}
	}, provs...)

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for i := range provs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := h.newDevice(t, provs[i], nil)
			if err := d.Join(ctx); err != nil {
				t.Errorf("device %d join: %v", i, err)
				return
			}
			for j := 0; j < 3; j++ {
				if err := d.SendConfirmed(ctx, []byte{byte(i), byte(j)}); err != nil {
					t.Errorf("device %d send %d: %v", i, j, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i := range counts {
		if atomic.LoadInt64(&counts[i]) != 3 {
			t.Fatalf("device %d deliveries = %d, want 3", i, counts[i])
		}
	}
}

// Invalid-frame noise must not block a healthy device's join or traffic:
// garbage never routes to a peer inbox (unknown DevAddr/DevEUI), and even a
// saturated peer queue only drops that peer's own frames.
func TestNoiseDoesNotBlockHealthyDevice(t *testing.T) {
	healthy := provision(61)
	h := newHarness(t, nil, healthy)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// Data frame with an unknown DevAddr and bogus body.
				_ = h.pipe.DeviceEnd().Send(ctx,
					[]byte{0x80, 0, 0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
			}
		}
	}()

	d := h.newDevice(t, healthy, nil)
	if err := d.Join(ctx); err != nil {
		t.Fatalf("join amid noise: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := d.SendConfirmed(ctx, []byte{byte(i)}); err != nil {
			t.Fatalf("healthy traffic blocked by noise: %v", err)
		}
	}
	close(stop)
}

// A storm of correctly-addressed but bad-MIC frames from one device must be
// isolated: it only inflates that device's bad-frame counter, while a second
// device joins and exchanges traffic normally.
func TestOneDeviceStormIsolatedFromAnother(t *testing.T) {
	storm := provision(62)
	healthy := provision(63)
	h := newHarness(t, nil, storm, healthy)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// Bring the storm device up just to learn its DevAddr and keys.
	sd := h.newDevice(t, storm, nil)
	if err := sd.Join(ctx); err != nil {
		t.Fatal(err)
	}
	ss := sd.Session()

	// Build a valid frame then corrupt its MIC; send repeatedly from its own
	// endpoint (so routing resolves to its peer inbox) to saturate handling.
	good, err := lorawan.EncodeFrame(&ss, &lorawan.Frame{
		MType:   lorawan.MTypeConfirmedDataUp,
		DevAddr: ss.DevAddr,
		FCnt:    100,
		HasPort: true,
		FPort:   1,
		Payload: []byte("storm"),
	})
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0xff

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = sd.Air().Send(ctx, bad)
			}
		}
	}()

	// Meanwhile, an unrelated device joins and does confirmed traffic.
	hd := h.newDevice(t, healthy, nil)
	if err := hd.Join(ctx); err != nil {
		t.Fatalf("healthy join during storm: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := hd.SendConfirmed(ctx, []byte{byte(i)}); err != nil {
			t.Fatalf("healthy traffic during storm: %v", err)
		}
	}
	close(stop)

	_, stormBad, ok := h.server.Stats(storm.DevEUI)
	if !ok || stormBad == 0 {
		t.Fatalf("storm bad frames not counted: %d", stormBad)
	}
	_, healthyBad, _ := h.server.Stats(healthy.DevEUI)
	if healthyBad != 0 {
		t.Fatalf("healthy device accumulated bad frames: %d", healthyBad)
	}
}
