package mac

import (
	"encoding/binary"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
)

// handleFrame is the server radio callback. It only classifies and enqueues
// frames onto the owning device's private channel; all stateful processing
// happens in that device's own goroutine.
func (s *NetworkServer) handleFrame(from radio.EndpointID, f radio.Frame) {
	if len(f.Payload) == 0 {
		s.log("warn", "server received empty frame; dropped")
		return
	}
	mtype := protocol.MType(f.Payload[0] >> 5)

	switch mtype {
	case protocol.MTJoinRequest:
		req, err := protocol.ParseJoinRequest(f.Payload)
		if err != nil {
			s.log("warn", "malformed join request dropped")
			return
		}
		s.dispatch(req.DevEUI, from, f)
	case protocol.MTUnconfirmedDataUp, protocol.MTConfirmedDataUp:
		// DevAddr -> DevEUI lookup; address bytes 4..1 wire are little
		// endian. The server allocated addresses are stored directly.
		var devAddr protocol.DevAddr
		copy(devAddr[:], f.Payload[1:5])
		for i, j := 0, 3; i < j; i, j = i+1, j-1 {
			devAddr[i], devAddr[j] = devAddr[j], devAddr[i]
		}
		devEUI, ok := s.deviceByAddr(devAddr)
		if !ok {
			s.log("warn", "uplink from unknown DevAddr dropped")
			return
		}
		s.dispatch(devEUI, from, f)
	default:
		s.log("warn", "server received unexpected MType; dropped")
	}
}

func (s *NetworkServer) dispatch(devEUI protocol.EUI64, from radio.EndpointID, f radio.Frame) {
	s.mu.RLock()
	sd, ok := s.devices[devEUI]
	started := s.started
	s.mu.RUnlock()
	if !ok {
		s.log("warn", "frame from unprovisioned device dropped")
		return
	}
	if !started {
		return
	}
	// Non-blocking enqueue: a device in retransmission storm that fills
	// its own 32-deep queue loses its excess frames but cannot affect any
	// other device or the shared dispatch path.
	select {
	case sd.inbound <- radioInbound{from: from, f: f, sentAt: f.SentAt}:
	default:
		s.log("warn", "device inbound queue full; frame dropped (storm isolation)")
	}
}

// deviceLoop is one device's isolated serial processing goroutine.
func (s *NetworkServer) deviceLoop(sd *serverDevice) {
	for in := range sd.inbound {
		mtype := protocol.MType(in.f.Payload[0] >> 5)
		switch mtype {
		case protocol.MTJoinRequest:
			s.processJoin(sd, in)
		case protocol.MTUnconfirmedDataUp, protocol.MTConfirmedDataUp:
			s.processUplink(sd, in)
		}
	}
}

func (s *NetworkServer) deviceByAddr(addr protocol.DevAddr) (protocol.EUI64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sd := range s.devices {
		sd.mu.Lock()
		match := sd.st.Joined && sd.st.DevAddr == addr
		eui := sd.st.DevEUI
		sd.mu.Unlock()
		if match {
			return eui, true
		}
	}
	return protocol.EUI64{}, false
}

// processJoin handles an OTAA Join-Request. A duplicate DevNonce is rejected
// by not responding (the device deterministically times out), and the event
// is logged. A fresh nonce produces a persisted Join-Accept in RX1.
func (s *NetworkServer) processJoin(sd *serverDevice, in radioInbound) {
	req, err := protocol.ParseJoinRequest(in.f.Payload)
	if err != nil {
		s.log("warn", "malformed join request dropped")
		return
	}
	sd.mu.Lock()
	provisioned := sd.provisioned
	joinEUI := sd.st.JoinEUI
	appKey := sd.st.AppKey
	sd.mu.Unlock()

	if !provisioned {
		s.log("warn", "join from unprovisioned device ignored")
		return
	}
	if req.JoinEUI != joinEUI {
		s.log("warn", "join JoinEUI mismatch ignored")
		return
	}
	if in.f.Corrupt || !protocol.VerifyJoinRequestMIC(appKey, in.f.Payload) {
		s.log("warn", "join request MIC failure; dropped")
		return
	}

	sd.mu.Lock()
	for _, used := range sd.st.UsedDevNonces {
		if used == req.DevNonce {
			sd.mu.Unlock()
			s.log("warn", "duplicate DevNonce rejected; no join accept sent")
			return
		}
	}
	appNonce, devAddr := s.nextIdentifiers()
	keys := deriveSessionKeys(appKey, appNonce, s.cfg.NetID, req.DevNonce)

	sd.st.Joined = true
	sd.st.DevAddr = devAddr
	sd.st.Keys = keys
	sd.st.UsedDevNonces = append(sd.st.UsedDevNonces, req.DevNonce)
	sd.st.NwkFCntUp = 0
	sd.st.NwkFCntDown = 0
	sd.st.LastDeliveredFCnt = 0
	sd.st.LastACKedFCnt = 0
	sd.upGuard = newFrameGuard(s.cfg.MaxFCntGap)
	if err := s.persistDeviceLocked(sd); err != nil {
		sd.mu.Unlock()
		s.log("warn", "join state persistence failed: "+err.Error())
		return
	}

	acc := protocol.JoinAccept{
		AppNonce: appNonce,
		NetID:    s.cfg.NetID,
		DevAddr:  devAddr,
		RXDelay:  rxDelayField(s.cfg.RX1Delay),
	}
	raw, err := protocol.MarshalJoinAccept(appKey, acc)
	sd.mu.Unlock()
	if err != nil {
		s.log("warn", "join accept marshal failed")
		return
	}

	s.scheduleDownlink(in.from, in.sentAt, raw, 0, false)
	s.log("info", "join accepted and session persisted")
}

// processUplink validates MIC/counters, deduplicates, delivers application
// payload at most once, answers confirmed frames exactly once and schedules
// any pending downlink/LinkADR into the current receive windows.
func (s *NetworkServer) processUplink(sd *serverDevice, in radioInbound) {
	sd.mu.Lock()
	if !sd.st.Joined {
		sd.mu.Unlock()
		s.log("warn", "uplink from non-joined device dropped")
		return
	}
	if in.f.Corrupt {
		sd.mu.Unlock()
		s.log("warn", "corrupt uplink dropped before parse")
		return
	}
	keys := sd.st.Keys
	wireFC := binary.LittleEndian.Uint16(in.f.Payload[6:8])
	cands := counterCandidatesWithReplay(sd.upGuard.lastAccepted, wireFC)

	var dec *protocol.DecodedFrame
	var fcnt32 uint32
	for _, cand := range cands {
		parsed, err := protocol.ParseDataFrame(keys, false, cand, in.f.Payload)
		if err != nil {
			sd.mu.Unlock()
			s.log("warn", "malformed uplink dropped: "+err.Error())
			return
		}
		if parsed.MICValid {
			dec, fcnt32 = parsed, cand
			break
		}
	}
	if dec == nil {
		sd.mu.Unlock()
		s.log("warn", "uplink MIC verification failed; dropped")
		return
	}

	isReplay := fcnt32 <= sd.upGuard.lastAccepted
	confirmed := dec.MType == protocol.MTConfirmedDataUp

	if isReplay {
		// Retransmission/replay: never deliver to the application again.
		// A confirmed retransmission of the LAST frame still gets exactly
		// one ACK behavior overall: if it was already ACKed we re-send the
		// ACK (idempotent) but never redeliver data.
		if confirmed && fcnt32 == sd.st.LastACKedFCnt {
			ackFrame := s.buildDownlinkLocked(sd, protocol.MTUnconfirmedDataDN, true, nil, 0)
			if ackFrame != nil {
				target := in.from
				sentAt := in.sentAt
				dr := in.f.DataRate
				rx2 := sd.rx2Fallback
				sd.mu.Unlock()
				s.scheduleDownlink(target, sentAt, ackFrame, dr, rx2)
				return
			}
			sd.mu.Unlock()
			return
		}
		sd.mu.Unlock()
		s.log("warn", "replayed uplink counter; data not redelivered")
		return
	}
	if fcnt32-sd.upGuard.lastAccepted > sd.upGuard.maxGap && sd.upGuard.lastAccepted != 0 {
		sd.mu.Unlock()
		s.log("warn", "uplink counter outside replay window; dropped")
		return
	}

	// Fresh frame: advance counters and persist before delivery.
	sd.upGuard.lastAccepted = fcnt32
	sd.st.NwkFCntUp = fcnt32
	if confirmed {
		sd.st.LastACKedFCnt = fcnt32
	}
	sd.st.LastDeliveredFCnt = fcnt32
	// If the uplink acknowledges a confirmed downlink, record the ACK.
	if dec.FCtrl.ACK && sd.pendingConfirmedDown != 0 {
		sd.st.LastDownAckedFCnt = sd.pendingConfirmedDown
		sd.pendingConfirmedDown = 0
		sd.st.PendingConfirmedDown = 0
	}

	var downRaw []byte
	rx2Fallback := false
	hasDown := len(sd.downQueue) > 0 || sd.adrArmed || confirmed
	if hasDown {
		rx2Fallback = sd.rx2Fallback
		mtype := protocol.MTUnconfirmedDataDN
		var pending *PendingDown
		if len(sd.downQueue) > 0 {
			pending = &sd.downQueue[0]
			if pending.Confirmed {
				mtype = protocol.MTConfirmedDataDN
			}
		}
		downRaw = s.buildDownlinkLocked(sd, mtype, confirmed, pending, fcnt32)
		if pending != nil {
			sd.downQueue = sd.downQueue[1:]
		}
		sd.st.DownQueue = append([]PendingDown(nil), sd.downQueue...)
	}

	onUp := s.cfg.OnUp
	port := dec.FPort
	payload := dec.FRMPayload
	if err := s.persistDeviceLocked(sd); err != nil {
		s.log("warn", "uplink state persistence failed: "+err.Error())
	}
	sd.mu.Unlock()

	// Deliver to the application exactly once, outside the device lock so
	// a slow application cannot block the server.
	if dec.FPort > 0 && len(dec.FRMPayload) > 0 && onUp != nil {
		buf := append([]byte(nil), payload...)
		onUp(sd.st.DevEUI, uint8(port), buf)
	}

	if downRaw != nil {
		s.scheduleDownlink(in.from, in.sentAt, downRaw, in.f.DataRate, rx2Fallback)
	}
}

// buildDownlinkLocked constructs the downlink frame for the current receive
// window: ACK flag, optional LinkADRReq and optional application payload.
// Caller holds sd.mu.
func (s *NetworkServer) buildDownlinkLocked(sd *serverDevice, mtype protocol.MType,
	ack bool, pending *PendingDown, upFCnt uint32) []byte {
	f := protocol.DataFrame{
		MType:   mtype,
		DevAddr: sd.st.DevAddr,
		FCtrl: protocol.FCtrl{
			ACK: ack,
		},
		FPort: -1,
	}

	if sd.adrArmed {
		req := protocol.LinkADRReq{DataRate: sd.adrDR, TXPower: txPowerToIndex(sd.adrTP, s.cfg.ADRConfig), ChMask: 1}
		f.FOpts = append(f.FOpts, protocol.MACCommand{CID: protocol.CIDLinkADRReq, Payload: protocol.EncodeLinkADRReq(req)})
		sd.adrArmed = false
	}

	if pending != nil {
		f.FPort = int(pending.FPort)
		f.FRMPayload = pending.Payload
	}

	if sd.st.NwkFCntDown == ^uint32(0) {
		s.log("warn", "downlink counter exhausted; frame not sent")
		return nil
	}
	sd.st.NwkFCntDown++
	f.FCnt = uint16(sd.st.NwkFCntDown)
	if mtype == protocol.MTConfirmedDataDN {
		sd.pendingConfirmedDown = sd.st.NwkFCntDown
		sd.st.PendingConfirmedDown = sd.pendingConfirmedDown
	}
	raw, err := protocol.MarshalDataFrame(sd.st.Keys, f, sd.st.NwkFCntDown)
	if err != nil {
		s.log("warn", "downlink marshal failed")
		return nil
	}
	return raw
}

// scheduleDownlink emits one server frame into the Class-A receive windows
// following an uplink. By default the frame is scheduled at the RX1 offset
// with the uplink data rate. When the space loss hook drops the RX1 copy
// the device's RX2 window stays open and a second copy (RX2 offset, RX2
// data rate) can be enabled per device via SetRX2Fallback; the device
// decides acceptance solely from the arrival timestamp, so duplicate
// delivery is impossible (the counter/MIC reject any second acceptance).
func (s *NetworkServer) scheduleDownlink(target radio.EndpointID, uplinkSentAt time.Time,
	raw []byte, upDR uint8, alsoRX2 bool) {
	s.cfg.Space.TransmitAt(uplinkSentAt.Add(s.cfg.RX1Delay), s.cfg.Endpoint, target,
		radio.Frame{Payload: raw, DataRate: upDR})
	if alsoRX2 {
		s.cfg.Space.TransmitAt(uplinkSentAt.Add(s.cfg.RX2Delay), s.cfg.Endpoint, target,
			radio.Frame{Payload: raw, DataRate: s.cfg.RX2DataRate})
	}
}

// ScheduleRX2FallbackFor is used by tests/demo to additionally schedule the
// RX2 copy for a device after the next uplink.
func (s *NetworkServer) ScheduleRX2FallbackFor(devEUI protocol.EUI64, enabled bool) error {
	s.mu.RLock()
	sd, ok := s.devices[devEUI]
	s.mu.RUnlock()
	if !ok {
		return ErrUnknownDevice
	}
	sd.mu.Lock()
	sd.rx2Fallback = enabled
	sd.mu.Unlock()
	return nil
}

// rxDelayField maps a duration to the LoRaWAN RXDelay nibble (1..15
// seconds); larger values saturate at 15.
func rxDelayField(d time.Duration) byte {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	if secs > 15 {
		secs = 15
	}
	return byte(secs)
}
