package lorawan

import "errors"

// Frame size limits (documented in README).
//
// The data-frame MIC is computed over a B0 block whose length field is a
// single byte, so the MIC input (everything except the trailing 4-byte MIC)
// is at most 255 bytes. The local wire header is 14 bytes and the MIC is 4,
// which fixes the hard limits below. Frames at exactly the limit are legal;
// anything larger is rejected deterministically with ErrFrameTooLarge on BOTH
// the encoding and the decoding path, before any allocation or cryptography
// proportional to the claimed length takes place.
const (
	// MaxFrameSize is the maximum total wire size of a data frame in bytes,
	// including the 4-byte MIC: 14 (header) + 255 - 14 (body) + 4 (MIC).
	MaxFrameSize = 259

	// frameHeaderSize is the fixed wire header size before FOpts/payload.
	frameHeaderSize = 14

	// micSize is the trailing MIC size.
	micSize = 4

	// maxMICInput is the largest byte count the B0 length field can express.
	maxMICInput = 255

	// MaxPayloadSize is the largest application payload a frame can carry
	// with empty FOpts: MaxFrameSize - header - MIC.
	MaxPayloadSize = MaxFrameSize - frameHeaderSize - micSize // 241
)

// ErrFrameTooLarge is returned when an encoded or received frame exceeds
// MaxFrameSize (or a payload exceeds what fits). It is distinct from
// ErrMalformed (structural corruption), ErrBadMIC (integrity failure),
// ErrReplay/ErrDuplicate (frame-counter reuse) and ErrCounterOverflow
// (counter space exhausted) so callers can tell length rejection apart from
// every other failure mode.
var ErrFrameTooLarge = errors.New("lorawan: frame exceeds maximum size")
