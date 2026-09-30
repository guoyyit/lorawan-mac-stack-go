package lorawan

// JoinRequest is an OTAA join-request body (MHdr excluded).
type JoinRequest struct {
	JoinEUI  EUI
	DevEUI   EUI
	DevNonce DevNonce
}

// JoinAccept is an OTAA join-accept body (MHdr excluded).
type JoinAccept struct {
	JoinNonce   JoinNonce
	NetID       NetID
	DevAddr     DevAddr
	RX1DROffset uint8
	RX2DR       uint8
	RXDelay     uint8
}

// Frame is the local, decoded representation of a LoRaWAN data frame.
// The wire encoding is a fixed, documented local format (not the over-the-air
// LoRaWAN byte layout), carrying the full 32-bit frame counter.
type Frame struct {
	MType   MType
	DevAddr DevAddr
	ADR     bool
	ACK     bool
	FCnt    uint32
	HasPort bool
	FPort   uint8
	FOpts   []byte
	Payload []byte
}

// IsDown reports whether the frame is a downlink.
func (f *Frame) IsDown() bool {
	return f.MType == MTypeUnconfirmedDataDown || f.MType == MTypeConfirmedDataDown
}

const (
	ctlADR   = 0x40
	ctlACK   = 0x20
	ctlFPort = 0x80
)

// EncodeJoinRequest signs and encodes a join request (23 bytes).
func EncodeJoinRequest(appKey AESKey, jr *JoinRequest) ([]byte, error) {
	if jr == nil {
		return nil, ErrMalformed
	}
	body := make([]byte, 0, 23)
	body = append(body, byte(MTypeJoinRequest))
	body = append(body, jr.JoinEUI[:]...)
	body = append(body, jr.DevEUI[:]...)
	body = appendU16LE(body, uint16(jr.DevNonce))
	mic := joinMIC(appKey, body)
	return append(body, mic[:]...), nil
}

// DecodeJoinRequest validates MIC and decodes a join request.
func DecodeJoinRequest(appKey AESKey, raw []byte) (*JoinRequest, error) {
	if len(raw) != 23 || raw[0] != byte(MTypeJoinRequest) {
		return nil, ErrMalformed
	}
	var mic [4]byte
	copy(mic[:], raw[19:23])
	if joinMIC(appKey, raw[:19]) != mic {
		return nil, ErrBadMIC
	}
	jr := &JoinRequest{}
	copy(jr.JoinEUI[:], raw[1:9])
	copy(jr.DevEUI[:], raw[9:17])
	jr.DevNonce = DevNonce(readU16LE(raw[17:19]))
	return jr, nil
}

// EncodeJoinAccept signs and encodes a join accept (17 bytes).
func EncodeJoinAccept(appKey AESKey, ja *JoinAccept) ([]byte, error) {
	if ja == nil {
		return nil, ErrMalformed
	}
	body := make([]byte, 0, 17)
	body = append(body, byte(MTypeJoinAccept))
	body = append(body, ja.JoinNonce[:]...)
	body = append(body, ja.NetID[:]...)
	body = append(body, ja.DevAddr[:]...)
	body = append(body, ja.RX1DROffset, (ja.RX2DR<<4)|ja.RXDelay)
	mic := joinMIC(appKey, body)
	return append(body, mic[:]...), nil
}

// DecodeJoinAccept validates MIC and decodes a join accept.
func DecodeJoinAccept(appKey AESKey, raw []byte) (*JoinAccept, error) {
	if len(raw) != 17 || raw[0] != byte(MTypeJoinAccept) {
		return nil, ErrMalformed
	}
	var mic [4]byte
	copy(mic[:], raw[13:17])
	if joinMIC(appKey, raw[:13]) != mic {
		return nil, ErrBadMIC
	}
	return &JoinAccept{
		JoinNonce:   JoinNonce(raw[1:4]),
		NetID:       NetID(raw[4:7]),
		DevAddr:     DevAddr(raw[7:11]),
		RX1DROffset: raw[11],
		RX2DR:       raw[12] >> 4,
		RXDelay:     raw[12] & 0x0f,
	}, nil
}

// Local data-frame wire layout (all multi-byte integers little-endian):
//
//	0    MType
//	1    FCtrl (bit6 ADR, bit5 ACK, bit7 FPort present)
//	2-5  DevAddr
//	6-9  FCnt (full 32-bit counter)
//	10   FOpts length
//	11-12 Payload length
//	13   FPort (0 if absent)
//	14.. FOpts
//	..   encrypted Payload
//	last 4  NwkSKey MIC over all preceding bytes

// EncodeFrame serializes, encrypts and signs a data frame for the session.
func EncodeFrame(s *Session, f *Frame) ([]byte, error) {
	if s == nil || f == nil {
		return nil, ErrMalformed
	}
	if len(f.FOpts) > 255 || len(f.Payload) > 65535 {
		return nil, ErrMalformed
	}
	port := byte(0)
	if f.HasPort {
		port = f.FPort
	}
	var ctl byte
	if f.ADR {
		ctl |= ctlADR
	}
	if f.ACK {
		ctl |= ctlACK
	}
	if f.HasPort {
		ctl |= ctlFPort
	}

	out := make([]byte, 0, 14+len(f.FOpts)+len(f.Payload)+4)
	out = append(out, byte(f.MType), ctl)
	out = append(out, f.DevAddr[:]...)
	out = appendU32LE(out, f.FCnt)
	out = append(out, byte(len(f.FOpts)))
	out = appendU16LE(out, uint16(len(f.Payload)))
	out = append(out, port)
	out = append(out, f.FOpts...)
	down := f.IsDown()
	if len(f.Payload) > 0 {
		out = append(out, encryptPayload(payloadKey(s, port), f.DevAddr, down, f.FCnt, port, f.Payload)...)
	}
	mic := computeMIC(s.NwkSKey, f.DevAddr, down, f.FCnt, out)
	return append(out, mic[:]...), nil
}

// DecodeFrame parses, verifies and decrypts a data frame for the session.
// On any structural or integrity failure it returns an error and no state.
func DecodeFrame(s *Session, raw []byte) (*Frame, error) {
	if s == nil || len(raw) < 18 {
		return nil, ErrMalformed
	}
	mt := MType(raw[0])
	if mt != MTypeUnconfirmedDataUp && mt != MTypeUnconfirmedDataDown &&
		mt != MTypeConfirmedDataUp && mt != MTypeConfirmedDataDown {
		return nil, ErrMalformed
	}
	ctl := raw[1]
	f := &Frame{
		MType:   mt,
		DevAddr: DevAddr(raw[2:6]),
		FCnt:    readU32LE(raw[6:10]),
		ADR:     ctl&ctlADR != 0,
		ACK:     ctl&ctlACK != 0,
		HasPort: ctl&ctlFPort != 0,
		FPort:   raw[13],
	}
	optsLen := int(raw[10])
	payloadLen := int(readU16LE(raw[11:13]))
	end := 14 + optsLen + payloadLen
	if end+4 != len(raw) {
		return nil, ErrMalformed
	}
	f.FOpts = append([]byte(nil), raw[14:14+optsLen]...)
	down := f.IsDown()
	var mic [4]byte
	copy(mic[:], raw[end:end+4])
	if computeMIC(s.NwkSKey, f.DevAddr, down, f.FCnt, raw[:end]) != mic {
		return nil, ErrBadMIC
	}
	if f.DevAddr != s.DevAddr {
		return nil, ErrNoSession
	}
	if payloadLen > 0 {
		cipher := raw[14+optsLen : end]
		f.Payload = encryptPayload(payloadKey(s, f.FPort), f.DevAddr, down, f.FCnt, f.FPort, cipher)
	}
	return f, nil
}

// ParseLinkADRReq decodes a LinkADRReq MAC command (CID 0x03 down).
func ParseLinkADRReq(p []byte) (dr uint8, txPower int, err error) {
	if len(p) < 4 {
		return 0, 0, ErrMalformed
	}
	return p[0] >> 4, int(p[0] & 0x0f), nil
}

// MarshalLinkADRReq encodes a LinkADRReq command (without CID): 4 bytes,
// all channels enabled in ChMask.
func MarshalLinkADRReq(dr uint8, txPower int) []byte {
	return []byte{(dr << 4) | (byte(txPower) & 0x0f), 0xff, 0xff, 0x00}
}

// ParseLinkADRAns decodes a LinkADRAns uplink command.
func ParseLinkADRAns(p []byte) (ok bool, err error) {
	if len(p) < 1 {
		return false, ErrMalformed
	}
	return p[0] == 0x07, nil // channelmask-ack | data-rate-ack | tx-power-ack
}

// MarshalLinkADRAns encodes LinkADRAns status byte (without CID).
func MarshalLinkADRAns(rateOK, txOK, maskOK bool) []byte {
	var b byte
	if maskOK {
		b |= 0x01
	}
	if rateOK {
		b |= 0x02
	}
	if txOK {
		b |= 0x04
	}
	return []byte{b}
}

func payloadKey(s *Session, port byte) AESKey {
	if port == 0 {
		return s.NwkSKey
	}
	return s.AppSKey
}

func appendU16LE(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}

func appendU32LE(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func readU16LE(b []byte) uint16 {
	return uint16(b[0]) | uint16(b[1])<<8
}

func readU32LE(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
