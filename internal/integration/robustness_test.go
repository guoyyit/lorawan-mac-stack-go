package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/device"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/mac"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/server"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/transport"
)

// appCollector records downlink payloads delivered to a device.
type appCollector struct {
	mu   sync.Mutex
	data [][]byte
}

func (c *appCollector) add(b []byte) {
	c.mu.Lock()
	c.data = append(c.data, append([]byte(nil), b...))
	c.mu.Unlock()
}

func (c *appCollector) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.data...)
}

// buildOversizedFrame crafts a wire frame that declares a huge payload and
// really is that long -- the input that used to panic the receiver.
func buildOversizedFrame(addr lorawan.DevAddr) []byte {
	raw := make([]byte, 14+65535+4)
	raw[0] = byte(lorawan.MTypeUnconfirmedDataUp)
	copy(raw[2:6], addr[:])
	raw[11], raw[12] = 0xff, 0xff // declared payload length 65535
	return raw
}

// An oversize payload must be rejected by the send call with a distinguishable
// error -- no panic, no counter consumed, no session change, and the next
// legal send uses the very next counter value.
func TestOversizedUplinkRejectedWithoutSideEffects(t *testing.T) {
	p := provision(90)
	var srvMu sync.Mutex
	var srvGot [][]byte
	h := newHarness(t, func(_ lorawan.EUI, b []byte) {
		srvMu.Lock()
		srvGot = append(srvGot, append([]byte(nil), b...))
		srvMu.Unlock()
	}, p)
	d := h.newDevice(t, p, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}

	for _, size := range []int{lorawan.MaxPayloadSize + 1, 4096, 1 << 20} {
		if err := d.SendUnconfirmed(ctx, make([]byte, size)); !errors.Is(err, lorawan.ErrFrameTooLarge) {
			t.Fatalf("unconfirmed size %d: err = %v, want ErrFrameTooLarge", size, err)
		}
		if err := d.SendConfirmed(ctx, make([]byte, size)); !errors.Is(err, lorawan.ErrFrameTooLarge) {
			t.Fatalf("confirmed size %d: err = %v, want ErrFrameTooLarge", size, err)
		}
	}
	if got := d.Session().FCntUp; got != 0 {
		t.Fatalf("rejected sends consumed frame counters: FCntUp = %d", got)
	}

	// A payload at exactly the limit goes through; the counter continues
	// from 0 with no gap left by the rejected sends.
	if err := d.SendUnconfirmed(ctx, make([]byte, lorawan.MaxPayloadSize)); err != nil {
		t.Fatalf("max-size payload should send: %v", err)
	}
	if got := d.Session().FCntUp; got != 1 {
		t.Fatalf("FCntUp after first legal send = %d, want 1", got)
	}
	time.Sleep(50 * time.Millisecond)
	srvMu.Lock()
	defer srvMu.Unlock()
	if len(srvGot) != 1 || len(srvGot[0]) != lorawan.MaxPayloadSize {
		t.Fatalf("server deliveries = %d, want exactly the max-size frame", len(srvGot))
	}
}

// Injecting oversize and malformed frames at the server must neither kill the
// process nor advance any counter; the device survives oversize garbage in
// the downlink direction too, and both sides keep working.
func TestServerAndDeviceSurviveOversizedFrames(t *testing.T) {
	p := provision(91)
	var srvMu sync.Mutex
	var srvGot [][]byte
	h := newHarness(t, func(_ lorawan.EUI, b []byte) {
		srvMu.Lock()
		srvGot = append(srvGot, b)
		srvMu.Unlock()
	}, p)
	d := h.newDevice(t, p, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	sess := d.Session()
	before, err := h.server.Session(p.DevEUI)
	if err != nil {
		t.Fatal(err)
	}

	// Oversize uplink (declared 65535 payload, really that long), a truncated
	// frame claiming a large payload, and random garbage.
	if err := h.inject(d, buildOversizedFrame(sess.DevAddr)); err != nil {
		t.Fatal(err)
	}
	trunc := []byte{0x40, 0, 1, 2, 3, 4, 0, 0, 0, 0, 0, 0xff, 0xff, 0, 1, 2, 3, 4}
	if err := h.inject(d, trunc); err != nil {
		t.Fatal(err)
	}
	if err := h.inject(d, []byte{0x40, 9, 9, 9}); err != nil {
		t.Fatal(err)
	}
	// Oversize garbage in the downlink direction (broadcast to all devices).
	down := buildOversizedFrame(sess.DevAddr)
	down[0] = byte(lorawan.MTypeUnconfirmedDataDown)
	if err := h.pipe.ServerEnd().Send(ctx, down); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	after, err := h.server.Session(p.DevEUI)
	if err != nil {
		t.Fatal(err)
	}
	if after.FCntUp != before.FCntUp {
		t.Fatalf("rejected frames advanced FCntUp: %d -> %d", before.FCntUp, after.FCntUp)
	}
	_, bad, ok := h.server.Stats(p.DevEUI)
	if !ok || bad == 0 {
		t.Fatalf("oversize/malformed frames not counted as bad (bad=%d)", bad)
	}

	// Both sides are still fully functional.
	if err := d.SendConfirmed(ctx, []byte("still alive")); err != nil {
		t.Fatalf("stack did not survive oversize input: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	srvMu.Lock()
	defer srvMu.Unlock()
	if len(srvGot) != 1 || string(srvGot[0]) != "still alive" {
		t.Fatalf("deliveries = %q, want only the post-attack uplink", srvGot)
	}
}

// A faulty device hammering the server with oversize and malformed frames
// must not affect other devices joining, sending confirmed uplinks and
// receiving downlinks concurrently.
func TestFaultyDeviceIsolatedFromHealthyDevices(t *testing.T) {
	faulty := provision(92)
	healthyA := provision(93)
	healthyB := provision(94)
	h := newHarness(t, nil, faulty, healthyA, healthyB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The faulty device joins only to obtain a routable DevAddr, then floods.
	fd := h.newDevice(t, faulty, nil)
	if err := fd.Join(ctx); err != nil {
		t.Fatal(err)
	}
	fsess := fd.Session()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// Sustained pressure uses just-over-limit frames (300 > MaxFrameSize)
		// so the flood exercises the rejection path without needless
		// allocation churn; the one-shot tests use the full 64 KiB monster.
		oversize := make([]byte, lorawan.MaxFrameSize+41)
		oversize[0] = byte(lorawan.MTypeUnconfirmedDataUp)
		copy(oversize[2:6], fsess.DevAddr[:])
		garbage := []byte{0x80, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}
		for {
			select {
			case <-stop:
				return
			default:
				_ = fd.Air().Send(ctx, oversize)
				_ = fd.Air().Send(ctx, garbage)
			}
		}
	}()

	// Healthy devices join, run confirmed uplinks and receive a downlink each.
	var wg sync.WaitGroup
	for _, p := range []server.DeviceProvision{healthyA, healthyB} {
		wg.Add(1)
		go func(p server.DeviceProvision) {
			defer wg.Done()
			col := &appCollector{}
			d := h.newDevice(t, p, col.add)
			if err := d.Join(ctx); err != nil {
				t.Errorf("healthy join during flood: %v", err)
				return
			}
			for i := 0; i < 3; i++ {
				if err := d.SendConfirmed(ctx, []byte{byte(i)}); err != nil {
					t.Errorf("healthy confirmed uplink during flood: %v", err)
					return
				}
			}
			if err := h.server.EnqueueDown(p.DevEUI, []byte("down-"+string(rune(p.DevEUI[7])))); err != nil {
				t.Errorf("enqueue: %v", err)
				return
			}
			if err := d.SendUnconfirmed(ctx, []byte("open-window")); err != nil {
				t.Errorf("window uplink: %v", err)
				return
			}
			got := col.all()
			if len(got) != 1 {
				t.Errorf("healthy downlink deliveries = %d, want 1", len(got))
			}
		}(p)
	}
	wg.Wait()

	// The flood was attributed to the faulty device alone.
	_, bad, ok := h.server.Stats(faulty.DevEUI)
	if !ok || bad == 0 {
		t.Fatalf("faulty device's bad frames not counted: %d", bad)
	}
	for _, p := range []server.DeviceProvision{healthyA, healthyB} {
		if _, b, _ := h.server.Stats(p.DevEUI); b != 0 {
			t.Fatalf("healthy device %x accumulated bad frames: %d", p.DevEUI, b)
		}
	}
}

// After receiving a downlink and restarting, the device must keep its
// downlink counter: the server's next application downlink and MAC command
// are still received, each exactly once, without rejoining.
func TestDownlinkCounterSurvivesRestart(t *testing.T) {
	p := provision(95)
	devDir, srvDir := t.TempDir(), t.TempDir()
	col := &appCollector{}

	open := func(t *testing.T) (*server.Server, *device.Device) {
		t.Helper()
		srvStore, err := lorawan.NewFileStore(lorawan.FileStoreConfig{Dir: srvDir, AtomicRename: true, Fsync: true})
		if err != nil {
			t.Fatal(err)
		}
		devStore, err := lorawan.NewFileStore(lorawan.FileStoreConfig{Dir: devDir, AtomicRename: true, Fsync: true})
		if err != nil {
			t.Fatal(err)
		}
		pipe := transport.NewPipe()
		srv, err := server.New(server.Config{
			NetID:    lorawan.NetID{0, 0, 1},
			Store:    srvStore,
			Air:      pipe.ServerEnd(),
			Devices:  []server.DeviceProvision{p},
			RX1Delay: rx1,
			RX2Delay: rx2,
			RXWindow: rxWin,
		})
		if err != nil {
			t.Fatal(err)
		}
		dev, err := device.New(device.Config{
			DevEUI:    p.DevEUI,
			JoinEUI:   p.JoinEUI,
			AppKey:    p.AppKey,
			Store:     devStore,
			Air:       pipe.DeviceEnd(),
			RX1Delay:  rx1,
			RX2Delay:  rx2,
			RXWindow:  rxWin,
			Retries:   2,
			Backoff:   backoff,
			Airtime:   time.Millisecond,
			InitialDR: 3,
			ADR: mac.ADRConfig{
				Enabled: true, MinDR: 0, MaxDR: 5, MinTXPower: 0, MaxTXPower: 3,
			},
			OnAppData: col.add,
		})
		if err != nil {
			t.Fatal(err)
		}
		return srv, dev
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Phase 1: join and receive one application downlink (FCntDown 0 -> 1).
	srv1, dev1 := open(t)
	if err := dev1.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := srv1.EnqueueDown(p.DevEUI, []byte("down-1")); err != nil {
		t.Fatal(err)
	}
	if err := dev1.SendUnconfirmed(ctx, []byte("up-1")); err != nil {
		t.Fatal(err)
	}
	if got := col.all(); len(got) != 1 || string(got[0]) != "down-1" {
		t.Fatalf("phase 1 deliveries = %q", got)
	}
	if got := dev1.Session().FCntDown; got != 1 {
		t.Fatalf("FCntDown after first downlink = %d, want 1", got)
	}
	_ = srv1.Close()
	_ = dev1.Close()

	// Phase 2: restart both sides from disk. The device must resume with
	// FCntDown == 1; the server's next downlink (FCnt 1) must be accepted.
	srv2, dev2 := open(t)
	defer func() { _ = srv2.Close(); _ = dev2.Close() }()
	if got := dev2.Session().FCntDown; got != 1 {
		t.Fatalf("FCntDown rolled back across restart: %d, want 1", got)
	}
	if err := srv2.EnqueueDown(p.DevEUI, []byte("down-2")); err != nil {
		t.Fatal(err)
	}
	if err := srv2.SetLinkADR(p.DevEUI, 4, 1); err != nil {
		t.Fatal(err)
	}
	if err := dev2.SendUnconfirmed(ctx, []byte("up-2")); err != nil {
		t.Fatal(err)
	}
	got := col.all()
	if len(got) != 2 || string(got[0]) != "down-1" || string(got[1]) != "down-2" {
		t.Fatalf("deliveries after restart = %q, want down-1 then down-2 exactly once", got)
	}
	// The MAC command piggybacked on the post-restart downlink was applied.
	if dr := dev2.Session().DataRate; dr != 4 {
		t.Fatalf("LinkADR after restart not applied: DR = %d, want 4", dr)
	}
	if got := dev2.Session().FCntDown; got != 2 {
		t.Fatalf("FCntDown after second downlink = %d, want 2", got)
	}

	// Phase 3: one more restart; downlink flow still works (no permanent
	// replay-stall), and nothing is ever delivered twice.
	sess2, err := srv2.Session(p.DevEUI)
	if err != nil {
		t.Fatal(err)
	}
	_ = srv2.Close()
	_ = dev2.Close()
	srv3, dev3 := open(t)
	defer func() { _ = srv3.Close(); _ = dev3.Close() }()
	if got := dev3.Session().FCntDown; got != 2 {
		t.Fatalf("FCntDown after second restart = %d, want 2", got)
	}
	if sess2.FCntDown != 2 {
		t.Fatalf("server FCntDown = %d, want 2", sess2.FCntDown)
	}
	if err := srv3.EnqueueConfirmedDown(p.DevEUI, []byte("down-3")); err != nil {
		t.Fatal(err)
	}
	if err := dev3.SendUnconfirmed(ctx, []byte("up-3")); err != nil {
		t.Fatal(err)
	}
	got = col.all()
	if len(got) != 3 || string(got[2]) != "down-3" {
		t.Fatalf("deliveries after second restart = %q", got)
	}
}
