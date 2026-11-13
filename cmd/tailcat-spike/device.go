// Device mode: dial the hub, exchange framed batches, and record round-trip
// latency per payload size plus the operational limits of the run.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sizeResult holds the latency distribution for one payload size.
type sizeResult struct {
	PayloadBytes int     `json:"payload_bytes"`
	Count        int     `json:"count"`
	MinUs        int64   `json:"min_us"`
	P50Us        int64   `json:"p50_us"`
	P95Us        int64   `json:"p95_us"`
	MaxUs        int64   `json:"max_us"`
	MeanUs       float64 `json:"mean_us"`
	ThroughputMB float64 `json:"throughput_mb_s"`
}

// deviceResult is the device-side record of a spike run.
type deviceResult struct {
	Transport       string       `json:"transport"`
	Hub             string       `json:"hub"`
	DialMs          int64        `json:"dial_ms"`
	FirstExchangeMs int64        `json:"first_exchange_ms"`
	Sizes           []sizeResult `json:"sizes"`
	TotalBytes      uint64       `json:"total_bytes"`
	TotalMs         int64        `json:"total_ms"`
	RSSBytes        uint64       `json:"rss_bytes"`
	StateDirBytes   int64        `json:"state_dir_bytes"`
}

func runDevice(f flags) {
	ctx, stop := stopOnSignal()
	defer stop()

	sizes, err := parseSizes(f.Payloads)
	if err != nil {
		log.Fatalf("device: %v", err)
	}
	device, err := newDeviceTransport(f)
	if err != nil {
		log.Fatalf("device: %v", err)
	}
	defer device.Close()

	dialCtx, cancel := context.WithTimeout(ctx, f.Timeout)
	start := time.Now()
	conn, err := device.Dial(dialCtx, f.Hub)
	cancel()
	if err != nil {
		log.Fatalf("device dial: %v", err)
	}
	defer conn.Close()
	res := deviceResult{Transport: f.Transport, Hub: f.Hub, DialMs: time.Since(start).Milliseconds()}
	log.Printf("device connected to %s in %d ms", f.Hub, res.DialMs)

	batch := newBatcher()
	// One warmup exchange absorbs connection setup, then the measured loops.
	if _, _, err := batch.exchange(conn, 1024); err != nil {
		log.Fatalf("device warmup: %v", err)
	}
	res.FirstExchangeMs = time.Since(start).Milliseconds()

	totalStart := time.Now()
	for _, size := range sizes {
		s, err := measureSize(conn, batch, size, f.Count)
		if err != nil {
			log.Fatalf("device: size %d: %v", size, err)
		}
		res.Sizes = append(res.Sizes, s)
		res.TotalBytes += uint64(size) * uint64(f.Count)
		log.Printf("%7d B x %3d: p50 %6d us  p95 %6d us  max %6d us  %7.1f MiB/s",
			size, s.Count, s.P50Us, s.P95Us, s.MaxUs, s.ThroughputMB)
	}
	res.TotalMs = time.Since(totalStart).Milliseconds()
	res.RSSBytes = readRSS()
	res.StateDirBytes = dirSize(f.StateDir)
	writeResults(f.Out, res)
	log.Printf("device done: %d bytes in %d ms, rss %d MiB, state %d KiB",
		res.TotalBytes, res.TotalMs, res.RSSBytes>>20, res.StateDirBytes>>10)
}

// measureSize runs count exchanges at one payload size and summarizes them.
func measureSize(conn net.Conn, batch *batcher, size, count int) (sizeResult, error) {
	rtts := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		rtt, _, err := batch.exchange(conn, size)
		if err != nil {
			return sizeResult{}, err
		}
		rtts = append(rtts, rtt.Microseconds())
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	var sum int64
	for _, r := range rtts {
		sum += r
	}
	totalSec := float64(sum) / 1e6
	return sizeResult{
		PayloadBytes: size,
		Count:        count,
		MinUs:        rtts[0],
		P50Us:        rtts[len(rtts)/2],
		P95Us:        rtts[(len(rtts)*95)/100],
		MaxUs:        rtts[len(rtts)-1],
		MeanUs:       float64(sum) / float64(len(rtts)),
		ThroughputMB: (float64(size) * float64(count) / (1024 * 1024)) / totalSec,
	}, nil
}

// parseSizes parses the comma-separated payload size list.
func parseSizes(s string) ([]int, error) {
	var sizes []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n <= 0 || n > maxPayloadBytes {
			return nil, fmt.Errorf("payload size %q is not a byte count within 1..%d", part, maxPayloadBytes)
		}
		sizes = append(sizes, n)
	}
	if len(sizes) == 0 {
		return nil, fmt.Errorf("no payload sizes given")
	}
	return sizes, nil
}

// batcher sends verified request/acknowledgment exchanges on one connection.
type batcher struct {
	nextID    uint64
	committed uint64
}

// newBatcher returns a batcher whose first batch has ID 0.
func newBatcher() *batcher { return &batcher{} }

// exchange sends one batch of size bytes and waits for its acknowledgment.
// The round trip spans the first byte written to the acknowledgment read;
// every acknowledgment is checked for identity, digest and cursor.
func (b *batcher) exchange(conn net.Conn, size int) (time.Duration, uint64, error) {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + int(b.nextID))
	}
	id := b.nextID
	b.nextID++
	start := time.Now()
	if err := writeRequest(conn, id, payload); err != nil {
		return 0, 0, fmt.Errorf("write batch %d: %w", id, err)
	}
	ackID, cursor, digest, err := readAck(conn)
	rtt := time.Since(start)
	if err != nil {
		return 0, 0, fmt.Errorf("read ack for batch %d: %w", id, err)
	}
	want := sha256.Sum256(payload)
	b.committed += uint64(size)
	if ackID != id || digest != want || cursor != b.committed {
		return 0, 0, fmt.Errorf("ack mismatch for batch %d: got (%d, cursor %d), want digest match and cursor %d",
			id, ackID, cursor, b.committed)
	}
	return rtt, cursor, nil
}
