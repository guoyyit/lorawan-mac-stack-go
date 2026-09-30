package mac

import (
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/clock"
)

func TestDutyCycleBlocksThenReleases(t *testing.T) {
	fc := clock.NewFakeClock(time.Unix(1000, 0))
	dt := NewDutyTracker(fc, 0.01, func(uint8, int) time.Duration {
		return 100 * time.Millisecond
	})

	if !dt.CanSend() {
		t.Fatal("should be free initially")
	}
	dt.Record(0, 10)
	// T=0.1s at 1% => silence 0.1*99 = 9.9s after the emission ends.
	if dt.CanSend() {
		t.Fatal("should be blocked immediately after emission")
	}
	if got := dt.Wait(); got != 10*time.Second {
		t.Fatalf("expected 10s block (incl. air time), got %v", got)
	}

	fc.Advance(9*time.Second + 999*time.Millisecond)
	if dt.CanSend() {
		t.Fatal("still blocked at 9.999s")
	}
	fc.Advance(1 * time.Millisecond)
	if !dt.CanSend() {
		t.Fatal("should be released at 10s")
	}
}

func TestDutyCycleDisabled(t *testing.T) {
	fc := clock.NewFakeClock(time.Unix(0, 0))
	dt := NewDutyTracker(fc, 0, nil)
	dt.Record(0, 200)
	if !dt.CanSend() || dt.Wait() != 0 {
		t.Fatal("disabled duty tracker must never block")
	}
}

func TestDefaultTimeOnAirMonotonic(t *testing.T) {
	if DefaultTimeOnAir(0, 20) <= DefaultTimeOnAir(3, 20) {
		t.Fatal("lower data rate must have longer air time")
	}
	if DefaultTimeOnAir(2, 40) <= DefaultTimeOnAir(2, 10) {
		t.Fatal("longer payload must have longer air time")
	}
}
