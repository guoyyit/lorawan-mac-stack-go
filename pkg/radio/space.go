// Package radio implements the in-memory "air interface" used to connect
// end devices and the network server. It models propagation delay, the
// Class-A RX1/RX2 receive windows, observed SNR and optional loss/corruption.
package radio

import (
	"sync"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/clock"
)

// EndpointID identifies a device or the network server on the air.
type EndpointID string

// Frame is one over-the-air transmission: raw LoRaWAN PHYPayload bytes plus
// the physical-layer context the MAC stack needs for scheduling/ADR.
type Frame struct {
	// Payload is the raw PHYPayload.
	Payload []byte
	// DataRate index of the transmission (0..15, LoRaWAN regional style).
	DataRate uint8
	// TXPower is transmit power in dBm.
	TXPower int
	// SNR is the demodulation margin as observed by the receiver (dB).
	SNR float64
	// Corrupt marks a frame whose bytes survived delivery but failed
	// integrity (test hook: MIC check must drop it).
	Corrupt bool
	// SentAt is the clock time at which the transmitter emitted the frame.
	SentAt time.Time
	// ReceivedAt is the clock time at which the receiver got the frame.
	ReceivedAt time.Time
}

// FrameHandler processes one frame delivered to an endpoint.
type FrameHandler func(from EndpointID, f Frame)

// Space is the shared in-memory air interface.
type Space struct {
	clk       clock.Clock
	prop      time.Duration
	mu        sync.RWMutex
	endpoints map[EndpointID]FrameHandler
	loss      LossHook
	sync      bool
}

// Config configures a Space.
type Config struct {
	Clock            clock.Clock
	PropagationDelay time.Duration
	// SynchronousDelivery delivers frames in the scheduler's callback
	// instead of a new goroutine. Set it with FakeClock for fully
	// deterministic tests; the network server still isolates slow devices
	// internally. Leave it false with a real clock so a blocked receiver
	// cannot block the transmitter.
	SynchronousDelivery bool
}

// NewSpace creates an empty in-memory air interface.
func NewSpace(cfg Config) *Space {
	if cfg.Clock == nil {
		panic("radio: Config.Clock is required")
	}
	return &Space{
		clk:       cfg.Clock,
		prop:      cfg.PropagationDelay,
		endpoints: make(map[EndpointID]FrameHandler),
		sync:      cfg.SynchronousDelivery,
	}
}

// Register attaches an endpoint with its receive handler.
func (s *Space) Register(id EndpointID, h FrameHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoints[id] = h
}

// Unregister removes an endpoint.
func (s *Space) Unregister(id EndpointID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.endpoints, id)
}

// Transmit emits a frame from from to to. The frame arrives at the receiver
// after the configured propagation delay unless it is lost/corrupted by the
// installed loss hook.
func (s *Space) Transmit(from, to EndpointID, f Frame) bool {
	t := s.clk.Now()
	if !f.SentAt.IsZero() {
		t = f.SentAt
	}
	return s.TransmitAt(t, from, to, f)
}

// TransmitAt behaves like Transmit but schedules emission at time t, so
// network-server downlinks land inside the device's receive windows.
func (s *Space) TransmitAt(t time.Time, from, to EndpointID, f Frame) bool {
	s.mu.RLock()
	_, ok := s.endpoints[to]
	loss := s.loss
	s.mu.RUnlock()
	if !ok {
		return false
	}

	deliver := true
	corrupt := f.Corrupt
	if loss != nil {
		deliver, corrupt = loss(from, to, f)
	}
	if !deliver {
		return false
	}

	sentAt := t
	receivedAt := t.Add(s.prop)
	out := f
	out.Corrupt = corrupt
	out.SentAt = sentAt
	out.ReceivedAt = receivedAt
	if len(f.Payload) != 0 {
		out.Payload = append([]byte(nil), f.Payload...)
	}

	// Wait until the emission instant, then propagate for s.prop before
	// the receiver callback fires.
	emissionDelay := t.Sub(s.clk.Now())
	if emissionDelay < 0 {
		emissionDelay = 0
	}
	s.clk.AfterFunc(emissionDelay, func() {
		s.clk.AfterFunc(s.prop, func() {
			s.mu.RLock()
			h, ok := s.endpoints[to]
			s.mu.RUnlock()
			if !ok {
				return
			}
			if s.sync {
				h(from, out)
				return
			}
			// Asynchronous delivery means a slow receiver can never block the
			// transmitter (device isolation at the radio layer).
			go h(from, out)
		})
	})
	return true
}

// LossHook decides per-transmission delivery behavior.
type LossHook func(from, to EndpointID, f Frame) (deliver, corrupt bool)

// SetLossHook installs a deterministic loss/corruption hook (test support).
func (s *Space) SetLossHook(h LossHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loss = h
}
