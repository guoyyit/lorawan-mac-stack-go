package mac

import (
	"strings"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
)

// DefaultServerConfig fills timing defaults into a ServerConfig.
func DefaultServerConfig(cfg ServerConfig) ServerConfig {
	if cfg.Clock == nil {
		panic("mac: ServerConfig.Clock is required")
	}
	if cfg.Space == nil {
		panic("mac: ServerConfig.Space is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "server"
	}
	if cfg.RX1Delay == 0 {
		cfg.RX1Delay = 5 * time.Second
	}
	if cfg.RX2Delay == 0 {
		cfg.RX2Delay = 6 * time.Second
	}
	if cfg.RX1Window == 0 {
		cfg.RX1Window = time.Second
	}
	if cfg.RX2Window == 0 {
		cfg.RX2Window = 2 * time.Second
	}
	if cfg.MaxFCntGap == 0 {
		cfg.MaxFCntGap = 16384
	}
	return cfg
}

// RegisterDevice provisions a device credential (DevEUI/JoinEUI/AppKey).
// It is idempotent: re-registering the same DevEUI replaces its credential.
func (s *NetworkServer) RegisterDevice(devEUI, joinEUI protocol.EUI64, appKey [16]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sd, ok := s.devices[devEUI]; ok {
		sd.mu.Lock()
		sd.provisioned = true
		sd.st.JoinEUI = joinEUI
		sd.st.AppKey = appKey
		err := s.persistDeviceLocked(sd)
		sd.mu.Unlock()
		return err
	}
	state := ServerDeviceState{
		DevEUI:        devEUI,
		JoinEUI:       joinEUI,
		AppKey:        appKey,
		UsedDevNonces: nil,
	}
	sd := s.newDeviceLocked(state, true)
	s.devices[devEUI] = sd
	if s.started {
		go s.deviceLoop(sd)
	}
	return s.persistDeviceLocked(sd)
}

func (s *NetworkServer) newDeviceLocked(state ServerDeviceState, provisioned bool) *serverDevice {
	sd := &serverDevice{
		st:          state,
		provisioned: provisioned,
		upGuard:     newFrameGuard(s.cfg.MaxFCntGap),
		inbound:     make(chan radioInbound, 32),
	}
	sd.upGuard.lastAccepted = state.NwkFCntUp
	sd.downQueue = append(sd.downQueue, state.DownQueue...)
	sd.pendingConfirmedDown = state.PendingConfirmedDown
	return sd
}

// QueueDownlink enqueues application data for a device. Class-A downlinks
// are transmitted only inside RX1/RX2 following the device's next uplink.
func (s *NetworkServer) QueueDownlink(devEUI protocol.EUI64, confirmed bool, port uint8, payload []byte) error {
	if port == 0 {
		return ErrInvalidConfig
	}
	s.mu.RLock()
	sd, ok := s.devices[devEUI]
	s.mu.RUnlock()
	if !ok {
		return ErrUnknownDevice
	}
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.downQueue = append(sd.downQueue, PendingDown{
		Confirmed: confirmed,
		FPort:     port,
		Payload:   append([]byte(nil), payload...),
		QueuedAt:  s.cfg.Clock.Now().UnixNano(),
	})
	sd.st.DownQueue = append([]PendingDown(nil), sd.downQueue...)
	return s.persistDeviceLocked(sd)
}

// ArmLinkADR marks the device so its next receive window carries a
// LinkADRReq asking for dataRate/txPower.
func (s *NetworkServer) ArmLinkADR(devEUI protocol.EUI64, dataRate uint8, txPower int) error {
	s.mu.RLock()
	sd, ok := s.devices[devEUI]
	s.mu.RUnlock()
	if !ok {
		return ErrUnknownDevice
	}
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.adrArmed = true
	sd.adrDR = dataRate
	sd.adrTP = txPower
	return s.persistDeviceLocked(sd)
}

func (s *NetworkServer) persistDeviceLocked(sd *serverDevice) error {
	return SaveServerDeviceState(s.cfg.Store, serverDeviceKey(sd.st.DevEUI), &sd.st)
}

func isServerStateKey(key string, devEUI protocol.EUI64) bool {
	return strings.HasPrefix(key, "server-device:")
}
