package mac

import "errors"

// ADRConfig bounds adaptive data-rate and power adaptation.
type ADRConfig struct {
	Enabled       bool
	MinDR         uint8
	MaxDR         uint8
	MinTXPower    int // strongest allowed (small index)
	MaxTXPower    int // weakest allowed
	UpMargin      int // dB surplus that justifies a rate/power increase
	DownMargin    int // dB deficit at which to step down immediately
	BackoffMisses int // consecutive missed acks before stepping down
}

// ErrADRRejected is returned when a LinkADRReq asks for out-of-bounds values.
var ErrADRRejected = errors.New("mac: link-adr request outside configured bounds")

// ADREngine tracks link adaptation state for one device. It never touches
// session keys or frame counters, so ADR changes cannot lose join state.
type ADREngine struct {
	cfg      ADRConfig
	dr       uint8
	txPower  int
	misses   int
	increase int // consecutive observations with margin >= UpMargin
}

// NewADREngine constructs an engine starting at dr/txPower, clamped to bounds.
func NewADREngine(cfg ADRConfig, dr uint8, txPower int) *ADREngine {
	e := &ADREngine{cfg: cfg}
	e.dr = clampDR(dr, cfg)
	e.txPower = clampPower(txPower, cfg)
	return e
}

// HandleLinkADRReq applies a server LinkADRReq within configured bounds.
// Out-of-range requests fail and leave state untouched.
func (e *ADREngine) HandleLinkADRReq(dr uint8, txPower int) error {
	if dr < e.cfg.MinDR || dr > e.cfg.MaxDR ||
		txPower < e.cfg.MinTXPower || txPower > e.cfg.MaxTXPower {
		return ErrADRRejected
	}
	e.dr, e.txPower = dr, txPower
	e.misses, e.increase = 0, 0
	return nil
}

// Observe feeds one uplink result (link margin in dB and whether it was
// acknowledged) and returns the resulting dr/power plus whether either
// changed.
//
// Policy:
//   - unacked outcomes accumulate; after BackoffMisses consecutive misses
//     the device steps down one DR (robustness fallback) and resets.
//   - a deficit margin (<= DownMargin) steps down immediately.
//   - surplus margins (>= UpMargin) accumulate; two in a row first reduce TX
//     power, then raise DR, never exceeding the configured bounds.
func (e *ADREngine) Observe(margin int, acked bool) (uint8, int, bool) {
	beforeDR, beforePower := e.dr, e.txPower
	if !e.cfg.Enabled {
		return beforeDR, beforePower, false
	}

	if e.cfg.DownMargin != 0 && margin <= e.cfg.DownMargin {
		e.stepDown()
		e.misses, e.increase = 0, 0
		return e.dr, e.txPower, e.dr != beforeDR || e.txPower != beforePower
	}

	if !acked {
		e.misses++
		e.increase = 0
		if e.cfg.BackoffMisses > 0 && e.misses >= e.cfg.BackoffMisses {
			e.stepDown()
			e.misses = 0
		}
		return e.dr, e.txPower, e.dr != beforeDR || e.txPower != beforePower
	}

	// Acknowledged and healthy.
	e.misses = 0
	if e.cfg.UpMargin != 0 && margin >= e.cfg.UpMargin {
		e.increase++
		if e.increase >= 2 {
			e.increase = 0
			e.stepUp()
		}
	} else {
		e.increase = 0
	}
	return e.dr, e.txPower, e.dr != beforeDR || e.txPower != beforePower
}

func (e *ADREngine) stepDown() {
	// Prefer lower DR (robustness); raise power first if margin allowed.
	if e.dr > e.cfg.MinDR {
		e.dr--
		return
	}
	if e.txPower > e.cfg.MinTXPower {
		e.txPower--
	}
}

func (e *ADREngine) stepUp() {
	if e.txPower < e.cfg.MaxTXPower {
		e.txPower++
		return
	}
	if e.dr < e.cfg.MaxDR {
		e.dr++
	}
}

// State returns the current dr and tx power.
func (e *ADREngine) State() (dr uint8, txPower int) { return e.dr, e.txPower }

// SetEnabled toggles ADR without resetting counters/session.
func (e *ADREngine) SetEnabled(on bool) { e.cfg.Enabled = on }

func clampDR(dr uint8, cfg ADRConfig) uint8 {
	if dr < cfg.MinDR {
		return cfg.MinDR
	}
	if dr > cfg.MaxDR {
		return cfg.MaxDR
	}
	return dr
}

func clampPower(p int, cfg ADRConfig) int {
	if p < cfg.MinTXPower {
		return cfg.MinTXPower
	}
	if p > cfg.MaxTXPower {
		return cfg.MaxTXPower
	}
	return p
}
