package mac

import (
	"errors"
	"testing"
)

func testADR() *ADREngine {
	return NewADREngine(ADRConfig{
		Enabled:       true,
		MinDR:         0,
		MaxDR:         5,
		MinTXPower:    0,
		MaxTXPower:    3,
		UpMargin:      10,
		DownMargin:    -15,
		BackoffMisses: 3,
	}, 3, 1)
}

func TestLinkADRReqBounded(t *testing.T) {
	e := testADR()
	if err := e.HandleLinkADRReq(5, 3); err != nil {
		t.Fatal(err)
	}
	if dr, tx := e.State(); dr != 5 || tx != 3 {
		t.Fatalf("state = %d %d", dr, tx)
	}
	if err := e.HandleLinkADRReq(6, 0); !errors.Is(err, ErrADRRejected) {
		t.Fatalf("dr out of bounds = %v", err)
	}
	if dr, _ := e.State(); dr != 5 {
		t.Fatal("rejected request mutated state")
	}
}

func TestADRFallbackOnMisses(t *testing.T) {
	e := testADR()
	for i := 0; i < 2; i++ {
		if _, _, changed := e.Observe(0, false); changed {
			t.Fatal("changed before backoff threshold")
		}
	}
	dr, _, changed := e.Observe(0, false)
	if !changed || dr != 2 {
		t.Fatalf("after misses: dr=%d changed=%v", dr, changed)
	}
}

func TestADRFallbackOnBadMargin(t *testing.T) {
	e := testADR()
	dr, _, changed := e.Observe(-20, true)
	if !changed || dr != 2 {
		t.Fatalf("bad margin: dr=%d changed=%v", dr, changed)
	}
}

func TestADRIncreaseNeedsTwoGoodSamples(t *testing.T) {
	e := testADR()
	if _, _, changed := e.Observe(15, true); changed {
		t.Fatal("single good sample must not change rate")
	}
	dr, tx, changed := e.Observe(15, true)
	if !changed || dr != 3 || tx != 2 {
		t.Fatalf("second good sample: dr=%d tx=%d changed=%v", dr, tx, changed)
	}
}

func TestADRDisabled(t *testing.T) {
	e := testADR()
	e.SetEnabled(false)
	for i := 0; i < 10; i++ {
		if _, _, changed := e.Observe(-30, false); changed {
			t.Fatal("ADR changed while disabled")
		}
	}
}
