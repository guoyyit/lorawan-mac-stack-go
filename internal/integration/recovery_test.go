package integration

import (
	"context"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/device"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/server"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/transport"
)

// restartStack tears down and recreates server and device from the same
// on-disk stores, simulating both sides crashing and restarting.
func restartStack(t *testing.T, devDir, srvDir string, p server.DeviceProvision) (*transport.Medium, *server.Server, *device.Device) {
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return pipe, srv, dev
}

func TestCrashRecoveryKeepsSessionAndCounters(t *testing.T) {
	p := provision(30)
	devDir, srvDir := t.TempDir(), t.TempDir()

	_, srv, dev := restartStack(t, devDir, srvDir, p)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := dev.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dev.SendConfirmed(ctx, []byte("before crash 1")); err != nil {
		t.Fatal(err)
	}
	if err := dev.SendConfirmed(ctx, []byte("before crash 2")); err != nil {
		t.Fatal(err)
	}
	upBefore := dev.Session().FCntUp
	addrBefore := dev.Session().DevAddr
	_ = srv.Close()
	_ = dev.Close()

	// Restart from disk.
	_, srv2, dev2 := restartStack(t, devDir, srvDir, p)
	defer func() { _ = srv2.Close(); _ = dev2.Close() }()

	// Session restored: already joined, no rejoin needed.
	s2 := dev2.Session()
	if !s2.Joined || s2.DevAddr != addrBefore {
		t.Fatalf("session not restored: joined=%v addr=%x", s2.Joined, s2.DevAddr)
	}
	if s2.FCntUp != upBefore {
		t.Fatalf("FCntUp after restart = %d, want %d", s2.FCntUp, upBefore)
	}
	ss, err := srv2.Session(p.DevEUI)
	if err != nil || ss.DevAddr != addrBefore {
		t.Fatalf("server session not restored: %+v %v", ss, err)
	}

	// Communication continues; counters move forward without reuse.
	if err := dev2.SendConfirmed(ctx, []byte("after crash")); err != nil {
		t.Fatalf("communication did not resume: %v", err)
	}
	if got := dev2.Session().FCntUp; got != upBefore+1 {
		t.Fatalf("FCntUp after resume = %d, want %d", got, upBefore+1)
	}
}

// After both sides restart, a frame carrying a pre-crash FCnt is a replay.
func TestRecoveryRejectsReplayedOldFrame(t *testing.T) {
	p := provision(31)
	devDir, srvDir := t.TempDir(), t.TempDir()

	_, srv, dev := restartStack(t, devDir, srvDir, p)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := dev.Join(ctx); err != nil {
		t.Fatal(err)
	}
	s := dev.Session()
	stale, err := lorawan.EncodeFrame(&s, &lorawan.Frame{
		MType:   lorawan.MTypeUnconfirmedDataUp,
		DevAddr: s.DevAddr,
		FCnt:    0,
		HasPort: true,
		FPort:   1,
		Payload: []byte("stale"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Move the real floor past 0.
	if err := dev.SendConfirmed(ctx, []byte("advance")); err != nil {
		t.Fatal(err)
	}
	if err := dev.SendConfirmed(ctx, []byte("advance2")); err != nil {
		t.Fatal(err)
	}
	_ = srv.Close()
	_ = dev.Close()

	_, srv2, dev2 := restartStack(t, devDir, srvDir, p)
	defer func() { _ = srv2.Close(); _ = dev2.Close() }()

	// Inject the captured pre-crash frame; a replay increments badFrames,
	// not FCntUp.
	before, _ := srv2.Session(p.DevEUI)
	if err := dev2.Air().Send(ctx, stale); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	_, bad, ok := srv2.Stats(p.DevEUI)
	if !ok || bad == 0 {
		t.Fatalf("replay after restart not recorded as bad frame (bad=%d)", bad)
	}
	after, _ := srv2.Session(p.DevEUI)
	if after.FCntUp != before.FCntUp {
		t.Fatalf("replay advanced uplink floor: %d -> %d", before.FCntUp, after.FCntUp)
	}
	// Legitimate post-restart traffic still works with a fresh counter.
	if err := dev2.SendConfirmed(ctx, []byte("alive")); err != nil {
		t.Fatal(err)
	}
}
