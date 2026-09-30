package server

import (
	"context"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

// peerLoop serializes all processing for one device, so no per-device lock
// contention and no cross-device blocking.
func (s *Server) peerLoop(st *peerState) {
	defer s.wg.Done()
	for {
		select {
		case <-s.closed:
			return
		case raw := <-st.inbox:
			s.handleFrame(st, raw)
		}
	}
}

func (s *Server) handleFrame(st *peerState, raw []byte) {
	if len(raw) == 0 {
		return
	}
	switch lorawan.MType(raw[0]) {
	case lorawan.MTypeJoinRequest:
		s.handleJoin(st, raw)
	default:
		s.handleData(st, raw)
	}
}

func (s *Server) handleJoin(st *peerState, raw []byte) {
	prov := s.provs[lorawan.EUIHex(st.devEUI)]

	st.mu.Lock()
	var prevNonce uint16
	var joinCounter uint32
	if st.session != nil {
		prevNonce = st.session.LastDevNonce
		joinCounter = st.session.JoinNonceCounter
	}
	st.mu.Unlock()

	jr, err := lorawan.DecodeJoinRequest(prov.AppKey, raw)
	if err != nil {
		s.countBad(st)
		return
	}
	if jr.DevEUI != st.devEUI || jr.JoinEUI != prov.JoinEUI {
		s.countBad(st)
		return
	}
	// DevNonce must be strictly greater than the last accepted one. Reuse is
	// rejected deterministically and no join-accept is ever sent.
	if uint16(jr.DevNonce) <= prevNonce {
		s.countBad(st)
		return
	}

	joinCounter++
	jn := encodeJoinNonce(s.cfg.JoinNonceBase + joinCounter)
	devAddr := s.allocateAddr(st.devEUI, joinCounter)

	ja := &lorawan.JoinAccept{
		JoinNonce: jn,
		NetID:     s.cfg.NetID,
		DevAddr:   devAddr,
		RX2DR:     0,
		RXDelay:   0, // field 0 means default 1s; device uses config durations
	}
	out, err := lorawan.EncodeJoinAccept(prov.AppKey, ja)
	if err != nil {
		return
	}

	nwk, app := lorawan.DeriveSessionKeys(prov.AppKey, jn, s.cfg.NetID, jr.DevNonce)
	sess := lorawan.NewSession(st.devEUI, prov.JoinEUI, prov.AppKey)
	sess.NwkSKey, sess.AppSKey = nwk, app
	sess.DevAddr, sess.NetID, sess.JoinNonce = devAddr, s.cfg.NetID, jn
	sess.Joined = true
	sess.LastDevNonce = uint16(jr.DevNonce)
	sess.JoinNonceCounter = joinCounter
	sess.UpdatedAt = time.Now()

	// Persist the new session before responding: a crash after accept but
	// before delivery simply requires the device to join once more; the server
	// never forgets the keys it used.
	if err := s.cfg.Store.Save(context.Background(), lorawan.EUIHex(st.devEUI), sess); err != nil {
		return
	}

	st.mu.Lock()
	oldAddr := lorawan.DevAddr{}
	if st.session != nil {
		oldAddr = st.session.DevAddr
	}
	st.session = sess
	st.downQ = nil
	st.linkADR = nil
	st.mu.Unlock()

	s.mu.Lock()
	if oldAddr != (lorawan.DevAddr{}) && oldAddr != devAddr {
		delete(s.byAddr, oldAddr)
	}
	s.byAddr[devAddr] = lorawan.EUIHex(st.devEUI)
	s.mu.Unlock()

	// Join-accept is sent in RX1 (deterministic window choice).
	s.scheduleAt(func() {
		_ = s.cfg.Air.Send(context.Background(), out)
	}, s.cfg.RX1Delay)
}

func (s *Server) countBad(st *peerState) {
	st.mu.Lock()
	st.badFrames++
	st.mu.Unlock()
}

// allocateAddr is deterministic: NetID prefix plus a stable per-device
// rolling suffix.
func (s *Server) allocateAddr(dev lorawan.EUI, counter uint32) lorawan.DevAddr {
	var addr lorawan.DevAddr
	addr[0] = s.cfg.NetID[0]
	addr[1] = dev[6] ^ byte(counter)
	addr[2] = dev[7]
	addr[3] = byte(counter >> 8)
	return addr
}

func encodeJoinNonce(v uint32) lorawan.JoinNonce {
	var jn lorawan.JoinNonce
	jn[0] = byte(v)
	jn[1] = byte(v >> 8)
	jn[2] = byte(v >> 16)
	return jn
}

// scheduleAt runs fn after d. Timers are not tracked across Close beyond
// process lifetime; sends after close are no-ops at the transport.
func (s *Server) scheduleAt(fn func(), d time.Duration) {
	t := time.NewTimer(d)
	go func() {
		select {
		case <-s.closed:
			t.Stop()
		case <-t.C:
			fn()
		}
	}()
}
