// Package mac contains duty-cycle and adaptive-data-rate primitives.
package mac

import (
	"context"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

// DutyPolicy selects behavior while the duty cycle limits transmission.
type DutyPolicy int

const (
	// DutyWait blocks the sender until transmission is allowed.
	DutyWait DutyPolicy = iota
	// DutyDrop rejects the request immediately.
	DutyDrop
)

// DutyConfig configures the duty-cycle limiter.
type DutyConfig struct {
	// MaxDutyCycle is the maximum time-fraction on air, e.g. 0.01 == 1%.
	// Values <= 0 or >= 1 disable limiting.
	MaxDutyCycle float64
	Policy       DutyPolicy
	MaxWait      time.Duration
}

// DutyLimiter enforces aggregated time-on-air limits for one device.
//
// Deterministic model: a frame of airtime A sent at t creates a debt window
// [t, t + A/D) in which the shared air is considered occupied (D is the
// max duty fraction). Frames are accepted only when no prior window blocks
// the requested instant. Expired records are pruned on every call.
type DutyLimiter struct {
	cfg DutyConfig
	mu  chan struct{}
	log []airRecord
}

type airRecord struct {
	debtEnd time.Time
}

// NewDutyLimiter constructs a stateful limiter.
func NewDutyLimiter(cfg DutyConfig) *DutyLimiter {
	l := &DutyLimiter{cfg: cfg, mu: make(chan struct{}, 1)}
	l.mu <- struct{}{}
	return l
}

// Config returns the limiter configuration.
func (l *DutyLimiter) Config() DutyConfig { return l.cfg }

// Reserve requests to send a frame of the given airtime at now. It records
// the transmission and returns the actual start time. With DutyDrop, a
// request made during a blocked window returns ErrDutyCycle immediately;
// with DutyWait it blocks until the window opens (or MaxWait/ctx fires).
func (l *DutyLimiter) Reserve(ctx context.Context, now time.Time, airtime time.Duration) (time.Time, error) {
	if l.cfg.MaxDutyCycle <= 0 || l.cfg.MaxDutyCycle >= 1 || airtime <= 0 {
		return now, nil
	}

	select {
	case <-l.mu:
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	}
	defer func() { l.mu <- struct{}{} }()

	blockedUntil := l.prune(now)
	if blockedUntil.After(now) {
		switch l.cfg.Policy {
		case DutyDrop:
			return blockedUntil, lorawan.ErrDutyCycle
		case DutyWait:
			wait := blockedUntil.Sub(now)
			if l.cfg.MaxWait > 0 && wait > l.cfg.MaxWait {
				return blockedUntil, lorawan.ErrDutyCycle
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return blockedUntil, ctx.Err()
			case <-t.C:
			}
			now = blockedUntil
		}
	}

	l.log = append(l.log, airRecord{
		debtEnd: now.Add(time.Duration(float64(airtime) / l.cfg.MaxDutyCycle)),
	})
	return now, nil
}

// prune drops expired debt windows and returns the end of the latest live
// window (zero time when the channel is immediately free).
func (l *DutyLimiter) prune(now time.Time) time.Time {
	kept := l.log[:0]
	var blockedUntil time.Time
	for _, r := range l.log {
		if !r.debtEnd.After(now) {
			continue
		}
		kept = append(kept, r)
		if r.debtEnd.After(blockedUntil) {
			blockedUntil = r.debtEnd
		}
	}
	l.log = kept
	return blockedUntil
}
