// Part of package mac: Class-A end device lifecycle and public API.
package mac

import (
	"context"
	"sync"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
)

// EndDevice is a Class-A LoRaWAN end device. All exported methods are safe
// for concurrent use; uplink transmissions are serialized Class-A style
// (one transmission, then RX1/RX2, then the next transmission).
type EndDevice struct {
	cfg DeviceConfig

	mu sync.Mutex

	session DeviceSession
	adr     *ADREngine
	duty    *DutyTracker

	started bool

	// receive-window bookkeeping for the most recent uplink/join
	upSentAt  time.Time
	rx1Open   time.Time
	rx1Close  time.Time
	rx2Open   time.Time
	rx2Close  time.Time
	pendingFC uint32 // uplink FCnt awaiting a downlink ACK; 0 = none
	ackCh     chan struct{}
	ackDownFC uint32 // confirmed downlink FCnt to ACK on the next uplink

	// join bookkeeping
	joinCh    chan joinResult
	joinNonce protocol.DevNonce

	// serialized FIFO send queue
	sendCh    chan sendRequest
	queueSize int

	// downlink replay protection
	downGuard frameGuard
}

type joinResult struct{ err error }

type sendRequest struct {
	confirmed bool
	port      uint8
	payload   []byte
	result    chan error
	ctx       context.Context
}

// DefaultDeviceConfig fills timing/retry/duty-cycle defaults into cfg while
// keeping its identity and transport fields.
func DefaultDeviceConfig(cfg DeviceConfig) DeviceConfig {
	if cfg.Clock == nil {
		panic("mac: DeviceConfig.Clock is required")
	}
	if cfg.Space == nil {
		panic("mac: DeviceConfig.Space is required")
	}
	if cfg.JoinTimeout == 0 {
		cfg.JoinTimeout = 8 * time.Second
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
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.BaseBackoff == 0 {
		cfg.BaseBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 32 * time.Second
	}
	if cfg.MaxQueuedSends == 0 {
		cfg.MaxQueuedSends = 16
	}
	if cfg.InitialTXPower == 0 {
		cfg.InitialTXPower = 14
	}
	if cfg.TimeOnAir == nil {
		cfg.TimeOnAir = DefaultTimeOnAir
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = radio.EndpointID("device:" + cfg.DevEUI.String())
	}
	if cfg.Server == "" {
		cfg.Server = "server"
	}
	return cfg
}

// NewEndDevice validates the config and builds a device. Existing join
// state is loaded from the store so a restarted device resumes its session.
func NewEndDevice(cfg DeviceConfig) (*EndDevice, error) {
	cfg = DefaultDeviceConfig(cfg)
	if cfg.Store == nil {
		return nil, ErrInvalidConfig
	}

	d := &EndDevice{
		cfg:       cfg,
		sendCh:    make(chan sendRequest),
		downGuard: newFrameGuard(65535),
	}
	d.adr = NewADREngine(cfg.ADRConfig, cfg.InitialDataRate, cfg.InitialTXPower)
	d.duty = NewDutyTracker(cfg.Clock, cfg.MaxDutyCycle, cfg.TimeOnAir)

	if sess, err := LoadDeviceSession(cfg.Store, deviceSessionKey(cfg.DevEUI)); err == nil {
		d.session = *sess
		d.adr.Restore(sess.ADR)
		d.downGuard.lastAccepted = sess.NwkFCntDown
	} else if !IsNotFound(err) {
		return nil, err
	}
	return d, nil
}

// Start registers the device on the air interface.
func (d *EndDevice) Start() error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return nil
	}
	d.started = true
	d.mu.Unlock()

	d.cfg.Space.Register(d.cfg.Endpoint, d.handleFrame)
	go d.sendLoop()
	return nil
}

// Close stops background processing and persists state.
func (d *EndDevice) Close() error {
	d.cfg.Space.Unregister(d.cfg.Endpoint)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.persistLocked()
}

// Joined reports whether the device holds a valid session.
func (d *EndDevice) Joined() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.session.Joined
}

// Session returns a copy of the persisted session state.
func (d *EndDevice) Session() (DeviceSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.session.Joined {
		return DeviceSession{}, ErrNotJoined
	}
	return d.session, nil
}

// Send transmits an application frame. For confirmed frames it blocks until
// an ACK is received in RX1/RX2 or all retransmissions are exhausted; for
// unconfirmed frames it blocks until the transmission is emitted (after any
// duty-cycle wait). Requests made while duty-cycle blocked are enqueued
// FIFO or rejected with ErrSendQueueFull per DeviceConfig.
func (d *EndDevice) Send(ctx context.Context, confirmed bool, port uint8, payload []byte) error {
	if port == 0 {
		return ErrInvalidConfig
	}
	// If queueing on duty-cycle block is disabled, reject immediately when
	// the channel is currently blocked (deterministic "drop" policy).
	if d.cfg.DropIfBlocked {
		d.mu.Lock()
		blocked := d.duty != nil && !d.duty.CanSend()
		d.mu.Unlock()
		if blocked {
			return ErrDutyCycleBlocked
		}
	}
	d.mu.Lock()
	if d.queueSize >= d.cfg.MaxQueuedSends {
		d.mu.Unlock()
		return ErrSendQueueFull
	}
	d.queueSize++
	d.mu.Unlock()

	req := sendRequest{
		confirmed: confirmed,
		port:      port,
		payload:   append([]byte(nil), payload...),
		result:    make(chan error, 1),
		ctx:       ctx,
	}
	select {
	case d.sendCh <- req:
	case <-ctx.Done():
		d.decQueue()
		return ctx.Err()
	}
	select {
	case err := <-req.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetLinkMargin feeds the most recent downlink demodulation margin into the
// device-side ADR engine (margin-driven rate/power fallback).
func (d *EndDevice) SetLinkMargin(margin float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, _, changed := d.adr.RecordMargin(margin); changed {
		d.session.ADR = d.adr.Snapshot()
		_ = d.persistLocked()
	}
}

// PendingQueued returns how many send requests are currently accepted but
// waiting (for the sender or for the duty-cycle limit).
func (d *EndDevice) PendingQueued() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queueSize
}

func (d *EndDevice) decQueue() {
	d.mu.Lock()
	if d.queueSize > 0 {
		d.queueSize--
	}
	d.mu.Unlock()
}

func (d *EndDevice) log(level, msg string) {
	if d.cfg.Logger != nil {
		d.cfg.Logger(level, msg)
	}
}
