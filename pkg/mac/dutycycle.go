package mac

import (
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/clock"
)

// DutyTracker enforces the single-sub-band aggregated duty-cycle limit:
// after an emission of air time T with limit d, the device must stay silent
// for T*(1/d - 1). State is purely timing based and deterministic.
type DutyTracker struct {
	limit       float64
	timeOnAir   TimeOnAirFunc
	clk         clock.Clock
	availableAt time.Time
}

// NewDutyTracker builds a tracker with the given maximum duty-cycle fraction
// (e.g. 0.01 for 1%). A limit <= 0 disables enforcement.
func NewDutyTracker(clk clock.Clock, limit float64, toa TimeOnAirFunc) *DutyTracker {
	if toa == nil {
		toa = DefaultTimeOnAir
	}
	return &DutyTracker{limit: limit, timeOnAir: toa, clk: clk}
}

// CanSend reports whether a transmission may start at the clock's now.
func (t *DutyTracker) CanSend() bool {
	if t.limit <= 0 {
		return true
	}
	return !t.clk.Now().Before(t.availableAt)
}

// BlockedUntil returns the earliest time at which the next transmission may
// start (zero time when not blocked).
func (t *DutyTracker) BlockedUntil() time.Time {
	if t.CanSend() {
		return time.Time{}
	}
	return t.availableAt
}

// Wait returns how long to wait before the next transmission is allowed.
func (t *DutyTracker) Wait() time.Duration {
	if t.limit <= 0 {
		return 0
	}
	if d := t.availableAt.Sub(t.clk.Now()); d > 0 {
		return d
	}
	return 0
}

// Record marks an emission at the current time with the given air time and
// pushes the blocked horizon forward.
func (t *DutyTracker) Record(dataRate uint8, payloadLen int) {
	if t.limit <= 0 {
		return
	}
	toa := t.timeOnAir(dataRate, payloadLen)
	// LoRaWAN aggregated duty-cycle rule: after time-on-air T at limit d,
	// the device must stay silent for T*(1/d - 1).
	off := time.Duration(float64(toa) * (1.0/t.limit - 1.0))
	end := t.clk.Now().Add(toa).Add(off)
	if end.After(t.availableAt) {
		t.availableAt = end
	}
}

// Reset clears the blocked horizon (test support).
func (t *DutyTracker) Reset() { t.availableAt = time.Time{} }
