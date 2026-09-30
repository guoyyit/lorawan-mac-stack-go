# lorawan-mac-stack-go

A locally-runnable LoRaWAN MAC stack in Go implementing the **Class A**
end-device and network-server semantics over an **in-memory loopback radio**.
It covers OTAA activation, confirmed/unconfirmed uplinks with receive windows
and retransmission, frame counters with replay protection, adaptive data rate,
duty-cycle enforcement, Class A downlink queuing, crash-safe persistence, and
concurrent service of many devices.

> Scope note: this is a MAC-semantics implementation for local execution and
> testing. Cryptography and key derivation follow LoRaWAN 1.0.2 exactly, but
> the on-the-wire data-frame layout is a **documented local binary format**
> that carries the full 32-bit frame counter (not the real PHY 16-bit counter
> with ambiguity window), so it is not wire-compatible with physical radios.

## Layout

| Path | Responsibility |
| --- | --- |
| `internal/lorawan` | Types, AES-128 key derivation / CTR / CMAC, frame codec, session counters, persistence stores |
| `internal/transport` | In-memory multi-endpoint "air" (`Medium`), latency & controllable packet loss |
| `internal/mac` | Duty-cycle limiter and adaptive-data-rate engine |
| `internal/device` | Class A end device: join, RX1/RX2, retries, ADR, persistence |
| `internal/server` | Network server: per-device workers, OTAA, dedup/ACK, downlink queue |
| `internal/integration` | End-to-end tests across device + server + transport |
| `cmd/demo` | Runnable end-to-end scenario including crash/restart |

## Running

```sh
# all automated tests (unit + end-to-end), with the race detector
go test -race ./...

# runnable scenario: join -> retransmission -> downlink -> ADR -> crash/restart
go run ./cmd/demo
```

The demo writes JSON session files to a temp directory (removed on exit) and
logs every phase. Tests create their own temp directories via `t.TempDir()`.

## Configuration

`device.Config` (see `internal/device/device.go`):

| Field | Meaning |
| --- | --- |
| `RX1Delay`, `RX2Delay`, `RXWindow` | Window start offsets and each window's open duration |
| `Retries` | Extra transmissions of a confirmed uplink after the first attempt |
| `Backoff`, `BackoffMax` | Exponential backoff base and cap (`Backoff * 2^(attempt-1)`) |
| `Airtime` | Time-on-air charged per uplink for duty-cycle accounting |
| `Duty` | `mac.DutyConfig`: `MaxDutyCycle` (fraction, e.g. `0.01`), `Policy` (`DutyWait`/`DutyDrop`), `MaxWait` |
| `ADR` | `mac.ADRConfig`: enable, `MinDR/MaxDR`, `MinTXPower/MaxTXPower`, margins, `BackoffMisses` |
| `InitialDR`, `InitialTXPower` | Starting rate/power |
| `Store` | `lorawan.Store` persistence backend |

`server.Config` (see `internal/server/server.go`):

| Field | Meaning |
| --- | --- |
| `NetID`, `Devices` | Network id and provisioned `DevEUI/JoinEUI/AppKey` records |
| `RX1Delay/RX2Delay/RXWindow` | Class A window schedule used to time downlinks |
| `QueueLimit` | Per-device inbox **and** downlink-queue bound (storm isolation) |
| `JoinNonceBase` | Offset so multiple servers can emit non-overlapping JoinNonces |

## Protocol semantics

### OTAA join

* The device sends a join-request (`MHdr | JoinEUI | DevEUI | DevNonce`)
  authenticated with `AES-CMAC(AppKey)`. `NwkSKey/AppSKey` are derived with
  the LoRaWAN 1.0.2 `0x01/0x02 | JoinNonce | NetID | DevNonce` construction.
* **DevNonce is monotonic and persisted before the request goes on air.** The
  server accepts only `DevNonce > last accepted DevNonce`; an equal or lower
  value is rejected deterministically (no join-accept, bad-frame counted).
* The join-accept is always sent in **RX1**. Keys, DevAddr, NetID and JoinNonce
* are persisted by the server before responding, and by the device on receipt.
* Frame counters start at **0** on every successful join.

### Confirmed uplinks, ACKs and retransmission

* One logical confirmed transmission **reserves a single frame counter**;
  retransmissions send the identical frame (same FCnt, same MIC).
* The device waits first in **RX1**, then **RX2**. With no ACK in either
  window it backs off exponentially (`Backoff`, capped at `BackoffMax`) and
  retries, up to `Retries` extra attempts, then returns
  `lorawan.ErrNoACK`.
* The server ACKs confirmed uplinks with a downlink whose `ACK` bit is set.
* An exact duplicate of the last accepted frame is recognized as a
  retransmission: it is ACKed again (so retries can complete) but the
  application callback fires **exactly once**.

### Frame counters, integrity and malformed input

* Both sides keep `FCntUp` and `FCntDown`. Each value holds the **next counter
  to use** (reservation semantics) and is persisted *before* transmission, so a
  crash wastes at most one counter value and never reuses one.
* A receiver accepts only `fcnt == next`. The immediately previous value is
  `ErrDuplicate` (legal retransmission); every other value — older replays and
  forward gaps/out-of-order — is `ErrReplay` and dropped. This strictness is
  possible because the local format carries the full 32-bit counter.
* Every data frame is integrity-protected (`AES-CMAC(NwkSKey)`) and the
  application payload is AES-CTR encrypted (`AppSKey`, or `NwkSKey` on FPort 0).
  A bad MIC, truncated frame, or length mismatch returns `ErrBadMIC`/
  `ErrMalformed`; the frame is discarded and counted as bad **without touching
  counters or session state** — no panic, no state pollution.
* **Overflow:** counters are 32-bit. `0xFFFFFFFF` is the last legal value;
  reserving past it returns `ErrCounterOverflow` and latches persisted
  `UpExhausted/DownExhausted` flags. No further frames are sent or accepted
  until a fresh OTAA join resets the session.

### Adaptive data rate

* The server commands rate/power with a **LinkADRReq** MAC command carried in
  the next downlink's FOpts; the device validates it against its configured
  `MinDR/MaxDR` / power bounds and rejects out-of-range values without changing
  state.
* The device also adapts autonomously from observed outcomes: sustained good
  margin raises efficiency (reduce power, then raise DR), a deficit margin
  steps down immediately, and `BackoffMisses` consecutive unacknowledged
  attempts step the DR down (robustness fallback).
* ADR changes update only `DataRate/TXPower` (persisted). Keys, DevAddr and
  frame counters are never altered by adaptation.

### Duty cycle and Class A downlinks

* The limiter models airtime as a debt window: a frame of airtime `A` at max
  fraction `D` occupies the air until `t + A/D`. `DutyDrop` returns
  `lorawan.ErrDutyCycle` immediately while limited; `DutyWait` blocks until the
  window opens (bounded by `MaxWait`). Admission happens **before** a frame
  counter is reserved, so a dropped send consumes no counter and sends nothing.
* Class A devices listen only after an uplink. `EnqueueDown` on the server
  keeps payload in a FIFO queue; **one** item is delivered in RX1 after the
  next uplink (piggybacked on the ACK when one is needed). If no uplink occurs,
  the item simply stays queued — that is the defined "window missed" behavior;
  nothing is sent outside a window.
* A pure ACK with no payload/MAC command is a **zero-counter control frame**
  and can be repeated for retransmissions without consuming a downlink
  counter. Any content-bearing downlink reserves and persists `FCntDown`.

### Crash recovery

* Session keys, DevAddr, both frame counters, DevNonce high-water mark,
  JoinNonce counter, exhaustion flags and ADR rate/power are persisted to a
  `lorawan.FileStore` (atomic temp-file + rename, optional fsync) or an
  in-memory store.
* The server saves each accepted fresh uplink (advancing `FCntUp`) before
  sending the ACK; the device reserves `FCntUp` before transmitting. After both
  sides restart, each resumes from durable counters and never accepts a value
  below its floor.
* **State divergence policy:** counters only move forward. A stale frame is
  rejected as replay/duplicate; the application sees each frame once. If the
  persistent state is irrecoverably out of sync (e.g. one side lost its store),
  the defined recovery is a **fresh OTAA join**, which deterministically resets
  both counters to 0 and issues new session keys.

### Concurrency and isolation

* The server runs one serialized worker goroutine per provisioned device with
  a bounded inbox (`QueueLimit`). Frames are routed by DevEUI (join) or DevAddr
  (data). A storm or crash-loop on one device fills only that device's inbox
  and drops its own excess frames; other devices' joins and traffic proceed.
* Shared maps and each peer's state are mutex-protected; the full suite passes
  under `go test -race`.

## Cryptographic verification

`internal/lorawan/crypto_test.go` verifies the AES-CMAC implementation against
RFC 4493 vectors, the join MIC against a worked example, and key derivation /
payload CTR round-trips against independently computed values.
