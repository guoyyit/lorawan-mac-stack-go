package mac

import (
	"testing"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
)

func TestADRPowerDownThenRateUp(t *testing.T) {
	cfg := DefaultADRConfig()
	e := NewADREngine(cfg, 0, 14)

	// three healthy reports lower TX power by 3 dB
	dr, tp, ch := e.RecordMargin(20)
	if ch || dr != 0 || tp != 14 {
		t.Fatalf("first report changed: %d %d %v", dr, tp, ch)
	}
	e.RecordMargin(20)
	dr, tp, ch = e.RecordMargin(20)
	if !ch || dr != 0 || tp != 11 {
		t.Fatalf("expected power down to 11, got dr=%d tp=%d", dr, tp)
	}

	// keep healthy until power is minimum, then data rate must increase
	var lastDR uint8
	for i := 0; i < 30; i++ {
		dr, _, _ = e.RecordMargin(20)
		dr, _, _ = e.RecordMargin(20)
		dr, _, _ = e.RecordMargin(20)
		lastDR = dr
	}
	if lastDR != cfg.MaxDataRate {
		t.Fatalf("expected max data rate %d, got %d", cfg.MaxDataRate, lastDR)
	}
}

func TestADRFallback(t *testing.T) {
	cfg := DefaultADRConfig()
	e := NewADREngine(cfg, 3, 8)
	e.RecordMargin(-5)
	dr, _, ch := e.RecordMargin(-5)
	if !ch || dr != 2 {
		t.Fatalf("expected fallback to DR2, got dr=%d changed=%v", dr, ch)
	}

	// direct link failure forces exactly one fallback step
	dr, tp, ch := e.LinkFailed()
	if !ch || dr != 1 || tp != 8 {
		t.Fatalf("expected DR1 after LinkFailed, got %d/%d", dr, tp)
	}
}

func TestADRLinkADRCommand(t *testing.T) {
	cfg := DefaultADRConfig()
	e := NewADREngine(cfg, 0, 14)

	ans := e.ApplyLinkADR(protocol.LinkADRReq{
		DataRate: 4, TXPower: 1, ChMask: 1, Redundancy: 0,
	})
	if !ans.PowerACK || !ans.DataRateACK || !ans.ChannelMaskACK {
		t.Fatalf("valid LinkADR rejected: %+v", ans)
	}
	if e.DataRate() != 4 || e.TXPower() != 11 {
		t.Fatalf("engine not updated: dr=%d tp=%d", e.DataRate(), e.TXPower())
	}

	// out-of-range data rate must be rejected without changing state
	ans = e.ApplyLinkADR(protocol.LinkADRReq{DataRate: 15, TXPower: 0, ChMask: 1})
	if ans.DataRateACK {
		t.Fatal("DR15 must be rejected")
	}
	if e.DataRate() != 4 {
		t.Fatal("rejected command mutated data rate")
	}
}

func TestADRSnapshotRestore(t *testing.T) {
	cfg := DefaultADRConfig()
	e := NewADREngine(cfg, 2, 11)
	e.LinkFailed()
	snap := e.Snapshot()

	e2 := NewADREngine(cfg, 0, 14)
	e2.Restore(snap)
	if e2.DataRate() != 1 || e2.TXPower() != 11 {
		t.Fatalf("restore failed: dr=%d tp=%d", e2.DataRate(), e2.TXPower())
	}
}
