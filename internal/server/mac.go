package server

import "github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"

// processUpMAC parses uplink MAC commands. LinkADRReq is 5 bytes on the down
// link; its uplink answer LinkADRAns is 2 bytes (CID + status).
func processUpMAC(st *peerState, opts []byte) {
	for len(opts) >= 2 {
		cid := opts[0]
		switch cid {
		case lorawan.CIDLinkADRAns:
			if ok, err := lorawan.ParseLinkADRAns(opts[1:2]); err == nil && ok {
				st.adrAcks++
			}
			opts = opts[2:]
		default:
			// Unknown/unsupported command: skip the CID byte defensively.
			opts = opts[1:]
		}
	}
}
