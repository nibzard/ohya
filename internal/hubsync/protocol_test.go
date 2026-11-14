package hubsync

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/store"
)

const (
	testDevice = "0123456789abcdef0123456789abcdef"
	testSource = "fedcba0987654321fedcba0987654321"
)

func testRecords() []Record {
	return []Record{
		{Offset: 0, JSON: []byte(`{"type":"user","sessionId":"s","message":{"role":"user","content":"one"}}`)},
		{Offset: 0, JSON: []byte(`{"type":"assistant","sessionId":"s"}`)},
	}
}

func withEnds(rs []Record) []Record {
	out := make([]Record, len(rs))
	copy(out, rs)
	for i := range out {
		if i > 0 {
			out[i].Offset = out[i-1].End()
		}
	}
	return out
}

func TestProtocolRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	reg := Registration{DeviceID: testDevice, SourceID: testSource,
		Agent: "claude-code", Path: "/home/dev/.claude/projects/p/s.jsonl"}
	if err := WriteRegistration(&buf, reg); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Registration == nil || *frame.Registration != reg {
		t.Fatalf("registration round trip = %+v, %v", frame.Registration, err)
	}

	records := withEnds(testRecords())
	batch := Batch{DeviceID: testDevice, SourceID: testSource,
		Start: 0, End: records[len(records)-1].End(), Records: records}
	if err := WriteBatch(&buf, batch); err != nil {
		t.Fatal(err)
	}
	frame, err = ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got := frame.Batch
	if got == nil {
		t.Fatal("batch frame did not decode")
	}
	if got.DeviceID != batch.DeviceID || got.SourceID != batch.SourceID ||
		got.Start != batch.Start || got.End != batch.End || len(got.Records) != len(batch.Records) {
		t.Fatalf("batch round trip = %+v", got)
	}
	for i := range got.Records {
		if got.Records[i].Offset != batch.Records[i].Offset ||
			!bytes.Equal(got.Records[i].JSON, batch.Records[i].JSON) {
			t.Fatalf("record %d round trip = %+v", i, got.Records[i])
		}
		if end := got.Records[i].End(); end != got.Records[i].Offset+int64(len(got.Records[i].JSON))+1 {
			t.Fatalf("record %d End = %d", i, end)
		}
	}

	for _, ack := range []Ack{
		{Cursor: 6528, Inserted: 16},
		{Stopped: true, Cursor: 1094, StopOffset: 1094, Stop: "claudecode: source stops at offset 1094: record is not valid JSON"},
	} {
		if err := WriteAck(&buf, ack); err != nil {
			t.Fatal(err)
		}
		gotAck, perr, err := ReadReply(&buf)
		if err != nil || perr != nil {
			t.Fatalf("ack round trip = %+v, %v, %v", gotAck, perr, err)
		}
		if gotAck != ack {
			t.Fatalf("ack round trip = %+v, want %+v", gotAck, ack)
		}
	}

	if err := WriteError(&buf, CodeRejected, "gap at 42"); err != nil {
		t.Fatal(err)
	}
	_, perr, err := ReadReply(&buf)
	if err != nil || perr == nil || perr.Code != CodeRejected || perr.Message != "gap at 42" {
		t.Fatalf("error round trip = %v, %+v", err, perr)
	}
}

// TestReadFrameRejectsBrokenFrames feeds structurally invalid frames; every
// one must fail without panic, and bound violations must identify themselves
// through ErrTooLarge or ErrVersion.
func TestReadFrameRejectsBrokenFrames(t *testing.T) {
	oversizedRecord := make([]byte, 0, 64)
	oversizedRecord = append(oversizedRecord, 'B', ProtocolVersion)
	oversizedRecord = appendLen16(oversizedRecord, testDevice)
	oversizedRecord = append(oversizedRecord, testDevice...)
	oversizedRecord = appendLen16(oversizedRecord, testSource)
	oversizedRecord = append(oversizedRecord, testSource...)
	// start 0, end 16, count 1, offset 0, len MaxRecordBytes+1.
	oversizedRecord = append(oversizedRecord, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 16, 0, 0, 0, 1)
	oversizedRecord = append(oversizedRecord, 0, 0, 0, 0, 0, 0, 0, 0)
	var badLen [4]byte
	binary.BigEndian.PutUint32(badLen[:], uint32(MaxRecordBytes+1))
	oversizedRecord = append(oversizedRecord, badLen[:]...)

	for _, tc := range []struct {
		name  string
		frame []byte
		want  error
		sub   string
	}{
		{"bad marker", []byte{'X', ProtocolVersion}, nil, "bad frame marker"},
		{"bad version", []byte{'B', ProtocolVersion + 1}, ErrVersion, "version"},
		{"truncated", []byte{'B', ProtocolVersion, 0}, nil, "read frame"},
		{"empty device", append([]byte{'R', ProtocolVersion, 0, 0}, 2), nil, "empty device ID"},
		{"oversized record length", oversizedRecord, ErrTooLarge, "record"},
		{"oversized path length", append([]byte{'R', ProtocolVersion}, 1, 32), nil, ""},
	} {
		_, err := ReadFrame(bytes.NewReader(tc.frame))
		if err == nil {
			t.Fatalf("%s: decode succeeded, want failure", tc.name)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v; want %v", tc.name, err, tc.want)
		}
		if tc.sub != "" && !strings.Contains(err.Error(), tc.sub) {
			t.Fatalf("%s: err = %v; want text %q", tc.name, err, tc.sub)
		}
	}
}

// TestReadFrameRejectsOversizedBatch declares in-range per-record lengths
// whose total, span, and count each break a different bound.
func TestReadFrameRejectsOversizedBatch(t *testing.T) {
	head := func(start, end int64, count uint32) []byte {
		b := []byte{'B', ProtocolVersion}
		b = appendLen16(b, testDevice)
		b = append(b, testDevice...)
		b = appendLen16(b, testSource)
		b = append(b, testSource...)
		var wide [8]byte
		binary.BigEndian.PutUint64(wide[:], uint64(start))
		b = append(b, wide[:]...)
		binary.BigEndian.PutUint64(wide[:], uint64(end))
		b = append(b, wide[:]...)
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], count)
		b = append(b, n[:]...)
		return b
	}
	if _, err := ReadFrame(bytes.NewReader(head(0, MaxBatchBytes+1, 0))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("span error = %v; want ErrTooLarge", err)
	}
	if _, err := ReadFrame(bytes.NewReader(head(-1, 0, 0))); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative start error = %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader(head(5, 1, 0))); err == nil || !strings.Contains(err.Error(), "precedes") {
		t.Fatalf("inverted range error = %v", err)
	}
	count := MaxRecords + 1
	if _, err := ReadFrame(bytes.NewReader(head(0, 0, uint32(count)))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("count error = %v; want ErrTooLarge", err)
	}
}

// TestWritersRefuseOversizedFrames pins the sender-side bounds so a device
// never builds a frame the hub must reject.
func TestWritersRefuseOversizedFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRegistration(&buf, Registration{
		DeviceID: testDevice, SourceID: testSource, Agent: "claude-code",
		Path: strings.Repeat("p", maxPathLen+1),
	}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized path err = %v; want ErrTooLarge", err)
	}
	big := strings.Repeat("x", int(MaxRecordBytes)+1)
	batch := Batch{DeviceID: testDevice, SourceID: testSource, Start: 0, End: MaxBatchBytes, Records: []Record{
		{Offset: 0, JSON: []byte(big)},
	}}
	if err := WriteBatch(&buf, batch); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized record err = %v; want ErrTooLarge", err)
	}
	batch = Batch{DeviceID: testDevice, SourceID: testSource, Start: 0, End: MaxBatchBytes + 1}
	if err := WriteBatch(&buf, batch); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized span err = %v; want ErrTooLarge", err)
	}
	batch = Batch{DeviceID: testDevice, SourceID: testSource, Start: 0, End: 0, Records: make([]Record, MaxRecords+1)}
	if err := WriteBatch(&buf, batch); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized count err = %v; want ErrTooLarge", err)
	}
	if err := WriteBatch(&buf, Batch{DeviceID: "", SourceID: testSource}); err == nil {
		t.Fatal("empty device ID was framed")
	}
}

// FuzzReadFrame checks the decoder against arbitrary and mutated frames: it
// must never panic, and a successful decode of a valid encoding must round
// trip. Run with go test -fuzz=FuzzReadFrame for deep fuzzing.
func FuzzReadFrame(f *testing.F) {
	var buf bytes.Buffer
	records := withEnds(testRecords())
	if err := WriteRegistration(&buf, Registration{DeviceID: testDevice, SourceID: testSource,
		Agent: "claude-code", Path: "/p/s.jsonl"}); err != nil {
		f.Fatal(err)
	}
	if err := WriteBatch(&buf, Batch{DeviceID: testDevice, SourceID: testSource,
		Start: 0, End: records[1].End(), Records: records}); err != nil {
		f.Fatal(err)
	}
	if err := WriteAck(&buf, Ack{Stopped: true, Cursor: 3, Stop: "stopped"}); err != nil {
		f.Fatal(err)
	}
	if err := WriteError(&buf, CodeBusy, "full"); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			return // rejected input, including truncation
		}
		var out bytes.Buffer
		switch {
		case frame.Registration != nil:
			err = WriteRegistration(&out, *frame.Registration)
		case frame.Batch != nil:
			err = WriteBatch(&out, *frame.Batch)
		}
		if err != nil {
			return // decode accepted what the writer refuses; uneven but safe
		}
		again, err := ReadFrame(&out)
		if err != nil {
			t.Fatalf("re-decoding a re-encoded frame failed: %v", err)
		}
		if (frame.Registration == nil) != (again.Registration == nil) ||
			(frame.Batch == nil) != (again.Batch == nil) {
			t.Fatal("frame kind changed across the round trip")
		}
	})
}

// TestAuthFrameRoundTrip covers the pairing and hello frames: the client
// writers and the readers decode each other, and the first-frame reader
// consumes exactly the frame's bytes, leaving the next frame intact.
func TestAuthFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHello(&buf, "cred-1"); err != nil {
		t.Fatal(err)
	}
	if err := WriteRegistration(&buf, Registration{DeviceID: testDevice, SourceID: testSource,
		Agent: "claude-code", Path: "/p/s.jsonl"}); err != nil {
		t.Fatal(err)
	}
	hello, pair, err := ReadAuthFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if pair != nil || hello == nil || hello.Credential != "cred-1" {
		t.Fatalf("hello = %+v, pair = %+v; want the credential hello", hello, pair)
	}
	frame, err := ReadFrame(&buf)
	if err != nil || frame.Registration == nil {
		t.Fatalf("next frame = %+v, %v; want the registration that followed", frame, err)
	}

	buf.Reset()
	if err := WritePairRequest(&buf, "inv-1", testDevice); err != nil {
		t.Fatal(err)
	}
	hello, pair, err = ReadAuthFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if hello != nil || pair == nil || pair.Invitation != "inv-1" || pair.DeviceID != testDevice {
		t.Fatalf("hello = %+v, pair = %+v; want the pairing request", hello, pair)
	}

	buf.Reset()
	if err := WriteCredentialReply(&buf, CredentialReply{DeviceID: testDevice, Credential: "cred-1"}); err != nil {
		t.Fatal(err)
	}
	reply, perr, err := ReadCredentialReply(&buf)
	if err != nil || perr != nil || reply.DeviceID != testDevice || reply.Credential != "cred-1" {
		t.Fatalf("credential reply = %+v, %+v, %v", reply, perr, err)
	}

	// Writers refuse what the reader would reject.
	if err := WriteHello(&buf, ""); err == nil {
		t.Fatal("an empty credential was framed")
	}
	if err := WriteHello(&buf, strings.Repeat("c", maxSecretLen+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized credential err = %v; want ErrTooLarge", err)
	}
	if err := WritePairRequest(&buf, "", testDevice); err == nil {
		t.Fatal("an empty invitation was framed")
	}
}

// TestReadAuthFrameRejectsBrokenFrames pins the first-frame gate: wrong
// version, wrong marker, oversize and truncation all fail, and the bounds
// hold before any allocation.
func TestReadAuthFrameRejectsBrokenFrames(t *testing.T) {
	oversized := []byte{'H', ProtocolVersion}
	oversized = appendLen16(oversized, strings.Repeat("c", maxSecretLen+1))
	oversized = append(oversized, strings.Repeat("c", maxSecretLen+1)...)
	for _, tc := range []struct {
		name  string
		frame []byte
		want  error
		sub   string
	}{
		{"registration first", []byte{'R', ProtocolVersion}, nil, "bad auth frame marker"},
		{"batch first", []byte{'B', ProtocolVersion}, nil, "bad auth frame marker"},
		{"bad version", []byte{'H', ProtocolVersion + 1}, ErrVersion, "version"},
		{"truncated", []byte{'P', ProtocolVersion, 0}, nil, "read"},
		{"empty credential", []byte{'H', ProtocolVersion, 0, 0}, nil, "empty credential"},
		{"oversized credential", oversized, ErrTooLarge, "credential length"},
	} {
		_, _, err := ReadAuthFrame(bytes.NewReader(tc.frame))
		if err == nil {
			t.Fatalf("%s: decode succeeded, want failure", tc.name)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v; want %v", tc.name, err, tc.want)
		}
		if tc.sub != "" && !strings.Contains(err.Error(), tc.sub) {
			t.Fatalf("%s: err = %v; want text %q", tc.name, err, tc.sub)
		}
	}
}

// FuzzReadAuthFrame checks the first-frame decoder against arbitrary input:
// it must never panic. Run with go test -fuzz=FuzzReadAuthFrame for deep
// fuzzing.
func FuzzReadAuthFrame(f *testing.F) {
	var buf bytes.Buffer
	if err := WriteHello(&buf, "cred-1"); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	buf.Reset()
	if err := WritePairRequest(&buf, "inv-1", testDevice); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte{'H', ProtocolVersion, 0})
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		hello, pair, err := ReadAuthFrame(bytes.NewReader(data))
		if err != nil {
			return // rejected input, including truncation
		}
		if hello == nil && pair == nil {
			t.Fatal("a decoded frame carries neither hello nor pairing request")
		}
	})
}

func TestQueryAnnotateFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteQuery(&buf, "SELECT count(*) FROM events"); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Query == nil || frame.Query.SQL != "SELECT count(*) FROM events" {
		t.Fatalf("query round trip = %+v", frame.Query)
	}

	request := []byte(`{"operations":[{"op":"delete","event_id":1,"namespace":"t","key":"k"}]}`)
	if err := WriteAnnotate(&buf, request); err != nil {
		t.Fatal(err)
	}
	frame, err = ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Annotate == nil || !bytes.Equal(frame.Annotate.Request, request) {
		t.Fatalf("annotate round trip = %+v", frame.Annotate)
	}

	// Replies do not cross: an ack is not a query reply, an error frame is.
	if err := WriteAck(&buf, Ack{}); err != nil {
		t.Fatal(err)
	}
	if _, perr, err := ReadQueryReply(&buf); err == nil ||
		!strings.Contains(err.Error(), "bad query reply marker") || perr != nil {
		t.Fatalf("ack as query reply = %+v, %v", perr, err)
	}
	buf.Reset()
	if err := WriteError(&buf, CodeRejected, "no"); err != nil {
		t.Fatal(err)
	}
	if _, perr, err := ReadAnnotateReply(&buf); err != nil || perr == nil || perr.Code != CodeRejected {
		t.Fatalf("error as annotate reply = %+v, %+v, %v", store.AnnotateResult{}, perr, err)
	}

	// Writer-side bounds: empty and oversized requests never reach the wire.
	if err := WriteQuery(&buf, ""); err == nil || !strings.Contains(err.Error(), "no SQL text") {
		t.Fatalf("empty query error = %v", err)
	}
	if err := WriteQuery(&buf, strings.Repeat("a", MaxQueryBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized query error = %v", err)
	}
	if err := WriteAnnotate(&buf, nil); err == nil {
		t.Fatal("empty annotate request was written")
	}
	if err := WriteAnnotate(&buf, make([]byte, MaxAnnotateRequestBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized annotate error = %v", err)
	}

	// Read-side bounds: a declared length past the bound is refused before
	// allocation, and an empty payload is refused after it.
	declared := func(marker byte, n uint32) *bytes.Buffer {
		var b bytes.Buffer
		b.Write([]byte{marker, ProtocolVersion})
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], n)
		b.Write(l[:])
		return &b
	}
	if _, err := ReadFrame(declared('Q', MaxQueryBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized query frame error = %v", err)
	}
	if _, err := ReadFrame(declared('N', MaxAnnotateRequestBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized annotate frame error = %v", err)
	}
	if _, err := ReadFrame(declared('Q', 0)); err == nil || !strings.Contains(err.Error(), "no SQL text") {
		t.Fatalf("empty query frame error = %v", err)
	}
	if _, err := ReadFrame(declared('N', 0)); err == nil || !strings.Contains(err.Error(), "no request JSON") {
		t.Fatalf("empty annotate frame error = %v", err)
	}

	// A result frame declaring past its bound is refused before allocation.
	big := bytes.NewBuffer([]byte{markerQueryResult, ProtocolVersion})
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(MaxQueryResultBytes+1))
	big.Write(l[:])
	if _, _, err := ReadQueryReply(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized result error = %v", err)
	}

	// Annotate result counts are sanity-checked: a hostile count is refused.
	hostile := bytes.NewBuffer([]byte{markerAnnotateRes, ProtocolVersion})
	var wide [8]byte
	binary.BigEndian.PutUint64(wide[:], 1<<40)
	hostile.Write(wide[:])
	binary.BigEndian.PutUint64(wide[:], 0)
	hostile.Write(wide[:])
	if _, _, err := ReadAnnotateReply(hostile); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("hostile annotate counts error = %v", err)
	}
}
