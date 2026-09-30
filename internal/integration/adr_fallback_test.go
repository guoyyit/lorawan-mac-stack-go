package integration

import (
	"context"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/device"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/mac"
)

// Persistent missed ACKs cause an autonomous DR step-down; join/counters
// remain intact.
func TestADRFallbackOnPersistentLoss(t *testing.T) {
	p := provision(70)
	h := newHarness(t, nil, p)

	d, err := device.New(device.Config{
		DevEUI:   p.DevEUI,
		JoinEUI:  p.JoinEUI,
		AppKey:   p.AppKey,
		Store:    lorawan.NewMemoryStore(),
		Air:      h.pipe.NewDeviceEnd(),
		RX1Delay: rx1,
		RX2Delay: rx2,
		RXWindow: rxWin,
		Retries:  2,
		Backoff:  backoff,
		Airtime:  time.Millisecond,
		Duty:     mac.DutyConfig{},
		ADR: mac.ADRConfig{
			Enabled:       true,
			MinDR:         0,
			MaxDR:         5,
			MinTXPower:    0,
			MaxTXPower:    3,
			DownMargin:    -1,
			BackoffMisses: 6,
		},
		InitialDR: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	addr := d.Session().DevAddr

	// Lose all of this device's uplinks; none can be acknowledged.
	d.Air().DropNext(100)
	for i := 0; i < 2; i++ {
		_ = d.SendConfirmed(ctx, []byte{byte(i)})
	}

	s := d.Session()
	if s.DataRate != 3 {
		t.Fatalf("DataRate after persistent loss = %d, want fallback to 3", s.DataRate)
	}
	if !s.Joined || s.DevAddr != addr {
		t.Fatal("join state lost during ADR fallback")
	}
}
