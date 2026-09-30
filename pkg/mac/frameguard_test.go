package mac

import "testing"

func TestFrameGuardSequential(t *testing.T) {
	g := newFrameGuard(65535)
	for _, want := range []uint32{1, 2, 3, 1000, 65535} {
		got, err := g.ValidateFCnt(uint16(want))
		if err != nil || got != want {
			t.Fatalf("fcnt %d: got %d err %v", want, got, err)
		}
		g.lastAccepted = got
	}
}

func TestFrameGuard16BitRollover(t *testing.T) {
	g := newFrameGuard(65535)
	g.lastAccepted = 65535
	got, err := g.ValidateFCnt(0)
	if err != nil || got != 65536 {
		t.Fatalf("rollover: got %d err %v", got, err)
	}
	g.lastAccepted = got
	got, err = g.ValidateFCnt(1)
	if err != nil || got != 65537 {
		t.Fatalf("post-rollover: got %d err %v", got, err)
	}
}

func TestFrameGuardRejectsReplayAndGap(t *testing.T) {
	g := newFrameGuard(100)
	g.lastAccepted = 1000

	// older counter: replay
	if _, err := g.ValidateFCnt(999); err != ErrReplay {
		t.Fatalf("replay err = %v", err)
	}
	// exact same low16 as last: duplicate
	if _, err := g.ValidateFCnt(uint16(1000)); err != ErrReplay {
		t.Fatalf("duplicate err = %v", err)
	}
	// too far ahead
	if _, err := g.ValidateFCnt(uint16(1000 + 101)); err != ErrReplay {
		t.Fatalf("gap err = %v", err)
	}
	// within gap accepted
	got, err := g.ValidateFCnt(uint16(1000 + 100))
	if err != nil || got != 1100 {
		t.Fatalf("within-gap: got %d err %v", got, err)
	}
}

func TestFrameGuardOverflow(t *testing.T) {
	g := newFrameGuard(10)
	g.lastAccepted = 0xFFFFFFFF
	if _, err := g.ValidateFCnt(0xFFFF); err != ErrFCntOverflow {
		t.Fatalf("overflow err = %v", err)
	}
}
