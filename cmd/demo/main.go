// Command demo runs a local end-to-end Class A OTAA scenario over an
// in-memory loopback, including confirmed uplink retransmission, Class A
// downlink queuing and a crash/restart recovery from on-disk session state.
//
// Run with:  go run ./cmd/demo
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/internal/device"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/lorawan"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/mac"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/server"
	"github.com/guoyyit/lorawan-mac-stack-go/internal/transport"
)

func main() {
	logger := log.New(os.Stdout, "demo: ", log.LstdFlags|log.Lmsgprefix)

	base, err := os.MkdirTemp("", "lorawan-demo-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(base)

	prov := server.DeviceProvision{
		DevEUI:  lorawan.EUI{0, 0, 0, 0, 0, 0, 0, 0x01},
		JoinEUI: lorawan.EUI{0, 0, 0, 0, 0, 0, 0, 0xee},
		AppKey:  lorawan.AESKey{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var upCount int64

	// ---- Phase 1: bring up the network and a device, OTAA join ----
	air := transport.NewMedium()
	srv := mustServer(base, air, prov, logger, &upCount)
	dev := mustDevice(base, 1, air, prov, logger)

	if err := dev.Join(ctx); err != nil {
		log.Fatalf("join failed: %v", err)
	}
	sess := dev.Session()
	logger.Printf("joined: DevAddr=%02x NwkSKey=%04x AppSKey=%04x",
		sess.DevAddr, sess.NwkSKey[:2], sess.AppSKey[:2])

	// ---- Phase 2: confirmed uplink with an injected first-attempt loss ----
	dev.Air().DropNext(1)
	if err := dev.SendConfirmed(ctx, []byte("confirmed-hello")); err != nil {
		log.Fatalf("confirmed uplink failed: %v", err)
	}
	logger.Printf("confirmed uplink delivered after retransmission; server receipts=%d",
		atomic.LoadInt64(&upCount))

	// ---- Phase 3: Class A downlink delivered in the next window ----
	if err := srv.EnqueueDown(prov.DevEUI, []byte("downlink-payload")); err != nil {
		log.Fatal(err)
	}
	if err := dev.SendUnconfirmed(ctx, []byte("open-the-window")); err != nil {
		log.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	// ---- Phase 4: server-driven ADR ----
	if err := srv.SetLinkADR(prov.DevEUI, 5, 2); err != nil {
		log.Fatal(err)
	}
	if err := dev.SendConfirmed(ctx, []byte("adapt-rate")); err != nil {
		log.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	logger.Printf("after LinkADR: device DR=%d TXPower=%d",
		dev.Session().DataRate, dev.Session().TXPower)

	fcntBeforeCrash := dev.Session().FCntUp

	// ---- Phase 5: crash both sides; rebuild from on-disk sessions ----
	if err := srv.Close(); err != nil {
		log.Fatal(err)
	}
	if err := dev.Close(); err != nil {
		log.Fatal(err)
	}
	logger.Print("--- both sides crashed; restarting from disk ---")

	air2 := transport.NewMedium()
	srv2 := mustServer(base, air2, prov, logger, &upCount)
	defer srv2.Close()
	dev2 := mustDevice(base, 1, air2, prov, logger)
	defer dev2.Close()

	s2 := dev2.Session()
	logger.Printf("restored session: joined=%v DevAddr=%02x FCntUp=%d",
		s2.Joined, s2.DevAddr, s2.FCntUp)

	if err := dev2.SendConfirmed(ctx, []byte("after-restart")); err != nil {
		log.Fatalf("post-restart traffic failed: %v", err)
	}
	if got := dev2.Session().FCntUp; got != fcntBeforeCrash+1 {
		log.Fatalf("counter reused or reset: %d != %d", got, fcntBeforeCrash+1)
	}
	logger.Printf("post-restart confirmed uplink OK, FCntUp=%d (no counter reuse)",
		dev2.Session().FCntUp)

	logger.Print("all phases completed successfully")
}

func mustServer(base string, air *transport.Medium, prov server.DeviceProvision,
	logger *log.Logger, upCount *int64) *server.Server {
	store, err := lorawan.NewFileStore(lorawan.FileStoreConfig{
		Dir: filepath.Join(base, "server"), AtomicRename: true, Fsync: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	srv, err := server.New(server.Config{
		NetID:    lorawan.NetID{0, 0, 1},
		Store:    store,
		Air:      air.ServerEnd(),
		Devices:  []server.DeviceProvision{prov},
		RX1Delay: 100 * time.Millisecond,
		RX2Delay: 200 * time.Millisecond,
		RXWindow: 100 * time.Millisecond,
		OnAppData: func(_ lorawan.EUI, b []byte) {
			n := atomic.AddInt64(upCount, 1)
			logger.Printf("server app data (#%d): %q", n, b)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	return srv
}

func mustDevice(base string, n byte, air *transport.Medium,
	prov server.DeviceProvision, logger *log.Logger) *device.Device {
	store, err := lorawan.NewFileStore(lorawan.FileStoreConfig{
		Dir: filepath.Join(base, fmt.Sprintf("device-%d", n)), AtomicRename: true, Fsync: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	dev, err := device.New(device.Config{
		DevEUI:     prov.DevEUI,
		JoinEUI:    prov.JoinEUI,
		AppKey:     prov.AppKey,
		Store:      store,
		Air:        air.NewDeviceEnd(),
		RX1Delay:   100 * time.Millisecond,
		RX2Delay:   200 * time.Millisecond,
		RXWindow:   100 * time.Millisecond,
		Retries:    4,
		Backoff:    50 * time.Millisecond,
		BackoffMax: 400 * time.Millisecond,
		Airtime:    20 * time.Millisecond,
		Duty:       mac.DutyConfig{MaxDutyCycle: 0.01, Policy: mac.DutyWait},
		ADR: mac.ADRConfig{
			Enabled: true, MinDR: 0, MaxDR: 5,
			MinTXPower: 0, MaxTXPower: 3, UpMargin: 10, DownMargin: -15,
			BackoffMisses: 3,
		},
		InitialDR: 3,
		OnAppData: func(b []byte) { logger.Printf("device app data: %q", b) },
	})
	if err != nil {
		log.Fatal(err)
	}
	return dev
}
