package lorawan

import "errors"

// EUI is an 8-byte LoRaWAN EUI (JoinEUI / DevEUI).
type EUI [8]byte

// DevAddr is a 4-byte end-device address.
type DevAddr [4]byte

// AESKey is a 128-bit AES key (AppKey / NwkSKey / AppSKey).
type AESKey [16]byte

// DevNonce identifies a join request; sent by the device.
type DevNonce uint16

// JoinNonce is issued by the server for a join-accept.
type JoinNonce [3]byte

// NetID is the 3-byte network identifier.
type NetID [3]byte

// MType is the LoRaWAN MAC message type (upper 3 bits of MHdr).
type MType uint8

const (
	MTypeJoinRequest         MType = 0x00
	MTypeJoinAccept          MType = 0x20
	MTypeUnconfirmedDataUp   MType = 0x40
	MTypeUnconfirmedDataDown MType = 0x60
	MTypeConfirmedDataUp     MType = 0x80
	MTypeConfirmedDataDown   MType = 0xA0
)

// MAC command identifiers used by this stack.
const (
	CIDLinkCheckReq = 0x02
	CIDLinkCheckAns = 0x02
	CIDLinkADRReq   = 0x03
	CIDLinkADRAns   = 0x03
	CIDDevStatusReq = 0x06
	CIDDevStatusAns = 0x06
)

// Protocol errors. They are deterministic and stable for callers/tests.
var (
	// ErrMalformed is returned for truncated or structurally invalid frames.
	ErrMalformed = errors.New("lorawan: malformed frame")
	// ErrBadMIC is returned when the integrity check fails.
	ErrBadMIC = errors.New("lorawan: bad MIC")
	// ErrReplay is returned for frames with a frame counter below the replay floor.
	ErrReplay = errors.New("lorawan: frame counter replay")
	// ErrDuplicate is returned for an exact duplicate of the last accepted frame.
	ErrDuplicate = errors.New("lorawan: duplicate frame")
	// ErrCounterOverflow is returned when a 32-bit frame counter would overflow.
	ErrCounterOverflow = errors.New("lorawan: frame counter overflow")
	// ErrDevNonceReused is returned when a device re-uses a non-increasing DevNonce.
	ErrDevNonceReused = errors.New("lorawan: dev nonce reused")
	// ErrNoSession is returned when no session exists for the addressed device.
	ErrNoSession = errors.New("lorawan: no session")
	// ErrUnknownDevice is returned for an unregistered device.
	ErrUnknownDevice = errors.New("lorawan: unknown device")
	// ErrDutyCycle is returned when a transmit request is rejected by duty-cycle policy.
	ErrDutyCycle = errors.New("lorawan: duty cycle limited")
	// ErrNoACK is returned when all confirmed-uplink retries exhausted without an ACK.
	ErrNoACK = errors.New("lorawan: no downlink acknowledgement")
	// ErrWindowClosed is returned when a downlink arrives outside a Class A window.
	ErrWindowClosed = errors.New("lorawan: receive window closed")
	// ErrNotFound is returned by persistence stores for missing records.
	ErrNotFound = errors.New("lorawan: record not found")
)
