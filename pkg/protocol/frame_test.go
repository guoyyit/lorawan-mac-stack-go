package protocol

import (
	"bytes"
	"testing"

	lwcrypto "github.com/guoyyit/lorawan-mac-stack-go/pkg/crypto"
)

func testKey() lwcrypto.AES128Key {
	var k lwcrypto.AES128Key
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestJoinRequestRoundTrip(t *testing.T) {
	appKey := testKey()
	req := JoinRequest{
		JoinEUI:  EUI64{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		DevEUI:   EUI64{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
		DevNonce: DevNonce{0xaa, 0xbb},
	}
	raw, err := MarshalJoinRequest(appKey, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 23 {
		t.Fatalf("len=%d", len(raw))
	}
	parsed, err := ParseJoinRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.JoinEUI != req.JoinEUI || parsed.DevEUI != req.DevEUI || parsed.DevNonce != req.DevNonce {
		t.Fatalf("join request round trip mismatch: %+v", parsed)
	}
	if !VerifyJoinRequestMIC(appKey, raw) {
		t.Fatal("join request MIC invalid")
	}
	raw[19] ^= 0xff
	if VerifyJoinRequestMIC(appKey, raw) {
		t.Fatal("corrupted join request MIC accepted")
	}
}

func TestJoinAcceptRoundTrip(t *testing.T) {
	appKey := testKey()
	acc := JoinAccept{
		AppNonce:   AppNonce{1, 2, 3},
		NetID:      NetID{4, 5, 6},
		DevAddr:    DevAddr{0x2e, 0x1a, 0x00, 0x01},
		DLSettings: 0x31,
		RXDelay:    0x10,
	}
	raw, err := MarshalJoinAccept(appKey, acc)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 17 {
		t.Fatalf("len=%d", len(raw))
	}
	parsed, err := ParseJoinAccept(appKey, raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AppNonce != acc.AppNonce || parsed.NetID != acc.NetID ||
		parsed.DevAddr != acc.DevAddr || parsed.DLSettings != acc.DLSettings ||
		parsed.RXDelay != acc.RXDelay {
		t.Fatalf("join accept round trip mismatch: %+v", parsed)
	}
	raw[5] ^= 0x01
	if _, err := ParseJoinAccept(appKey, raw); err == nil {
		t.Fatal("corrupted join accept accepted")
	}
}

func TestDataFrameRoundTrip(t *testing.T) {
	keys := lwcrypto.SessionKeys{NwkSKey: testKey(), AppSKey: testKey()}
	devAddr := DevAddr{0x2e, 0x1a, 0x00, 0x01}

	for _, fport := range []int{0, 5} {
		f := DataFrame{
			MType:      MTConfirmedDataUp,
			DevAddr:    devAddr,
			FCtrl:      FCtrl{ADR: true},
			FPort:      fport,
			FRMPayload: []byte("temperature=21.5;hum=44"),
			FOpts: []MACCommand{
				{CID: CIDDevStatusAns, Payload: EncodeDevStatusAns(DevStatusAns{Battery: 200, Margin: 7})},
			},
		}
		raw, err := MarshalDataFrame(keys, f, 42)
		if err != nil {
			t.Fatal(err)
		}
		// wire payload must not expose plaintext
		if bytes.Contains(raw, []byte("temperature")) {
			t.Fatal("plaintext leaked on wire")
		}
		dec, err := ParseDataFrame(keys, false, 42, raw)
		if err != nil {
			t.Fatal(err)
		}
		if !dec.MICValid {
			t.Fatal("MIC invalid after round trip")
		}
		if dec.MType != MTConfirmedDataUp || dec.DevAddr != devAddr || dec.FCnt != 42 {
			t.Fatalf("header mismatch %+v", dec.DataFrame)
		}
		if !bytes.Equal(dec.FRMPayload, f.FRMPayload) {
			t.Fatalf("payload mismatch %q", dec.FRMPayload)
		}
		if len(dec.FOpts) != 1 {
			t.Fatalf("fopts %d", len(dec.FOpts))
		}

		// direction mismatch must fail (uplink keyed as downlink)
		if _, err := ParseDataFrame(keys, true, 42, raw); err == nil {
			t.Fatal("uplink frame parsed as downlink")
		}

		// corrupted frame must fail MIC
		raw[10] ^= 0xff
		dec2, err := ParseDataFrame(keys, false, 42, raw)
		if err != nil {
			t.Fatal(err)
		}
		if dec2.MICValid {
			t.Fatal("corrupted frame passed MIC")
		}
	}
}

func TestMalformedFramesRejected(t *testing.T) {
	keys := lwcrypto.SessionKeys{NwkSKey: testKey(), AppSKey: testKey()}
	for _, bad := range [][]byte{nil, {}, {0}, make([]byte, 5), make([]byte, 20)} {
		if _, err := ParseDataFrame(keys, false, 0, bad); err == nil {
			t.Fatalf("malformed frame accepted: %x", bad)
		}
	}
}

func TestDataFrame32BitCounterWrap(t *testing.T) {
	keys := lwcrypto.SessionKeys{NwkSKey: testKey(), AppSKey: testKey()}
	devAddr := DevAddr{0x2e, 0x1a, 0, 1}
	high := uint32(0x10003) // low16 == 3
	f := DataFrame{MType: MTConfirmedDataUp, DevAddr: devAddr,
		FPort: 5, FRMPayload: []byte("wrap")}
	raw, err := MarshalDataFrame(keys, f, high)
	if err != nil {
		t.Fatal(err)
	}
	if raw[6] != 3 || raw[7] != 0 {
		t.Fatalf("wire FCnt bytes = %02x %02x, want 03 00", raw[6], raw[7])
	}
	dec, err := ParseDataFrame(keys, false, high, raw)
	if err != nil || !dec.MICValid || dec.FCnt32 != high {
		t.Fatalf("high-counter round trip: %+v err %v", dec, err)
	}
	// parsing with a wrong recovered high half must fail MIC
	bad, err := ParseDataFrame(keys, false, 0x20003, raw)
	if err != nil {
		t.Fatal(err)
	}
	if bad.MICValid {
		t.Fatal("frame verified MIC with wrong counter high bits")
	}
}
