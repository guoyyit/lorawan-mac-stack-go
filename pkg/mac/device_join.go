package mac

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
)

// Join performs an OTAA join: it sends a Join-Request with a fresh DevNonce,
// waits for the Join-Accept in the join receive windows and derives session
// keys. The result is persisted before Join returns.
//
// Determinism notes:
//   - The DevNonce is a strictly monotonic counter persisted BEFORE the
//     request is transmitted, so a crash can never reuse a nonce.
//   - Initial uplink and downlink frame counters are both 0 and are
//     persisted together with the derived keys.
func (d *EndDevice) Join(ctx context.Context) error {
	d.mu.Lock()
	if d.session.Joined {
		d.mu.Unlock()
		return nil
	}
	nonce := d.nextDevNonceLocked()
	d.joinCh = make(chan joinResult, 1)
	d.joinNonce = nonce

	req := protocol.JoinRequest{JoinEUI: d.cfg.JoinEUI, DevEUI: d.cfg.DevEUI, DevNonce: nonce}
	raw, err := protocol.MarshalJoinRequest(d.cfg.AppKey, req)
	if err != nil {
		d.mu.Unlock()
		return err
	}

	// Record the pending nonce before transmission.
	d.session.Joined = false
	d.session.DevEUI = d.cfg.DevEUI
	d.session.JoinEUI = d.cfg.JoinEUI
	d.session.DevNonce = nonce
	if err := d.persistLocked(); err != nil {
		d.mu.Unlock()
		return err
	}
	joinCh := d.joinCh
	timeout := d.cfg.JoinTimeout
	d.mu.Unlock()

	if err := d.waitForAir(ctx); err != nil {
		return err
	}

	d.mu.Lock()
	now := d.cfg.Clock.Now()
	d.setWindowsLocked(now)
	d.duty.Record(d.adr.DataRate(), len(raw))
	frame := radio.Frame{Payload: raw, DataRate: d.adr.DataRate(), TXPower: d.adr.TXPower(), SentAt: now}
	d.cfg.Space.Transmit(d.cfg.Endpoint, d.cfg.Server, frame)
	d.mu.Unlock()

	select {
	case res := <-joinCh:
		d.clearJoinChan()
		return res.err
	case <-ctx.Done():
		d.clearJoinChan()
		return ctx.Err()
	case <-d.cfg.Clock.After(timeout):
		d.clearJoinChan()
		return ErrJoinTimeout
	}
}

func (d *EndDevice) clearJoinChan() {
	d.mu.Lock()
	d.joinCh = nil
	d.mu.Unlock()
}

// nextDevNonceLocked returns a fresh strictly-monotonic DevNonce. After a
// join the accepted nonce lives in the session; before the first join a
// zero-value session still provides the monotonic counter.
func (d *EndDevice) nextDevNonceLocked() protocol.DevNonce {
	if d.cfg.DevNonce != nil {
		return d.cfg.DevNonce()
	}
	counter := binary.LittleEndian.Uint16(d.session.DevNonce[:]) + 1
	if counter == 0 {
		counter = 1
	}
	binary.LittleEndian.PutUint16(d.session.DevNonce[:], counter)
	return d.session.DevNonce
}

// setWindowsLocked computes RX1/RX2 open/close times relative to the end of
// an uplink. The same offsets are used for the join accept windows.
func (d *EndDevice) setWindowsLocked(upEnd time.Time) {
	d.upSentAt = upEnd
	d.rx1Open = upEnd.Add(d.cfg.RX1Delay)
	d.rx1Close = d.rx1Open.Add(d.cfg.RX1Window)
	d.rx2Open = upEnd.Add(d.cfg.RX2Delay)
	d.rx2Close = d.rx2Open.Add(d.cfg.RX2Window)
}

func (d *EndDevice) inReceiveWindow(at time.Time) bool {
	return (!at.Before(d.rx1Open) && at.Before(d.rx1Close)) ||
		(!at.Before(d.rx2Open) && at.Before(d.rx2Close))
}

// waitForAir blocks until the duty-cycle limit allows a new transmission or
// ctx is cancelled. It does not itself enqueue (Send's FIFO serializes
// callers); it simply parks on the known release time.
func (d *EndDevice) waitForAir(ctx context.Context) error {
	for {
		d.mu.Lock()
		wait := d.duty.Wait()
		d.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		select {
		case <-d.cfg.Clock.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (d *EndDevice) handleJoinAccept(f radio.Frame) {
	d.mu.Lock()
	ch := d.joinCh
	wantNonce := d.joinNonce
	if !d.inReceiveWindow(f.ReceivedAt) {
		d.mu.Unlock()
		d.log("warn", "join accept arrived outside join receive windows; dropped")
		if ch != nil {
			ch <- joinResult{err: ErrWindowMissed}
		}
		return
	}
	if f.Corrupt {
		d.mu.Unlock()
		d.log("warn", "corrupt join accept dropped")
		if ch != nil {
			ch <- joinResult{err: ErrMIC}
		}
		return
	}
	acc, err := protocol.ParseJoinAccept(d.cfg.AppKey, f.Payload)
	d.mu.Unlock()
	if err != nil {
		d.log("warn", "join accept MIC/parse failure; dropped")
		if ch != nil {
			ch <- joinResult{err: err}
		}
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	keys := deriveSessionKeys(d.cfg.AppKey, acc.AppNonce, acc.NetID, wantNonce)
	d.session.Joined = true
	d.session.DevAddr = acc.DevAddr
	d.session.Keys = keys
	d.session.UsedDevNonces = append(d.session.UsedDevNonces, wantNonce)
	d.session.NwkFCntUp = 0
	d.session.NwkFCntDown = 0
	d.session.LastUpAcked = 0
	d.session.ADR = d.adr.Snapshot()
	d.downGuard = newFrameGuard(65535)
	if err := d.persistLocked(); err != nil && ch != nil {
		ch <- joinResult{err: err}
		return
	}
	d.log("info", "OTAA join complete; session keys derived and persisted")
	if ch != nil {
		ch <- joinResult{err: nil}
	}
}
