package mac

// frameGuard centralizes the 32-bit frame-counter recovery and replay rules
// shared by both sides of the link, so device and network server agree
// exactly on which frames are accepted.
//
// Only the low 16 bits of a counter travel on the air. The receiver keeps
// the last accepted counter and recovers the full 32-bit value by choosing
// the candidate whose low 16 bits equal the wire value and which is:
//
//  1. strictly greater than the last accepted counter, and
//  2. no more than maxGap ahead of it.
//
// A value <= lastAccepted is a duplicate/replay and is rejected. Frames too
// far ahead are rejected as a desynchronization/attack signal.
//
// Overflow: when the last accepted counter is within maxGap of 2^32-1 a
// strictly-larger counter cannot exist; ValidateFCnt returns ErrFCntOverflow
// and the documented recovery action is a new OTAA join (the counters start
// at 0 for a fresh session).
type frameGuard struct {
	lastAccepted uint32
	maxGap       uint32
}

func newFrameGuard(maxGap uint32) frameGuard {
	if maxGap == 0 {
		maxGap = 65535
	}
	return frameGuard{maxGap: maxGap}
}

// ValidateFCnt recovers and validates a wire FCnt. On success it returns the
// full 32-bit counter; on failure the returned error is ErrReplay or
// ErrFCntOverflow. The guard is not mutated: callers persist/advance the
// counter only after MIC verification and dedup decisions.
func (g frameGuard) ValidateFCnt(wireLow16 uint16) (uint32, error) {
	base := g.lastAccepted &^ uint32(0xFFFF)
	next := base + uint32(wireLow16)
	if next <= g.lastAccepted {
		next += 1 << 16
	}

	if next <= g.lastAccepted {
		// Possible only if even the wrap candidate is not larger: the
		// 32-bit counter space is exhausted.
		return 0, ErrFCntOverflow
	}
	if next-g.lastAccepted > g.maxGap {
		// Either a replay with a stale high-half guess or a real
		// desynchronization; both are rejected deterministically.
		return 0, ErrReplay
	}
	return next, nil
}

// IsDuplicate reports whether wireLow16 equals the low 16 bits of the last
// accepted counter (retransmission of the immediately previous frame).
func (g frameGuard) IsDuplicate(wireLow16 uint16) bool {
	return uint16(g.lastAccepted) == wireLow16
}

// counterCandidatesWithReplay returns counters to MIC-check for a received
// wire FCnt: the retransmission/duplicate value (when the low16 matches the
// last accepted counter) plus the next strictly-increasing candidate. MIC
// verification must precede any replay decision so a forged replay cannot
// be confused with a genuine retransmission.
func counterCandidatesWithReplay(lastAccepted uint32, low16 uint16) []uint32 {
	var out []uint32
	if uint16(lastAccepted) == low16 && lastAccepted != 0 {
		out = append(out, lastAccepted)
	}
	high := uint32(lastAccepted>>16) << 16
	cand := high + uint32(low16)
	if cand <= lastAccepted {
		cand += 1 << 16
	}
	if cand > lastAccepted {
		out = append(out, cand)
	}
	return out
}
