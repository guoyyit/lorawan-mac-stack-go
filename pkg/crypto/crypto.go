// Package crypto implements the LoRaWAN AES-128 cryptographic primitives
// used by this stack: OTAA session-key derivation, frame payload
// AES-CTR encryption, AES-CMAC frame MICs and Join-Accept ECB encryption.
package crypto

import (
	"crypto/aes"
	"encoding/binary"
	"errors"
)

// errBadLength is returned when a join-accept payload is not 16 bytes.
var errBadLength = errors.New("crypto: join-accept payload must be 16 bytes")

func newCipher(key AES128Key) cipherBlock {
	c, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err) // a 16-byte key always builds a valid AES-128 cipher
	}
	return c
}

// cipherBlock is the small slice of cipher.Block used here.
type cipherBlock interface {
	BlockSize() int
	Encrypt(dst, src []byte)
	Decrypt(dst, src []byte)
}

// AES128Key is a 128-bit AES key (AppKey, NwkSKey or AppSKey).
type AES128Key [16]byte

// SessionKeys holds the keys derived during a successful OTAA join.
type SessionKeys struct {
	NwkSKey AES128Key
	AppSKey AES128Key
}

// DeriveSessionKeys derives NwkSKey/AppSKey from AppKey, AppNonce, NetID
// and DevNonce following the LoRaWAN 1.0.x key-derivation scheme.
func DeriveSessionKeys(appKey AES128Key, appNonce [3]byte, netID [3]byte, devNonce [2]byte) SessionKeys {
	c := newCipher(appKey)

	base := make([]byte, 16)
	copy(base[0:3], appNonce[:])
	copy(base[3:6], netID[:])
	copy(base[6:8], devNonce[:])

	nwkInput := append([]byte(nil), base...)
	nwkInput[15] = 0x01
	nwk := make([]byte, 16)
	c.Encrypt(nwk, nwkInput)

	appInput := append([]byte(nil), base...)
	appInput[15] = 0x02
	app := make([]byte, 16)
	c.Encrypt(app, appInput)

	return SessionKeys{
		NwkSKey: AES128Key(nwk),
		AppSKey: AES128Key(app),
	}
}

// EncryptFramePayload performs AES-CTR encryption/decryption of FRMPayload.
// Encryption is symmetric, so the same routine is used in both directions.
func EncryptFramePayload(key AES128Key, devAddr [4]byte, downlink bool, fcnt uint32, payload []byte) []byte {
	c := newCipher(key)

	a := make([]byte, 16)
	a[0] = 0x01
	copy(a[6:10], devAddr[:])
	binary.LittleEndian.PutUint32(a[10:], fcnt)
	if downlink {
		a[5] = 1
	}

	out := make([]byte, len(payload))
	counter := uint32(1)
	for i := 0; i < len(payload); i += 16 {
		binary.LittleEndian.PutUint16(a[14:16], uint16(counter))
		ks := make([]byte, 16)
		c.Encrypt(ks, a)
		end := i + 16
		if end > len(payload) {
			end = len(payload)
		}
		for j := i; j < end; j++ {
			out[j] = payload[j] ^ ks[j-i]
		}
		counter++
	}
	return out
}

// ComputeMIC returns the 4-byte AES-CMAC message integrity code for a data
// frame (the 32-bit frame counter is used in B0).
func ComputeMIC(key AES128Key, devAddr [4]byte, downlink bool, fcnt uint32, msg []byte) [4]byte {
	b0 := make([]byte, 16)
	b0[0] = 0x49
	copy(b0[6:10], devAddr[:])
	binary.LittleEndian.PutUint32(b0[10:], fcnt)
	b0[15] = byte(len(msg))
	if downlink {
		b0[5] = 1
	}

	mac := aesCMAC(newCipher(key), append(b0, msg...))
	return [4]byte(mac[:4])
}

// JoinMessageMIC returns the 4-byte AES-CMAC MIC for a Join-Request or
// (encrypted) Join-Accept message.
func JoinMessageMIC(key AES128Key, msg []byte) [4]byte {
	mac := aesCMAC(newCipher(key), msg)
	return [4]byte(mac[:4])
}

// EncryptJoinAcceptPayload AES-ECB encrypts the Join-Accept payload with AppKey.
func EncryptJoinAcceptPayload(key AES128Key, plaintext []byte) []byte {
	c := newCipher(key)
	reversed := reverseBytes(plaintext)
	out := make([]byte, len(reversed))
	for i := 0; i < len(reversed); i += 16 {
		c.Encrypt(out[i:i+16], reversed[i:i+16])
	}
	return reverseBytes(out)
}

// DecryptJoinAcceptPayload AES-ECB decrypts a Join-Accept payload with AppKey.
func DecryptJoinAcceptPayload(key AES128Key, ciphertext []byte) ([]byte, error) {
	if len(ciphertext)%16 != 0 {
		return nil, errBadLength
	}
	c := newCipher(key)
	reversed := reverseBytes(ciphertext)
	out := make([]byte, len(reversed))
	for i := 0; i < len(reversed); i += 16 {
		c.Decrypt(out[i:i+16], reversed[i:i+16])
	}
	return reverseBytes(out), nil
}

// reverseBytes returns a reversed copy of b.
func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

// aesCMAC computes AES-CMAC (RFC 4493) over data.
func aesCMAC(c cipherBlock, data []byte) [16]byte {
	const blockSize = 16

	// L = AES(0^128)
	var zero, l [blockSize]byte
	c.Encrypt(l[:], zero[:])

	// K1 = (L << 1) xor Rb when MSB(L)=1, else L << 1; same for K2.
	// Rb for AES-128 is 0x00...87 in the final byte.
	k1 := cmacSubkey(l[:])
	k2Shift := shiftLeftOne(k1)
	if k1[0]&0x80 != 0 {
		k2Shift[len(k2Shift)-1] ^= 0x87
	}
	k2 := k2Shift

	var last [blockSize]byte
	if len(data) != 0 && len(data)%blockSize == 0 {
		copy(last[:], data[len(data)-blockSize:])
		for i := range last {
			last[i] ^= k1[i]
		}
	} else {
		// last = (tail || 10*) xor K2
		off := len(data) - len(data)%blockSize
		copy(last[:], data[off:])
		last[len(data)-off] = 0x80
		for i := range last {
			last[i] ^= k2[i]
		}
	}

	var x [blockSize]byte
	// Process every complete block except the final one; the final block
	// is the subkey-tweaked value in `last` (which always exists, so the
	// empty-message case is covered too).
	intermediateEnd := len(data)
	if intermediateEnd%blockSize == 0 && intermediateEnd > 0 {
		intermediateEnd -= blockSize
	} else {
		intermediateEnd -= intermediateEnd % blockSize
	}
	for off := 0; off < intermediateEnd; off += blockSize {
		blk := data[off : off+blockSize]
		y := make([]byte, blockSize)
		for i := range x {
			y[i] = x[i] ^ blk[i]
		}
		c.Encrypt(x[:], y)
	}

	y := make([]byte, blockSize)
	for i := range x {
		y[i] = x[i] ^ last[i]
	}
	var tag [blockSize]byte
	c.Encrypt(tag[:], y)
	return tag
}

// shiftLeftOne left-shifts the 16-byte value by one bit, where byte 0 is
// the most significant byte.
func shiftLeftOne(b []byte) []byte {
	out := make([]byte, len(b))
	var carry byte
	for i := len(b) - 1; i >= 0; i-- {
		msb := b[i] >> 7
		out[i] = (b[i] << 1) | carry
		carry = msb
	}
	return out
}

// cmacSubkey computes the next CMAC subkey: (b << 1) xor Rb if the most
// significant bit of b is set, otherwise just b << 1.
func cmacSubkey(b []byte) []byte {
	const rbLast = 0x87
	out := shiftLeftOne(b)
	if b[0]&0x80 != 0 {
		out[len(b)-1] ^= rbLast
	}
	return out
}
