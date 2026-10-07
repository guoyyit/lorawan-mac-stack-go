package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/server"
)

// A device that continuously injects oversize and structurally malformed
// frames must not kill the server or its worker, and must not block, lose or
// cross-talk traffic for healthy devices.
func TestOversizeAndMalformedStormIsolation(t *testing.T) {
	bad := provision(70)
	good1 := provision(71)
	good2 := provision(72)

	var got1, got2 int64
	h := newHarness(t, func(id lorawan.EUI, p []byte) {
		switch id {
		case good1.DevEUI:
			atomic.AddInt64(&got1, 1)
		case good2.DevEUI:
			atomic.AddInt64(&got2, 1)
		}
	}, bad, good1, good2)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	bd := h.newDevice(t, bad, nil)
	if err := bd.Join(ctx); err != nil {
		t.Fatal(err)
	}
	bs := bd.Session()

	// A valid max-limit frame from the bad session, then tampered/extended
	// variants: one byte over the cap, a far-oversize buffer, and frames whose
	// advertised length fields disagree with the bytes on hand.
	atLimit, err := lorawan.EncodeFrame(&bs, &lorawan.Frame{
		MType:   lorawan.MTypeConfirmedDataUp,
		DevAddr: bs.DevAddr,
		FCnt:    1000,
		HasPort: true,
		FPort:   1,
		Payload: make([]byte, lorawan.MaxPayload),
	})
	if err != nil {
		t.Fatal(err)
	}
	oneOver := append(append([]byte(nil), atLimit...), 0)
	farOver := make([]byte, 1<<18)
	farOver[0] = byte(lorawan.MTypeConfirmedDataUp)
	copy(farOver[2:6], bs.DevAddr[:])
	lying := make([]byte, 40)
	lying[0] = byte(lorawan.MTypeConfirmedDataUp)
	copy(lying[2:6], bs.DevAddr[:])
	lying[11], lying[12] = 0xff, 0xff // payloadLen 65535, only 40 bytes present
	short := make([]byte, 10)
	short[0] = byte(lorawan.MTypeConfirmedDataUp)
	copy(short[2:6], bs.DevAddr[:])
	garbage := []byte{0x40, 0, 1, 2, 3, 4, 0, 0, 0, 0}
	badMIC := append([]byte(nil), atLimit...)
	badMIC[len(badMIC)-1] ^= 0xaa

	flavors := [][]byte{oneOver, farOver, lying, short, garbage, badMIC}

	stop := make(chan struct{})
	stormDone := make(chan struct{})
	go func() {
		defer close(stormDone)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				_ = bd.Air().Send(ctx, flavors[i%len(flavors)])
				i++
			}
		}
	}()

	// Healthy devices join and exchange confirmed traffic throughout.
	d1 := h.newDevice(t, good1, nil)
	d2 := h.newDevice(t, good2, nil)
	if err := d1.Join(ctx); err != nil {
		t.Fatalf("good1 join: %v", err)
	}
	if err := d2.Join(ctx); err != nil {
		t.Fatalf("good2 join: %v", err)
	}
	for i := 0; i < 6; i++ {
		if err := d1.SendConfirmed(ctx, []byte("g1")); err != nil {
			t.Fatalf("good1 send %d: %v", i, err)
		}
		if err := d2.SendConfirmed(ctx, []byte("g2")); err != nil {
			t.Fatalf("good2 send %d: %v", i, err)
		}
	}
	close(stop)
	<-stormDone

	if atomic.LoadInt64(&got1) != 6 || atomic.LoadInt64(&got2) != 6 {
		t.Fatalf("healthy deliveries got1=%d got2=%d, want 6/6", got1, got2)
	}
	// The server stays responsive and reports bad frames for the storm peer.
	if _, _, ok := h.server.Stats(bad.DevEUI); !ok {
		t.Fatal("server died: stats unavailable after storm")
	}
	ss, err := h.server.Session(good1.DevEUI)
	if err != nil || !ss.Joined || ss.FCntUp != 6 {
		t.Fatalf("good1 server state wrong after storm: %+v %v", ss, err)
	}
}

// EnqueueDown with an oversized payload must return ErrFrameTooLong and leave
// the queue and counters untouched; subsequent legal downlinks still flow.
func TestServerRejectsOversizeDownlinkEnqueue(t *testing.T) {
	p := provision(73)
	var got int64
	h := newHarness(t, func(_ lorawan.EUI, _ []byte) {}, p)
	d := h.newDevice(t, p, func(_ []byte) { atomic.AddInt64(&got, 1) })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}

	before, _ := h.server.Session(p.DevEUI)
	if err := h.server.EnqueueDown(p.DevEUI, make([]byte, lorawan.MaxPayload+1)); err != lorawan.ErrFrameTooLong {
		t.Fatalf("enqueue oversize = %v, want ErrFrameTooLong", err)
	}
	after, _ := h.server.Session(p.DevEUI)
	if after.FCntDown != before.FCntDown {
		t.Fatalf("FCntDown changed %d -> %d on rejected enqueue", before.FCntDown, after.FCntDown)
	}

	// Legal downlink still delivered after the rejected one.
	if err := h.server.EnqueueDown(p.DevEUI, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := d.SendUnconfirmed(ctx, []byte("poke")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt64(&got) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if atomic.LoadInt64(&got) != 1 {
		t.Fatal("legal downlink not delivered after rejected enqueue")
	}
}

// Concurrent joins and confirmed uplinks/downlinks for several healthy devices
// while one peer hammers the server with oversize/malformed frames: every
// healthy device gets exactly its own uplinks ACKed and its own downlinks once,
// and per-device counters stay deterministic.
func TestConcurrentFaultyPeerDoesNotCrossTalk(t *testing.T) {
	const healthy = 4
	provs := []server.DeviceProvision{provision(74)} // faulty peer
	for i := byte(0); i < healthy; i++ {
		provs = append(provs, provision(75+i))
	}

	var mu sync.Mutex
	deliv := map[byte]int{}
	h := newHarness(t, func(id lorawan.EUI, _ []byte) {
		mu.Lock()
		deliv[id[7]]++
		mu.Unlock()
	}, provs...)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	faulty := h.newDevice(t, provs[0], nil)
	if err := faulty.Join(ctx); err != nil {
		t.Fatal(err)
	}
	fs := faulty.Session()
	huge := make([]byte, 1<<17)
	huge[0] = byte(lorawan.MTypeConfirmedDataUp)
	copy(huge[2:6], fs.DevAddr[:])
	junk := make([]byte, 30)
	junk[0] = byte(lorawan.MTypeConfirmedDataUp)
	copy(junk[2:6], fs.DevAddr[:])

	stop := make(chan struct{})
	faultDone := make(chan struct{})
	go func() {
		defer close(faultDone)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				if i%2 == 0 {
					_ = faulty.Air().Send(ctx, huge)
				} else {
					_ = faulty.Air().Send(ctx, junk)
				}
				i++
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 1; i <= healthy; i++ {
		p := provs[i]
		wg.Add(1)
		go func(idx int, prov server.DeviceProvision) {
			defer wg.Done()
			var local int
			d := h.newDevice(t, prov, func(_ []byte) { local++ })
			if err := d.Join(ctx); err != nil {
				t.Errorf("device %d join: %v", idx, err)
				return
			}
			for j := 0; j < 4; j++ {
				// A downlink rides the ACK window of each confirmed uplink.
				if err := h.server.EnqueueDown(prov.DevEUI, []byte{byte(idx), byte(j)}); err != nil {
					t.Errorf("enqueue %d: %v", idx, err)
					return
				}
				if err := d.SendConfirmed(ctx, []byte{byte(idx), byte(j)}); err != nil {
					t.Errorf("device %d send %d: %v", idx, j, err)
					return
				}
			}
			// Allow async RX1 delivery to settle before asserting locally.
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) && local < 4 {
				time.Sleep(2 * time.Millisecond)
			}
			if local != 4 {
				t.Errorf("device %d got %d downlinks, want 4", idx, local)
			}
			ss, err := h.server.Session(prov.DevEUI)
			if err != nil || ss.FCntUp != 4 || ss.FCntDown != 4 {
				t.Errorf("device %d counters up=%d down=%d err=%v, want 4/4",
					idx, ss.FCntUp, ss.FCntDown, err)
			}
		}(i, p)
	}
	wg.Wait()
	close(stop)
	<-faultDone

	for i := 1; i <= healthy; i++ {
		if deliv[provs[i].DevEUI[7]] != 4 {
			t.Fatalf("server-side deliveries for device %d = %d, want 4",
				i, deliv[provs[i].DevEUI[7]])
		}
	}
	// Faulty peer's own counters never advanced from its malformed traffic.
	fsAfter, err := h.server.Session(provs[0].DevEUI)
	if err != nil || fsAfter.FCntUp != 0 {
		t.Fatalf("faulty peer FCntUp = %d err=%v, malformed frames advanced it",
			fsAfter.FCntUp, err)
	}
}

// A max-size queued payload and a pending LinkADRReq cannot share one frame;
// neither may be lost: the MAC command goes out alone first and the payload is
// delivered on the following uplink's window.
func TestMaxPayloadAndLinkADRNotLost(t *testing.T) {
	p := provision(79)
	var got int64
	h := newHarness(t, nil, p)
	d := h.newDevice(t, p, func(_ []byte) { atomic.AddInt64(&got, 1) })
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := d.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.server.EnqueueDown(p.DevEUI, make([]byte, lorawan.MaxPayload)); err != nil {
		t.Fatal(err)
	}
	if err := h.server.SetLinkADR(p.DevEUI, 5, 2); err != nil {
		t.Fatal(err)
	}
	if err := d.SendConfirmed(ctx, []byte("up1")); err != nil {
		t.Fatal(err)
	}
	// Window 1 should carry the LinkADRReq (content, counter 0); answer it,
	// and window 2 carries the queued payload (counter 1).
	if err := d.SendUnconfirmed(ctx, []byte("up2")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if acks, _ := h.server.ADRACKCount(p.DevEUI); acks >= 1 && atomic.LoadInt64(&got) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if acks, _ := h.server.ADRACKCount(p.DevEUI); acks < 1 {
		t.Fatalf("LinkADRReq lost, acks=%d", acks)
	}
	if atomic.LoadInt64(&got) != 1 {
		t.Fatal("max-size payload lost when combined with a LinkADRReq")
	}
	if down := d.Session().FCntDown; down != 2 {
		t.Fatalf("FCntDown = %d, want 2 (command + payload)", down)
	}
}
