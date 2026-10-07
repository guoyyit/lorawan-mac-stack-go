package server

import (
	"context"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

func (s *Server) handleData(st *peerState, raw []byte) {
	st.mu.Lock()
	sess := st.session
	st.mu.Unlock()
	if sess == nil {
		s.countBad(st)
		return
	}

	f, err := lorawan.DecodeFrame(sess, raw)
	if err != nil {
		// Bad MIC / malformed: drop and record, counters untouched.
		s.countBad(st)
		return
	}

	var appPayload []byte
	var downRaw []byte

	st.mu.Lock()
	fresh := false
	switch err := st.session.AcceptUp(f.FCnt); err {
	case nil:
		fresh = true
		processUpMAC(st, f.FOpts)
		if len(f.Payload) > 0 {
			appPayload = append([]byte(nil), f.Payload...)
		}
	case lorawan.ErrDuplicate:
		// Retransmission: the frame was already accepted; no application
		// redelivery. The ACK is rebuilt below as an empty downlink.
	default:
		st.badFrames++
		st.mu.Unlock()
		return
	}
	// Every accepted fresh uplink advances FCntUp and must be durable before
	// we ACK, so a crash after ACK cannot make the device's counter look stale.
	if fresh {
		if err := s.cfg.Store.Save(context.Background(), lorawan.EUIHex(st.devEUI), st.session); err != nil {
			st.mu.Unlock()
			return
		}
	}

	down, ok := s.prepareDownLocked(st, f)
	if ok {
		var err error
		downRaw, err = lorawan.EncodeFrame(st.session, down)
		if err != nil {
			downRaw = nil
		}
	}
	st.mu.Unlock()

	if appPayload != nil && s.cfg.OnAppData != nil {
		s.cfg.OnAppData(st.devEUI, appPayload)
	}

	if downRaw != nil {
		// ACK/downlink is deterministically delivered in RX1.
		s.scheduleAt(func() {
			_ = s.cfg.Air.Send(context.Background(), downRaw)
		}, s.cfg.RX1Delay)
	}
}

// prepareDownLocked builds the single downlink for the current windows.
// It pops at most one queued Class A downlink. Called with st.mu held.
func (s *Server) prepareDownLocked(st *peerState, up *lorawan.Frame) (*lorawan.Frame, bool) {
	needACK := up.MType == lorawan.MTypeConfirmedDataUp
	hasQueue := len(st.downQ) > 0
	hasADR := st.linkADR != nil
	if !needACK && !hasQueue && !hasADR {
		return nil, false
	}

	// Stage the variable fields and validate the local size cap BEFORE any
	// queue pop or counter reservation, so an oversize combination has no
	// side effects. If the queued payload plus a pending LinkADRReq would
	// overflow the frame, send the MAC command alone in this window and keep
	// the payload queued for the next one (the ACK is still sent); neither a
	// counter nor a queue item is lost to the length rejection.
	var nextPayload []byte
	var opts []byte
	if hasQueue {
		nextPayload = st.downQ[0].payload
	}
	if hasADR {
		opts = append(opts, lorawan.CIDLinkADRReq)
		opts = append(opts, lorawan.MarshalLinkADRReq(st.linkADR.dr, st.linkADR.txPower)...)
	}
	sendPayload := hasQueue
	if len(opts)+len(nextPayload) > lorawan.MaxFrameBody-lorawan.MaxFrameHeader {
		if hasADR {
			nextPayload = nil
			sendPayload = false
		} else {
			// Payload alone is oversize (EnqueueDown normally rejects this);
			// do not pop it or reserve a counter.
			return nil, false
		}
	}

	down := &lorawan.Frame{
		DevAddr: st.session.DevAddr,
		ACK:     needACK,
	}
	content := false
	if sendPayload {
		first := st.downQ[0]
		st.downQ = st.downQ[1:]
		if first.confirmed {
			down.MType = lorawan.MTypeConfirmedDataDown
		} else {
			down.MType = lorawan.MTypeUnconfirmedDataDown
		}
		down.HasPort = true
		down.FPort = 1
		down.Payload = append([]byte(nil), nextPayload...)
		content = true
	} else {
		down.MType = lorawan.MTypeUnconfirmedDataDown
	}
	if hasADR {
		down.FOpts = opts
		st.linkADR = nil
		content = true
	}

	if content {
		// Content-bearing downlinks consume the monotonic downlink counter.
		fcnt, err := st.session.ReserveDown()
		if err != nil {
			// Down counter exhausted: skip the downlink; rejoin required.
			return nil, false
		}
		down.FCnt = fcnt
		if err := s.cfg.Store.Save(context.Background(), lorawan.EUIHex(st.devEUI), st.session); err != nil {
			return nil, false
		}
	}
	// A pure ACK (no payload, no MAC command) is a zero-counter control
	// frame; it can be repeated for retransmissions without using counters.
	return down, true
}
