package mac

import "github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"

// Margin step in dB between successive ADR power/rate levels.
const adrMarginStep = 3

// ADRConfig is the adaptive-data-rate policy shared by device and server.
type ADRConfig struct {
	// MinMargin is the minimum demodulation margin (dB) the device keeps
	// while the network raises its data rate / lowers its power.
	MinMargin float64
	// MinDataRate/MaxDataRate bound the selectable rate indices.
	MinDataRate uint8
	MaxDataRate uint8
	// MinTXPower/MaxTXPower bound transmit power in dBm (lower = weaker).
	MinTXPower int
	MaxTXPower int
	// IncreaseCount consecutive healthy reports are required before
	// increasing rate; DecreaseCount consecutive poor reports force a
	// fallback to a lower rate.
	IncreaseCount int
	DecreaseCount int
}

// DefaultADRConfig returns the standard policy used by demo and tests.
func DefaultADRConfig() ADRConfig {
	return ADRConfig{
		MinMargin:     10,
		MinDataRate:   0,
		MaxDataRate:   5,
		MinTXPower:    5,
		MaxTXPower:    14,
		IncreaseCount: 3,
		DecreaseCount: 2,
	}
}

func (cfg ADRConfig) normalize() ADRConfig {
	d := DefaultADRConfig()
	if cfg.MinMargin <= 0 {
		cfg.MinMargin = d.MinMargin
	}
	if cfg.MaxDataRate == 0 {
		cfg.MaxDataRate = d.MaxDataRate
	}
	if cfg.MinTXPower == 0 {
		cfg.MinTXPower = d.MinTXPower
	}
	if cfg.MaxTXPower == 0 {
		cfg.MaxTXPower = d.MaxTXPower
	}
	if cfg.IncreaseCount <= 0 {
		cfg.IncreaseCount = d.IncreaseCount
	}
	if cfg.DecreaseCount <= 0 {
		cfg.DecreaseCount = d.DecreaseCount
	}
	return cfg
}

// ADREngine tracks link-margin history and proposes rate/power changes.
// It is used device-side for margin-driven fallback and holds the current
// link settings; the server keeps a copy of the current settings per device
// for server-command-driven adaptation.
type ADREngine struct {
	cfg       ADRConfig
	dataRate  uint8
	txPower   int
	goodCount int
	badCount  int
}

// NewADREngine builds an engine starting at (dataRate, txPower).
func NewADREngine(cfg ADRConfig, dataRate uint8, txPower int) *ADREngine {
	cfg = cfg.normalize()
	if dataRate < cfg.MinDataRate {
		dataRate = cfg.MinDataRate
	}
	if dataRate > cfg.MaxDataRate {
		dataRate = cfg.MaxDataRate
	}
	if txPower < cfg.MinTXPower {
		txPower = cfg.MinTXPower
	}
	if txPower > cfg.MaxTXPower {
		txPower = cfg.MaxTXPower
	}
	return &ADREngine{cfg: cfg, dataRate: dataRate, txPower: txPower}
}

// DataRate returns the current data rate index.
func (e *ADREngine) DataRate() uint8 { return e.dataRate }

// TXPower returns the current transmit power in dBm.
func (e *ADREngine) TXPower() int { return e.txPower }

// RecordMargin feeds one demodulation-margin sample (dB). It returns the
// (possibly unchanged) rate/power and whether anything changed.
func (e *ADREngine) RecordMargin(margin float64) (dataRate uint8, txPower int, changed bool) {
	// Healthy margin: after IncreaseCount samples, first lower power
	// (better efficiency), and once power is at the minimum, raise the
	// data rate. Poor margin: after DecreaseCount samples, first lower the
	// data rate, and at the minimum rate raise power (more link budget).
	if margin >= e.cfg.MinMargin {
		e.goodCount++
		e.badCount = 0
		if e.goodCount >= e.cfg.IncreaseCount {
			e.goodCount = 0
			return e.apply(e.improve())
		}
		return e.dataRate, e.txPower, false
	}

	e.badCount++
	e.goodCount = 0
	if e.badCount >= e.cfg.DecreaseCount {
		e.badCount = 0
		return e.apply(e.degrade())
	}
	return e.dataRate, e.txPower, false
}

// LinkFailed forces one fallback step to a lower data rate (and higher
// power if available), used when downlink ACKs are repeatedly missed.
func (e *ADREngine) LinkFailed() (dataRate uint8, txPower int, changed bool) {
	e.goodCount = 0
	return e.apply(e.degrade())
}

// ApplyLinkADR applies a server LinkADRReq, validating the bounds and
// returning the answer bits the device must send back.
func (e *ADREngine) ApplyLinkADR(req protocol.LinkADRReq) protocol.LinkADRAns {
	ans := protocol.LinkADRAns{
		PowerACK:       int(req.TXPower) <= 15,
		DataRateACK:    req.DataRate >= e.cfg.MinDataRate && req.DataRate <= e.cfg.MaxDataRate,
		ChannelMaskACK: req.ChMask != 0,
	}
	// This stack models a single enabled channel mask (all channels).
	if req.ChMask != 1 && req.ChMask != 0xffff {
		ans.ChannelMaskACK = false
	}
	if !ans.PowerACK || !ans.DataRateACK || !ans.ChannelMaskACK {
		return ans
	}

	e.dataRate = req.DataRate
	if power, ok := txPowerFromIndex(req.TXPower, e.cfg); ok {
		e.txPower = power
	} else {
		e.txPower = e.cfg.MaxTXPower
	}
	e.goodCount, e.badCount = 0, 0
	return ans
}

// Snapshot is the serializable ADR state embedded in persisted sessions.
type Snapshot struct {
	DataRate  uint8
	TXPower   int
	GoodCount int
	BadCount  int
}

// Snapshot returns the current state for persistence.
func (e *ADREngine) Snapshot() Snapshot {
	return Snapshot{DataRate: e.dataRate, TXPower: e.txPower,
		GoodCount: e.goodCount, BadCount: e.badCount}
}

// Restore loads ADR state after a restart.
func (e *ADREngine) Restore(s Snapshot) {
	e.dataRate = s.DataRate
	e.txPower = s.TXPower
	e.goodCount = s.GoodCount
	e.badCount = s.BadCount
}

func (e *ADREngine) apply(dr uint8, tp int, ok bool) (uint8, int, bool) {
	if !ok {
		return e.dataRate, e.txPower, false
	}
	e.dataRate, e.txPower = dr, tp
	return dr, tp, true
}

// improve returns a (dataRate, txPower, possible) step that uses the link
// more efficiently: lower TX power first, then higher data rate.
func (e *ADREngine) improve() (uint8, int, bool) {
	if e.txPower-adrMarginStep >= e.cfg.MinTXPower {
		return e.dataRate, e.txPower - adrMarginStep, true
	}
	if e.dataRate < e.cfg.MaxDataRate {
		return e.dataRate + 1, e.txPower, true
	}
	return e.dataRate, e.txPower, false
}

// degrade returns a fallback step that buys link budget: lower data rate
// first, then raise TX power.
func (e *ADREngine) degrade() (uint8, int, bool) {
	if e.dataRate > e.cfg.MinDataRate {
		return e.dataRate - 1, e.txPower, true
	}
	if e.txPower+adrMarginStep <= e.cfg.MaxTXPower {
		return e.dataRate, e.txPower + adrMarginStep, true
	}
	return e.dataRate, e.txPower, false
}

// txPowerFromIndex maps the LoRaWAN TXPower index field to an absolute dBm
// value under this stack's simple linear 3 dB grid. Index 0 means maximum
// power; unknown indices report false.
func txPowerFromIndex(idx uint8, cfg ADRConfig) (int, bool) {
	p := cfg.MaxTXPower - int(idx)*adrMarginStep
	if p < cfg.MinTXPower || p > cfg.MaxTXPower {
		return 0, false
	}
	return p, true
}

// txPowerToIndex is the inverse of txPowerFromIndex.
func txPowerToIndex(txPower int, cfg ADRConfig) uint8 {
	if txPower >= cfg.MaxTXPower {
		return 0
	}
	return uint8((cfg.MaxTXPower - txPower + adrMarginStep - 1) / adrMarginStep)
}
