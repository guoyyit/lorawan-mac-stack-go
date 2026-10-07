// Package server implements a LoRaWAN network server for Class A devices.
package server

import (
	"context"
	"sync"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/transport"
)

// DeviceProvision is the server-side provisioning record for one device.
type DeviceProvision struct {
	DevEUI  lorawan.EUI
	JoinEUI lorawan.EUI
	AppKey  lorawan.AESKey
}

// AppHandler receives de-duplicated uplink application payloads.
type AppHandler func(devEUI lorawan.EUI, payload []byte)

// Config configures the network Server.
type Config struct {
	NetID lorawan.NetID
	Store lorawan.Store
	Air   *transport.Endpoint

	Devices   []DeviceProvision
	OnAppData AppHandler

	RX1Delay time.Duration
	RX2Delay time.Duration
	RXWindow time.Duration

	// JoinNonceBase lets multiple servers produce non-overlapping nonces.
	JoinNonceBase uint32

	// QueueLimit bounds buffered frames per device; a storm cannot exhaust
	// server memory or block other devices.
	QueueLimit int
}

// Server is the network-side MAC stack.
type Server struct {
	cfg Config

	mu       sync.RWMutex
	sessions map[string]*peerState // keyed by DevEUI hex

	byAddr map[lorawan.DevAddr]string

	provs map[string]DeviceProvision

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// peerState is one device's isolated processing context.
type peerState struct {
	devEUI lorawan.EUI

	mu      sync.Mutex
	session *lorawan.Session

	inbox chan []byte // raw frames, capacity QueueLimit

	// downQ holds Class A downlinks awaiting the next receive windows.
	downQ []queuedDown
	// linkADR, when set, is attached to the next downlink as FOpts.
	linkADR *adrCommand

	// stats
	droppedQueue int64
	badFrames    int64

	// adrAcks counts accepted LinkADRAns responses.
	adrAcks int64
}

type queuedDown struct {
	confirmed bool
	payload   []byte
}

type adrCommand struct {
	dr      uint8
	txPower int
}

// New constructs a server and starts serving frames from Air.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Air == nil {
		return nil, errConfig
	}
	if cfg.RX1Delay <= 0 {
		cfg.RX1Delay = time.Second
	}
	if cfg.RX2Delay <= 0 {
		cfg.RX2Delay = 2 * time.Second
	}
	if cfg.RXWindow <= 0 {
		cfg.RXWindow = time.Second
	}
	if cfg.QueueLimit <= 0 {
		cfg.QueueLimit = 64
	}

	s := &Server{
		cfg:      cfg,
		sessions: make(map[string]*peerState),
		byAddr:   make(map[lorawan.DevAddr]string),
		provs:    make(map[string]DeviceProvision),
		closed:   make(chan struct{}),
	}

	ctx := context.Background()
	for _, p := range cfg.Devices {
		id := lorawan.EUIHex(p.DevEUI)
		s.provs[id] = p
		st := &peerState{
			devEUI: p.DevEUI,
			inbox:  make(chan []byte, cfg.QueueLimit),
		}
		if sess, err := cfg.Store.Load(ctx, id); err == nil {
			st.session = sess
			s.byAddr[sess.DevAddr] = id
		} else if err != nil && err != lorawan.ErrNotFound {
			return nil, err
		}
		s.sessions[id] = st
	}

	cfg.Air.SetHandler(s.onAir)
	for _, st := range s.sessions {
		s.wg.Add(1)
		go s.peerLoop(st)
	}
	return s, nil
}

// onAir dispatches a raw frame to the owning device's inbox. It never blocks:
// an overloaded device's frames are dropped at its own queue boundary, and it
// never panics: an oversize or malformed frame is discarded at the edge so a
// hostile device cannot exhaust memory or crash the dispatcher (which serves
// every device), and no other device's traffic is affected.
func (s *Server) onAir(rawIn []byte) {
	defer func() { _ = recover() }()
	// Hard edge bound: no legal frame in the local format can exceed this;
	// reject before copying or consulting length fields, so an advertised
	// multi-megabyte frame cannot drive a matching allocation.
	if len(rawIn) == 0 || len(rawIn) > lorawan.MaxFrameLength {
		return
	}
	raw := append([]byte(nil), rawIn...)
	id := s.route(raw)
	if id == "" {
		return
	}
	st := s.sessions[id]
	if st == nil {
		return
	}
	select {
	case st.inbox <- raw:
	default:
		st.mu.Lock()
		st.droppedQueue++
		st.mu.Unlock()
	}
}

// route determines the device id from a raw frame without mutating state:
// join requests carry DevEUI; data frames carry DevAddr.
func (s *Server) route(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	switch lorawan.MType(raw[0]) {
	case lorawan.MTypeJoinRequest:
		if len(raw) < 17 {
			return ""
		}
		var dev lorawan.EUI
		copy(dev[:], raw[9:17])
		id := lorawan.EUIHex(dev)
		if _, ok := s.provs[id]; !ok {
			return ""
		}
		return id
	default:
		if len(raw) < 6 {
			return ""
		}
		var addr lorawan.DevAddr
		copy(addr[:], raw[2:6])
		s.mu.RLock()
		id := s.byAddr[addr]
		s.mu.RUnlock()
		return id
	}
}

// Session returns a copy of the persisted session for a device.
func (s *Server) Session(devEUI lorawan.EUI) (lorawan.Session, error) {
	st := s.sessions[lorawan.EUIHex(devEUI)]
	if st == nil {
		return lorawan.Session{}, lorawan.ErrUnknownDevice
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.session == nil {
		return lorawan.Session{}, lorawan.ErrNoSession
	}
	return *st.session.Clone(), nil
}

// Close drains workers and releases resources.
func (s *Server) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	s.wg.Wait()
	return nil
}
