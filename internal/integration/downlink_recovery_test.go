package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/device"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/mac"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/server"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/transport"
)

// waitForAppData fails unless at least n distinct payloads are collected.
func waitForAppData(t *testing.T, mu *sync.Mutex, got *[][]byte, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		k := len(*got)
		mu.Unlock()
		if k >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("received %d payloads, want %d: %q", len(*got), n, *got)
}

// restartDeviceOn closes the old device and creates a new one attached to the
// same medium and persistent directory, optionally with an app handler. The
// server is NOT restarted, modelling a device-only crash (the reported bug).
func restartDeviceOn(t *testing.T, pipe *transport.Medium, p server.DeviceProvision, devDir string, onData device.AppHandler) *device.Device {
	t.Helper()
	devStore, err := lorawan.NewFileStore(lorawan.FileStoreConfig{Dir: devDir, AtomicRename: true, Fsync: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := device.New(device.Config{
		DevEUI:    p.DevEUI,
		JoinEUI:   p.JoinEUI,
		AppKey:    p.AppKey,
		Store:     devStore,
		Air:       pipe.NewDeviceEnd(),
		RX1Delay:  rx1,
		RX2Delay:  rx2,
		RXWindow:  rxWin,
		Retries:   2,
		Backoff:   backoff,
		Airtime:   time.Millisecond,
		InitialDR: 3,
		ADR: mac.ADRConfig{
			Enabled:    true,
			MinDR:      0,
			MaxDR:      5,
			MinTXPower: 0,
			MaxTXPower: 3,
		},
		OnAppData: onData,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// After the device receives a downlink and restarts, its persisted FCntDown
// must not roll back: subsequent server application data and MAC commands are
// still received, and each payload is delivered exactly once.
func TestDownlinkSurvivesDeviceRestart(t *testing.T) {
	p := provision(80)
	devDir, srvDir := t.TempDir(), t.TempDir()

	pipe, srv, dev := restartStack(t, devDir, srvDir, p)
	defer func() { _ = srv.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := dev.Join(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got [][]byte
	collect := func(payload []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), payload...))
		mu.Unlock()
	}

	// Downlink #1 before restart: advances and (after the fix) persists
	// FCntDown to 1 on the device.
	if err := srv.EnqueueDown(p.DevEUI, []byte("before-restart")); err != nil {
		t.Fatal(err)
	}
	if err := dev.SendUnconfirmed(ctx, []byte("up1")); err != nil {
		t.Fatal(err)
	}
	// Give the RX1 delivery time to land and persist before the crash.
	time.Sleep(100 * time.Millisecond)
	if down := dev.Session().FCntDown; down != 1 {
		t.Fatalf("FCntDown before restart = %d, want 1", down)
	}
	_ = dev.Close()

	// Device crashes and restarts from disk; server keeps running.
	dev2 := restartDeviceOn(t, pipe, p, devDir, collect)
	defer func() { _ = dev2.Close() }()
	if down := dev2.Session().FCntDown; down != 1 {
		t.Fatalf("FCntDown rolled back after restart = %d, want 1", down)
	}

	// Downlink #2, application data, after restart: must be delivered.
	if err := srv.EnqueueDown(p.DevEUI, []byte("after-restart-1")); err != nil {
		t.Fatal(err)
	}
	if err := dev2.SendUnconfirmed(ctx, []byte("up2")); err != nil {
		t.Fatalf("uplink after restart: %v", err)
	}

	// Downlink #3 carried together with a LinkADRReq MAC command: the device
	// must still accept and process the content at the next counter.
	if err := srv.EnqueueDown(p.DevEUI, []byte("after-restart-2")); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetLinkADR(p.DevEUI, 4, 1); err != nil {
		t.Fatal(err)
	}
	if err := dev2.SendUnconfirmed(ctx, []byte("up3")); err != nil {
		t.Fatalf("uplink with downlink+MAC after restart: %v", err)
	}

	waitForAppData(t, &mu, &got, 2)
	mu.Lock()
	if string(got[0]) != "after-restart-1" || string(got[1]) != "after-restart-2" {
		t.Fatalf("unexpected/redelivered payloads: %q", got)
	}
	mu.Unlock()

	if down := dev2.Session().FCntDown; down != 3 {
		t.Fatalf("FCntDown after restart traffic = %d, want 3", down)
	}
	// The LinkADRAns is carried on a subsequent uplink; send one and poll for
	// the server to observe the answer and the device to persist the new DR.
	if err := dev2.SendUnconfirmed(ctx, []byte("up4")); err != nil {
		t.Fatalf("answer uplink: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if acks, _ := srv.ADRACKCount(p.DevEUI); acks == 1 && dev2.Session().DataRate == 4 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if dr := dev2.Session().DataRate; dr != 4 {
		t.Fatalf("LinkADRReq after restart not applied, DR = %d", dr)
	}
	acks, ok := srv.ADRACKCount(p.DevEUI)
	if !ok || acks != 1 {
		t.Fatalf("LinkADRAns after restart not received: %d (ok=%v)", acks, ok)
	}
}
