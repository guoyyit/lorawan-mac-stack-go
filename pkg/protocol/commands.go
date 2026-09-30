package protocol

import "errors"

var (
	errBadLinkADRReq   = errors.New("protocol: LinkADRReq payload must be 4 bytes")
	errBadLinkADRAns   = errors.New("protocol: LinkADRAns payload must be 1 byte")
	errBadDevStatusAns = errors.New("protocol: DevStatusAns payload must be 2 bytes")
)

// MAC command identifiers (CID) used by this stack.
const (
	CIDLinkADRReq   byte = 0x03
	CIDLinkADRAns   byte = 0x03
	CIDDevStatusReq byte = 0x06
	CIDDevStatusAns byte = 0x06
)

// MACCommand is a single MAC command carried in FOpts.
type MACCommand struct {
	CID     byte
	Payload []byte
}

// LinkADRReq is the server-to-device LinkADR command.
type LinkADRReq struct {
	DataRate   uint8
	TXPower    uint8
	ChMask     uint16
	Redundancy uint8
}

// LinkADRAns is the device acknowledgement of LinkADRReq.
type LinkADRAns struct {
	PowerACK       bool
	DataRateACK    bool
	ChannelMaskACK bool
}

// DevStatusAns reports battery and demodulation margin to the server.
type DevStatusAns struct {
	Battery uint8
	Margin  int8
}

// EncodeLinkADRReq serializes LinkADRReq payload (4 bytes).
func EncodeLinkADRReq(r LinkADRReq) []byte {
	return []byte{
		r.DataRate&0x0f<<4 | r.TXPower&0x0f,
		byte(r.ChMask),
		byte(r.ChMask >> 8),
		r.Redundancy,
	}
}

// DecodeLinkADRReq parses a LinkADRReq payload.
func DecodeLinkADRReq(b []byte) (LinkADRReq, error) {
	if len(b) != 4 {
		return LinkADRReq{}, errBadLinkADRReq
	}
	return LinkADRReq{
		DataRate:   b[0] >> 4,
		TXPower:    b[0] & 0x0f,
		ChMask:     uint16(b[1]) | uint16(b[2])<<8,
		Redundancy: b[3],
	}, nil
}

// EncodeLinkADRAns serializes a LinkADRAns payload (1 byte).
func EncodeLinkADRAns(a LinkADRAns) []byte {
	var b byte
	if a.PowerACK {
		b |= 0x04
	}
	if a.DataRateACK {
		b |= 0x02
	}
	if a.ChannelMaskACK {
		b |= 0x01
	}
	return []byte{b}
}

// DecodeLinkADRAns parses a LinkADRAns payload.
func DecodeLinkADRAns(b []byte) (LinkADRAns, error) {
	if len(b) != 1 {
		return LinkADRAns{}, errBadLinkADRAns
	}
	return LinkADRAns{
		PowerACK:       b[0]&0x04 != 0,
		DataRateACK:    b[0]&0x02 != 0,
		ChannelMaskACK: b[0]&0x01 != 0,
	}, nil
}

// EncodeDevStatusAns serializes a DevStatusAns payload (2 bytes).
func EncodeDevStatusAns(a DevStatusAns) []byte {
	margin := byte(a.Margin) & 0x3f // bits 0..5, no reserved-bit sign extension
	return []byte{a.Battery, margin}
}

// DecodeDevStatusAns parses a DevStatusAns payload.
func DecodeDevStatusAns(b []byte) (DevStatusAns, error) {
	if len(b) != 2 {
		return DevStatusAns{}, errBadDevStatusAns
	}
	margin := int8(b[1] & 0x3f)
	if margin > 31 {
		margin -= 64
	}
	return DevStatusAns{Battery: b[0], Margin: margin}, nil
}
