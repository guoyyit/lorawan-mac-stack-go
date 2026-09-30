package mac

import (
	"errors"
	"fmt"

	lwcrypto "github.com/guoyyit/lorawan-mac-stack-go/pkg/crypto"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/storage"
)

// DeviceSession is the end device's persisted join state. Every field is
// written atomically with SaveSession so a crash at any point leaves either
// the old or the new complete state.
type DeviceSession struct {
	Joined        bool                 `json:"joined"`
	DevEUI        protocol.EUI64       `json:"dev_eui"`
	JoinEUI       protocol.EUI64       `json:"join_eui"`
	DevAddr       protocol.DevAddr     `json:"dev_addr"`
	Keys          lwcrypto.SessionKeys `json:"keys"`
	DevNonce      protocol.DevNonce    `json:"dev_nonce"`
	UsedDevNonces []protocol.DevNonce  `json:"used_dev_nonces,omitempty"`
	NwkFCntUp     uint32               `json:"nwk_fcnt_up"`
	NwkFCntDown   uint32               `json:"nwk_fcnt_down"`
	LastUpAcked   uint32               `json:"last_up_acked"`
	ADR           Snapshot             `json:"adr"`
}

// ServerDeviceState is the network server's persisted state for one device.
type ServerDeviceState struct {
	DevEUI  protocol.EUI64       `json:"dev_eui"`
	JoinEUI protocol.EUI64       `json:"join_eui"`
	AppKey  lwcrypto.AES128Key   `json:"app_key"`
	DevAddr protocol.DevAddr     `json:"dev_addr"`
	Joined  bool                 `json:"joined"`
	Keys    lwcrypto.SessionKeys `json:"keys"`
	// UsedDevNonces records every accepted DevNonce; a repeat must be
	// rejected even after a server restart.
	UsedDevNonces []protocol.DevNonce `json:"used_dev_nonces"`
	NwkFCntUp     uint32              `json:"nwk_fcnt_up"`
	NwkFCntDown   uint32              `json:"nwk_fcnt_down"`
	// LastDeliveredFCnt is the highest uplink counter whose payload was
	// delivered to the application, for duplicate suppression.
	LastDeliveredFCnt uint32 `json:"last_delivered_fcnt"`
	// LastACKedFCnt is the highest confirmed uplink counter acknowledged,
	// so retransmissions are answered exactly once.
	LastACKedFCnt uint32 `json:"last_acked_fcnt"`
	// LastDownAckedFCnt is the confirmed-downlink counter acknowledged by
	// the device's ACK bit (ACK state survives a server restart).
	LastDownAckedFCnt uint32 `json:"last_down_acked_fcnt"`
	// PendingConfirmedDown is the confirmed downlink awaiting device ACK.
	PendingConfirmedDown uint32   `json:"pending_confirmed_down"`
	ADR                  Snapshot `json:"adr"`
	DataRate             uint8    `json:"data_rate"`
	// DownQueue holds Class-A downlinks waiting for the next uplink; it is
	// persisted so a server restart does not lose queued application data.
	DownQueue []PendingDown `json:"down_queue"`
}

// PendingDown is one queued server-to-device downlink (Class A: delivered
// only inside a receive window following an uplink).
type PendingDown struct {
	Confirmed bool
	FPort     uint8
	Payload   []byte
	QueuedAt  int64
}

// LoadDeviceSession reads a device session from a store.
func LoadDeviceSession(store KVStore, key string) (*DeviceSession, error) {
	var s DeviceSession
	if err := store.Load(key, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveDeviceSession atomically persists a device session.
func SaveDeviceSession(store KVStore, key string, s *DeviceSession) error {
	return store.Save(key, s)
}

// LoadServerDeviceState reads one server device record from a store.
func LoadServerDeviceState(store KVStore, key string) (*ServerDeviceState, error) {
	var s ServerDeviceState
	if err := store.Load(key, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveServerDeviceState atomically persists one server device record.
func SaveServerDeviceState(store KVStore, key string, s *ServerDeviceState) error {
	return store.Save(key, s)
}

// deviceSessionKey / serverDeviceKey build deterministic storage keys.
func deviceSessionKey(devEUI protocol.EUI64) string {
	return fmt.Sprintf("device-session:%016x", devEUI)
}

func serverDeviceKey(devEUI protocol.EUI64) string {
	return fmt.Sprintf("server-device:%016x", devEUI)
}

// IsNotFound reports whether an error means no persisted record exists.
func IsNotFound(err error) bool { return errors.Is(err, storage.ErrNotFound) }

// deriveSessionKeys wraps crypto.DeriveSessionKeys for MAC-layer callers.
func deriveSessionKeys(appKey lwcrypto.AES128Key, appNonce protocol.AppNonce,
	netID protocol.NetID, devNonce protocol.DevNonce) lwcrypto.SessionKeys {
	return lwcrypto.DeriveSessionKeys(appKey, appNonce, netID, devNonce)
}
