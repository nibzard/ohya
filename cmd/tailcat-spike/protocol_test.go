package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
	"testing"
)

func TestRequestAckRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("a batch of transcript bytes")
	if err := writeRequest(&buf, 7, payload); err != nil {
		t.Fatalf("writeRequest: %v", err)
	}
	id, got, err := readRequest(&buf)
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if id != 7 || !bytes.Equal(got, payload) {
		t.Fatalf("round trip got (%d, %q)", id, got)
	}

	digest := sha256.Sum256(payload)
	if err := writeAck(&buf, 7, 1234, digest); err != nil {
		t.Fatalf("writeAck: %v", err)
	}
	ackID, cursor, gotDigest, err := readAck(&buf)
	if err != nil {
		t.Fatalf("readAck: %v", err)
	}
	if ackID != 7 || cursor != 1234 || gotDigest != digest {
		t.Fatalf("ack round trip got (%d, %d)", ackID, cursor)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes left unread", buf.Len())
	}
}

func TestReadRequestRejectsBadMarker(t *testing.T) {
	var head [headerLen]byte
	head[0] = 'X'
	binary.BigEndian.PutUint64(head[9:17], 0)
	var buf bytes.Buffer
	buf.Write(head[:])
	if _, _, err := readRequest(&buf); !errors.Is(err, errMarker) {
		t.Fatalf("want errMarker, got %v", err)
	}
}

func TestReadRequestRejectsOversizedPayload(t *testing.T) {
	var head [headerLen]byte
	head[0] = 'B'
	binary.BigEndian.PutUint64(head[9:17], maxPayloadBytes+1)
	var buf bytes.Buffer
	buf.Write(head[:])
	if _, _, err := readRequest(&buf); !errors.Is(err, errPayload) {
		t.Fatalf("want errPayload, got %v", err)
	}
}

func TestWriteRequestRejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRequest(&buf, 1, make([]byte, maxPayloadBytes+1)); !errors.Is(err, errPayload) {
		t.Fatalf("want errPayload, got %v", err)
	}
}

func TestReadRequestTruncatedFrameIsError(t *testing.T) {
	var head [headerLen]byte
	head[0] = 'B'
	binary.BigEndian.PutUint64(head[9:17], 16)
	var buf bytes.Buffer
	buf.Write(head[:])
	buf.Write(make([]byte, 5)) // payload cut short
	if _, _, err := readRequest(&buf); errors.Is(err, errClosed) || err == nil {
		t.Fatalf("truncated frame must be an error, got %v", err)
	}
}

func TestReadRequestCleanCloseBetweenFrames(t *testing.T) {
	var buf bytes.Buffer
	writeRequest(&buf, 1, []byte("ok"))
	readRequest(&buf)
	if _, _, err := readRequest(&buf); !errors.Is(err, errClosed) {
		t.Fatalf("want errClosed, got %v", err)
	}
}

// TestHubConnServesExchanges drives the hub side over an in-memory pipe and
// checks cursor advancement and digests from the device side.
func TestHubConnServesExchanges(t *testing.T) {
	dev, hubEnd := net.Pipe()
	defer dev.Close()

	c := &hubConn{r: hubEnd, w: hubEnd, digest: sha256.New()}
	done := make(chan error, 1)
	go func() {
		_, err := c.serve()
		done <- err
	}()

	b := newBatcher()
	for _, size := range []int{1, 1024, 70000} {
		if _, _, err := b.exchange(dev, size); err != nil {
			t.Fatalf("exchange %d: %v", size, err)
		}
	}
	dev.Close()
	if err := <-done; !errors.Is(err, errClosed) {
		t.Fatalf("hub serve: want errClosed, got %v", err)
	}
	if c.cursor != 1+1024+70000 {
		t.Fatalf("cursor = %d, want %d", c.cursor, 1+1024+70000)
	}
}

func TestParseSizes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
		ok   bool
	}{
		{"1024,65536", []int{1024, 65536}, true},
		{" 42 ", []int{42}, true},
		{"", nil, false},
		{"0", nil, false},
		{"-5", nil, false},
		{"abc", nil, false},
		{itoa(maxPayloadBytes + 1), nil, false},
	} {
		got, err := parseSizes(tc.in)
		if tc.ok && err != nil {
			t.Fatalf("parseSizes(%q): %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("parseSizes(%q) = %v, want error", tc.in, got)
		}
		if tc.ok && len(got) != len(tc.want) {
			t.Fatalf("parseSizes(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}
