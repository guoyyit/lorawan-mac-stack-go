package crypto

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestCMACRFC4493Vectors(t *testing.T) {
	keyBytes, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	var key AES128Key
	copy(key[:], keyBytes)
	msg16, _ := hex.DecodeString("6bc1bee22e409f96e93d7e117393172a")
	msg40, _ := hex.DecodeString("6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411")
	msg64, _ := hex.DecodeString("6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710")
	cases := []struct {
		msg []byte
		tag string
	}{
		{nil, "bb1d6929e95937287fa37d129b756746"},
		{msg16, "070a16b46b4d4144f79bdd9dd04a287c"},
		{msg40, "dfa66747de9ae63030ca32611497c827"},
		{msg64, "51f0bebf7e3b9d92fc49741779363cfe"},
	}
	c := newCipher(key)
	for _, tc := range cases {
		got := aesCMAC(c, tc.msg)
		want, _ := hex.DecodeString(tc.tag)
		if !bytes.Equal(got[:], want) {
			t.Fatalf("len=%d got %x want %s", len(tc.msg), got, tc.tag)
		}
	}
}

func TestLoRaWANDerivation(t *testing.T) {
	appKeyBytes, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	var appKey AES128Key
	copy(appKey[:], appKeyBytes)
	keys := DeriveSessionKeys(appKey,
		[3]byte{0x01, 0x02, 0x03},
		[3]byte{0x04, 0x05, 0x06},
		[2]byte{0x07, 0x08})
	if hex.EncodeToString(keys.NwkSKey[:]) != "cf91ad9eddf9ce645b515e2382d1c8cc" {
		t.Fatalf("NwkSKey=%x", keys.NwkSKey)
	}
	if hex.EncodeToString(keys.AppSKey[:]) != "600268e276a51d072904ae4a064712b4" {
		t.Fatalf("AppSKey=%x", keys.AppSKey)
	}

	// AES-CTR round trip.
	devAddr := [4]byte{0x01, 0x02, 0x03, 0x04}
	pt := []byte("hello lorawan mac payload!!")
	ct := EncryptFramePayload(keys.AppSKey, devAddr, false, 7, pt)
	if bytes.Equal(ct, pt) {
		t.Fatal("ciphertext equals plaintext")
	}
	back := EncryptFramePayload(keys.AppSKey, devAddr, false, 7, ct)
	if !bytes.Equal(back, pt) {
		t.Fatalf("round trip mismatch %q", back)
	}
}

func TestJoinAcceptECBRoundTrip(t *testing.T) {
	var key AES128Key
	keyBytes, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	copy(key[:], keyBytes)
	pt := make([]byte, 32)
	for i := range pt {
		pt[i] = byte(i + 1)
	}
	ct := EncryptJoinAcceptPayload(key, pt)
	back, err := DecryptJoinAcceptPayload(key, ct)
	if err != nil || !bytes.Equal(back, pt) {
		t.Fatalf("join-accept round trip err=%v match=%v", err, bytes.Equal(back, pt))
	}
}
