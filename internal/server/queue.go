package server

import (
	"errors"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
)

// EnqueueDown queues application payload for a device. Class A devices only
// listen after an uplink, so the payload is delivered in the RX1 window of
// the next received uplink. If no uplink follows, the item stays queued
// (documented "window missed" behavior). Queueing is FIFO and bounded.
func (s *Server) EnqueueDown(devEUI lorawan.EUI, payload []byte) error {
	return s.enqueue(devEUI, queuedDown{confirmed: false, payload: payload})
}

// EnqueueConfirmedDown queues a confirmed downlink.
func (s *Server) EnqueueConfirmedDown(devEUI lorawan.EUI, payload []byte) error {
	return s.enqueue(devEUI, queuedDown{confirmed: true, payload: payload})
}

func (s *Server) enqueue(devEUI lorawan.EUI, q queuedDown) error {
	// Reject oversize payloads at the API boundary with a distinguishable
	// error: nothing is queued, no counter is reserved, no state changes.
	if len(q.payload) > lorawan.MaxPayload {
		return lorawan.ErrFrameTooLong
	}
	st := s.sessions[lorawan.EUIHex(devEUI)]
	if st == nil {
		return lorawan.ErrUnknownDevice
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.downQ) >= s.cfg.QueueLimit {
		return errors.New("server: downlink queue full")
	}
	st.downQ = append(st.downQ, q)
	return nil
}

// SetLinkADR commands the device to change DR/TXPower; the command is carried
// in the next Class A downlink (piggybacked on an ACK if one is needed).
func (s *Server) SetLinkADR(devEUI lorawan.EUI, dr uint8, txPower int) error {
	st := s.sessions[lorawan.EUIHex(devEUI)]
	if st == nil {
		return lorawan.ErrUnknownDevice
	}
	st.mu.Lock()
	st.linkADR = &adrCommand{dr: dr, txPower: txPower}
	st.mu.Unlock()
	return nil
}

// Stats reports dropped (queue-saturated) and invalid frame counters.
func (s *Server) Stats(devEUI lorawan.EUI) (dropped, bad int64, ok bool) {
	st := s.sessions[lorawan.EUIHex(devEUI)]
	if st == nil {
		return 0, 0, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.droppedQueue, st.badFrames, true
}

// ADRACKCount reports accepted LinkADRAns responses for a device.
func (s *Server) ADRACKCount(devEUI lorawan.EUI) (int64, bool) {
	st := s.sessions[lorawan.EUIHex(devEUI)]
	if st == nil {
		return 0, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.adrAcks, true
}
