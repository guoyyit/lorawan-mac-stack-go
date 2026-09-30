package mac

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

func TestDutyLimiterDropPolicy(t *testing.T) {
	// 1% duty cycle: 100 ms airtime blocks for 10 s.
	l := NewDutyLimiter(DutyConfig{MaxDutyCycle: 0.01, Policy: DutyDrop})
	t0 := time.Unix(1000, 0)
	ctx := context.Background()
	if _, err := l.Reserve(ctx, t0, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// Anything within the debt window is rejected.
	when, err := l.Reserve(ctx, t0.Add(time.Second), 100*time.Millisecond)
	if !errors.Is(err, lorawan.ErrDutyCycle) {
		t.Fatalf("within window = %v", err)
	}
	want := t0.Add(10 * time.Second)
	if !when.Equal(want) {
		t.Fatalf("blocked until %v, want %v", when, want)
	}
	// After the window elapses, sending is allowed again.
	if _, err := l.Reserve(ctx, want, 100*time.Millisecond); err != nil {
		t.Fatalf("after window = %v", err)
	}
}

func TestDutyLimiterWaitPolicy(t *testing.T) {
	l := NewDutyLimiter(DutyConfig{MaxDutyCycle: 0.5, Policy: DutyWait})
	t0 := time.Unix(2000, 0)
	// 1s airtime at 50% blocks for 2s.
	_, _ = l.Reserve(context.Background(), t0, time.Second)
	start := t0.Add(500 * time.Millisecond)
	got, err := l.Reserve(context.Background(), start, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Real time advanced by ~1.5s; returned start must equal t0+2s.
	if !got.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("waited start = %v", got)
	}
}

func TestDutyLimiterWaitMaxWait(t *testing.T) {
	l := NewDutyLimiter(DutyConfig{
		MaxDutyCycle: 0.01, Policy: DutyWait, MaxWait: time.Second,
	})
	t0 := time.Unix(3000, 0)
	_, _ = l.Reserve(context.Background(), t0, 100*time.Millisecond)
	_, err := l.Reserve(context.Background(), t0.Add(10*time.Millisecond), 100*time.Millisecond)
	if !errors.Is(err, lorawan.ErrDutyCycle) {
		t.Fatalf("maxwait = %v", err)
	}
}

func TestDutyLimiterDisabled(t *testing.T) {
	l := NewDutyLimiter(DutyConfig{MaxDutyCycle: 0, Policy: DutyDrop})
	t0 := time.Unix(4000, 0)
	for i := 0; i < 100; i++ {
		if _, err := l.Reserve(context.Background(), t0, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
}
