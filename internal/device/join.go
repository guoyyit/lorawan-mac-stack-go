package device

import (
	"context"
	"errors"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

// Join performs OTAA activation. A restored, already-joined device returns
// immediately without consuming a new DevNonce.
func (d *Device) Join(ctx context.Context) error {
	d.mu.Lock()
	if d.session.Joined {
		d.mu.Unlock()
		return nil
	}
	// DevNonce strictly increments and is persisted BEFORE the request goes
	// on air, so a crash cannot reuse a nonce the server may have accepted.
	nonce := uint16(d.session.LastDevNonce) + 1
	if nonce == 0 {
		d.mu.Unlock()
		return lorawan.ErrCounterOverflow
	}
	d.session.LastDevNonce = nonce
	gen := d.joinGen + 1
	d.joinGen = gen
	if err := d.persistLocked(ctx); err != nil {
		d.mu.Unlock()
		return err
	}
	appKey := d.session.AppKey
	d.mu.Unlock()

	jr := &lorawan.JoinRequest{
		JoinEUI:  d.cfg.JoinEUI,
		DevEUI:   d.cfg.DevEUI,
		DevNonce: lorawan.DevNonce(nonce),
	}
	raw, err := lorawan.EncodeJoinRequest(appKey, jr)
	if err != nil {
		return err
	}
	if _, err := d.duty.Reserve(ctx, time.Now(), d.cfg.Airtime); err != nil {
		return err
	}
	if err := d.cfg.Air.Send(ctx, raw); err != nil {
		return err
	}

	in, err := d.waitWindows(ctx)
	if err != nil {
		return err
	}
	return d.applyJoinAccept(ctx, in.join, gen)
}

func (d *Device) applyJoinAccept(ctx context.Context, ja *lorawan.JoinAccept, gen uint64) error {
	if ja == nil {
		return lorawan.ErrMalformed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if gen != d.joinGen {
		return errors.New("device: stale join accept")
	}
	nwk, app := lorawan.DeriveSessionKeys(
		d.cfg.AppKey, ja.JoinNonce, ja.NetID, lorawan.DevNonce(d.session.LastDevNonce),
	)
	s := d.session
	s.JoinNonce, s.NetID, s.DevAddr = ja.JoinNonce, ja.NetID, ja.DevAddr
	s.NwkSKey, s.AppSKey = nwk, app
	s.RX1DROffset, s.RX2DR, s.RXDelay = ja.RX1DROffset, ja.RX2DR, ja.RXDelay
	s.Joined = true
	// Fresh join resets counters deterministically.
	s.FCntUp, s.FCntDown = 0, 0
	s.UpExhausted, s.DownExhausted = false, false
	s.DataRate, s.TXPower = d.adr.State()
	return d.persistLocked(ctx)
}

// drainStale removes downlinks buffered before the current window.
func (d *Device) drainStale() {
	for {
		select {
		case <-d.downCh:
		default:
			return
		}
	}
}

func (d *Device) recvWindow(ctx context.Context, win time.Duration) (incoming, bool) {
	t := time.NewTimer(win)
	defer t.Stop()
	select {
	case in := <-d.downCh:
		return in, true
	case <-t.C:
		return incoming{}, false
	case <-ctx.Done():
		return incoming{}, false
	}
}
