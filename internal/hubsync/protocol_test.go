package hubsync

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
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
	count := maxRecords + 1
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
	batch = Batch{DeviceID: testDevice, SourceID: testSource, Start: 0, End: 0, Records: make([]Record, maxRecords+1)}
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
