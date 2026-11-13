// Package hubsync implements the hub side of the sync contract from
// ARCHITECTURE.md: a device registers its sources, then uploads framed
// batches of one source's original records; the hub normalizes them with the
// same parser as local ingestion, commits through the transactional write
// path, and acknowledges only after the durable commit. The wire protocol
// lives here so the device uploader speaks exactly what the endpoint
// accepts. The transport stays behind internal/transport: this package sees
// only ordinary net values.
package hubsync

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/nibzard/ohya/internal/claudecode"
)

// ProtocolVersion is the wire protocol version every frame carries. A frame
// with any other version is refused; there is no negotiation.
const ProtocolVersion = 1

// Frame bounds. Every declared length is checked against its bound before
// the buffer is allocated, so a hostile or broken frame cannot make the hub
// allocate unbounded memory.
const (
	// maxDeviceIDLen and maxSourceIDLen hold the 32-hex-character identities
	// from internal/device and internal/watch with room to spare.
	maxDeviceIDLen = 64
	maxSourceIDLen = 64
	// maxAgentLen holds agent names, which are short lowercase slugs.
	maxAgentLen = 64
	// maxPathLen holds one filesystem path.
	maxPathLen = 4096
	// maxMessageLen bounds error and stop-reason text, which never carries
	// transcript content.
	maxMessageLen = 1024

	// MaxRecordBytes is one record's JSON text without its line terminator.
	// It is the hub parser's own record limit: a record the parser would
	// refuse is refused before allocation.
	MaxRecordBytes = claudecode.DefaultMaxRecordBytes

	// MaxBatchBytes bounds the record bytes one batch may carry, matching the
	// local ingestion batch bound. A batch never splits a record.
	MaxBatchBytes = 8 << 20

	// maxRecords bounds the record count of one batch. The byte bound alone
	// would admit millions of tiny records, each holding a full Event in
	// memory during normalization; this cap bounds that work at a level no
	// real batch reaches — the 8 MiB batch bound with typical records holds
	// far fewer.
	maxRecords = 65536
)

// Frame layout. All integers are big-endian.
//
//	registration = 'R' ver(1) devLen(2) dev srcLen(2) src agentLen(2) agent pathLen(4) path
//	batch        = 'B' ver(1) devLen(2) dev srcLen(2) src start(8) end(8) count(4)
//	                 count * [ offset(8) len(4) json ]
//	ack          = 'A' ver(1) status(1) cursor(8) inserted(8) stopOffset(8) stopLen(2) stop
//	error        = 'E' ver(1) code(1) msgLen(2) msg
const (
	markerRegistration = 'R'
	markerBatch        = 'B'
	markerAck          = 'A'
	markerError        = 'E'
)

// ErrTooLarge marks a frame whose declared lengths exceed the protocol
// bounds. The stream cannot be resynchronized past it.
var ErrTooLarge = errors.New("hubsync: frame exceeds its bounds")

// ErrVersion marks a frame carrying an unsupported protocol version.
var ErrVersion = errors.New("hubsync: unsupported protocol version")

// ErrorCode classifies one rejection so the uploader knows what follows:
// reconnect, repair, or a later retry.
type ErrorCode uint8

const (
	// CodeAuth reports a peer that is not a paired device, or a frame naming
	// another device's identity. The connection closes.
	CodeAuth ErrorCode = iota + 1
	// CodeVersion reports an unsupported protocol version. The connection
	// closes.
	CodeVersion
	// CodeMalformed reports an undecodable frame. The connection closes.
	CodeMalformed
	// CodeTooLarge reports a record or batch above its byte bound. The
	// connection closes.
	CodeTooLarge
	// CodeRejected reports a deterministic batch rejection: a gap ahead of
	// the stored cursor, conflicting content at a stored offset, or invalid
	// boundaries. Retrying the same bytes can never succeed.
	CodeRejected
	// CodeOwnership reports a source that belongs to another device.
	CodeOwnership
	// CodeUnknownSource reports a batch for a source the hub never
	// registered. The device must register the source first.
	CodeUnknownSource
	// CodeUnsupported reports an agent this hub cannot parse.
	CodeUnsupported
	// CodeBusy reports the hub at its connection bound; reconnect and retry.
	CodeBusy
	// CodeInternal reports a hub-side failure that survived retries; a later
	// retry may succeed.
	CodeInternal
)

// String names the code for diagnostics.
func (c ErrorCode) String() string {
	switch c {
	case CodeAuth:
		return "auth"
	case CodeVersion:
		return "version"
	case CodeMalformed:
		return "malformed"
	case CodeTooLarge:
		return "too-large"
	case CodeRejected:
		return "rejected"
	case CodeOwnership:
		return "ownership"
	case CodeUnknownSource:
		return "unknown-source"
	case CodeUnsupported:
		return "unsupported-agent"
	case CodeBusy:
		return "busy"
	case CodeInternal:
		return "internal"
	default:
		return fmt.Sprintf("code(%d)", uint8(c))
	}
}

// Registration announces one source before its first batch: the source ID the
// device durably persisted, the agent whose parser owns its records, and the
// device-local path. It maps onto store.RegisterSource, which binds the
// source to the authenticated device.
type Registration struct {
	DeviceID string
	SourceID string
	Agent    string
	Path     string
}

// Record is one original JSONL record without its line terminator, at its
// byte offset in the source file. The record's end offset is derived:
// Offset + len(JSON) + 1, exactly one terminator byte.
type Record struct {
	Offset int64
	JSON   []byte
}

// End returns the offset just past the record's line terminator.
func (r Record) End() int64 { return r.Offset + int64(len(r.JSON)) + 1 }

// Batch is one source's contiguous records and the byte range they cover.
// Start is the first record's offset (the device's acknowledged cursor when
// the batch was built); End is the offset just past the last record's
// terminator. A batch with no records is a cursor probe: Start must equal
// End.
type Batch struct {
	DeviceID string
	SourceID string
	Start    int64
	End      int64
	Records  []Record
}

// Ack acknowledges one committed exchange. Cursor is durable in the hub
// database before the acknowledgment is written. Inserted counts only newly
// inserted events, so a replayed batch reports zero. Stopped reports that the
// batch hit a record the parser refuses: the valid prefix before StopOffset
// committed, and Stop carries the parser's diagnostic — offsets and reason,
// never transcript content.
type Ack struct {
	Stopped    bool
	Cursor     int64
	Inserted   int64
	StopOffset int64
	Stop       string
}

// Error reports one rejected frame.
type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("hubsync: %s: %s", e.Code, e.Message)
}

// Frame is one decoded device-to-hub frame: exactly one of Registration or
// Batch is set.
type Frame struct {
	Registration *Registration
	Batch        *Batch
}

// frameReader reads the protocol's primitives. Every length is checked
// against its bound before the buffer is allocated.
type frameReader struct {
	r io.Reader
}

func (fr *frameReader) read(buf []byte) error {
	if _, err := io.ReadFull(fr.r, buf); err != nil {
		return fmt.Errorf("hubsync: read frame: %w", err)
	}
	return nil
}

func (fr *frameReader) u8() (byte, error) {
	var b [1]byte
	if err := fr.read(b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func (fr *frameReader) u16() (uint16, error) {
	var b [2]byte
	if err := fr.read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

func (fr *frameReader) u32() (uint32, error) {
	var b [4]byte
	if err := fr.read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

func (fr *frameReader) i64() (int64, error) {
	var b [8]byte
	if err := fr.read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b[:])), nil
}

// str16 reads one u16-prefixed string within max bytes, refusing emptiness.
func (fr *frameReader) str16(max int, what string) (string, error) {
	n, err := fr.u16()
	if err != nil {
		return "", err
	}
	s, err := fr.bytes(uint32(n), max, what)
	if err != nil {
		return "", err
	}
	if len(s) == 0 {
		return "", fmt.Errorf("hubsync: frame has an empty %s", what)
	}
	return string(s), nil
}

// bytes allocates n bytes after the bound check and reads them.
func (fr *frameReader) bytes(n uint32, max int, what string) ([]byte, error) {
	if int64(n) > int64(max) {
		return nil, fmt.Errorf("%w: %s length %d exceeds %d", ErrTooLarge, what, n, max)
	}
	buf := make([]byte, int(n))
	if err := fr.read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ReadFrame reads one request frame. Every declared length is checked against
// its bound before allocation. A frame past the bounds, an unsupported
// version, or a truncated read is an error; the caller must treat the
// connection as unsynchronized and close it.
func ReadFrame(r io.Reader) (Frame, error) {
	fr := &frameReader{r: r}
	marker, err := fr.u8()
	if err != nil {
		return Frame{}, err
	}
	version, err := fr.u8()
	if err != nil {
		return Frame{}, err
	}
	if version != ProtocolVersion {
		return Frame{}, fmt.Errorf("%w: frame carries version %d, want %d",
			ErrVersion, version, ProtocolVersion)
	}
	switch marker {
	case markerRegistration:
		reg, err := readRegistration(fr)
		if err != nil {
			return Frame{}, err
		}
		return Frame{Registration: reg}, nil
	case markerBatch:
		b, err := readBatch(fr)
		if err != nil {
			return Frame{}, err
		}
		return Frame{Batch: b}, nil
	default:
		return Frame{}, fmt.Errorf("hubsync: bad frame marker %q", marker)
	}
}

func readRegistration(fr *frameReader) (*Registration, error) {
	var reg Registration
	var err error
	if reg.DeviceID, err = fr.str16(maxDeviceIDLen, "device ID"); err != nil {
		return nil, err
	}
	if reg.SourceID, err = fr.str16(maxSourceIDLen, "source ID"); err != nil {
		return nil, err
	}
	if reg.Agent, err = fr.str16(maxAgentLen, "agent name"); err != nil {
		return nil, err
	}
	pathLen, err := fr.u32()
	if err != nil {
		return nil, err
	}
	path, err := fr.bytes(pathLen, maxPathLen, "path")
	if err != nil {
		return nil, err
	}
	if len(path) == 0 {
		return nil, fmt.Errorf("hubsync: frame has an empty path")
	}
	reg.Path = string(path)
	return &reg, nil
}

func readBatch(fr *frameReader) (*Batch, error) {
	var b Batch
	var err error
	if b.DeviceID, err = fr.str16(maxDeviceIDLen, "device ID"); err != nil {
		return nil, err
	}
	if b.SourceID, err = fr.str16(maxSourceIDLen, "source ID"); err != nil {
		return nil, err
	}
	if b.Start, err = fr.i64(); err != nil {
		return nil, err
	}
	if b.End, err = fr.i64(); err != nil {
		return nil, err
	}
	count, err := fr.u32()
	if err != nil {
		return nil, err
	}
	switch {
	case b.Start < 0:
		return nil, fmt.Errorf("hubsync: batch start %d is negative", b.Start)
	case b.End < b.Start:
		return nil, fmt.Errorf("hubsync: batch end %d precedes its start %d", b.End, b.Start)
	case b.End-b.Start > MaxBatchBytes:
		return nil, fmt.Errorf("%w: batch spans %d bytes, want at most %d", ErrTooLarge, b.End-b.Start, MaxBatchBytes)
	case count > maxRecords:
		return nil, fmt.Errorf("%w: batch carries %d records, want at most %d", ErrTooLarge, count, maxRecords)
	}
	b.Records = make([]Record, 0, min(int(count), 1024))
	total := int64(0)
	for i := uint32(0); i < count; i++ {
		offset, err := fr.i64()
		if err != nil {
			return nil, err
		}
		n, err := fr.u32()
		if err != nil {
			return nil, err
		}
		if offset < 0 {
			return nil, fmt.Errorf("hubsync: batch record %d has negative offset %d", i, offset)
		}
		if int64(n) > MaxRecordBytes {
			return nil, fmt.Errorf("%w: batch record %d at offset %d is %d bytes, want at most %d",
				ErrTooLarge, i, offset, n, MaxRecordBytes)
		}
		total += int64(n)
		if total > MaxBatchBytes {
			return nil, fmt.Errorf("%w: batch records total %d bytes, want at most %d", ErrTooLarge, total, MaxBatchBytes)
		}
		json, err := fr.bytes(n, MaxRecordBytes, "record")
		if err != nil {
			return nil, err
		}
		b.Records = append(b.Records, Record{Offset: offset, JSON: json})
	}
	return &b, nil
}

// ReadReply reads one hub reply: an acknowledgment, or an error frame. A
// non-nil *Error is a protocol-level rejection; a non-nil error is a broken
// or closed connection.
func ReadReply(r io.Reader) (Ack, *Error, error) {
	fr := &frameReader{r: r}
	marker, err := fr.u8()
	if err != nil {
		return Ack{}, nil, err
	}
	version, err := fr.u8()
	if err != nil {
		return Ack{}, nil, err
	}
	if version != ProtocolVersion {
		return Ack{}, nil, fmt.Errorf("%w: reply carries version %d, want %d",
			ErrVersion, version, ProtocolVersion)
	}
	switch marker {
	case markerAck:
		status, err := fr.u8()
		if err != nil {
			return Ack{}, nil, err
		}
		var a Ack
		if a.Cursor, err = fr.i64(); err != nil {
			return Ack{}, nil, err
		}
		if a.Inserted, err = fr.i64(); err != nil {
			return Ack{}, nil, err
		}
		if a.StopOffset, err = fr.i64(); err != nil {
			return Ack{}, nil, err
		}
		stopLen, err := fr.u16()
		if err != nil {
			return Ack{}, nil, err
		}
		stop, err := fr.bytes(uint32(stopLen), maxMessageLen, "stop reason")
		if err != nil {
			return Ack{}, nil, err
		}
		a.Stopped = status == 1
		a.Stop = string(stop)
		return a, nil, nil
	case markerError:
		code, err := fr.u8()
		if err != nil {
			return Ack{}, nil, err
		}
		msgLen, err := fr.u16()
		if err != nil {
			return Ack{}, nil, err
		}
		msg, err := fr.bytes(uint32(msgLen), maxMessageLen, "error message")
		if err != nil {
			return Ack{}, nil, err
		}
		if code == 0 {
			return Ack{}, nil, fmt.Errorf("hubsync: error frame carries no code")
		}
		return Ack{}, &Error{Code: ErrorCode(code), Message: string(msg)}, nil
	default:
		return Ack{}, nil, fmt.Errorf("hubsync: bad reply marker %q", marker)
	}
}

// WriteRegistration frames one source registration.
func WriteRegistration(w io.Writer, r Registration) error {
	for _, f := range []struct {
		value string
		max   int
		what  string
	}{
		{r.DeviceID, maxDeviceIDLen, "device ID"},
		{r.SourceID, maxSourceIDLen, "source ID"},
		{r.Agent, maxAgentLen, "agent name"},
		{r.Path, maxPathLen, "path"},
	} {
		if f.value == "" {
			return fmt.Errorf("hubsync: registration has an empty %s", f.what)
		}
		if len(f.value) > f.max {
			return fmt.Errorf("%w: %s length %d exceeds %d", ErrTooLarge, f.what, len(f.value), f.max)
		}
	}
	head := []byte{markerRegistration, ProtocolVersion}
	head = appendLen16(head, r.DeviceID)
	head = append(head, r.DeviceID...)
	head = appendLen16(head, r.SourceID)
	head = append(head, r.SourceID...)
	head = appendLen16(head, r.Agent)
	head = append(head, r.Agent...)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(r.Path)))
	head = append(head, l[:]...)
	head = append(head, r.Path...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write registration: %w", err)
	}
	return nil
}

// WriteBatch frames one batch. It refuses a batch past the byte or record
// bounds, so a sender never produces a frame the hub must reject.
func WriteBatch(w io.Writer, b Batch) error {
	if b.DeviceID == "" || b.SourceID == "" {
		return fmt.Errorf("hubsync: batch has an empty device or source ID")
	}
	if len(b.DeviceID) > maxDeviceIDLen || len(b.SourceID) > maxSourceIDLen {
		return fmt.Errorf("%w: batch identity exceeds its bound", ErrTooLarge)
	}
	if b.Start < 0 || b.End < b.Start {
		return fmt.Errorf("hubsync: batch range [%d, %d) is invalid", b.Start, b.End)
	}
	if b.End-b.Start > MaxBatchBytes {
		return fmt.Errorf("%w: batch spans %d bytes, want at most %d", ErrTooLarge, b.End-b.Start, MaxBatchBytes)
	}
	if len(b.Records) > maxRecords {
		return fmt.Errorf("%w: batch carries %d records, want at most %d", ErrTooLarge, len(b.Records), maxRecords)
	}
	total := int64(0)
	for i, r := range b.Records {
		if int64(len(r.JSON)) > MaxRecordBytes {
			return fmt.Errorf("%w: batch record %d is %d bytes, want at most %d",
				ErrTooLarge, i, len(r.JSON), MaxRecordBytes)
		}
		total += int64(len(r.JSON))
		if total > MaxBatchBytes {
			return fmt.Errorf("%w: batch records total %d bytes, want at most %d", ErrTooLarge, total, MaxBatchBytes)
		}
	}
	head := []byte{markerBatch, ProtocolVersion}
	head = appendLen16(head, b.DeviceID)
	head = append(head, b.DeviceID...)
	head = appendLen16(head, b.SourceID)
	head = append(head, b.SourceID...)
	var wide [8]byte
	binary.BigEndian.PutUint64(wide[:], uint64(b.Start))
	head = append(head, wide[:]...)
	binary.BigEndian.PutUint64(wide[:], uint64(b.End))
	head = append(head, wide[:]...)
	var n32 [4]byte
	binary.BigEndian.PutUint32(n32[:], uint32(len(b.Records)))
	head = append(head, n32[:]...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write batch: %w", err)
	}
	for _, r := range b.Records {
		var rh [12]byte
		binary.BigEndian.PutUint64(rh[:8], uint64(r.Offset))
		binary.BigEndian.PutUint32(rh[8:], uint32(len(r.JSON)))
		if _, err := w.Write(rh[:]); err != nil {
			return fmt.Errorf("hubsync: write batch: %w", err)
		}
		if len(r.JSON) > 0 {
			if _, err := w.Write(r.JSON); err != nil {
				return fmt.Errorf("hubsync: write batch: %w", err)
			}
		}
	}
	return nil
}

// WriteAck frames one acknowledgment.
func WriteAck(w io.Writer, a Ack) error {
	status := byte(0)
	if a.Stopped {
		status = 1
	}
	head := []byte{markerAck, ProtocolVersion, status}
	var wide [8]byte
	for _, v := range []int64{a.Cursor, a.Inserted, a.StopOffset} {
		binary.BigEndian.PutUint64(wide[:], uint64(v))
		head = append(head, wide[:]...)
	}
	stop := truncate(a.Stop, maxMessageLen)
	head = appendLen16(head, stop)
	head = append(head, stop...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write ack: %w", err)
	}
	return nil
}

// WriteError frames one rejection. Closing the connection is the caller's
// decision; the frame itself is complete.
func WriteError(w io.Writer, code ErrorCode, msg string) error {
	head := []byte{markerError, ProtocolVersion, byte(code)}
	msg = truncate(msg, maxMessageLen)
	head = appendLen16(head, msg)
	head = append(head, msg...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write error frame: %w", err)
	}
	return nil
}

// appendLen16 appends one u16 length prefix.
func appendLen16(buf []byte, s string) []byte {
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(s)))
	return append(buf, l[:]...)
}

// truncate cuts s to at most max bytes on a rune boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
