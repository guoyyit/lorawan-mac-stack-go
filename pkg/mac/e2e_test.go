package mac

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/clock"
	lwcrypto "github.com/guoyyit/lorawan-mac-stack-go/pkg/crypto"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/storage"
)

type e2eHarness struct {
	t       *testing.T
	clk     *clock.FakeClock
	space   *radio.Space
	server  *NetworkServer
	devices map[string]*EndDevice
	upMu    sync.Mutex
	upPorts []uint8
	upData  [][]byte
}

func (h *e2eHarness) onUp(_ protocol.EUI64, port uint8, payload []byte) {
	h.upMu.Lock()
	defer h.upMu.Unlock()
	h.upPorts = append(h.upPorts, port)
	h.upData = append(h.upData, append([]byte(nil), payload...))
}

func newHarness(t *testing.T) *e2eHarness {
	t.Helper()
	clk := clock.NewFakeClock(time.Unix(0, 0))
	space := radio.NewSpace(radio.Config{
		Clock: clk, PropagationDelay: 100 * time.Millisecond,
		SynchronousDelivery: true,
	})
	srv, err := NewNetworkServer(ServerConfig{
		NetID:     protocol.NetID{1, 2, 3},
		JoinEUI:   protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1},
		Space:     space,
		Clock:     clk,
		Store:     storage.NewMemoryStore(),
		RX1Delay:  5 * time.Second,
		RX2Delay:  6 * time.Second,
		RX1Window: time.Second,
		RX2Window: 2 * time.Second,
		Logger:    func(lvl, m string) { t.Logf("server[%s] %s", lvl, m) },
		OnUp:      nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	return &e2eHarness{t: t, clk: clk, space: space, server: srv, devices: map[string]*EndDevice{}}
}

func (h *e2eHarness) newDevice(name string, index byte) *EndDevice {
	var appKey lwcrypto.AES128Key
	for i := range appKey {
		appKey[i] = byte(i + 1)
	}
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, index}
	joinEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1}
	if err := h.server.RegisterDevice(devEUI, joinEUI, appKey); err != nil {
		h.t.Fatal(err)
	}
	dev, err := NewEndDevice(DeviceConfig{
		DevEUI:    devEUI,
		JoinEUI:   joinEUI,
		AppKey:    appKey,
		Space:     h.space,
		Endpoint:  radio.EndpointID(name),
		Server:    "server",
		Clock:     h.clk,
		Store:     storage.NewMemoryStore(),
		RX1Delay:  5 * time.Second,
		RX2Delay:  6 * time.Second,
		RX1Window: time.Second,
		RX2Window: 2 * time.Second,
		Logger:    func(lvl, m string) { h.t.Logf("device[%s] %s", lvl, m) },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := dev.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.devices[name] = dev
	return dev
}

// advance drives the virtual clock in small steps so all intermediate
// timers (uplink delivery, RX scheduling, propagation) fire deterministically.
func (h *e2eHarness) advance(d time.Duration) {
	step := 50 * time.Millisecond
	for elapsed := time.Duration(0); elapsed < d; elapsed += step {
		h.clk.Advance(step)
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

func TestE2EJoin(t *testing.T) {
	h := newHarness(t)
	dev := h.newDevice("dev1", 1)

	joinErr := make(chan error, 1)
	go func() { joinErr <- dev.Join(context.Background()) }()

	h.advance(8 * time.Second)

	select {
	case err := <-joinErr:
		if err != nil {
			t.Fatalf("join failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("join did not complete")
	}

	if !dev.Joined() {
		t.Fatal("device reports not joined")
	}
	sess, err := dev.Session()
	if err != nil {
		t.Fatal(err)
	}
	srvState, err := h.server.DeviceSnapshot(protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Keys != srvState.Keys {
		t.Fatalf("session keys differ:\ndevice %+v\nserver %+v", sess.Keys, srvState.Keys)
	}
	if sess.DevAddr != srvState.DevAddr {
		t.Fatalf("DevAddr mismatch %x vs %x", sess.DevAddr, srvState.DevAddr)
	}
	if sess.NwkFCntUp != 0 || srvState.NwkFCntUp != 0 {
		t.Fatal("initial frame counters must be zero")
	}
}

func joinDevice(t *testing.T, h *e2eHarness, name string, index byte) *EndDevice {
	dev := h.newDevice(name, index)
	done := make(chan error, 1)
	go func() { done <- dev.Join(context.Background()) }()
	h.advance(8 * time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("join: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("join timeout")
	}
	return dev
}

func TestE2EUnconfirmedUplink(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	dev := joinDevice(t, h, "dev1", 2)

	err := dev.Send(context.Background(), false, 5, []byte("temp=22.1"))
	if err != nil {
		t.Fatalf("unconfirmed send: %v", err)
	}
	h.advance(time.Second)

	h.upMu.Lock()
	defer h.upMu.Unlock()
	if len(h.upPorts) != 1 || h.upPorts[0] != 5 ||
		string(h.upData[0]) != "temp=22.1" {
		t.Fatalf("unexpected uplinks: %v %q", h.upPorts, h.upData)
	}
}

func TestE2EConfirmedUplinkACK(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	dev := joinDevice(t, h, "dev1", 3)

	errCh := make(chan error, 1)
	go func() { errCh <- dev.Send(context.Background(), true, 5, []byte("ping")) }()
	h.advance(8 * time.Second)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("confirmed send: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed send did not return")
	}

	sess, _ := dev.Session()
	if sess.LastUpAcked != 1 {
		t.Fatalf("LastUpAcked=%d want 1", sess.LastUpAcked)
	}
}

func TestE2EConfirmedRetransmitOnLoss(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	dev := joinDevice(t, h, "dev1", 4)

	var downCount int
	h.space.SetLossHook(func(from, to radio.EndpointID, f radio.Frame) (bool, bool) {
		if from == "server" && len(f.Payload) > 0 {
			mt := f.Payload[0] >> 5
			if mt == 3 || mt == 5 {
				downCount++
				if downCount == 1 {
					return false, false
				}
			}
		}
		return true, false
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- dev.Send(context.Background(), true, 5, []byte("retry-me"))
	}()
	h.advance(30 * time.Second)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("confirmed send eventually failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed send never returned")
	}

	h.upMu.Lock()
	defer h.upMu.Unlock()
	if len(h.upData) != 1 || string(h.upData[0]) != "retry-me" {
		t.Fatalf("expected exactly one app delivery, got %q", h.upData)
	}
}

func TestE2EAllACKsLostFails(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	dev := joinDevice(t, h, "dev1", 5)

	h.space.SetLossHook(func(from, to radio.EndpointID, f radio.Frame) (bool, bool) {
		if from == "server" {
			return false, false
		}
		return true, false
	})

	errCh := make(chan error, 1)
	go func() { errCh <- dev.Send(context.Background(), true, 5, []byte("x")) }()
	h.advance(60 * time.Second)

	select {
	case err := <-errCh:
		if err != ErrNoACK {
			t.Fatalf("want ErrNoACK, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("send should have exhausted retries")
	}
}

func TestE2EDuplicateDevNonceRejected(t *testing.T) {
	h := newHarness(t)
	var appKey lwcrypto.AES128Key
	for i := range appKey {
		appKey[i] = byte(i + 1)
	}
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 9}
	joinEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1}
	if err := h.server.RegisterDevice(devEUI, joinEUI, appKey); err != nil {
		t.Fatal(err)
	}

	// Device A joins successfully with a fixed DevNonce (0x1111).
	mkDev := func(ep string) *EndDevice {
		d, err := NewEndDevice(DeviceConfig{
			DevEUI:   devEUI,
			JoinEUI:  joinEUI,
			AppKey:   appKey,
			Space:    h.space,
			Endpoint: radio.EndpointID(ep),
			Server:   "server",
			Clock:    h.clk,
			Store:    storage.NewMemoryStore(),
			DevNonce: func() protocol.DevNonce { return protocol.DevNonce{0x11, 0x11} },
		})
		if err != nil {
			t.Fatal(err)
		}
		d.Start()
		return d
	}

	devA := mkDev("devA")
	done := make(chan error, 1)
	go func() { done <- devA.Join(context.Background()) }()
	h.advance(8 * time.Second)
	if err := waitDone(done); err != nil {
		t.Fatalf("first join should succeed: %v", err)
	}

	// Device B is a fresh (post-crash/reinstall) device instance that
	// replays the same DevNonce. The server must reject it by silence.
	devB := mkDev("devB")
	done2 := make(chan error, 1)
	go func() { done2 <- devB.Join(context.Background()) }()
	h.advance(10 * time.Second)
	if err := waitDone(done2); err == nil {
		t.Fatal("replay of used DevNonce unexpectedly accepted")
	}

	state, err := h.server.DeviceSnapshot(devEUI)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.UsedDevNonces) != 1 {
		t.Fatalf("expected one accepted nonce, got %d", len(state.UsedDevNonces))
	}
}

func waitDone(ch chan error) error {
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		return context.DeadlineExceeded
	}
}

func TestE2ECorruptUplinkDropped(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	dev := joinDevice(t, h, "dev1", 10)

	h.space.SetLossHook(func(from, to radio.EndpointID, f radio.Frame) (bool, bool) {
		if from == "dev1" && len(f.Payload) > 0 {
			mt := f.Payload[0] >> 5
			if mt == 2 || mt == 4 {
				return true, true // mark uplink bytes corrupt
			}
		}
		return true, false
	})

	err := dev.Send(context.Background(), false, 5, []byte("corrupt"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	h.advance(time.Second)

	h.upMu.Lock()
	defer h.upMu.Unlock()
	if len(h.upData) != 0 {
		t.Fatalf("corrupt frame delivered to application: %q", h.upData)
	}
}

func TestE2EReplayFrameRejected(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	dev := joinDevice(t, h, "dev1", 11)

	// Capture a legitimate uplink frame from the air, then replay it.
	var captured []byte
	h.space.SetLossHook(func(from, to radio.EndpointID, f radio.Frame) (bool, bool) {
		if from == "dev1" && len(f.Payload) > 0 {
			mt := f.Payload[0] >> 5
			if mt == 2 || mt == 4 {
				captured = append([]byte(nil), f.Payload...)
			}
		}
		return true, false
	})
	if err := dev.Send(context.Background(), false, 5, []byte("first")); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)
	if captured == nil {
		t.Fatal("frame not captured")
	}

	// Inject a raw replay directly at the space as if from the device.
	h.space.Transmit("dev1", "server", radio.Frame{Payload: captured, DataRate: 0})
	h.advance(time.Second)

	// Send a second fresh frame; server must still track counters correctly.
	h.space.SetLossHook(nil)
	if err := dev.Send(context.Background(), false, 5, []byte("second")); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)

	h.upMu.Lock()
	defer h.upMu.Unlock()
	if len(h.upData) != 2 || string(h.upData[0]) != "first" || string(h.upData[1]) != "second" {
		t.Fatalf("replay mishandled: %q", h.upData)
	}
}

func TestE2EClassADownlinkAfterUplink(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 12}
	downs := make(chan appMessage, 4)
	dev := h.newDevice("dev1", 12)
	dev.cfg.OnDown = func(port uint8, payload []byte) {
		downs <- appMessage{port: port, data: append([]byte(nil), payload...)}
	}
	join := joinDeviceWith(t, h, dev, devEUI)
	_ = join

	if err := h.server.QueueDownlink(devEUI, false, 8, []byte("led=on")); err != nil {
		t.Fatal(err)
	}

	// The queued downlink must NOT be delivered until the device uplinks.
	h.advance(7 * time.Second)
	select {
	case m := <-downs:
		t.Fatalf("downlink delivered without uplink: %+v", m)
	default:
	}

	// Any device uplink opens RX1; the server flushes the queue into it.
	go func() { _ = dev.Send(context.Background(), false, 5, []byte("poke")) }()
	h.advance(8 * time.Second)

	select {
	case m := <-downs:
		if m.port != 8 || string(m.data) != "led=on" {
			t.Fatalf("bad downlink: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("queued downlink not delivered in RX window")
	}

	// The downlink counter must have advanced on both sides.
	sess, _ := dev.Session()
	if sess.NwkFCntDown != 1 {
		t.Fatalf("device NFCntDown=%d want 1", sess.NwkFCntDown)
	}
	state, _ := h.server.DeviceSnapshot(devEUI)
	if state.NwkFCntDown != 1 {
		t.Fatalf("server NFCntDown=%d want 1", state.NwkFCntDown)
	}
}

type appMessage struct {
	port uint8
	data []byte
}

// joinDeviceWith joins an already-constructed device (used when the test
// needs to set the OnDown callback before join registration).
func joinDeviceWith(t *testing.T, h *e2eHarness, dev *EndDevice, _ protocol.EUI64) *EndDevice {
	done := make(chan error, 1)
	go func() { done <- dev.Join(context.Background()) }()
	h.advance(8 * time.Second)
	if err := waitDone(done); err != nil {
		t.Fatalf("join: %v", err)
	}
	return dev
}

func TestE2EServerLinkADR(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 13}
	dev := joinDevice(t, h, "dev1", 13)

	before, _ := dev.Session()
	if before.ADR.DataRate != 0 {
		t.Fatalf("initial DR=%d want 0", before.ADR.DataRate)
	}

	if err := h.server.ArmLinkADR(devEUI, 3, 14); err != nil {
		t.Fatal(err)
	}
	// The LinkADRReq rides in the downlink FOpts following an uplink.
	if err := dev.Send(context.Background(), false, 5, []byte("t")); err != nil {
		t.Fatal(err)
	}
	h.advance(8 * time.Second)

	sess, _ := dev.Session()
	if sess.ADR.DataRate != 3 {
		t.Fatalf("device DR after LinkADR=%d want 3", sess.ADR.DataRate)
	}
	if sess.NwkFCntUp != 1 {
		t.Fatalf("frame counters must be preserved during ADR, FCntUp=%d", sess.NwkFCntUp)
	}
	if !dev.Joined() {
		t.Fatal("join state lost after ADR")
	}
}

func TestE2EADRFallbackOnBadMargin(t *testing.T) {
	h := newHarness(t)
	dev := joinDevice(t, h, "dev1", 14)
	// push device up to DR3 via a valid server command
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 14}
	if err := h.server.ArmLinkADR(devEUI, 3, 14); err != nil {
		t.Fatal(err)
	}
	_ = dev.Send(context.Background(), false, 5, []byte("t"))
	h.advance(8 * time.Second)
	if s, _ := dev.Session(); s.ADR.DataRate != 3 {
		t.Fatalf("setup DR=%d", s.ADR.DataRate)
	}

	// bad margins force device-side fallback
	dev.SetLinkMargin(-8)
	dev.SetLinkMargin(-8)
	s, _ := dev.Session()
	if s.ADR.DataRate >= 3 {
		t.Fatalf("expected fallback below DR3, got %d", s.ADR.DataRate)
	}
}

func TestE2EDutyCycleQueuesSends(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	dev := joinDevice(t, h, "dev1", 15)

	// Force a strict 1% duty cycle with a fixed long air time.
	dev.mu.Lock()
	dev.duty = NewDutyTracker(h.clk, 0.01, func(uint8, int) time.Duration {
		return 500 * time.Millisecond
	})
	dev.cfg.MaxQueuedSends = 4
	dev.mu.Unlock()

	// First send transmits immediately but blocks the channel for ~49.5s.
	firstDone := make(chan error, 1)
	go func() { firstDone <- dev.Send(context.Background(), false, 5, []byte("a")) }()
	h.advance(2 * time.Second)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("first send should complete after emission")
	}

	// Second send is accepted (queued) but cannot emit until ~50s elapse.
	secondDone := make(chan error, 1)
	go func() { secondDone <- dev.Send(context.Background(), false, 5, []byte("b")) }()
	h.advance(time.Millisecond)
	if dev.PendingQueued() == 0 {
		t.Fatal("expected the blocked send to be queued")
	}
	h.advance(40 * time.Second)
	select {
	case err := <-secondDone:
		t.Fatalf("second send emitted during duty block: %v", err)
	default:
	}
	h.advance(15 * time.Second)
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second send after duty release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued send did not run after duty cycle release")
	}
}

func TestE2EDutyCycleQueueFullRejected(t *testing.T) {
	h := newHarness(t)
	dev := joinDevice(t, h, "dev1", 16)
	dev.mu.Lock()
	dev.duty = NewDutyTracker(h.clk, 0.01, func(uint8, int) time.Duration {
		return 500 * time.Millisecond
	})
	dev.cfg.MaxQueuedSends = 1
	dev.mu.Unlock()

	firstDone := make(chan error, 1)
	go func() { firstDone <- dev.Send(context.Background(), false, 5, []byte("a")) }()
	h.advance(2 * time.Second)
	<-firstDone

	// one queued slot fills, the next request must be rejected deterministically
	go func() { _ = dev.Send(context.Background(), false, 5, []byte("b")) }()
	h.advance(time.Millisecond)
	if err := dev.Send(context.Background(), false, 5, []byte("c")); err != ErrSendQueueFull {
		t.Fatalf("want ErrSendQueueFull, got %v", err)
	}
}

func TestE2EMultiDeviceConcurrency(t *testing.T) {
	h := newHarness(t)
	var recvMu sync.Mutex
	counts := map[string]int{}
	h.server.cfg.OnUp = func(_ protocol.EUI64, _ uint8, payload []byte) {
		recvMu.Lock()
		counts[string(payload)]++
		recvMu.Unlock()
	}

	const n = 6
	devs := make([]*EndDevice, n)
	for i := 0; i < n; i++ {
		devs[i] = joinDevice(t, h, "d"+string(rune('a'+i)), byte(20+i))
	}

	h.space.SetLossHook(func(from, to radio.EndpointID, f radio.Frame) (bool, bool) {
		if (to == "da" || to == "db") && from == "server" {
			return false, false
		}
		return true, false
	})

	var wg sync.WaitGroup
	for i, dev := range devs {
		wg.Add(1)
		go func(i int, d *EndDevice) {
			defer wg.Done()
			switch i {
			case 0, 1:
				_ = d.Send(context.Background(), true, 5, []byte("storm"))
			default:
				if err := d.Send(context.Background(), false, 5,
					[]byte("quiet-"+string(rune('a'+i)))); err != nil {
					t.Errorf("device %d send: %v", i, err)
				}
			}
		}(i, dev)
	}
	h.advance(60 * time.Second)
	wg.Wait()

	recvMu.Lock()
	defer recvMu.Unlock()
	for i := 2; i < n; i++ {
		want := "quiet-" + string(rune('a'+i))
		if counts[want] != 1 {
			t.Fatalf("device %c payload %q delivered %d times, want 1",
				rune('a'+i), want, counts[want])
		}
	}
}

func TestE2ECrashRecovery(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFakeClock(time.Unix(0, 0))
	space := radio.NewSpace(radio.Config{
		Clock: clk, PropagationDelay: 100 * time.Millisecond,
		SynchronousDelivery: true,
	})
	appKey := func() lwcrypto.AES128Key {
		var k lwcrypto.AES128Key
		for i := range k {
			k[i] = byte(i*2 + 1)
		}
		return k
	}()
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 30}
	joinEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1}

	var upMu2 sync.Mutex
	upCount := 0
	buildAll := func() (*NetworkServer, *EndDevice) {
		srv, err := NewNetworkServer(ServerConfig{
			NetID: protocol.NetID{1, 2, 3}, JoinEUI: joinEUI,
			Space: space, Clock: clk,
			Store: storage.MustOpen(dir + "/server"),
			OnUp: func(_ protocol.EUI64, _ uint8, _ []byte) {
				upMu2.Lock()
				upCount++
				upMu2.Unlock()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		srv.Start()
		if _, err := srv.DeviceSnapshot(devEUI); err != nil {
			if err := srv.RegisterDevice(devEUI, joinEUI, appKey); err != nil {
				t.Fatal(err)
			}
		}
		dev, err := NewEndDevice(DeviceConfig{
			DevEUI: devEUI, JoinEUI: joinEUI, AppKey: appKey,
			Space: space, Endpoint: "dev1", Server: "server",
			Clock: clk, Store: storage.MustOpen(dir + "/device"),
		})
		if err != nil {
			t.Fatal(err)
		}
		dev.Start()
		return srv, dev
	}

	srv, dev := buildAll()
	joinDone := make(chan error, 1)
	go func() { joinDone <- dev.Join(context.Background()) }()
	advanceClk(clk, 8*time.Second)
	if err := waitDone(joinDone); err != nil {
		t.Fatal(err)
	}

	// two unconfirmed uplinks before the crash
	for i := 0; i < 2; i++ {
		if err := dev.Send(context.Background(), false, 5, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		advanceClk(clk, time.Second)
	}
	srv.Close()
	dev.Close()

	// simulate a restart: rebuild everything from the persisted files
	srv2, dev2 := buildAll()
	defer srv2.Close()
	defer dev2.Close()

	if !dev2.Joined() {
		t.Fatal("device did not recover joined state")
	}
	joined, _ := srv2.DeviceJoined(devEUI)
	if !joined {
		t.Fatal("server did not recover joined state")
	}

	oldSess, _ := dev.Session()
	newSess, _ := newSessErr(dev2)
	if oldSess.Keys != newSess.Keys || oldSess.DevAddr != newSess.DevAddr {
		t.Fatal("recovered session keys/address differ")
	}
	if newSess.NwkFCntUp != 2 {
		t.Fatalf("recovered FCntUp=%d want 2", newSess.NwkFCntUp)
	}

	// After restart the next uplink MUST use FCnt 3 (no counter reuse).
	upMu2.Lock()
	upBefore := upCount
	upMu2.Unlock()
	if err := dev2.Send(context.Background(), false, 5, []byte("after-crash")); err != nil {
		t.Fatal(err)
	}
	advanceClk(clk, time.Second)
	upMu2.Lock()
	got := upCount - upBefore
	upMu2.Unlock()
	if got != 1 {
		t.Fatalf("post-crash uplink delivered %d times", got)
	}
	state, _ := srv2.DeviceSnapshot(devEUI)
	if state.NwkFCntUp != 3 {
		t.Fatalf("server FCntUp after crash uplink=%d want 3", state.NwkFCntUp)
	}
}

func newSessErr(d *EndDevice) (DeviceSession, error) { return d.Session() }

func drainCount(ch chan int) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

func advanceClk(clk *clock.FakeClock, d time.Duration) {
	step := 50 * time.Millisecond
	for e := time.Duration(0); e < d; e += step {
		clk.Advance(step)
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

func TestE2EACKViaRX2Fallback(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 40}
	dev := joinDevice(t, h, "dev1", 40)
	if err := h.server.ScheduleRX2FallbackFor(devEUI, true); err != nil {
		t.Fatal(err)
	}

	// Lose only the RX1 copy (arrives ~5.1s), allow the RX2 copy (~6.1s).
	h.space.SetLossHook(func(from, to radio.EndpointID, f radio.Frame) (bool, bool) {
		if from != "server" {
			return true, false
		}
		if !f.ReceivedAt.IsZero() {
			// RX1 frames are scheduled for emission at t+5s, RX2 at t+6s.
			if f.SentAt.Sub(time.Unix(0, 0)) < 5500*time.Millisecond {
				return false, false
			}
		}
		return true, false
	})

	errCh := make(chan error, 1)
	go func() { errCh <- dev.Send(context.Background(), true, 5, []byte("rx2")) }()
	h.advance(9 * time.Second)
	if err := waitDone(errCh); err != nil {
		t.Fatalf("confirmed send should succeed via RX2: %v", err)
	}
}

func TestE2EDutyCycleDropPolicy(t *testing.T) {
	h := newHarness(t)
	var appKey lwcrypto.AES128Key
	for i := range appKey {
		appKey[i] = byte(i + 1)
	}
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 17}
	if err := h.server.RegisterDevice(devEUI, protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1}, appKey); err != nil {
		t.Fatal(err)
	}
	dev, err := NewEndDevice(DeviceConfig{
		DevEUI: devEUI, JoinEUI: protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1},
		AppKey: appKey, Space: h.space, Endpoint: "d17", Server: "server",
		Clock: h.clk, Store: storage.NewMemoryStore(), DropIfBlocked: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	dev.Start()
	join := make(chan error, 1)
	go func() { join <- dev.Join(context.Background()) }()
	h.advance(8 * time.Second)
	if err := waitDone(join); err != nil {
		t.Fatal(err)
	}

	dev.mu.Lock()
	dev.duty = NewDutyTracker(h.clk, 0.01, func(uint8, int) time.Duration {
		return 500 * time.Millisecond
	})
	dev.mu.Unlock()

	go func() { _ = dev.Send(context.Background(), false, 5, []byte("a")) }()
	h.advance(2 * time.Second)

	if err := dev.Send(context.Background(), false, 5, []byte("b")); err != ErrDutyCycleBlocked {
		t.Fatalf("drop policy: want ErrDutyCycleBlocked, got %v", err)
	}
}

func TestE2EDuplicateDownlinkDropped(t *testing.T) {
	h := newHarness(t)
	got := make(chan uint8, 4)
	dev := h.newDevice("dev1", 50)
	dev.cfg.OnDown = func(port uint8, _ []byte) { got <- port }
	joinDone := make(chan error, 1)
	go func() { joinDone <- dev.Join(context.Background()) }()
	h.advance(8 * time.Second)
	if err := waitDone(joinDone); err != nil {
		t.Fatal(err)
	}

	sess, sessErr := dev.Session()
	if sessErr != nil {
		t.Fatalf("session: %v", sessErr)
	}
	mkDown := func(fcnt uint32, ack bool, port int, payload []byte) []byte {
		f := protocol.DataFrame{
			MType:      protocol.MTUnconfirmedDataDN,
			DevAddr:    sess.DevAddr,
			FCtrl:      protocol.FCtrl{ACK: ack},
			FCnt:       uint16(fcnt),
			FPort:      port,
			FRMPayload: payload,
		}
		raw, err := protocol.MarshalDataFrame(sess.Keys, f, fcnt)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	// Open RX1 right around "now" so the injected frame lands inside it.
	openRX := func() {
		dev.mu.Lock()
		dev.rx1Open = h.clk.Now().Add(-time.Millisecond)
		dev.rx1Close = h.clk.Now().Add(time.Second)
		dev.rx2Open = h.clk.Now().Add(2 * time.Second)
		dev.rx2Close = h.clk.Now().Add(4 * time.Second)
		dev.mu.Unlock()
	}
	inject := func(raw []byte, corrupt bool) {
		openRX()
		dev.handleFrame("server", radio.Frame{
			Payload: raw, DataRate: 0, Corrupt: corrupt,
			ReceivedAt: h.clk.Now().Add(10 * time.Millisecond),
		})
	}

	raw1 := mkDown(1, false, 8, []byte("one"))
	inject(raw1, false)
	select {
	case p := <-got:
		if p != 8 {
			t.Fatalf("port=%d", p)
		}
	default:
		t.Fatal("first downlink not delivered")
	}

	// replay of the identical frame must not redeliver
	inject(raw1, false)
	select {
	case <-got:
		t.Fatal("replayed downlink redelivered")
	default:
	}

	// a corrupt frame inside window must be dropped
	bad := append([]byte(nil), raw1...)
	bad[12] ^= 0xff
	inject(bad, true)
	select {
	case <-got:
		t.Fatal("corrupt downlink delivered")
	default:
	}

	// next valid counter delivers once
	raw2 := mkDown(2, false, 9, []byte("two"))
	inject(raw2, false)
	select {
	case p := <-got:
		if p != 9 {
			t.Fatalf("second downlink port=%d", p)
		}
	default:
		t.Fatal("second downlink not delivered")
	}

	s2, _ := dev.Session()
	if s2.NwkFCntDown != 2 {
		t.Fatalf("NFCntDown=%d want 2", s2.NwkFCntDown)
	}
}

func TestE2EFCntOverflowRequiresRejoin(t *testing.T) {
	h := newHarness(t)
	dev := joinDevice(t, h, "dev1", 60)

	// Drive the uplink counter to its 16-bit-representable high end via the
	// replay window semantics. The documented overflow policy triggers
	// when no strictly larger in-window counter can be used; we set the
	// counter to max-1, send the final frame (max), then the next send
	// must fail with ErrFCntOverflow.
	dev.mu.Lock()
	dev.session.NwkFCntUp = 0xFFFFFFFE
	if err := dev.persistLocked(); err != nil {
		t.Fatal(err)
	}
	dev.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- dev.Send(context.Background(), false, 5, []byte("max")) }()
	h.advance(2 * time.Second)
	if err := waitDone(errCh); err != nil {
		t.Fatal(err) // max value 0xFFFFFFFF used successfully
	}

	// The next send cannot allocate a strictly larger 32-bit counter.
	errCh2 := make(chan error, 1)
	go func() { errCh2 <- dev.Send(context.Background(), false, 5, []byte("overflow")) }()
	h.advance(2 * time.Second)
	select {
	case err := <-errCh2:
		if err != ErrFCntOverflow {
			t.Fatalf("want ErrFCntOverflow, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("overflow send did not resolve")
	}
}

func TestE2EConfirmedDownlinkAcked(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.OnUp = h.onUp
	devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 70}
	downs := make(chan appMessage, 4)
	dev := h.newDevice("dev1", 70)
	dev.cfg.OnDown = func(port uint8, p []byte) { downs <- appMessage{port: port, data: p} }
	done := make(chan error, 1)
	go func() { done <- dev.Join(context.Background()) }()
	h.advance(8 * time.Second)
	if err := waitDone(done); err != nil {
		t.Fatal(err)
	}

	if err := h.server.QueueDownlink(devEUI, true, 9, []byte("conf-dn")); err != nil {
		t.Fatal(err)
	}
	if err := dev.Send(context.Background(), false, 5, []byte("pull")); err != nil {
		t.Fatal(err)
	}
	h.advance(8 * time.Second)
	select {
	case m := <-downs:
		if m.port != 9 || string(m.data) != "conf-dn" {
			t.Fatalf("bad confirmed downlink %+v", m)
		}
	default:
		t.Fatal("confirmed downlink not delivered")
	}

	if err := dev.Send(context.Background(), false, 5, []byte("ack-uplink")); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Second)

	state, err := h.server.DeviceSnapshot(devEUI)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastDownAckedFCnt != 1 {
		t.Fatalf("server did not record downlink ACK: %d", state.LastDownAckedFCnt)
	}
}

func TestE2ECounterGapDesyncRejected(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.MaxFCntGap = 5
	h.server.cfg.OnUp = h.onUp
	dev := joinDevice(t, h, "dev1", 80)

	// establish counter 1
	if err := dev.Send(context.Background(), false, 5, []byte("one")); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)

	// force the device to jump far ahead (beyond the replay window)
	dev.mu.Lock()
	dev.session.NwkFCntUp = 100
	if err := dev.persistLocked(); err != nil {
		t.Fatal(err)
	}
	dev.mu.Unlock()
	if err := dev.Send(context.Background(), false, 5, []byte("far")); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)

	h.upMu.Lock()
	defer h.upMu.Unlock()
	if len(h.upData) != 1 || string(h.upData[0]) != "one" {
		t.Fatalf("desynced far-ahead frame must be rejected, got %q", h.upData)
	}
}

func TestE2EMalformedGarbageNoPanic(t *testing.T) {
	h := newHarness(t)
	dev := joinDevice(t, h, "dev1", 90)
	_ = dev

	garbage := [][]byte{
		{},
		{0xff},
		make([]byte, 8),
		make([]byte, 100),
		{0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	for _, g := range garbage {
		// must not panic and must not mutate state
		h.server.handleFrame("evil", radio.Frame{Payload: g})
		dev.handleFrame("evil", radio.Frame{Payload: g})
	}
	if !dev.Joined() {
		t.Fatal("garbage frames must not corrupt join state")
	}
}

func TestE2EServerRejectsLateJoinAccept(t *testing.T) {
	h := newHarness(t)
	dev := joinDevice(t, h, "dev1", 95)
	sess1, _ := dev.Session()

	// A second join after a successful one: current policy treats the device
	// as already joined and returns immediately without a new exchange.
	if err := dev.Join(context.Background()); err != nil {
		t.Fatalf("idempotent rejoin: %v", err)
	}
	sess2, _ := dev.Session()
	if sess2.DevAddr != sess1.DevAddr {
		t.Fatal("rejoin should keep the established session until counters overflow")
	}
}
