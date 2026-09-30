// Package integration holds end-to-end device<->server tests over the
// in-memory loopback transport.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/device"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/mac"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/server"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/transport"
)

const (
	rx1     = 20 * time.Millisecond
	rx2     = 40 * time.Millisecond
	rxWin   = 20 * time.Millisecond
	backoff = 5 * time.Millisecond
)

type harness struct {
	pipe   *transport.Medium
	server *server.Server
	app    func(lorawan.EUI, []byte)
}

func newHarness(t *testing.T, onApp server.AppHandler, devs ...server.DeviceProvision) *harness {
	t.Helper()
	p := transport.NewPipe()
	srv, err := server.New(server.Config{
		NetID:     lorawan.NetID{0x00, 0x00, 0x01},
		Store:     lorawan.NewMemoryStore(),
		Air:       p.ServerEnd(),
		Devices:   devs,
		OnAppData: onApp,
		RX1Delay:  rx1,
		RX2Delay:  rx2,
		RXWindow:  rxWin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return &harness{pipe: p, server: srv, app: onApp}
}

func provision(n byte) server.DeviceProvision {
	return server.DeviceProvision{
		DevEUI:  lorawan.EUI{0, 0, 0, 0, 0, 0, 0, n},
		JoinEUI: lorawan.EUI{0, 0, 0, 0, 0, 0, 0, 0xee},
		AppKey:  lorawan.AESKey{n, n + 1, n + 2},
	}
}

func (h *harness) newDevice(t *testing.T, p server.DeviceProvision, onData device.AppHandler) *device.Device {
	t.Helper()
	d, err := device.New(device.Config{
		DevEUI:     p.DevEUI,
		JoinEUI:    p.JoinEUI,
		AppKey:     p.AppKey,
		Store:      lorawan.NewMemoryStore(),
		Air:        h.pipe.NewDeviceEnd(),
		RX1Delay:   rx1,
		RX2Delay:   rx2,
		RXWindow:   rxWin,
		Retries:    3,
		Backoff:    backoff,
		BackoffMax: 20 * time.Millisecond,
		Airtime:    time.Millisecond,
		Duty:       mac.DutyConfig{}, // disabled by default
		ADR: mac.ADRConfig{
			Enabled:       true,
			MinDR:         0,
			MaxDR:         5,
			MinTXPower:    0,
			MaxTXPower:    3,
			UpMargin:      10,
			DownMargin:    -15,
			BackoffMisses: 3,
		},
		InitialDR: 3,
		OnAppData: onData,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// inject sends raw bytes as if from the given device's radio endpoint.
func (h *harness) inject(d *device.Device, raw []byte) error {
	return d.Air().Send(context.Background(), raw)
}

func TestOTAAJoin(t *testing.T) {
	p := provision(1)
	h := newHarness(t, nil, p)
	d := h.newDevice(t, p, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatalf("join: %v", err)
	}
	ds := d.Session()
	ss, err := h.server.Session(p.DevEUI)
	if err != nil {
		t.Fatal(err)
	}
	if ds.DevAddr != ss.DevAddr {
		t.Fatalf("DevAddr mismatch dev=%x srv=%x", ds.DevAddr, ss.DevAddr)
	}
	if ds.NwkSKey != ss.NwkSKey || ds.AppSKey != ss.AppSKey {
		t.Fatal("session keys differ between sides")
	}
	if ds.FCntUp != 0 || ds.FCntDown != 0 {
		t.Fatalf("counter init = up %d down %d, want 0", ds.FCntUp, ds.FCntDown)
	}
}
