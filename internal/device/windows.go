package device

import (
	"context"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

// waitWindows implements the Class A RX1 then RX2 reception timeline.
// Delays are relative to the end of the preceding uplink: RX1 opens at
// RX1Delay for RXWindow, RX2 opens at RX2Delay for RXWindow. A frame is
// accepted in the first window that contains one.
func (d *Device) waitWindows(ctx context.Context) (incoming, error) {
	d.drainStale()

	if in, ok := d.openWindow(ctx, 0, d.cfg.RX1Delay, d.cfg.RXWindow); ok {
		return in, nil
	}
	if in, ok := d.openWindow(ctx, d.cfg.RX1Delay+d.cfg.RXWindow, d.cfg.RX2Delay, d.cfg.RXWindow); ok {
		return in, nil
	}
	return incoming{}, lorawan.ErrNoACK
}

// openWindow sleeps from elapsed to startAt (both offsets from the uplink),
// then listens for win.
func (d *Device) openWindow(ctx context.Context, elapsed, startAt, win time.Duration) (incoming, bool) {
	if wait := startAt - elapsed; wait > 0 {
		if err := sleepCtx(ctx, wait); err != nil {
			return incoming{}, false
		}
	}
	return d.recvWindow(ctx, win)
}
