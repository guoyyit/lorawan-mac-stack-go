package mac

import "errors"

// Protocol-level errors.
var (
	ErrNotJoined         = errors.New("mac: device has not joined")
	ErrDuplicateDevNonce = errors.New("mac: duplicate DevNonce rejected")
	ErrUnknownDevice     = errors.New("mac: unknown device")
	ErrMIC               = errors.New("mac: frame integrity check failed")
	ErrMalformed         = errors.New("mac: malformed frame")
	ErrReplay            = errors.New("mac: replayed or out-of-window frame counter")
	ErrFCntOverflow      = errors.New("mac: frame counter overflow, rejoin required")
	ErrDutyCycleBlocked  = errors.New("mac: duty cycle blocked")
	ErrSendQueueFull     = errors.New("mac: send queue full while duty-cycle blocked")
	ErrNoACK             = errors.New("mac: confirmed uplink not acknowledged")
	ErrJoinTimeout       = errors.New("mac: join timeout")
	ErrADRRejected       = errors.New("mac: link ADR command rejected")
	ErrWindowMissed      = errors.New("mac: downlink missed all receive windows")
	ErrInvalidConfig     = errors.New("mac: invalid configuration")
)
