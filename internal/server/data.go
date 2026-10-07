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

	down := &lorawan.Frame{
		DevAddr: st.session.DevAddr,
		ACK:     needACK,
	}
	content := false
	if hasQueue {
		first := st.downQ[0]
		st.downQ = st.downQ[1:]
		if first.confirmed {
			down.MType = lorawan.MTypeConfirmedDataDown
		} else {
			down.MType = lorawan.MTypeUnconfirmedDataDown
		}
		down.HasPort = true
		down.FPort = 1
		down.Payload = append([]byte(nil), first.payload...)
		content = true
	} else {
		down.MType = lorawan.MTypeUnconfirmedDataDown
	}
	if hasADR {
		fopts := append([]byte{lorawan.CIDLinkADRReq},
			lorawan.MarshalLinkADRReq(st.linkADR.dr, st.linkADR.txPower)...)
		// Attach the MAC command only if the whole frame still fits; a
		// (near-)max-size queued payload takes precedence and the command
		// stays pending for the next downlink instead of being dropped
		// together with the payload it would have unencoded.
		if lorawan.ValidateBodySize(len(fopts), len(down.Payload)) == nil {
			down.FOpts = fopts
			st.linkADR = nil
			content = true
		}
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
