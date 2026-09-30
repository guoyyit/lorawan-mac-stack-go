package lorawan

import (
	"errors"
	"testing"
)

func newTestSession() *Session {
	return NewSession(EUI{1}, EUI{2}, AESKey{3})
}

func TestReserveIsMonotonic(t *testing.T) {
	s := newTestSession()
	for want := uint32(0); want < 5; want++ {
		got, err := s.ReserveUp()
		if err != nil || got != want {
			t.Fatalf("ReserveUp = %d, %v", got, err)
		}
	}
	if s.FCntUp != 5 {
		t.Fatalf("FCntUp = %d", s.FCntUp)
	}
}

func TestAcceptUpDuplicateAndReplay(t *testing.T) {
	s := newTestSession()
	if err := s.AcceptUp(0); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptUp(0); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("repeat last = %v, want ErrDuplicate", err)
	}
	if err := s.AcceptUp(1); err != nil {
		t.Fatal(err)
	}
	// Old frame after progress -> replay.
	if err := s.AcceptUp(0); !errors.Is(err, ErrReplay) {
		t.Fatalf("old frame = %v, want ErrReplay", err)
	}
	// Out of order / gap -> rejected.
	if err := s.AcceptUp(5); !errors.Is(err, ErrReplay) {
		t.Fatalf("gap frame = %v, want ErrReplay", err)
	}
	if s.FCntUp != 2 {
		t.Fatalf("floor advanced wrongly: %d", s.FCntUp)
	}
}

func TestCounterOverflow(t *testing.T) {
	s := newTestSession()
	s.FCntDown = maxUint32
	if _, err := s.ReserveDown(); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("ReserveDown overflow = %v", err)
	}
	// Receiving the last legal frame is fine; afterwards reserves still fail
	// until rejoin resets the counter.
	s.FCntUp = maxUint32
	if err := s.AcceptUp(maxUint32); err != nil {
		t.Fatalf("accept last frame = %v", err)
	}
	if s.FCntUp != 0 {
		t.Fatalf("floor after last frame = %d", s.FCntUp)
	}
	if _, err := s.ReserveUp(); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("reserve after exhaustion = %v", err)
	}
}

func TestCloneIsIndependent(t *testing.T) {
	s := newTestSession()
	s.FCntUp = 9
	cp := s.Clone()
	cp.FCntUp = 100
	if s.FCntUp != 9 {
		t.Fatal("clone aliased session state")
	}
}
