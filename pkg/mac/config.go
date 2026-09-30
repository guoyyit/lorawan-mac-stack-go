// Package mac implements the LoRaWAN Class-A MAC layer for both the end
// device and the network server: OTAA join, confirmed/unconfirmed traffic,
// RX1/RX2 windows, retransmissions, ADR, duty-cycle enforcement, replay
// protection, crash recovery and multi-device concurrency.
package mac

import (
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/clock"
	lwcrypto "github.com/guoyyit/lorawan-mac-stack-go/pkg/crypto"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
)

// KVStore is the persistence abstraction (satisfied by storage.Store and
// storage.MemoryStore).
type KVStore interface {
	Save(key string, v any) error
	Load(key string, v any) error
	Delete(key string) error
	Keys() ([]string, error)
}

// LogFunc receives structured log lines (join, drops, MIC failures...).
type LogFunc func(level, msg string)

// DownlinkFunc is the application callback for data received by a device.
type DownlinkFunc func(port uint8, payload []byte)

// UplinkFunc is the application callback for data received by the server.
// It is invoked at most once per unique confirmed/unconfirmed uplink frame.
type UplinkFunc func(devEUI protocol.EUI64, port uint8, payload []byte)

// TimeOnAirFunc returns the air time of a payload at a given data rate.
type TimeOnAirFunc func(dataRate uint8, payloadLen int) time.Duration

// DefaultTimeOnAir is the default (deterministic) air-time model. It is a
// simplified model used for duty-cycle bookkeeping: air time grows with
// payload length and shrinks with data rate. Deployments can supply a
// regional-curve implementation through TimeOnAirFunc.
func DefaultTimeOnAir(dataRate uint8, payloadLen int) time.Duration {
	if payloadLen < 0 {
		payloadLen = 0
	}
	// DR0 is the slowest (~50 ms preamble/header plus 4 ms per payload
	// byte); every data-rate step halves the air time, floor 10 ms.
	base := 50*time.Millisecond + time.Duration(payloadLen)*4*time.Millisecond
	for i := 0; i < int(dataRate); i++ {
		base = (base + 1) / 2
		if base <= 10*time.Millisecond {
			base = 10 * time.Millisecond
			break
		}
	}
	if base < 10*time.Millisecond {
		base = 10 * time.Millisecond
	}
	return base
}

// DeviceConfig configures a Class-A end device.
type DeviceConfig struct {
	DevEUI    protocol.EUI64
	JoinEUI   protocol.EUI64
	AppKey    lwcrypto.AES128Key
	Space     *radio.Space
	Endpoint  radio.EndpointID
	Server    radio.EndpointID
	Clock     clock.Clock
	Store     KVStore
	Logger    LogFunc
	OnDown    DownlinkFunc
	TimeOnAir TimeOnAirFunc

	// Join behaviour.
	JoinTimeout time.Duration

	// Class-A receive windows relative to the end of an uplink.
	RX1Delay    time.Duration
	RX2Delay    time.Duration
	RX1Window   time.Duration
	RX2Window   time.Duration
	RX2DataRate uint8

	// Confirmed-uplink retransmission policy.
	MaxRetries  int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration

	// Duty cycle: fraction of air time allowed (0.01 = 1%).
	MaxDutyCycle float64
	// DropIfBlocked selects the policy during a duty-cycle block: false
	// (default, zero value) queues the request FIFO up to MaxQueuedSends;
	// true rejects it immediately with ErrDutyCycleBlocked.
	DropIfBlocked  bool
	MaxQueuedSends int

	// ADR / power.
	InitialDataRate uint8
	InitialTXPower  int
	ADRConfig       ADRConfig

	// DevNonce source (defaults to a deterministic monotonic counter).
	DevNonce func() protocol.DevNonce
}

// ServerConfig configures the network server.
type ServerConfig struct {
	NetID    protocol.NetID
	JoinEUI  protocol.EUI64
	Space    *radio.Space
	Endpoint radio.EndpointID
	Clock    clock.Clock
	Store    KVStore
	Logger   LogFunc
	OnUp     UplinkFunc

	// Class-A receive-window timing offered to devices.
	RX1Delay    time.Duration
	RX2Delay    time.Duration
	RX1Window   time.Duration
	RX2Window   time.Duration
	RX2DataRate uint8

	// Replay protection: frames more than MaxFCntGap ahead of the last
	// accepted counter are rejected as possible replays/desync.
	MaxFCntGap uint32

	// ADR decision policy.
	ADRConfig ADRConfig

	// AppNonce source (defaults to a deterministic counter).
	AppNonce func() protocol.AppNonce
}
