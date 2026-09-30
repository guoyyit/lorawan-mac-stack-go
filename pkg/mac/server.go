package mac

import (
	"sync"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
)

// NetworkServer serves many Class-A devices concurrently. Per-device state
// is isolated behind its own lock and its own inbound queue/goroutine, so
// one device's retransmission storm or a stuck handler can never block
// another device's join or traffic.
type NetworkServer struct {
	cfg ServerConfig

	mu      sync.RWMutex
	metaMu  sync.Mutex
	devices map[protocol.EUI64]*serverDevice

	started bool
}

// serverDevice is the server-side per-device state. The inbound channel is
// unbuffered with per-frame non-blocking dispatch, so a flooded device
// drops its own excess frames rather than stalling the shared server.
type serverDevice struct {
	mu sync.Mutex

	st          ServerDeviceState
	provisioned bool
	upGuard     frameGuard

	downQueue   []PendingDown
	adrArmed    bool
	adrDR       uint8
	adrTP       int
	rx2Fallback bool
	// pendingConfirmedDown is the FCnt of the confirmed downlink awaiting
	// the device ACK bit on the next uplink (0 = none pending).
	pendingConfirmedDown uint32

	inbound chan radioInbound
}

type radioInbound struct {
	from   radio.EndpointID
	f      radio.Frame
	sentAt time.Time
}

// NewNetworkServer validates the configuration and builds the server.
// Device records provisioned earlier and found in the store are restored.
func NewNetworkServer(cfg ServerConfig) (*NetworkServer, error) {
	cfg = DefaultServerConfig(cfg)
	if cfg.Store == nil {
		return nil, ErrInvalidConfig
	}
	s := &NetworkServer{
		cfg:     cfg,
		devices: make(map[protocol.EUI64]*serverDevice),
	}

	// Restore previously provisioned/joined devices from persistent state.
	keys, err := cfg.Store.Keys()
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		var state ServerDeviceState
		if err := cfg.Store.Load(k, &state); err != nil {
			continue
		}
		if !isServerStateKey(k, state.DevEUI) {
			continue
		}
		sd := s.newDeviceLocked(state, true)
		s.devices[state.DevEUI] = sd
		if s.started {
			go s.deviceLoop(sd)
		}
	}
	return s, nil
}

// Start registers the server endpoint on the air interface and starts
// per-device processing loops for restored devices.
func (s *NetworkServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	s.started = true
	s.cfg.Space.Register(s.cfg.Endpoint, s.handleFrame)
	for _, sd := range s.devices {
		go s.deviceLoop(sd)
	}
	return nil
}

// Close flushes state and removes the endpoint.
func (s *NetworkServer) Close() error {
	s.cfg.Space.Unregister(s.cfg.Endpoint)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = false
	return nil
}

// DeviceJoined reports provisioning/session status for a device.
func (s *NetworkServer) DeviceJoined(devEUI protocol.EUI64) (bool, error) {
	s.mu.RLock()
	sd, ok := s.devices[devEUI]
	s.mu.RUnlock()
	if !ok {
		return false, ErrUnknownDevice
	}
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.st.Joined, nil
}

// DeviceSnapshot returns a copy of a device's persisted server state.
func (s *NetworkServer) DeviceSnapshot(devEUI protocol.EUI64) (ServerDeviceState, error) {
	s.mu.RLock()
	sd, ok := s.devices[devEUI]
	s.mu.RUnlock()
	if !ok {
		return ServerDeviceState{}, ErrUnknownDevice
	}
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.st, nil
}

func (s *NetworkServer) log(level, msg string) {
	if s.cfg.Logger != nil {
		s.cfg.Logger(level, msg)
	}
}
