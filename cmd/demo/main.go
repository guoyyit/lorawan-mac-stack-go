// Command demo runs the LoRaWAN MAC stack locally: a network server and
// several Class-A end devices connected through the in-memory radio space,
// demonstrating OTAA join, confirmed/unconfirmed traffic, retransmission,
// ADR, duty-cycle queuing, receive-window scheduling and crash recovery.
//
// Usage:
//
//	go run ./cmd/demo
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/guoyyit/lorawan-mac-stack-go/pkg/clock"
	lwcrypto "github.com/guoyyit/lorawan-mac-stack-go/pkg/crypto"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/mac"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/protocol"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/radio"
	"github.com/guoyyit/lorawan-mac-stack-go/pkg/storage"
)

func main() {
	logger := func(side string) mac.LogFunc {
		return func(level, msg string) {
			if level == "info" || level == "warn" {
				fmt.Printf("  [%s/%s] %s\n", side, level, msg)
			}
		}
	}

	// Wall-clock time and a tiny propagation delay. A fixed directory under
	// the OS temp root persists sessions, so re-running the demo shows the
	// devices resuming their existing sessions (crash recovery). Delete it
	// to start from a fresh (unjoined) state.
	clk := clock.NewRealClock()
	space := radio.NewSpace(radio.Config{Clock: clk, PropagationDelay: 20 * time.Millisecond})
	dataDir := filepath.Join(os.TempDir(), "lorawan-mac-demo")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatal(err)
	}

	serverStore := storage.MustOpen(filepath.Join(dataDir, "server"))
	srv, err := mac.NewNetworkServer(mac.ServerConfig{
		NetID:    protocol.NetID{0, 0, 1},
		JoinEUI:  protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1},
		Space:    space,
		Clock:    clk,
		Store:    serverStore,
		Logger:   logger("server"),
		RX1Delay: time.Second,
		RX2Delay: 2 * time.Second,
		OnUp: func(devEUI protocol.EUI64, port uint8, payload []byte) {
			fmt.Printf("  >> app uplink dev=%s port=%d data=%q\n", devEUI, port, payload)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	var appKey lwcrypto.AES128Key
	for i := range appKey {
		appKey[i] = byte(i + 1)
	}
	joinEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, 1}

	var wg sync.WaitGroup
	for i := byte(1); i <= 2; i++ {
		devEUI := protocol.EUI64{0, 0, 0, 0, 0, 0, 0, i}
		if err := srv.RegisterDevice(devEUI, joinEUI, appKey); err != nil {
			log.Fatal(err)
		}
		dev, err := mac.NewEndDevice(mac.DeviceConfig{
			DevEUI:       devEUI,
			JoinEUI:      joinEUI,
			AppKey:       appKey,
			Space:        space,
			Endpoint:     radio.EndpointID("device-" + fmt.Sprint(i)),
			Clock:        clk,
			Store:        storage.MustOpen(filepath.Join(dataDir, "device-"+fmt.Sprint(i))),
			Logger:       logger(fmt.Sprintf("device%d", i)),
			RX1Delay:     time.Second,
			RX2Delay:     2 * time.Second,
			BaseBackoff:  time.Second,
			JoinTimeout:  5 * time.Second,
			MaxRetries:   2,
			MaxDutyCycle: 0.01,
			OnDown: func(port uint8, payload []byte) {
				fmt.Printf("  << app downlink port=%d data=%q\n", port, payload)
			},
		})
		if err != nil {
			log.Fatal(err)
		}
		if err := dev.Start(); err != nil {
			log.Fatal(err)
		}

		wg.Add(1)
		go func(i byte, d *mac.EndDevice, eui protocol.EUI64) {
			defer wg.Done()
			ctx := context.Background()
			fmt.Printf("device%d: OTAA join...\n", i)
			if err := d.Join(ctx); err != nil {
				fmt.Printf("device%d join error: %v\n", i, err)
				return
			}
			fmt.Printf("device%d: joined, sending uplinks\n", i)
			for n := 0; n < 2; n++ {
				confirmed := n == 0
				msg := fmt.Sprintf("sensor%d reading=%d", i, 20+n)
				if err := d.Send(ctx, confirmed, 5, []byte(msg)); err != nil {
					fmt.Printf("device%d send error: %v\n", i, err)
				}
				time.Sleep(3 * time.Second)
			}
			if i == 1 {
				// Demonstrate queued Class-A downlink delivery.
				if err := srv.QueueDownlink(eui, false, 8, []byte("set-led=1")); err != nil {
					fmt.Printf("queue downlink: %v\n", err)
				}
				if err := d.Send(ctx, false, 5, []byte("poke for downlink")); err != nil {
					fmt.Printf("device1 poke: %v\n", err)
				}
				time.Sleep(3 * time.Second)
			}
		}(i, dev, devEUI)
	}
	wg.Wait()

	fmt.Println("\ndemo complete; sessions persisted under", dataDir)
	fmt.Println("re-run to resume the existing sessions (crash recovery); delete the dir to rejoin from scratch.")
}
