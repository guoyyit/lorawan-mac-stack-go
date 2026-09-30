package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/device"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/mac"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/server"
)

func newDutyDevice(t *testing.T, n byte, duty mac.DutyConfig) (*harness, *device.Device) {
	t.Helper()
	p := provision(n)
	h := newHarness(t, nil, p)
	d, err := device.New(device.Config{
		DevEUI:    p.DevEUI,
		JoinEUI:   p.JoinEUI,
		AppKey:    p.AppKey,
		Store:     lorawan.NewMemoryStore(),
		Air:       h.pipe.DeviceEnd(),
		RX1Delay:  rx1,
		RX2Delay:  rx2,
		RXWindow:  rxWin,
		Backoff:   backoff,
		Airtime:   50 * time.Millisecond,
		Duty:      duty,
		InitialDR: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	// The join request itself opened a 100 ms debt window; let it elapse.
	time.Sleep(120 * time.Millisecond)
	return h, d
}

// Under DutyDrop, a second send inside the debt window fails immediately and
// deterministically with ErrDutyCycle.
func TestDutyCycleDrops(t *testing.T) {
	// 50 ms airtime at 50% => 100 ms debt window.
	_, d := newDutyDevice(t, 20, mac.DutyConfig{MaxDutyCycle: 0.5, Policy: mac.DutyDrop})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.SendUnconfirmed(ctx, []byte("a")); err != nil {
		t.Fatal(err)
	}
	err := d.SendUnconfirmed(ctx, []byte("b"))
	if !errors.Is(err, lorawan.ErrDutyCycle) {
		t.Fatalf("second send = %v, want ErrDutyCycle", err)
	}
	// The rejected send must not have consumed an uplink counter.
	if got := d.Session().FCntUp; got != 1 {
		t.Fatalf("FCntUp = %d, want 1", got)
	}
}

// Under DutyWait, a limited send blocks until the window opens, then succeeds.
func TestDutyCycleWaits(t *testing.T) {
	_, d := newDutyDevice(t, 21, mac.DutyConfig{MaxDutyCycle: 0.5, Policy: mac.DutyWait})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.SendUnconfirmed(ctx, []byte("a")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := d.SendUnconfirmed(ctx, []byte("b")); err != nil {
		t.Fatalf("queued send should succeed after waiting: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("send returned too fast after duty limit: %v", elapsed)
	}
	if got := d.Session().FCntUp; got != 2 {
		t.Fatalf("FCntUp = %d, want 2", got)
	}
}

var _ = server.DeviceProvision{}
