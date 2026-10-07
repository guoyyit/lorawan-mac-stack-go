package lorawan

import (
	"errors"
	"testing"
)

func maxLegalFrame(mt MType) *Frame {
	return &Frame{
		MType:   mt,
		DevAddr: DevAddr{1, 2, 3, 4},
		FCnt:    1,
		HasPort: true,
		FPort:   1,
		Payload: make([]byte, MaxPayload), // no FOpts: fills the cap exactly
	}
}

// A frame exactly at the documented cap encodes, decodes and verifies.
func TestFrameAtExactLimitRoundTrips(t *testing.T) {
	s := testDataSession()
	f := maxLegalFrame(MTypeConfirmedDataUp)
	raw, err := EncodeFrame(s, f)
	if err != nil {
		t.Fatalf("encode at cap: %v", err)
	}
	if len(raw) != MaxFrameLength {
		t.Fatalf("len = %d, want %d", len(raw), MaxFrameLength)
	}
	got, err := DecodeFrame(s, raw)
	if err != nil {
		t.Fatalf("decode at cap: %v", err)
	}
	if len(got.Payload) != MaxPayload || got.FCnt != 1 {
		t.Fatalf("decoded payload len=%d fcnt=%d", len(got.Payload), got.FCnt)
	}

	// FOpts + payload together may also fill the cap exactly.
	f2 := maxLegalFrame(MTypeUnconfirmedDataDown)
	f2.Payload = make([]byte, MaxPayload-3)
	f2.FOpts = []byte{1, 2, 3}
	raw2, err := EncodeFrame(s, f2)
	if err != nil {
		t.Fatalf("encode combined cap: %v", err)
	}
	if len(raw2) != MaxFrameLength {
		t.Fatalf("combined len = %d, want %d", len(raw2), MaxFrameLength)
	}
	if _, err := DecodeFrame(s, raw2); err != nil {
		t.Fatalf("decode combined cap: %v", err)
	}
}

// One byte over the cap is rejected on encode, distinctly.
func TestFrameOneOverLimitRejected(t *testing.T) {
	s := testDataSession()
	f := maxLegalFrame(MTypeConfirmedDataUp)
	f.Payload = make([]byte, MaxPayload+1)
	if _, err := EncodeFrame(s, f); !errors.Is(err, ErrFrameTooLong) {
		t.Fatalf("encode 1 over = %v, want ErrFrameTooLong", err)
	}
	// FOpts pushing the combined body over the cap is rejected as well.
	f2 := maxLegalFrame(MTypeUnconfirmedDataUp)
	f2.Payload = make([]byte, MaxPayload)
	f2.FOpts = []byte{1}
	if _, err := EncodeFrame(s, f2); !errors.Is(err, ErrFrameTooLong) {
		t.Fatalf("encode combined over = %v", err)
	}
}

// A wildly oversize payload is rejected deterministically (no panic, and no
// allocation proportional to the request).
func TestFrameFarOverLimitRejected(t *testing.T) {
	s := testDataSession()
	f := maxLegalFrame(MTypeUnconfirmedDataUp)
	f.Payload = make([]byte, 1<<20)
	if _, err := EncodeFrame(s, f); !errors.Is(err, ErrFrameTooLong) {
		t.Fatalf("encode far over = %v", err)
	}
}

// Decode must distinguish oversize input from malformed/bad-MIC/replay causes
// and must never panic.
func TestDecodeOversizeClassification(t *testing.T) {
	s := testDataSession()

	// One byte over the wire cap with otherwise arbitrary content.
	over := make([]byte, MaxFrameLength+1)
	over[0] = byte(MTypeConfirmedDataUp)
	if _, err := DecodeFrame(s, over); !errors.Is(err, ErrFrameTooLong) {
		t.Fatalf("decode 1 over = %v, want ErrFrameTooLong", err)
	}

	// Far over the cap.
	huge := make([]byte, 1<<20)
	huge[0] = byte(MTypeConfirmedDataUp)
	if _, err := DecodeFrame(s, huge); !errors.Is(err, ErrFrameTooLong) {
		t.Fatalf("decode far over = %v, want ErrFrameTooLong", err)
	}

	// A frame whose 16-bit length field advertises a huge payload while the
	// actual bytes are short is structurally inconsistent -> ErrMalformed,
	// never an allocation driven by the advertised length.
	adv := make([]byte, 32)
	adv[0] = byte(MTypeConfirmedDataUp)
	adv[11], adv[12] = 0xff, 0xff // payloadLen = 65535
	if _, err := DecodeFrame(s, adv); !errors.Is(err, ErrMalformed) {
		t.Fatalf("decode lying length = %v, want ErrMalformed", err)
	}

	// Distinguishable from the other rejection causes.
	if errors.Is(ErrFrameTooLong, ErrBadMIC) ||
		errors.Is(ErrFrameTooLong, ErrReplay) ||
		errors.Is(ErrFrameTooLong, ErrCounterOverflow) {
		t.Fatal("ErrFrameTooLong must be distinct from MIC/replay/overflow")
	}
}
