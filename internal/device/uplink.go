package device

import (
	"context"
	"errors"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

// SendUnconfirmed transmits an unconfirmed uplink and opens RX1/RX2 for any
// server-initiated downlink.
func (d *Device) SendUnconfirmed(ctx context.Context, payload []byte) error {
	return d.tx(ctx, lorawan.MTypeUnconfirmedDataUp, payload, false)
}

// SendConfirmed transmits a confirmed uplink, waits in both receive windows
// and retransmits with increasing backoff until ACKed or retries are spent.
func (d *Device) SendConfirmed(ctx context.Context, payload []byte) error {
	return d.tx(ctx, lorawan.MTypeConfirmedDataUp, payload, true)
}

func (d *Device) tx(ctx context.Context, mt lorawan.MType, payload []byte, needACK bool) error {
	attempts := 1
	if needACK {
		attempts += d.cfg.Retries
	}
	// A confirmed uplink reserves ONE counter value for the whole logical
	// transmission; every retransmission carries the same frame (same FCnt,
	// same MIC) so the server sees exact duplicates and ACKs them uniformly.
	var sharedRaw []byte
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, d.backoff(attempt)); err != nil {
				return err
			}
		}

		raw := sharedRaw
		if raw == nil {
			// Validate length/shape before anything is admitted or reserved:
			// an oversize or otherwise invalid send must not consume
			// duty-cycle budget, a frame counter or pending ACK/MAC state.
			if err := d.validateFrame(mt, payload); err != nil {
				return err
			}
			// Duty-cycle admission precedes counter reservation, so a limited
			// send consumes no counter.
			if _, err := d.duty.Reserve(ctx, time.Now(), d.cfg.Airtime); err != nil {
				return err
			}
			var err error
			_, raw, err = d.sealFrame(ctx, mt, payload)
			if err != nil {
				return err
			}
			if needACK {
				sharedRaw = raw
			}
		}
		if err := d.cfg.Air.Send(ctx, raw); err != nil {
			return err
		}

		in, err := d.waitWindows(ctx)
		ok := err == nil
		if !ok {
			lastErr = lorawan.ErrNoACK
			if needACK {
				d.recordADR(d.adr.Observe(0, false))
				continue
			}
			return nil
		}

		isContent := len(in.frame.Payload) > 0 || len(in.frame.FOpts) > 0
		if isContent {
			d.handleDown(ctx, in.frame)
		}
		if needACK && in.frame.ACK {
			d.recordADR(d.adr.Observe(0, true))
			return nil
		}
		if !needACK {
			return nil
		}
		lastErr = lorawan.ErrNoACK
	}
	return lastErr
}

// validateFrame checks join state and the local frame size cap without
// mutating any state. It is the no-side-effect gate for Send* calls.
func (d *Device) validateFrame(mt lorawan.MType, payload []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.session.Joined {
		return errors.New("device: not joined")
	}
	optsLen := len(d.pendingLinkADRAns)
	if optsLen > 255 || len(payload) > lorawan.MaxPayload ||
		optsLen+len(payload) > lorawan.MaxFrameBody-lorawan.MaxFrameHeader {
		return lorawan.ErrFrameTooLong
	}
	return nil
}

// sealFrame reserves and persists one uplink counter and encodes the frame.
// validateFrame must have succeeded and duty-cycle admission passed first.
func (d *Device) sealFrame(ctx context.Context, mt lorawan.MType, payload []byte) (*lorawan.Frame, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.session.Joined {
		return nil, nil, errors.New("device: not joined")
	}
	fcnt, err := d.session.ReserveUp()
	if err != nil {
		return nil, nil, err
	}
	if err := d.persistLocked(ctx); err != nil {
		return nil, nil, err
	}
	f := &lorawan.Frame{
		MType:   mt,
		DevAddr: d.session.DevAddr,
		ADR:     d.cfg.ADR.Enabled,
		ACK:     d.pendingACK,
		FCnt:    fcnt,
		HasPort: true,
		FPort:   1,
		Payload: append([]byte(nil), payload...),
		FOpts:   append([]byte(nil), d.pendingLinkADRAns...),
	}
	d.pendingACK = false
	d.pendingLinkADRAns = nil
	raw, err := lorawan.EncodeFrame(d.session, f)
	if err != nil {
		return nil, nil, err
	}
	return f, raw, nil
}

// handleDown validates the downlink counter, durably advances FCntDown,
// processes MAC commands and delivers application payload at most once per
// counter value. The advanced downlink floor is persisted BEFORE any other
// side effect: if the process restarts after having received a downlink, the
// floor never rolls back and that downlink cannot be accepted (and delivered)
// a second time.
func (d *Device) handleDown(ctx context.Context, f *lorawan.Frame) {
	var appData []byte
	d.mu.Lock()
	if err := d.session.AcceptDown(f.FCnt); err != nil {
		// Duplicate/replay: drop silently (already delivered/ACKed).
		d.mu.Unlock()
		return
	}
	if err := d.persistLocked(ctx); err != nil {
		// Durability failure: do not process or deliver this frame; the
		// in-memory floor still prevents a second acceptance in this run.
		d.mu.Unlock()
		return
	}
	d.processFOptsLocked(ctx, f.FOpts)
	if f.MType == lorawan.MTypeConfirmedDataDown {
		d.pendingACK = true
	}
	if len(f.Payload) > 0 && d.cfg.OnAppData != nil {
		appData = append([]byte(nil), f.Payload...)
	}
	d.mu.Unlock()
	if appData != nil {
		d.cfg.OnAppData(appData)
	}
}

func (d *Device) processFOptsLocked(ctx context.Context, opts []byte) {
	for len(opts) >= 5 && opts[0] == lorawan.CIDLinkADRReq {
		dr, txPower, err := lorawan.ParseLinkADRReq(opts[1:5])
		if err != nil {
			return
		}
		if err := d.adr.HandleLinkADRReq(dr, txPower); err == nil {
			d.session.DataRate, d.session.TXPower = dr, txPower
			_ = d.persistLocked(ctx)
			d.pendingLinkADRAns = append(d.pendingLinkADRAns[:0],
				lorawan.CIDLinkADRAns)
			d.pendingLinkADRAns = append(d.pendingLinkADRAns,
				lorawan.MarshalLinkADRAns(true, true, true)...)
		}
		opts = opts[5:]
	}
}

func (d *Device) backoff(attempt int) time.Duration {
	dur := d.cfg.Backoff
	for i := 1; i < attempt; i++ {
		dur *= 2
		if d.cfg.BackoffMax > 0 && dur >= d.cfg.BackoffMax {
			return d.cfg.BackoffMax
		}
	}
	return dur
}

// recordADR writes an ADR engine adaptation back into the persistent session
// so the chosen DR/power survives restart; keys and counters are untouched.
func (d *Device) recordADR(dr uint8, txPower int, changed bool) {
	if !changed {
		return
	}
	d.mu.Lock()
	d.session.DataRate, d.session.TXPower = dr, txPower
	_ = d.persistLocked(context.Background())
	d.mu.Unlock()
}
