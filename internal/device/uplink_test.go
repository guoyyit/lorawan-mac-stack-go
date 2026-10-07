package device

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/mac"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/transport"
)

func joinedTestDevice(t *testing.T) *Device {
	t.Helper()
	m := transport.NewMedium()
	dev, err := New(Config{
		DevEUI:    lorawan.EUI{1},
		JoinEUI:   lorawan.EUI{2},
		AppKey:    lorawan.AESKey{3},
		Store:     lorawan.NewMemoryStore(),
		Air:       m.DeviceEnd(),
		RX1Delay:  time.Second,
		RX2Delay:  2 * time.Second,
		RXWindow:  time.Second,
		Duty:      mac.DutyConfig{},
		ADR:       mac.ADRConfig{},
		OnAppData: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	s := dev.session
	s.Joined = true
	s.DevAddr = lorawan.DevAddr{9, 9, 9, 9}
	s.NwkSKey = lorawan.AESKey{0x10}
	s.AppSKey = lorawan.AESKey{0x20}
	return dev
}

// An oversize uplink must fail with ErrFrameTooLong and leave no side effects:
// no counter consumed, no ADR/ACK bits cleared, and a following legal send works.
func TestOversizeUplinkNoSideEffects(t *testing.T) {
	dev := joinedTestDevice(t)
	ctx := context.Background()

	dev.mu.Lock()
	dev.pendingACK = true
	dev.pendingLinkADRAns = []byte{lorawan.CIDLinkADRAns, 0x07}
	dev.mu.Unlock()

	before := dev.Session().FCntUp
	err := dev.SendUnconfirmed(ctx, make([]byte, lorawan.MaxPayload+1))
	if !errors.Is(err, lorawan.ErrFrameTooLong) {
		t.Fatalf("oversize send err = %v, want ErrFrameTooLong", err)
	}
	after := dev.Session()
	if after.FCntUp != before {
		t.Fatalf("FCntUp changed %d -> %d on rejected send", before, after.FCntUp)
	}
	dev.mu.Lock()
	ackPending := dev.pendingACK
	optPending := len(dev.pendingLinkADRAns)
	dev.mu.Unlock()
	if !ackPending || optPending != 2 {
		t.Fatalf("pending state consumed: ack=%v opts=%d", ackPending, optPending)
	}
}
