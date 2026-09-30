# LoRaWAN MAC Stack (Go)

A locally-runnable LoRaWAN Class-A MAC protocol stack with both the end-device
(ED) side and the network-server (NS) side, connected by an in-memory radio
loopback. It implements OTAA join, confirmed/unconfirmed uplinks, RX1/RX2
receive windows, adaptive data rate (ADR), duty-cycle enforcement, frame
integrity/replay protection, crash recovery, and safe multi-device
concurrency. It is a self-contained protocol model for learning and testing;
it is not a certified regional LoRaWAN implementation.

## Layout

- `pkg/crypto` — AES-128 LoRaWAN primitives: OTAA session-key derivation,
  AES-CTR frame payload encryption, AES-CMAC MIC, reversed-ECB Join-Accept.
  Verified against RFC 4493 CMAC vectors and an independent Python reference.
- `pkg/protocol` — wire serialization: Join-Request/Join-Accept, MAC data
  frames (MHDR/FCtrl/FOpts/FPort/FRMPayload/MIC), and LinkADR/DevStatus
  MAC commands.
- `pkg/clock` — `Clock` interface with a wall-clock implementation and a
  deterministic `FakeClock` (virtual time + priority timer heap) used by all
  tests for reproducible, zero-flake timing.
- `pkg/radio` — `Space`, the in-memory air interface: endpoint registry,
  propagation delay, scheduled transmissions (`TransmitAt` for receive
  windows), a loss/corruption hook, and synchronous (deterministic) or
  goroutine-isolated delivery.
- `pkg/storage` — crash-safe JSON document store (temp-file + fsync + atomic
  rename) and an in-memory variant.
- `pkg/mac` — the MAC layer:
  - `device*.go` — Class-A end device (join, send loop, RX windows, retransmit,
    ADR, duty cycle).
  - `server*.go` — multi-device network server (join, dedup, downlink queue,
    per-device goroutine isolation).
  - `frameguard.go` — shared 32-bit counter recovery / replay rules.
  - `adr.go`, `dutycycle.go`, `session.go`, `config.go`, `errors.go`.
- `cmd/demo` — runnable end-to-end demonstration with wall-clock time and
  file-backed persistence.

## Running

```sh
# build + vet
go build ./...
go vet ./...

# full test suite (unit + end-to-end, deterministic virtual clock)
go test ./...

# include the race detector for the concurrency scenarios
go test ./... -race

# runnable demo (real time, persists sessions to a temp dir)
go run ./cmd/demo
```

All timing-sensitive E2E tests use `FakeClock`, so results are deterministic
and the suite completes in seconds regardless of host speed.

## Implemented semantics

### 1. OTAA join

- Device sends a Join-Request carrying a strictly monotonic `DevNonce`. The
  pending nonce is persisted **before** the request is transmitted, so a crash
  can never reuse it.
- Server verifies AppKey MIC, rejects any previously accepted `DevNonce`
  (persisted set; rejection is silent and logged, so the device
  deterministically times out), allocates a unique `DevAddr` and `AppNonce`
  from persisted monotonic counters, and sends an ECB-encrypted Join-Accept
  scheduled into the join RX1 window.
- Both sides derive `NwkSKey`/`AppSKey` with the identical LoRaWAN 1.0.x
  derivation (`0x01`/`0x02` type bytes). Both start uplink/downlink frame
  counters at **0** and persist keys, address and counters atomically before
  reporting join success. (`TestE2EJoin`, `TestE2EDuplicateDevNonceRejected`)

### 2. Confirmed uplink and retransmission

- After a confirmed uplink the device opens **RX1 then RX2** (offsets and
  window lengths are configurable, shared between device and server).
- Missing ACK triggers retransmission with the **same FCnt** after an
  exponential backoff `min(BaseBackoff << attempt, MaxBackoff)`, for at most
  `MaxRetries` retries. Persistent ACK loss also drives one ADR fallback step.
- The server acknowledges each confirmed uplink; a retransmitted (duplicate
  FCnt) frame is re-ACKed idempotently but its application payload is
  delivered **exactly once**.
- Config: `MaxRetries`, `BaseBackoff`, `MaxBackoff`.
  (`TestE2EConfirmedUplinkACK`, `TestE2EConfirmedRetransmitOnLoss`,
  `TestE2EAllACKsLostFails`)

### 3. Frame counters and integrity

- Every data frame carries an AES-CMAC MIC over `B0 || message`; FRMPayload is
  AES-CTR encrypted.
- Each side keeps independent 32-bit uplink/downlink counters. Only the low 16
  bits are on the wire; the receiver recovers the full counter from its
  accepted-counter window (`frameGuard.go`), verifies the MIC **before**
  mutating state, and rejects:
  - counters ≤ last accepted → duplicate/replay,
  - counters more than `MaxFCntGap` ahead → desync/replay,
  - bad MIC or marked-corrupt frames → dropped and logged,
  - structurally malformed input → rejected at the parser (bounded lengths; no
    state pollution, no panics).
- **Overflow:** counters are 32-bit. Once the accepted counter cannot be
  followed by a strictly larger in-window value, uplink send returns
  `ErrFCntOverflow` and downlink construction is refused/logged; the
  documented recovery action is a **new OTAA join**, which starts fresh
  counters at 0 under a new session. (`TestFrameGuard*`,
  `TestE2ECorruptUplinkDropped`, `TestE2EReplayFrameRejected`,
  protocol malformed-frame tests)

### 4. Adaptive data rate

- Server-driven: `ArmLinkADR` queues a `LinkADRReq` in FOpts of the next
  downlink; the device validates bounds and either applies it (rate + power)
  or returns answer bits zeroed. Join state and frame counters are never reset.
- Margin-driven: `SetLinkMargin` feeds SNR margin samples; after
  `IncreaseCount` healthy samples power is reduced first then data rate
  raised; after `DecreaseCount` poor samples (or `LinkFailed` on ACK loss) the
  data rate is lowered, then power raised — a deterministic link-worsening
  fallback.
- Config: `ADRConfig{MinMargin, MinDataRate, MaxDataRate, MinTXPower,
  MaxTXPower, IncreaseCount, DecreaseCount}`.
  (`TestADR*`, `TestE2EServerLinkADR`, `TestE2EADRFallbackOnBadMargin`)

### 5. Duty cycle and downlink queuing

- After an emission of air time `T` at limit `d`, the device stays silent for
  `T·(1/d − 1)` (aggregated single-band rule). Air time comes from a
  pluggable deterministic `TimeOnAirFunc` (default linear model).
- A send while blocked is placed on a bounded FIFO (`MaxQueuedSends`) and
  emitted when the horizon clears; when the queue is full, `Send` returns
  `ErrSendQueueFull` immediately. A limit ≤ 0 disables enforcement.
- **Class-A downlinks are never pushed:** they are queued server-side and
  flushed only into RX1/RX2 that follow a device uplink. The default schedule
  uses RX1; per-device RX2 fallback additionally emits an RX2 copy when RX1 is
  lost. A downlink that reaches the device outside both windows is dropped and
  logged (`downlink missed RX1/RX2 windows`) and remains/returns according to
  queue consumption; the deterministic explanation is the timestamp window
  check in `device_rx.go`.
  (`TestE2EDutyCycleQueuesSends`, `TestE2EDutyCycleQueueFullRejected`,
  `TestE2EClassADownlinkAfterUplink`, `TestE2EACKViaRX2Fallback`)

### 6. Crash recovery

- Session keys, DevAddr, DevNonce history and frame counters are persisted
  through `storage.Store` (atomic write-replace), on the device
  (`device-session:<DevEUI>`) and server (`server-device:<DevEUI>`), plus
  server-wide nonce/address counters (`server-meta`) and the pending Class-A
  downlink queue.
- State is saved **before** transmission (so a crash never reuses a counter
  or nonce) and immediately after a fresh accepted frame. On restart both
  sides reload state; the next uplink continues from the next counter (no
  reuse), and acknowledged/duplicate state is retained so a retransmitted
  frame is still handled exactly once.
- **Inconsistent sides:** the server is authoritative for replay policy. If a
  device restores a counter ahead of the server (or vice versa) beyond
  `MaxFCntGap`, frames are rejected as desync; the deterministic recovery is a
  fresh OTAA join (new session, counters reset to 0). This bound is
  configurable and documented above.

**Rejoin policy:** while a device holds a valid session, `Join` is idempotent
and keeps the established keys/address/counters. A new OTAA exchange happens
only when no session exists (first boot, after `ErrFCntOverflow`, or when the
server has rejected the device as desynchronized); each exchange consumes a
fresh monotonic DevNonce, so nonce reuse is impossible.
  (`TestE2ECrashRecovery`)

### 7. Concurrency and isolation

- The server keeps a lock-protected record per device and runs **one inbound
  queue + one processing goroutine per device**. The radio callback only
  classifies and does a non-blocking enqueue (bounded 32); a flooding device
  drops its own excess frames and can never block other devices or the shared
  dispatch path. Application callbacks run outside the device lock.
- The device serializes transmissions through a single FIFO sender goroutine
  (one Class-A uplink → windows → next uplink). All exported methods are
  goroutine-safe; the whole suite passes `-race`.
  (`TestE2EMultiDeviceConcurrency`)

## Configuration reference

`DeviceConfig` (see `pkg/mac/config.go`):

| Field | Meaning |
| --- | --- |
| `DevEUI`, `JoinEUI`, `AppKey` | OTAA identity/credential |
| `Space`, `Endpoint`, `Server` | radio loopback binding |
| `Clock`, `Store`, `Logger`, `OnDown` | infrastructure hooks |
| `JoinTimeout` | wait for a Join-Accept (default 8s) |
| `RX1Delay/RX2Delay/RX1Window/RX2Window/RX2DataRate` | receive windows |
| `MaxRetries`, `BaseBackoff`, `MaxBackoff` | confirmed-uplink retries |
| `MaxDutyCycle`, `DropIfBlocked`, `MaxQueuedSends` | duty-cycle policy (queue by default; `DropIfBlocked=true` rejects) |
| `InitialDataRate`, `InitialTXPower`, `ADRConfig` | ADR/power |
| `DevNonce`, `TimeOnAir` | injectable nonce / air-time model |

`ServerConfig`:

| Field | Meaning |
| --- | --- |
| `NetID`, `JoinEUI`, `Space`, `Clock`, `Store`, `Logger`, `OnUp` | identity/infrastructure |
| `RX1Delay/RX2Delay/RX1Window/RX2Window/RX2DataRate` | offered windows |
| `MaxFCntGap` | replay/desync window (default 16384) |
| `ADRConfig`, `AppNonce` | ADR policy / injectable nonce |

Timing values are durations; the tests use compressed windows (5s/6s offsets)
 purely to make the virtual-time scenarios short, and the demo uses 1s/2s with
the real clock.

## Test matrix

| Requirement | Tests |
| --- | --- |
| OTAA + key agreement | `TestE2EJoin`, `TestLoRaWANDerivation`, join tests in `pkg/protocol` |
| Duplicate DevNonce | `TestE2EDuplicateDevNonceRejected` |
| Confirmed/ACK/retransmit/dedup | `TestE2EConfirmedUplinkACK`, `TestE2EConfirmedRetransmitOnLoss`, `TestE2EAllACKsLostFails` |
| Replay/corruption/malformed | `TestE2EReplayFrameRejected`, `TestE2ECorruptUplinkDropped`, `TestMalformedFramesRejected`, `TestDataFrameRoundTrip` |
| Counter rules/overflow | `TestFrameGuard*` |
| ADR command + fallback | `TestE2EServerLinkADR`, `TestE2EADRFallbackOnBadMargin`, `TestADR*` |
| Duty cycle/queue | `TestE2EDutyCycle*`, `TestDutyCycle*` |
| Downlink windows/RX2 | `TestE2EClassADownlinkAfterUplink`, `TestE2EACKViaRX2Fallback` |
| Confirmed downlink ACK | `TestE2EConfirmedDownlinkAcked` |
| Downlink replay/corrupt | `TestE2EDuplicateDownlinkDropped` |
| FCnt overflow | `TestE2EFCntOverflowRequiresRejoin`, `TestDataFrame32BitCounterWrap` |
| Crash recovery | `TestE2ECrashRecovery` |
| Multi-device concurrency | `TestE2EMultiDeviceConcurrency` |
| Crypto correctness | `TestCMACRFC4493Vectors`, `TestJoinAcceptECBRoundTrip` |
