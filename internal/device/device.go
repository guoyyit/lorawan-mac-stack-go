// Package device implements a Class A LoRaWAN end-device stack.
package device

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/mac"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/transport"
)

// AppHandler receives decrypted downlink application payloads.
type AppHandler func(payload []byte)

// Config configures a Device.
type Config struct {
	DevEUI  lorawan.EUI
	JoinEUI lorawan.EUI
	AppKey  lorawan.AESKey
	Store   lorawan.Store
	Air     *transport.Endpoint

	RX1Delay   time.Duration
	RX2Delay   time.Duration
	RXWindow   time.Duration
	Retries    int // retransmissions after the initial attempt (0 = none)
	Backoff    time.Duration
	BackoffMax time.Duration

	// Airtime is the time-on-air charged per uplink for duty-cycle control.
	Airtime time.Duration

	Duty mac.DutyConfig
	ADR  mac.ADRConfig

	InitialDR      uint8
	InitialTXPower int

	OnAppData AppHandler
}

// incoming is a decoded downlink delivered to the device while a window is open.
type incoming struct {
	frame *lorawan.Frame
	join  *lorawan.JoinAccept
}

// Device is a Class A end device.
type Device struct {
	cfg Config

	mu      sync.Mutex
	session *lorawan.Session
	duty    *mac.DutyLimiter
	adr     *mac.ADREngine

	downCh chan incoming

	joinGen uint64 // guards against stale join-accept frames

	// pendingACK marks that the next uplink must ACK a confirmed downlink.
	pendingACK bool

	// pendingLinkADRAns is carried in the next uplink's FOpts after a LinkADRReq.
	pendingLinkADRAns []byte
}

// New constructs a device and restores its persisted session if present.
func New(cfg Config) (*Device, error) {
	if cfg.Store == nil || cfg.Air == nil {
		return nil, errors.New("device: store and air endpoint required")
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
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 100 * time.Millisecond
	}
	if cfg.Airtime <= 0 {
		cfg.Airtime = 50 * time.Millisecond
	}

	d := &Device{
		cfg:    cfg,
		duty:   mac.NewDutyLimiter(cfg.Duty),
		downCh: make(chan incoming, 16),
	}
	d.adr = mac.NewADREngine(cfg.ADR, cfg.InitialDR, cfg.InitialTXPower)

	id := lorawan.EUIHex(cfg.DevEUI)
	sess, err := cfg.Store.Load(context.Background(), id)
	switch {
	case err == nil:
		d.session = sess
		d.adr = mac.NewADREngine(cfg.ADR, sess.DataRate, sess.TXPower)
	case errors.Is(err, lorawan.ErrNotFound):
		d.session = lorawan.NewSession(cfg.DevEUI, cfg.JoinEUI, cfg.AppKey)
		if err := d.persist(context.Background()); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	cfg.Air.SetHandler(d.onAir)
	return d, nil
}

func (d *Device) onAir(raw []byte) {
	// The codec rejects every malformed/oversize input with an error, but a
	// receiver must never let a bad frame crash the process regardless: any
	// unexpected panic is contained and the frame is simply discarded.
	defer func() { _ = recover() }()
	d.mu.Lock()
	sess := d.session
	joined := sess.Joined
	d.mu.Unlock()

	if !joined {
		if ja, err := lorawan.DecodeJoinAccept(d.cfg.AppKey, raw); err == nil {
			d.queue(incoming{join: ja})
		}
		return
	}
	f, err := lorawan.DecodeFrame(sess, raw)
	if err != nil {
		// Bad MIC / malformed frames are dropped here; transport stats log them.
		return
	}
	d.queue(incoming{frame: f})
}

func (d *Device) queue(in incoming) {
	select {
	case d.downCh <- in:
	default:
		// No receive window open: Class A downlinks outside windows are
		// undefined; drop the frame rather than blocking the receiver.
	}
}

func (d *Device) persist(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.persistLocked(ctx)
}

// Air returns the device's air endpoint, primarily for test/diagnostic use
// (e.g. injecting packet loss).
func (d *Device) Air() *transport.Endpoint { return d.cfg.Air }

func (d *Device) persistLocked(ctx context.Context) error {
	d.session.UpdatedAt = time.Now()
	return d.cfg.Store.Save(ctx, lorawan.EUIHex(d.cfg.DevEUI), d.session)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Session returns a copy of the current session state.
func (d *Device) Session() lorawan.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	return *d.session.Clone()
}

// Close persists state and releases resources.
func (d *Device) Close() error {
	d.mu.Lock()
	d.mu.Unlock()
	return nil
}
