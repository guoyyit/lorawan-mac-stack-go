package lorawan

import (
	"errors"
	"testing"
)

func testDataSession() *Session {
	s := &Session{}
	s.NwkSKey = AESKey{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f}
	s.AppSKey = AESKey{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f}
	s.DevAddr = DevAddr{0x01, 0x02, 0x03, 0x04}
	return s
}

func TestJoinRequestRoundTrip(t *testing.T) {
	appKey := mustKey(t, "000102030405060708090a0b0c0d0e0f")
	jr := &JoinRequest{JoinEUI: EUI{1, 2}, DevEUI: EUI{3, 4}, DevNonce: 0x1234}
	raw, err := EncodeJoinRequest(appKey, jr)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 23 {
		t.Fatalf("len = %d", len(raw))
	}
	got, err := DecodeJoinRequest(appKey, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.DevNonce != 0x1234 || got.DevEUI != jr.DevEUI {
		t.Fatalf("decoded = %+v", got)
	}
}

func TestJoinRequestBadMIC(t *testing.T) {
	appKey := mustKey(t, "000102030405060708090a0b0c0d0e0f")
	raw, _ := EncodeJoinRequest(appKey, &JoinRequest{DevNonce: 1})
	raw[19] ^= 0xff
	if _, err := DecodeJoinRequest(appKey, raw); !errors.Is(err, ErrBadMIC) {
		t.Fatalf("tampered MIC = %v", err)
	}
}

func TestJoinAcceptRoundTrip(t *testing.T) {
	appKey := mustKey(t, "000102030405060708090a0b0c0d0e0f")
	ja := &JoinAccept{
		JoinNonce:   JoinNonce{9, 8, 7},
		NetID:       NetID{1, 0, 0},
		DevAddr:     DevAddr{0xaa, 0xbb, 0xcc, 0xdd},
		RX1DROffset: 1,
		RX2DR:       3,
		RXDelay:     5,
	}
	raw, err := EncodeJoinAccept(appKey, ja)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJoinAccept(appKey, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.DevAddr != ja.DevAddr || got.RX2DR != 3 || got.RXDelay != 5 || got.RX1DROffset != 1 {
		t.Fatalf("decoded = %+v", got)
	}
}

func TestFrameRoundTripAndTamper(t *testing.T) {
	s := testDataSession()
	f := &Frame{
		MType:   MTypeConfirmedDataUp,
		DevAddr: s.DevAddr,
		FCnt:    42,
		HasPort: true,
		FPort:   1,
		ADR:     true,
		Payload: []byte("temperature=21.5"),
		FOpts:   []byte{CIDLinkADRAns, 0x07},
	}
	raw, err := EncodeFrame(s, f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFrame(s, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.FCnt != 42 || string(got.Payload) != "temperature=21.5" || !got.ADR || len(got.FOpts) != 2 {
		t.Fatalf("decoded = %+v payload=%q", got, got.Payload)
	}

	// Flip a payload byte: MIC must fail before decryption is trusted.
	bad := append([]byte(nil), raw...)
	bad[len(bad)-6] ^= 0xff
	if _, err := DecodeFrame(s, bad); !errors.Is(err, ErrBadMIC) {
		t.Fatalf("tampered frame = %v", err)
	}

	// Wrong session key -> bad MIC.
	s2 := s.Clone()
	s2.NwkSKey = AESKey{}
	if _, err := DecodeFrame(s2, raw); !errors.Is(err, ErrBadMIC) {
		t.Fatalf("foreign session = %v", err)
	}
}

func TestDecodeMalformedNoPanic(t *testing.T) {
	s := testDataSession()
	for _, raw := range [][]byte{nil, {}, {0x80}, make([]byte, 17), make([]byte, 300)} {
		if _, err := DecodeFrame(s, raw); !errors.Is(err, ErrMalformed) &&
			!errors.Is(err, ErrBadMIC) && !errors.Is(err, ErrFrameTooLarge) {
			t.Errorf("raw=%x err=%v", raw, err)
		}
	}
}

func TestLinkADRCodec(t *testing.T) {
	raw := MarshalLinkADRReq(5, 2)
	dr, tx, err := ParseLinkADRReq(raw)
	if err != nil || dr != 5 || tx != 2 {
		t.Fatalf("req = %d %d %v", dr, tx, err)
	}
	if ok, _ := ParseLinkADRAns(MarshalLinkADRAns(true, true, true)); !ok {
		t.Fatal("ans should be fully accepted")
	}
	if ok, _ := ParseLinkADRAns(MarshalLinkADRAns(false, true, true)); ok {
		t.Fatal("ans with rate failure must not be ok")
	}
}
