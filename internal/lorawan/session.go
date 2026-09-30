package lorawan

import "time"

// Counter semantics (documented in README, duplicated here for callers):
//
// FCntUp/FCntDown hold the NEXT counter value to use; they start at 0.
//
// A sender RESERVES a counter immediately before transmitting: the value is
// persisted first, then the frame goes on air. A crash between persist and
// transmit therefore wastes one counter value but never re-uses one.
//
// A receiver ACCEPTS fcnt only when fcnt == next expected. The immediately
// previous value is reported as ErrDuplicate (a retransmission of the last
// frame); every other value -- older (replay) or newer/out-of-order -- is
// rejected with ErrReplay. This is stricter than the LoRaWAN 16-bit gap
// tolerance, which is possible because the local wire format carries the
// full 32-bit counter.
//
// When the next value is 0xFFFFFFFF the reserve fails with
// ErrCounterOverflow; both sides must perform a fresh join (OTAA) to reset
// counters. The value 0xFFFFFFFF is never sent.

// Session is the persistent OTAA session state shared in shape by both
// device and server. FCntUp/FCntDown always hold the NEXT frame counter
// value to use (0 before the first frame).
type Session struct {
	DevE    EUI
	JoinEUI EUI
	AppKey  AESKey
	NwkSKey AESKey
	AppSKey AESKey
	DevAddr DevAddr
	NetID   NetID

	JoinNonce JoinNonce
	Joined    bool

	// Next frame counters (reservation semantics, see README).
	FCntUp   uint32
	FCntDown uint32
	// Exhausted flags latch once the last legal counter is used. They force a
	// rejoin before any further frame and are persisted across restarts.
	UpExhausted   bool
	DownExhausted bool

	// LastDevNonce is the highest DevNonce accepted/used (0 before join).
	LastDevNonce uint16
	// Server-side counter for deterministic JoinNonce issuance.
	JoinNonceCounter uint32

	RX1DROffset uint8
	RX2DR       uint8
	RXDelay     uint8

	DataRate uint8
	TXPower  int

	UpdatedAt time.Time
}

// NewSession returns a pre-join session holding only provisioning material.
func NewSession(devEUI, joinEUI EUI, appKey AESKey) *Session {
	return &Session{
		DevE:    devEUI,
		JoinEUI: joinEUI,
		AppKey:  appKey,
		TXPower: 0,
	}
}

// ReserveUp reserves and returns the next uplink counter.
func (s *Session) ReserveUp() (uint32, error) {
	if s.UpExhausted || s.FCntUp == maxUint32 {
		s.UpExhausted = true
		return 0, ErrCounterOverflow
	}
	v := s.FCntUp
	s.FCntUp++
	return v, nil
}

// AcceptUp validates an incoming uplink counter and advances the floor.
func (s *Session) AcceptUp(fcnt uint32) error {
	return acceptCounter(&s.FCntUp, fcnt, &s.UpExhausted)
}

// ReserveDown reserves and returns the next downlink counter.
func (s *Session) ReserveDown() (uint32, error) {
	if s.DownExhausted || s.FCntDown == maxUint32 {
		s.DownExhausted = true
		return 0, ErrCounterOverflow
	}
	v := s.FCntDown
	s.FCntDown++
	return v, nil
}

// AcceptDown validates an incoming downlink counter and advances the floor.
func (s *Session) AcceptDown(fcnt uint32) error {
	return acceptCounter(&s.FCntDown, fcnt, &s.DownExhausted)
}

// Clone returns a deep copy of the session.
func (s *Session) Clone() *Session {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

const maxUint32 = ^uint32(0)

func acceptCounter(next *uint32, fcnt uint32, exhausted *bool) error {
	switch {
	case fcnt == *next:
		if fcnt == maxUint32 {
			*exhausted = true
			*next = 0
		} else {
			*next = fcnt + 1
		}
		return nil
	case *next > 0 && fcnt == *next-1:
		return ErrDuplicate
	default:
		return ErrReplay
	}
}
