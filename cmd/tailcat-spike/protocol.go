// The exchange protocol: one request frame per batch, one response frame per
// acknowledgment. It is a miniature of the sync contract in ARCHITECTURE.md —
// a batch carries bytes and their boundaries; the hub commits them and
// answers with a cursor that only advances over durable receipt.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
)

// maxPayloadBytes bounds one batch payload, like the ingestion batch bound.
const maxPayloadBytes = 8 << 20

// Frame layout. All integers are big-endian.
//
//	request  = 'B' batchID(8) payloadLen(8) payload
//	ack      = 'A' batchID(8) cursor(8) digest(32)
const (
	headerLen  = 17 // marker + batchID + payloadLen
	ackLen     = 1 + 8 + 8 + sha256.Size
	digestSize = sha256.Size
)

var (
	errMarker    = errors.New("spike: bad frame marker")
	errPayload   = fmt.Errorf("spike: payload exceeds the %d byte bound", maxPayloadBytes)
	errShortRead = errors.New("spike: short read")
	// errClosed reports a peer that closed the connection between frames,
	// the normal end of a device run.
	errClosed = errors.New("spike: connection closed by peer")
)

// readFull reads exactly len(buf) bytes. A peer that closed between frames
// yields errClosed; anything else, including a truncated frame, is an error.
func readFull(r io.Reader, buf []byte) error {
	_, err := io.ReadFull(r, buf)
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) && len(buf) > 0 {
		// io.ReadFull returns io.EOF only when nothing was read: the
		// peer closed cleanly at a frame boundary.
		return errClosed
	}
	return fmt.Errorf("%w: %v", errShortRead, err)
}

// writeRequest frames and writes one batch. payload must be within the bound.
func writeRequest(w io.Writer, batchID uint64, payload []byte) error {
	if len(payload) > maxPayloadBytes {
		return errPayload
	}
	var head [headerLen]byte
	head[0] = 'B'
	binary.BigEndian.PutUint64(head[1:9], batchID)
	binary.BigEndian.PutUint64(head[9:17], uint64(len(payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// readRequest reads one framed batch. The payload is allocated only after
// the length bound is checked.
func readRequest(r io.Reader) (batchID uint64, payload []byte, err error) {
	var head [headerLen]byte
	if err := readFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	if head[0] != 'B' {
		return 0, nil, fmt.Errorf("%w: want 'B', got %q", errMarker, head[0])
	}
	batchID = binary.BigEndian.Uint64(head[1:9])
	n := binary.BigEndian.Uint64(head[9:17])
	if n > maxPayloadBytes {
		return 0, nil, errPayload
	}
	payload = make([]byte, n)
	if err := readFull(r, payload); err != nil {
		return 0, nil, err
	}
	return batchID, payload, nil
}

// writeAck frames and writes the acknowledgment for batchID: the cursor the
// hub has committed through, and the digest of the received payload.
func writeAck(w io.Writer, batchID, cursor uint64, digest [digestSize]byte) error {
	var ack [ackLen]byte
	ack[0] = 'A'
	binary.BigEndian.PutUint64(ack[1:9], batchID)
	binary.BigEndian.PutUint64(ack[9:17], cursor)
	copy(ack[17:], digest[:])
	_, err := w.Write(ack[:])
	return err
}

// readAck reads and validates one acknowledgment frame.
func readAck(r io.Reader) (batchID, cursor uint64, digest [digestSize]byte, err error) {
	var ack [ackLen]byte
	if err := readFull(r, ack[:]); err != nil {
		return 0, 0, digest, err
	}
	if ack[0] != 'A' {
		return 0, 0, digest, fmt.Errorf("%w: want 'A', got %q", errMarker, ack[0])
	}
	batchID = binary.BigEndian.Uint64(ack[1:9])
	cursor = binary.BigEndian.Uint64(ack[9:17])
	copy(digest[:], ack[17:])
	return batchID, cursor, digest, nil
}

// hubConn serves one device connection: every well-formed batch advances the
// connection cursor by its payload length and is acknowledged with the
// payload digest. A malformed frame stops the connection with an error.
type hubConn struct {
	r      io.Reader
	w      io.Writer
	cursor uint64
	digest hash.Hash
}

// serve exchanges frames until EOF or error. It returns the committed cursor.
func (c *hubConn) serve() (uint64, error) {
	for {
		batchID, payload, err := readRequest(c.r)
		if err != nil {
			return c.cursor, err
		}
		c.digest.Reset()
		c.digest.Write(payload)
		var d [digestSize]byte
		c.digest.Sum(d[:0])
		c.cursor += uint64(len(payload))
		if err := writeAck(c.w, batchID, c.cursor, d); err != nil {
			return c.cursor, err
		}
	}
}
