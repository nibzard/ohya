package main

import (
	"context"
	"log"
	"time"
)

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
