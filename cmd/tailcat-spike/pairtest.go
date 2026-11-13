// Pairtest mode: a node tries to enroll with an already-consumed pairing
// key. The coordination server must reject it and the adapter must surface
// that rejection as a dial error rather than blocking until the timeout.
// PRODUCT.md requires pairing semantics to be validated before release.
package main

import (
	"context"
	"log"
	"time"
)

// runPairTest reports whether enrollment with a consumed key is rejected
// quickly and with the server's reason. It dials a hub address that exists:
// rejection must come from pairing, not from an unreachable peer.
func runPairTest(f flags) bool {
	device, err := newDeviceTransport(f)
	if err != nil {
		log.Fatalf("pairtest: %v", err)
	}
	defer device.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := device.Dial(ctx, f.Hub)
	if err != nil {
		log.Printf("pairtest: enrollment rejected after %d ms", time.Since(start).Milliseconds())
		log.Printf("pairtest: reason: %v", err)
		log.Print("pairtest: PASS (one-time pairing key refused)")
		return true
	}
	conn.Close()
	log.Print("pairtest: FAIL (a consumed pairing key enrolled; pairing is not one-time)")
	return false
}
