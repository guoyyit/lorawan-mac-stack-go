package lorawan

import (
	"crypto/aes"
	"encoding/binary"
)

// DeriveSessionKeys derives NwkSKey/AppSKey from the LoRaWAN 1.0.2 key
// derivation (0x01/0x02 blocks over JoinNonce | NetID | DevNonce).
// The 3-byte fields and DevNonce are little-endian, exactly as on the air.
func DeriveSessionKeys(appKey AESKey, joinNonce JoinNonce, netID NetID, devNonce DevNonce) (AESKey, AESKey) {
	return deriveKey(appKey, 0x01, joinNonce, netID, devNonce),
		deriveKey(appKey, 0x02, joinNonce, netID, devNonce)
}

func deriveKey(appKey AESKey, tag byte, jn JoinNonce, netID NetID, dn DevNonce) AESKey {
	var buf [16]byte
	buf[0] = tag
	copy(buf[1:4], jn[:])
	copy(buf[4:7], netID[:])
	binary.LittleEndian.PutUint16(buf[7:9], uint16(dn))
	block, err := aes.NewCipher(appKey[:])
	if err != nil {
		panic("lorawan: invalid AppKey length")
	}
	var out AESKey
	block.Encrypt(out[:], buf[:])
	return out
}

// encryptPayload applies AES-CTR with the LoRaWAN Ai block construction.
// Encryption is symmetric: the same routine decrypts.
func encryptPayload(key AESKey, addr DevAddr, down bool, fcnt uint32, port uint8, data []byte) []byte {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic("lorawan: invalid session key length")
	}
	out := make([]byte, len(data))
	var a [16]byte
	a[0] = 0x01
	a[5] = dirByte(down)
	copy(a[6:10], addr[:])
	binary.LittleEndian.PutUint32(a[10:14], fcnt)
	// byte 15 (a.i) is the keystream block counter, starts at 1.
	for i, ai := 0, uint8(1); i < len(data); i, ai = i+16, ai+1 {
		a[15] = ai
		var ks [16]byte
		block.Encrypt(ks[:], a[:])
		n := len(data) - i
		if n > 16 {
			n = 16
		}
		for j := 0; j < n; j++ {
			out[i+j] = data[i+j] ^ ks[j]
		}
	}
	return out
}

func dirByte(down bool) byte {
	if down {
		return 0x01
	}
	return 0x00
}

// computeMIC returns the first 4 bytes of AES-CMAC(key, B0|msg), using the
// LoRaWAN B0 block (direction, DevAddr, full 32-bit frame counter).
func computeMIC(key AESKey, addr DevAddr, down bool, fcnt uint32, msg []byte) [4]byte {
	if len(msg) > 255 {
		// The B0 length field is one byte; the local wire format guarantees this.
		panic("lorawan: frame too long for MIC")
	}
	var b0 [16]byte
	b0[0] = 0x49
	b0[5] = dirByte(down)
	copy(b0[6:10], addr[:])
	binary.LittleEndian.PutUint32(b0[10:14], fcnt)
	b0[15] = byte(len(msg))
	combined := make([]byte, 0, 16+len(msg))
	combined = append(combined, b0[:]...)
	combined = append(combined, msg...)
	mac := aesCMAC(key, combined)
	var mic [4]byte
	copy(mic[:], mac[:4])
	return mic
}

// joinMIC computes AES-CMAC(AppKey, full-frame-bytes).
func joinMIC(appKey AESKey, body []byte) [4]byte {
	mac := aesCMAC(appKey, body)
	var mic [4]byte
	copy(mic[:], mac[:4])
	return mic
}

// aesCMAC is a self-contained AES-128-CMAC (RFC 4493).
func aesCMAC(key AESKey, msg []byte) []byte {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic("lorawan: invalid CMAC key length")
	}
	var zero [16]byte
	var l [16]byte
	block.Encrypt(l[:], zero[:])
	k1 := cmacSubkey(l[:])
	k2 := cmacSubkey(k1)

	n := len(msg) / 16
	if len(msg)%16 != 0 || len(msg) == 0 {
		n++
	}
	if n == 0 {
		n = 1
	}
	last := make([]byte, 16)
	if n == 1 && len(msg) == 0 {
		last[0] = 0x80
		for i := range last {
			last[i] ^= k2[i]
		}
	} else {
		tailStart := (n - 1) * 16
		tail := msg[tailStart:]
		if len(tail) == 16 {
			copy(last, tail)
			for i := range last {
				last[i] ^= k1[i]
			}
		} else {
			copy(last, tail)
			last[len(tail)] = 0x80
			for i := range last {
				last[i] ^= k2[i]
			}
		}
	}

	var x [16]byte
	// All blocks except the last are processed as-is; the final block is
	// padded and subkey-mixed into `last`.
	for off := 0; off+16 < n*16; off += 16 {
		for i := 0; i < 16; i++ {
			x[i] ^= msg[off+i]
		}
		block.Encrypt(x[:], x[:])
	}
	for i := 0; i < 16; i++ {
		x[i] ^= last[i]
	}
	block.Encrypt(x[:], x[:])
	return x[:]
}

func cmacSubkey(in []byte) []byte {
	out := make([]byte, 16)
	msb := in[0] >> 7
	for i := 0; i < 15; i++ {
		out[i] = (in[i] << 1) | (in[i+1] >> 7)
	}
	out[15] = in[15] << 1
	if msb == 1 {
		out[15] ^= 0x87
	}
	return out
}
