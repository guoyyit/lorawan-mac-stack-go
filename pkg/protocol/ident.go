package protocol

import "encoding/hex"

// String renders an EUI-64 as colon-separated hexadecimal octets.
func (e EUI64) String() string {
	s := hex.EncodeToString(e[:])
	out := make([]byte, 0, len(s)+7)
	for i := 0; i < len(s); i += 2 {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, s[i], s[i+1])
	}
	return string(out)
}

// String renders a DevAddr as 0x-prefixed hexadecimal (little-endian wire
// order is preserved by the callers; this prints the stored bytes).
func (a DevAddr) String() string { return hex.EncodeToString(a[:]) }
