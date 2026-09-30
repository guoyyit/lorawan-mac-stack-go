package mac

import (
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
)

// sendLoop serializes transmissions: one FIFO request at a time goes through
// a full Class-A cycle (uplink -> RX1/RX2 -> possible backoff/retry). This
// is the only goroutine mutating uplink transmission state, so concurrent
// Send callers can never interleave two uplinks.
func (d *EndDevice) sendLoop() {
	for req := range d.sendCh {
		err := d.runSend(req)
		req.result <- err
		d.decQueue()
	}
}

func (d *EndDevice) runSend(req sendRequest) error {
	d.mu.Lock()
	if !d.session.Joined {
		d.mu.Unlock()
		return ErrNotJoined
	}
	d.mu.Unlock()

	// Duty-cycle wait with fixed retry schedule for confirmed uplinks.
	var lastErr error
	attempts := 1
	if req.confirmed {
		attempts = d.cfg.MaxRetries + 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if err := d.waitForAir(req.ctx); err != nil {
			return err
		}
		delivered, err := d.transmitOnce(req, attempt > 0)
		if err != nil {
			return err
		}
		if !req.confirmed {
			return nil
		}
		if delivered {
			return nil
		}
		lastErr = ErrNoACK
		if attempt+1 < attempts {
			// ADR fallback: persistent ACK loss means the link margin is
			// overstated; reduce the data rate before the next attempt.
			d.mu.Lock()
			if _, _, changed := d.adr.LinkFailed(); changed {
				d.session.ADR = d.adr.Snapshot()
				_ = d.persistLocked()
			}
			bo := d.backoff(attempt)
			d.mu.Unlock()
			d.log("warn", "no ACK in RX1/RX2; backing off before retry")
			select {
			case <-d.cfg.Clock.After(bo):
			case <-req.ctx.Done():
				return req.ctx.Err()
			}
		}
	}
	return lastErr
}

// backoff returns the exponential backoff before retry number attempt.
func (d *EndDevice) backoff(attempt int) time.Duration {
	bo := d.cfg.BaseBackoff << attempt
	if bo > d.cfg.MaxBackoff || bo <= 0 {
		bo = d.cfg.MaxBackoff
	}
	return bo
}

// transmitOnce sends one uplink and, for confirmed frames, waits for an ACK
// in RX1/RX2. Retransmissions reuse the same FCnt (LoRaWAN rule) and carry
// no fresh application payload counter increment.
func (d *EndDevice) transmitOnce(req sendRequest, isRetry bool) (bool, error) {
	d.mu.Lock()
	now := d.cfg.Clock.Now()
	d.setWindowsLocked(now)

	if !isRetry {
		if d.session.NwkFCntUp == ^uint32(0) {
			d.mu.Unlock()
			return false, ErrFCntOverflow
		}
		d.session.NwkFCntUp++
	}
	fcnt := d.session.NwkFCntUp
	if req.confirmed {
		d.ackCh = make(chan struct{}, 1)
		d.pendingFC = fcnt
	}
	if err := d.persistLocked(); err != nil {
		d.mu.Unlock()
		return false, err
	}

	mtype := protocol.MTUnconfirmedDataUp
	if req.confirmed {
		mtype = protocol.MTConfirmedDataUp
	}
	frame := protocol.DataFrame{
		MType:   mtype,
		DevAddr: d.session.DevAddr,
		FCtrl: protocol.FCtrl{
			ADR: true,
			ACK: d.ackDownFC != 0,
		},
		FCnt:       uint16(fcnt),
		FPort:      int(req.port),
		FRMPayload: req.payload,
	}
	// ACK bit acknowledges the immediately previous confirmed downlink;
	// once attached to an uplink it is consumed.
	d.ackDownFC = 0
	raw, err := protocol.MarshalDataFrame(d.session.Keys, frame, fcnt)
	if err != nil {
		d.mu.Unlock()
		return false, err
	}
	d.duty.Record(d.adr.DataRate(), len(raw))
	ackCh := d.ackCh
	wait := d.rx2Close.Sub(now)
	d.cfg.Space.Transmit(d.cfg.Endpoint, d.cfg.Server, radio.Frame{
		Payload: raw, DataRate: d.adr.DataRate(), TXPower: d.adr.TXPower(), SentAt: now,
	})
	d.mu.Unlock()

	if !req.confirmed {
		return true, nil
	}
	select {
	case <-ackCh:
		return true, nil
	case <-d.cfg.Clock.After(wait):
		return false, nil
	case <-req.ctx.Done():
		return false, req.ctx.Err()
	}
}

// persistLocked writes the device session atomically (caller holds mu).
func (d *EndDevice) persistLocked() error {
	d.session.ADR = d.adr.Snapshot()
	return SaveDeviceSession(d.cfg.Store, deviceSessionKey(d.cfg.DevEUI), &d.session)
}
