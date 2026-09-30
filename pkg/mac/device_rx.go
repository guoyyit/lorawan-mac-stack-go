package mac

import (
	"encoding/binary"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
)

// handleFrame is the radio receive callback (join accepts and downlinks).
func (d *EndDevice) handleFrame(from radio.EndpointID, f radio.Frame) {
	if len(f.Payload) == 0 {
		d.log("warn", "device received empty frame; dropped")
		return
	}
	mtype := protocol.MType(f.Payload[0] >> 5)
	switch mtype {
	case protocol.MTJoinAccept:
		d.handleJoinAccept(f)
	case protocol.MTUnconfirmedDataDN, protocol.MTConfirmedDataDN:
		d.handleDownlink(f)
	default:
		d.log("warn", "device received unexpected MType; dropped")
	}
}

// handleDownlink processes a server frame: it enforces receive-window
// timing, MIC integrity, counter replay protection and application
// delivery, and resolves any pending confirmed-uplink ACK.
//
// All failure modes are handled by dropping the frame and logging it: a
// corrupt, late or replayed frame never mutates session state.
func (d *EndDevice) handleDownlink(f radio.Frame) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.session.Joined {
		d.log("warn", "downlink before join completed; dropped")
		return
	}
	if !d.inReceiveWindow(f.ReceivedAt) {
		d.log("warn", "downlink missed RX1/RX2 windows; dropped")
		return
	}
	if f.Corrupt {
		d.log("warn", "corrupt downlink dropped before parse")
		return
	}

	// MIC verification requires the recovered full counter, but we do not
	// know it before parsing the wire FCnt. Decrypt with a provisional
	// counter, validate MIC with every candidate (cheap: two window
	// candidates at most), then apply replay rules.
	wireFC := binary.LittleEndian.Uint16(f.Payload[6:8])
	candidates := counterCandidatesWithReplay(d.downGuard.lastAccepted, wireFC)
	var dec *protocol.DecodedFrame
	var fcnt32 uint32
	for _, cand := range candidates {
		parsed, err := protocol.ParseDataFrame(d.session.Keys, true, cand, f.Payload)
		if err != nil {
			d.log("warn", "malformed downlink dropped: "+err.Error())
			return
		}
		if parsed.MICValid {
			dec, fcnt32 = parsed, cand
			break
		}
	}
	if dec == nil {
		d.log("warn", "downlink MIC verification failed; dropped")
		return
	}

	if fcnt32 <= d.downGuard.lastAccepted {
		d.log("warn", "replayed/duplicate downlink counter; dropped")
		d.resolveACKLocked(dec.FCtrl.ACK)
		return
	}
	if fcnt32-d.downGuard.lastAccepted > d.downGuard.maxGap {
		d.log("warn", "downlink counter outside replay window; dropped")
		return
	}

	d.downGuard.lastAccepted = fcnt32
	d.session.NwkFCntDown = fcnt32
	if dec.MType == protocol.MTConfirmedDataDN {
		// A confirmed downlink is acknowledged via the ACK bit on the next
		// uplink (LoRaWAN rule), not with a separate frame.
		d.ackDownFC = fcnt32
	}

	// Process piggybacked MAC commands before delivering payload.
	for _, cmd := range dec.FOpts {
		d.handleDownCommandLocked(cmd)
	}

	acked := dec.FCtrl.ACK
	d.resolveACKLocked(acked)

	if dec.FPort > 0 && len(dec.FRMPayload) > 0 && d.cfg.OnDown != nil {
		payload := append([]byte(nil), dec.FRMPayload...)
		port := uint8(dec.FPort)
		d.cfg.OnDown(port, payload)
	}

	if err := d.persistLocked(); err != nil {
		d.log("warn", "session persistence failed: "+err.Error())
	}
}

func (d *EndDevice) resolveACKLocked(acked bool) {
	if acked && d.pendingFC != 0 {
		d.session.LastUpAcked = d.pendingFC
		d.pendingFC = 0
		ackCh := d.ackCh
		d.ackCh = nil
		if ackCh != nil {
			select {
			case ackCh <- struct{}{}:
			default:
			}
		}
	}
}

// handleDownCommandLocked applies server MAC commands. LinkADR changes
// rate/power only when the device accepts all answer bits; rejection leaves
// join state and frame counters untouched.
func (d *EndDevice) handleDownCommandLocked(cmd protocol.MACCommand) {
	switch cmd.CID {
	case protocol.CIDLinkADRReq:
		req, err := protocol.DecodeLinkADRReq(cmd.Payload)
		if err != nil {
			d.log("warn", "malformed LinkADRReq ignored")
			return
		}
		ans := d.adr.ApplyLinkADR(req)
		if ans.PowerACK && ans.DataRateACK && ans.ChannelMaskACK {
			d.session.ADR = d.adr.Snapshot()
			d.log("info", "ADR command applied")
		} else {
			d.log("warn", "ADR command rejected by device")
		}
	}
}

// counterCandidates returns the strictly-increasing 32-bit counters whose
// low 16 bits equal low16 near lastAccepted (at most two: same high half
// or high half + 1). Values <= lastAccepted are excluded.
func counterCandidates(lastAccepted uint32, low16 uint16) []uint32 {
	var out []uint32
	high := uint32(lastAccepted>>16) << 16
	cand := high + uint32(low16)
	if cand <= lastAccepted {
		cand += 1 << 16
	}
	if cand > lastAccepted {
		out = append(out, cand)
	}
	return out
}
