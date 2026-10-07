package lorawan

import (
	"errors"
	"testing"
)

// A frame at exactly the limit must encode, decode and verify.
func TestFrameExactlyAtMaxSizeRoundTrips(t *testing.T) {
	s := testDataSession()
	f := &Frame{
		MType:   MTypeUnconfirmedDataUp,
		DevAddr: s.DevAddr,
		FCnt:    7,
		HasPort: true,
		FPort:   1,
		Payload: make([]byte, MaxPayloadSize), // 241 -> total 259
	}
	for i := range f.Payload {
		f.Payload[i] = byte(i)
	}
	raw, err := EncodeFrame(s, f)
	if err != nil {
		t.Fatalf("encode at limit: %v", err)
	}
	if len(raw) != MaxFrameSize {
		t.Fatalf("len = %d, want %d", len(raw), MaxFrameSize)
	}
	got, err := DecodeFrame(s, raw)
	if err != nil {
		t.Fatalf("decode at limit: %v", err)
	}
	if len(got.Payload) != MaxPayloadSize || got.Payload[240] != f.Payload[240] {
		t.Fatalf("payload mismatch at limit")
	}

	// Same with FOpts present: 14 + 15 + 226 + 4 = 259 exactly.
	f.FOpts = make([]byte, 15)
	f.Payload = make([]byte, MaxPayloadSize-15)
	raw, err = EncodeFrame(s, f)
	if err != nil {
		t.Fatalf("encode at limit with FOpts: %v", err)
	}
	if len(raw) != MaxFrameSize {
		t.Fatalf("len with FOpts = %d, want %d", len(raw), MaxFrameSize)
	}
	if _, err := DecodeFrame(s, raw); err != nil {
		t.Fatalf("decode at limit with FOpts: %v", err)
	}
}

// Encoding one byte over the limit, and far over the limit, is a
// deterministic ErrFrameTooLarge -- never a panic.
func TestEncodeOverMaxSizeRejected(t *testing.T) {
	s := testDataSession()
	for _, n := range []int{MaxPayloadSize + 1, 4096, 65535, 1 << 20} {
		f := &Frame{
			MType:   MTypeUnconfirmedDataUp,
			DevAddr: s.DevAddr,
			HasPort: true,
			FPort:   1,
			Payload: make([]byte, n),
		}
		if _, err := EncodeFrame(s, f); !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("payload %d: err = %v, want ErrFrameTooLarge", n, err)
		}
	}
	// FOpts pushing the total one byte over the limit.
	f := &Frame{
		MType:   MTypeUnconfirmedDataUp,
		DevAddr: s.DevAddr,
		FOpts:   make([]byte, 15),
		Payload: make([]byte, MaxPayloadSize-14),
	}
	if _, err := EncodeFrame(s, f); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("FOpts overflow: err = %v, want ErrFrameTooLarge", err)
	}
}

// Decoding anything over MaxFrameSize is a deterministic ErrFrameTooLarge,
// including a frame that declares a huge payload length and actually carries
// it (the historical panic vector), and a far-larger buffer.
func TestDecodeOverMaxSizeRejected(t *testing.T) {
	s := testDataSession()

	// Exactly MaxFrameSize+1.
	if _, err := DecodeFrame(s, make([]byte, MaxFrameSize+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("260 bytes: err = %v, want ErrFrameTooLarge", err)
	}

	// A frame that declares a 65535-byte payload and is actually that long:
	// previously this reached computeMIC and panicked.
	huge := make([]byte, 14+65535+4)
	huge[0] = byte(MTypeUnconfirmedDataUp)
	copy(huge[2:6], s.DevAddr[:])
	huge[11], huge[12] = 0xff, 0xff // payload length 65535
	if _, err := DecodeFrame(s, huge); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("declared 65535 payload: err = %v, want ErrFrameTooLarge", err)
	}

	// Far above the limit.
	if _, err := DecodeFrame(s, make([]byte, 4<<20)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("4 MiB: err = %v, want ErrFrameTooLarge", err)
	}
}

// ErrFrameTooLarge must be distinguishable from every other rejection cause.
func TestFrameTooLargeIsDistinguishable(t *testing.T) {
	s := testDataSession()
	_, decErr := DecodeFrame(s, make([]byte, MaxFrameSize+1))
	_, encErr := EncodeFrame(s, &Frame{
		MType: MTypeUnconfirmedDataUp, DevAddr: s.DevAddr,
		Payload: make([]byte, MaxPayloadSize+1),
	})
	for _, err := range []error{decErr, encErr} {
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("%v is not ErrFrameTooLarge", err)
		}
		for _, other := range []error{ErrMalformed, ErrBadMIC, ErrReplay, ErrDuplicate, ErrCounterOverflow} {
			if errors.Is(err, other) {
				t.Fatalf("%v must not match %v", err, other)
			}
		}
	}
}

func TestValidateBodySizeBoundaries(t *testing.T) {
	cases := []struct {
		fopts, payload int
		ok             bool
	}{
		{0, 0, true},
		{0, MaxPayloadSize, true},
		{0, MaxPayloadSize + 1, false},
		{15, MaxPayloadSize - 15, true},
		{15, MaxPayloadSize - 14, false},
		{255, 0, false}, // FOpts alone can overflow the signed portion
		{-1, 0, false},
		{0, -1, false},
	}
	for _, c := range cases {
		err := ValidateBodySize(c.fopts, c.payload)
		if c.ok && err != nil {
			t.Errorf("fopts=%d payload=%d: %v", c.fopts, c.payload, err)
		}
		if !c.ok && !errors.Is(err, ErrFrameTooLarge) {
			t.Errorf("fopts=%d payload=%d: err=%v, want ErrFrameTooLarge", c.fopts, c.payload, err)
		}
	}
}
