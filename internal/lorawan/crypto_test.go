package lorawan

import (
	"encoding/hex"
	"testing"
)

func mustKey(t *testing.T, h string) AESKey {
	t.Helper()
	var k AESKey
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	copy(k[:], b)
	return k
}

func TestAESCMAC_RFC4493(t *testing.T) {
	key := mustKey(t, "2b7e151628aed2a6abf7158809cf4f3c")
	cases := []struct {
		msg string
		out string
	}{
		{"", "bb1d6929e95937287fa37d129b756746"},
		{"6bc1bee22e409f96e93d7e117393172a", "070a16b46b4d4144f79bdd9dd04a287c"},
		{"6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411", "dfa66747de9ae63030ca32611497c827"},
	}
	for _, tc := range cases {
		msg, err := hex.DecodeString(tc.msg)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(aesCMAC(key, msg)); got != tc.out {
			t.Errorf("cmac(%x) = %s, want %s", msg, got, tc.out)
		}
	}
}

func TestDeriveSessionKeys(t *testing.T) {
	// Values independently produced with openssl AES-128-ECB over the
	// 0x01/0x02 | JoinNonce | NetID | DevNonce | 0x00*7 blocks.
	// DevNonce 0x0001 is little-endian bytes [01 00].
	appKey := mustKey(t, "000102030405060708090a0b0c0d0e0f")
	jn := JoinNonce{0x03, 0x02, 0x01}
	netID := NetID{0x00, 0x00, 0x00}
	nwk, app := DeriveSessionKeys(appKey, jn, netID, 0x0001)
	if got := hex.EncodeToString(nwk[:]); got != "b6e25c10a9e990ce27fbf3ae25288202" {
		t.Fatalf("NwkSKey = %s", got)
	}
	if got := hex.EncodeToString(app[:]); got != "b665279274af95adce370148d0f89ab8" {
		t.Fatalf("AppSKey = %s", got)
	}
}

func TestJoinMIC(t *testing.T) {
	// Backend-interop worked join-request example: MIC over the 19-byte
	// MHdr|AppEUI|DevEUI|DevNonce body (AppEUI/DevEUI little-endian on wire).
	appKey := mustKey(t, "2B7E151628AED2A6ABF7158809CF4F3C")
	frame, err := hex.DecodeString("0001000000000000000123456789ABCDEF0100")
	if err != nil {
		t.Fatal(err)
	}
	got := joinMIC(appKey, frame)
	if hex.EncodeToString(got[:]) != "61b649e7" {
		t.Fatalf("joinMIC = %x, want 61b649e7", got)
	}
}

func TestEncryptPayloadRoundTrip(t *testing.T) {
	key := mustKey(t, "00112233445566778899aabbccddeeff")
	addr := DevAddr{0x01, 0x02, 0x03, 0x04}
	plain := []byte("hello lorawan")
	ct := encryptPayload(key, addr, false, 7, 1, plain)
	if string(ct) == string(plain) {
		t.Fatal("payload was not encrypted")
	}
	pt := encryptPayload(key, addr, false, 7, 1, ct)
	if string(pt) != string(plain) {
		t.Fatalf("round trip = %q", pt)
	}
}
