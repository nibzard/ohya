package hubsync

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
)

const ProtocolVersion = 1

const (
	maxDeviceIDLen = 64
	maxSourceIDLen = 64
	maxAgentLen    = 64
	maxPathLen     = 4096
	maxMessageLen  = 1024
	maxSecretLen   = 128

	MaxRecordBytes = claudecode.DefaultMaxRecordBytes

	MaxBatchBytes = 8 << 20

	MaxRecords = 65536

	MaxQueryBytes = 64 << 10

	MaxQueryResultBytes = 4 << 20

	MaxAnnotateRequestBytes = 1 << 20
)

const (
	markerRegistration = 'R'
	markerBatch        = 'B'
	markerAck          = 'A'
	markerError        = 'E'
	markerHello        = 'H'
	markerPair         = 'P'
	markerCredential   = 'C'
	markerQuery        = 'Q'
	markerQueryResult  = 'S'
	markerAnnotate     = 'N'
	markerAnnotateRes  = 'K'
)

var ErrTooLarge = errors.New("hubsync: frame exceeds its bounds")

var ErrVersion = errors.New("hubsync: unsupported protocol version")

type ErrorCode uint8

const (
	CodeAuth ErrorCode = iota + 1
	CodeVersion
	CodeMalformed
	CodeTooLarge
	CodeRejected
	CodeOwnership
	CodeUnknownSource
	CodeUnsupported
	CodeBusy
	CodeInternal
	CodeNoRemote
)

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
	case CodeNoRemote:
		return "no-remote"
	default:
		return fmt.Sprintf("code(%d)", uint8(c))
	}
}

type Registration struct {
	DeviceID string
	SourceID string
	Agent    string
	Path     string
}

type Record struct {
	Offset int64
	JSON   []byte
}

func (r Record) End() int64 { return r.Offset + int64(len(r.JSON)) + 1 }

type Batch struct {
	DeviceID string
	SourceID string
	Start    int64
	End      int64
	Records  []Record
}

type Ack struct {
	Stopped    bool
	Cursor     int64
	Inserted   int64
	StopOffset int64
	Stop       string
}

type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("hubsync: %s: %s", e.Code, e.Message)
}

type Query struct {
	SQL string
}

type Annotate struct {
	Request []byte
}

type Frame struct {
	Registration *Registration
	Batch        *Batch
	Query        *Query
	Annotate     *Annotate
}

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
	case markerQuery:
		q, err := readQuery(fr)
		if err != nil {
			return Frame{}, err
		}
		return Frame{Query: q}, nil
	case markerAnnotate:
		a, err := readAnnotate(fr)
		if err != nil {
			return Frame{}, err
		}
		return Frame{Annotate: a}, nil
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
	case count > MaxRecords:
		return nil, fmt.Errorf("%w: batch carries %d records, want at most %d", ErrTooLarge, count, MaxRecords)
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

func readQuery(fr *frameReader) (*Query, error) {
	n, err := fr.u32()
	if err != nil {
		return nil, err
	}
	sql, err := fr.bytes(n, MaxQueryBytes, "query")
	if err != nil {
		return nil, err
	}
	if len(sql) == 0 {
		return nil, fmt.Errorf("hubsync: query frame has no SQL text")
	}
	return &Query{SQL: string(sql)}, nil
}

func readAnnotate(fr *frameReader) (*Annotate, error) {
	n, err := fr.u32()
	if err != nil {
		return nil, err
	}
	request, err := fr.bytes(n, MaxAnnotateRequestBytes, "annotate request")
	if err != nil {
		return nil, err
	}
	if len(request) == 0 {
		return nil, fmt.Errorf("hubsync: annotate frame has no request JSON")
	}
	return &Annotate{Request: request}, nil
}

func replyHead(fr *frameReader) (byte, error) {
	marker, err := fr.u8()
	if err != nil {
		return 0, err
	}
	version, err := fr.u8()
	if err != nil {
		return 0, err
	}
	if version != ProtocolVersion {
		return 0, fmt.Errorf("%w: reply carries version %d, want %d",
			ErrVersion, version, ProtocolVersion)
	}
	return marker, nil
}

func readErrorFrame(fr *frameReader) (*Error, error) {
	code, err := fr.u8()
	if err != nil {
		return nil, err
	}
	msgLen, err := fr.u16()
	if err != nil {
		return nil, err
	}
	msg, err := fr.bytes(uint32(msgLen), maxMessageLen, "error message")
	if err != nil {
		return nil, err
	}
	if code == 0 {
		return nil, fmt.Errorf("hubsync: error frame carries no code")
	}
	return &Error{Code: ErrorCode(code), Message: string(msg)}, nil
}

func ReadReply(r io.Reader) (Ack, *Error, error) {
	fr := &frameReader{r: r}
	marker, err := replyHead(fr)
	if err != nil {
		return Ack{}, nil, err
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
		perr, err := readErrorFrame(fr)
		if err != nil {
			return Ack{}, nil, err
		}
		return Ack{}, perr, nil
	default:
		return Ack{}, nil, fmt.Errorf("hubsync: bad reply marker %q", marker)
	}
}

func ReadQueryReply(r io.Reader) (mcpserver.Result, *Error, error) {
	fr := &frameReader{r: r}
	marker, err := replyHead(fr)
	if err != nil {
		return mcpserver.Result{}, nil, err
	}
	switch marker {
	case markerQueryResult:
		n, err := fr.u32()
		if err != nil {
			return mcpserver.Result{}, nil, err
		}
		payload, err := fr.bytes(n, MaxQueryResultBytes, "query result")
		if err != nil {
			return mcpserver.Result{}, nil, err
		}
		var result mcpserver.Result
		if err := json.Unmarshal(payload, &result); err != nil {
			return mcpserver.Result{}, nil, fmt.Errorf("hubsync: decode query result: %w", err)
		}
		return result, nil, nil
	case markerError:
		perr, err := readErrorFrame(fr)
		if err != nil {
			return mcpserver.Result{}, nil, err
		}
		return mcpserver.Result{}, perr, nil
	default:
		return mcpserver.Result{}, nil, fmt.Errorf("hubsync: bad query reply marker %q", marker)
	}
}

func ReadAnnotateReply(r io.Reader) (store.AnnotateResult, *Error, error) {
	fr := &frameReader{r: r}
	marker, err := replyHead(fr)
	if err != nil {
		return store.AnnotateResult{}, nil, err
	}
	switch marker {
	case markerAnnotateRes:
		var r store.AnnotateResult
		applied, err := fr.i64()
		if err != nil {
			return store.AnnotateResult{}, nil, err
		}
		unchanged, err := fr.i64()
		if err != nil {
			return store.AnnotateResult{}, nil, err
		}
		if applied < 0 || unchanged < 0 || applied > 1<<31 || unchanged > 1<<31 {
			return store.AnnotateResult{}, nil, fmt.Errorf(
				"hubsync: annotate result counts %d, %d are out of range", applied, unchanged)
		}
		r.Applied, r.Unchanged = int(applied), int(unchanged)
		return r, nil, nil
	case markerError:
		perr, err := readErrorFrame(fr)
		if err != nil {
			return store.AnnotateResult{}, nil, err
		}
		return store.AnnotateResult{}, perr, nil
	default:
		return store.AnnotateResult{}, nil, fmt.Errorf("hubsync: bad annotate reply marker %q", marker)
	}
}

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
	if len(b.Records) > MaxRecords {
		return fmt.Errorf("%w: batch carries %d records, want at most %d", ErrTooLarge, len(b.Records), MaxRecords)
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

func writePayload(w io.Writer, marker byte, payload []byte) error {
	head := []byte{marker, ProtocolVersion}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	head = append(head, length[:]...)
	head = append(head, payload...)
	_, err := w.Write(head)
	return err
}

func WriteQuery(w io.Writer, sql string) error {
	if sql == "" {
		return fmt.Errorf("hubsync: query has no SQL text")
	}
	if len(sql) > MaxQueryBytes {
		return fmt.Errorf("%w: query holds %d bytes, want at most %d",
			ErrTooLarge, len(sql), MaxQueryBytes)
	}
	if err := writePayload(w, markerQuery, []byte(sql)); err != nil {
		return fmt.Errorf("hubsync: write query: %w", err)
	}
	return nil
}

func WriteAnnotate(w io.Writer, request []byte) error {
	if len(request) == 0 {
		return fmt.Errorf("hubsync: annotate request has no JSON")
	}
	if len(request) > MaxAnnotateRequestBytes {
		return fmt.Errorf("%w: annotate request holds %d bytes, want at most %d",
			ErrTooLarge, len(request), MaxAnnotateRequestBytes)
	}
	if err := writePayload(w, markerAnnotate, []byte(request)); err != nil {
		return fmt.Errorf("hubsync: write annotate request: %w", err)
	}
	return nil
}

func WriteQueryResult(w io.Writer, r mcpserver.Result) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("hubsync: encode query result: %w", err)
	}
	if len(payload) > MaxQueryResultBytes {
		return fmt.Errorf("%w: query result holds %d bytes, want at most %d; lower the engine's output limit",
			ErrTooLarge, len(payload), MaxQueryResultBytes)
	}
	if err := writePayload(w, markerQueryResult, []byte(payload)); err != nil {
		return fmt.Errorf("hubsync: write query result: %w", err)
	}
	return nil
}

func WriteAnnotateResult(w io.Writer, r store.AnnotateResult) error {
	head := []byte{markerAnnotateRes, ProtocolVersion}
	var wide [8]byte
	binary.BigEndian.PutUint64(wide[:], uint64(r.Applied))
	head = append(head, wide[:]...)
	binary.BigEndian.PutUint64(wide[:], uint64(r.Unchanged))
	head = append(head, wide[:]...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write annotate result: %w", err)
	}
	return nil
}

type Hello struct {
	Credential string
}

type PairRequest struct {
	Invitation string
	DeviceID   string
}

type CredentialReply struct {
	DeviceID   string
	Credential string
}

func ReadAuthFrame(r io.Reader) (*Hello, *PairRequest, error) {
	fr := &frameReader{r: r}
	marker, err := fr.u8()
	if err != nil {
		return nil, nil, fmt.Errorf("hubsync: read auth frame: %w", err)
	}
	version, err := fr.u8()
	if err != nil {
		return nil, nil, fmt.Errorf("hubsync: read auth frame: %w", err)
	}
	if version != ProtocolVersion {
		return nil, nil, fmt.Errorf("%w: auth frame carries version %d, want %d",
			ErrVersion, version, ProtocolVersion)
	}
	switch marker {
	case markerHello:
		credential, err := fr.str16(maxSecretLen, "credential")
		if err != nil {
			return nil, nil, err
		}
		return &Hello{Credential: credential}, nil, nil
	case markerPair:
		var p PairRequest
		if p.Invitation, err = fr.str16(maxSecretLen, "invitation"); err != nil {
			return nil, nil, err
		}
		if p.DeviceID, err = fr.str16(maxDeviceIDLen, "device ID"); err != nil {
			return nil, nil, err
		}
		return nil, &p, nil
	default:
		return nil, nil, fmt.Errorf("hubsync: bad auth frame marker %q; want a credential hello or a pairing request", marker)
	}
}

func WriteHello(w io.Writer, credential string) error {
	if credential == "" {
		return fmt.Errorf("hubsync: hello has an empty credential")
	}
	if len(credential) > maxSecretLen {
		return fmt.Errorf("%w: credential length %d exceeds %d", ErrTooLarge, len(credential), maxSecretLen)
	}
	head := []byte{markerHello, ProtocolVersion}
	head = appendLen16(head, credential)
	head = append(head, credential...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write hello: %w", err)
	}
	return nil
}

func WritePairRequest(w io.Writer, invitation, deviceID string) error {
	for _, f := range []struct {
		value string
		max   int
		what  string
	}{
		{invitation, maxSecretLen, "invitation"},
		{deviceID, maxDeviceIDLen, "device ID"},
	} {
		if f.value == "" {
			return fmt.Errorf("hubsync: pairing request has an empty %s", f.what)
		}
		if len(f.value) > f.max {
			return fmt.Errorf("%w: %s length %d exceeds %d", ErrTooLarge, f.what, len(f.value), f.max)
		}
	}
	head := []byte{markerPair, ProtocolVersion}
	head = appendLen16(head, invitation)
	head = append(head, invitation...)
	head = appendLen16(head, deviceID)
	head = append(head, deviceID...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write pairing request: %w", err)
	}
	return nil
}

func WriteCredentialReply(w io.Writer, r CredentialReply) error {
	for _, f := range []struct {
		value string
		max   int
		what  string
	}{
		{r.DeviceID, maxDeviceIDLen, "device ID"},
		{r.Credential, maxSecretLen, "credential"},
	} {
		if f.value == "" {
			return fmt.Errorf("hubsync: credential reply has an empty %s", f.what)
		}
		if len(f.value) > f.max {
			return fmt.Errorf("%w: %s length %d exceeds %d", ErrTooLarge, f.what, len(f.value), f.max)
		}
	}
	head := []byte{markerCredential, ProtocolVersion}
	head = appendLen16(head, r.DeviceID)
	head = append(head, r.DeviceID...)
	head = appendLen16(head, r.Credential)
	head = append(head, r.Credential...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write credential reply: %w", err)
	}
	return nil
}

func ReadCredentialReply(r io.Reader) (CredentialReply, *Error, error) {
	fr := &frameReader{r: r}
	marker, err := replyHead(fr)
	if err != nil {
		return CredentialReply{}, nil, err
	}
	switch marker {
	case markerCredential:
		var reply CredentialReply
		if reply.DeviceID, err = fr.str16(maxDeviceIDLen, "device ID"); err != nil {
			return CredentialReply{}, nil, err
		}
		if reply.Credential, err = fr.str16(maxSecretLen, "credential"); err != nil {
			return CredentialReply{}, nil, err
		}
		return reply, nil, nil
	case markerError:
		perr, err := readErrorFrame(fr)
		if err != nil {
			return CredentialReply{}, nil, err
		}
		return CredentialReply{}, perr, nil
	default:
		return CredentialReply{}, nil, fmt.Errorf("hubsync: bad pairing reply marker %q", marker)
	}
}

func Pair(conn net.Conn, invitation, deviceID string) (string, error) {
	if err := WritePairRequest(conn, invitation, deviceID); err != nil {
		return "", err
	}
	reply, perr, err := ReadCredentialReply(conn)
	if err != nil {
		return "", fmt.Errorf("hubsync: pair: %w", err)
	}
	if perr != nil {
		return "", perr
	}
	if reply.DeviceID != deviceID {
		return "", fmt.Errorf("hubsync: pair: hub enrolled device %s, not %s", reply.DeviceID, deviceID)
	}
	return reply.Credential, nil
}

func appendLen16(buf []byte, s string) []byte {
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(s)))
	return append(buf, l[:]...)
}

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
