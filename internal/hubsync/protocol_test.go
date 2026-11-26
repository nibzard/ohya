package hubsync

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/workspace"
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

func TestReadFrameRejectsBrokenFrames(t *testing.T) {
	oversizedRecord := make([]byte, 0, 64)
	oversizedRecord = append(oversizedRecord, 'B', ProtocolVersion)
	oversizedRecord = appendLen16(oversizedRecord, testDevice)
	oversizedRecord = append(oversizedRecord, testDevice...)
	oversizedRecord = appendLen16(oversizedRecord, testSource)
	oversizedRecord = append(oversizedRecord, testSource...)
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

func pseudoRandom(n int, seed uint64) []byte {
	out := make([]byte, n)
	x := seed | 1
	for i := range out {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		out[i] = byte(x)
	}
	return out
}

func compressedPayload(t *testing.T, b Batch) ([]byte, []byte) {
	t.Helper()
	section := appendRecordSection(nil, b)
	return section, batchEncoder().EncodeAll(section, nil)
}

func rawZBatch(b Batch, rawLen int64, payload []byte) []byte {
	out := []byte{markerZBatch, ProtocolVersion}
	out = appendLen16(out, b.DeviceID)
	out = append(out, b.DeviceID...)
	out = appendLen16(out, b.SourceID)
	out = append(out, b.SourceID...)
	out = binary.BigEndian.AppendUint64(out, uint64(b.Start))
	out = binary.BigEndian.AppendUint64(out, uint64(b.End))
	out = binary.BigEndian.AppendUint32(out, uint32(len(b.Records)))
	out = binary.BigEndian.AppendUint64(out, uint64(rawLen))
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)))
	return append(out, payload...)
}

func TestCompressedBatchRoundTrip(t *testing.T) {
	records := withEnds(testRecords())
	batch := Batch{DeviceID: testDevice, SourceID: testSource,
		Start: 0, End: records[len(records)-1].End(), Records: records}
	var buf bytes.Buffer
	if err := WriteBatchCompressed(&buf, batch); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 || buf.Bytes()[0] != markerZBatch {
		t.Fatalf("frame marker = %q; want the compressed marker %q", buf.Bytes()[0], markerZBatch)
	}
	frame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Batch == nil {
		t.Fatal("compressed batch frame did not decode")
	}
	got := frame.Batch
	if got.DeviceID != batch.DeviceID || got.SourceID != batch.SourceID ||
		got.Start != batch.Start || got.End != batch.End || len(got.Records) != len(batch.Records) {
		t.Fatalf("compressed batch round trip = %+v", got)
	}
	for i := range got.Records {
		if got.Records[i].Offset != batch.Records[i].Offset ||
			!bytes.Equal(got.Records[i].JSON, batch.Records[i].JSON) {
			t.Fatalf("record %d round trip = %+v", i, got.Records[i])
		}
	}

	buf.Reset()
	incompressible := []Record{
		{Offset: 0, JSON: pseudoRandom(4096, 1)},
		{Offset: 0, JSON: pseudoRandom(4096, 2)},
	}
	incompressible = withEnds(incompressible)
	plain := Batch{DeviceID: testDevice, SourceID: testSource,
		Start: 0, End: incompressible[len(incompressible)-1].End(), Records: incompressible}
	if err := WriteBatchCompressed(&buf, plain); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[0] != markerBatch {
		t.Fatalf("incompressible batch marker = %q; want the plain marker %q", buf.Bytes()[0], markerBatch)
	}
	frame, err = ReadFrame(&buf)
	if err != nil || frame.Batch == nil {
		t.Fatalf("plain fallback decode = %+v, %v", frame.Batch, err)
	}

	buf.Reset()
	probe := Batch{DeviceID: testDevice, SourceID: testSource, Start: 6528, End: 6528}
	if err := WriteBatchCompressed(&buf, probe); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[0] != markerBatch {
		t.Fatalf("empty batch marker = %q; want the plain marker %q", buf.Bytes()[0], markerBatch)
	}
}

func TestReadFrameRejectsBrokenCompressedBatches(t *testing.T) {
	records := withEnds(testRecords())
	batch := Batch{DeviceID: testDevice, SourceID: testSource,
		Start: 0, End: records[len(records)-1].End(), Records: records}
	section, payload := compressedPayload(t, batch)

	for _, tc := range []struct {
		name string
		why  error
		sub  string
	}{
		{"declared length too small", ErrTooLarge, "expands past"},
		{"declared length too large", nil, "decodes to"},
		{"declared length past the bound", ErrTooLarge, "payload bytes"},
		{"truncated compressed stream", nil, "decompress"},
		{"corrupt compressed stream", nil, "decompress"},
		{"expansion past the declared length", ErrTooLarge, "expands past"},
		{"oversized compressed payload", ErrTooLarge, "compressed batch payload"},
		{"trailing record-section bytes", nil, "trailing bytes"},
	} {
		var frame []byte
		switch tc.name {
		case "declared length too small":
			frame = rawZBatch(batch, int64(len(section))-1, payload)
		case "declared length too large":
			frame = rawZBatch(batch, int64(len(section))+1, payload)
		case "declared length past the bound":
			bound := int64(MaxBatchBytes) + int64(len(batch.Records))*batchRecordFrameBytes
			frame = rawZBatch(batch, bound+1, payload)
		case "truncated compressed stream":
			frame = rawZBatch(batch, int64(len(section)), payload[:len(payload)/2])
		case "corrupt compressed stream":
			bad := bytes.Clone(payload)
			bad[len(bad)/2] ^= 0xff
			frame = rawZBatch(batch, int64(len(section)), bad)
		case "expansion past the declared length":
			bomb := batchEncoder().EncodeAll(bytes.Repeat([]byte{0}, 1<<20), nil)
			frame = rawZBatch(batch, 1<<19, bomb)
		case "oversized compressed payload":
			head := rawZBatch(batch, 0, nil)
			head = head[:len(head)-4]
			head = binary.BigEndian.AppendUint32(head, uint32(MaxBatchBytes+1))
			frame = head
		case "trailing record-section bytes":
			withTail := appendRecordSection(nil, batch)
			withTail = append(withTail, "trailing"...)
			frame = rawZBatch(batch, int64(len(withTail)), batchEncoder().EncodeAll(withTail, nil))
		}
		_, err := ReadFrame(bytes.NewReader(frame))
		if err == nil {
			t.Fatalf("%s: decode succeeded, want failure", tc.name)
		}
		if tc.why != nil && !errors.Is(err, tc.why) {
			t.Fatalf("%s: err = %v; want %v", tc.name, err, tc.why)
		}
		if !strings.Contains(err.Error(), tc.sub) {
			t.Fatalf("%s: err = %v; want text %q", tc.name, err, tc.sub)
		}
	}

	var valid bytes.Buffer
	if err := WriteBatchCompressed(&valid, batch); err != nil {
		t.Fatal(err)
	}
	frame := valid.Bytes()
	for cut := 1; cut < len(frame); cut++ {
		if _, err := ReadFrame(bytes.NewReader(frame[:cut])); err == nil {
			t.Fatalf("prefix of %d bytes decoded, want failure", cut)
		}
	}
}

func TestCapabilityFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCapabilities(&buf, CapZstdBatches); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Capabilities == nil || *frame.Capabilities != CapZstdBatches {
		t.Fatalf("capability frame = %+v", frame.Capabilities)
	}

	buf.Reset()
	if err := WriteWelcome(&buf, CapZstdBatches); err != nil {
		t.Fatal(err)
	}
	caps, perr, err := ReadWelcome(&buf)
	if err != nil || perr != nil || caps != CapZstdBatches {
		t.Fatalf("welcome = %v, %+v, %v", caps, perr, err)
	}

	buf.Reset()
	if err := WriteError(&buf, CodeMalformed, "bad frame marker 'V'"); err != nil {
		t.Fatal(err)
	}
	caps, perr, err = ReadWelcome(&buf)
	if err != nil || perr == nil || perr.Code != CodeMalformed || caps != 0 {
		t.Fatalf("welcome over error = %v, %+v, %v", caps, perr, err)
	}

	buf.Reset()
	if err := WriteCapabilities(&buf, 0); err == nil {
		t.Fatal("zero capability mask was framed")
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{markerCaps, ProtocolVersion, 0, 0})); err == nil ||
		!strings.Contains(err.Error(), "no capabilities") {
		t.Fatalf("zero capability mask err = %v", err)
	}
	if _, _, err := ReadWelcome(bytes.NewReader([]byte{markerAck, ProtocolVersion})); err == nil ||
		!strings.Contains(err.Error(), "bad welcome marker") {
		t.Fatalf("bad welcome marker err = %v", err)
	}
}

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
	if err := WriteBatchCompressed(&buf, Batch{DeviceID: testDevice, SourceID: testSource,
		Start: 0, End: records[1].End(), Records: records}); err != nil {
		f.Fatal(err)
	}
	if err := WriteCapabilities(&buf, CapZstdBatches); err != nil {
		f.Fatal(err)
	}
	if err := WriteAck(&buf, Ack{Stopped: true, Cursor: 3, Stop: "stopped"}); err != nil {
		f.Fatal(err)
	}
	if err := WriteError(&buf, CodeBusy, "full"); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	buf.Reset()
	if err := WriteWorkspaces(&buf, Workspaces{DeviceID: testDevice, Agent: "claude-code",
		Observations: []workspace.Observation{{Path: "/p", Repo: "github.com/x/y"}, {Path: "/q"}}}); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	buf.Reset()
	if err := WriteSourceWorkspace(&buf, SourceWorkspace{DeviceID: testDevice, SourceID: testSource,
		Path: "/brain/conversations/c1/.system_generated/logs/transcript_full.jsonl"}); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	buf.Reset()
	if err := WriteDiagnosticsRequest(&buf, DiagnosticsFormatVersion); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	buf.Reset()
	if err := WriteProbes(&buf, Probes{DeviceID: testDevice, SourceIDs: []string{testSource}}); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			return
		}
		var out bytes.Buffer
		switch {
		case frame.Registration != nil:
			err = WriteRegistration(&out, *frame.Registration)
		case frame.Batch != nil:
			err = WriteBatch(&out, *frame.Batch)
		case frame.Capabilities != nil:
			err = WriteCapabilities(&out, *frame.Capabilities)
		case frame.Query != nil:
			err = WriteQuery(&out, frame.Query.SQL)
		case frame.Annotate != nil:
			err = WriteAnnotate(&out, frame.Annotate.Request)
		case frame.Workspaces != nil:
			err = WriteWorkspaces(&out, *frame.Workspaces)
		case frame.Diagnostics != nil:
			err = WriteDiagnosticsRequest(&out, frame.Diagnostics.FormatVersion)
		case frame.SourceWorkspace != nil:
			err = WriteSourceWorkspace(&out, *frame.SourceWorkspace)
		case frame.Probes != nil:
			err = WriteProbes(&out, *frame.Probes)
		}
		if err != nil {
			return
		}
		again, err := ReadFrame(&out)
		if err != nil {
			t.Fatalf("re-decoding a re-encoded frame failed: %v", err)
		}
		if (frame.Registration == nil) != (again.Registration == nil) ||
			(frame.Batch == nil) != (again.Batch == nil) ||
			(frame.Capabilities == nil) != (again.Capabilities == nil) ||
			(frame.Query == nil) != (again.Query == nil) ||
			(frame.Annotate == nil) != (again.Annotate == nil) ||
			(frame.Workspaces == nil) != (again.Workspaces == nil) ||
			(frame.Diagnostics == nil) != (again.Diagnostics == nil) ||
			(frame.SourceWorkspace == nil) != (again.SourceWorkspace == nil) ||
			(frame.Probes == nil) != (again.Probes == nil) {
			t.Fatal("frame kind changed across the round trip")
		}
	})
}

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
			return
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

	big := bytes.NewBuffer([]byte{markerQueryResult, ProtocolVersion})
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(MaxQueryResultBytes+1))
	big.Write(l[:])
	if _, _, err := ReadQueryReply(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized result error = %v", err)
	}

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

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPayloadFramesWireFormatAndWriteErrors(t *testing.T) {
	cases := []struct {
		name    string
		write   func(io.Writer) error
		marker  byte
		payload string
	}{
		{"query", func(w io.Writer) error { return WriteQuery(w, "SELECT 1") }, 'Q', "SELECT 1"},
		{"annotate", func(w io.Writer) error { return WriteAnnotate(w, []byte("{}")) }, 'N', "{}"},
		{"result", func(w io.Writer) error { return WriteQueryResult(w, mcpserver.Result{}) }, 'S', `{"columns":null,"rows":null,"row_count":0,"truncated":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := tc.write(&b); err != nil {
				t.Fatal(err)
			}
			want := []byte{tc.marker, ProtocolVersion, 0, 0, 0, byte(len(tc.payload))}
			want = append(want, tc.payload...)
			if !bytes.Equal(b.Bytes(), want) {
				t.Fatalf("frame = %x, want %x", b.Bytes(), want)
			}
			if err := tc.write(failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("write error = %v", err)
			}
		})
	}
}

func TestProbesRoundTripAndBounds(t *testing.T) {
	var buf bytes.Buffer
	req := Probes{DeviceID: testDevice, SourceIDs: []string{testSource, "11111111111111111111111111111111"}}
	if err := WriteProbes(&buf, req); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Probes == nil || frame.Probes.DeviceID != req.DeviceID || len(frame.Probes.SourceIDs) != 2 {
		t.Fatalf("probes round trip = %+v", frame.Probes)
	}

	reply := ProbesReply{
		Entries: []store.SourceProbe{
			{Status: store.SourceProbeKnown, Cursor: 4096},
			{Status: store.SourceProbeUnknown},
			{Status: store.SourceProbeOwnership, Message: "held by another device"},
			{Status: store.SourceProbePurged, Message: "purged by owner"},
		},
	}
	buf.Reset()
	if err := WriteProbesReply(&buf, reply); err != nil {
		t.Fatal(err)
	}
	gotReply, perr, err := ReadProbesReply(&buf)
	if err != nil || perr != nil || len(gotReply.Entries) != 4 {
		t.Fatalf("probes reply round trip = %+v, %v, %v", gotReply, perr, err)
	}
	for i := range reply.Entries {
		if gotReply.Entries[i] != reply.Entries[i] {
			t.Fatalf("entry[%d] = %+v, want %+v", i, gotReply.Entries[i], reply.Entries[i])
		}
	}

	buf.Reset()
	if err := WriteError(&buf, CodeAuth, "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, perr, err := ReadProbesReply(&buf); err != nil || perr == nil || perr.Code != CodeAuth {
		t.Fatalf("error over probes reply = %+v, %v", perr, err)
	}

	if err := WriteProbes(&buf, Probes{DeviceID: testDevice}); err == nil {
		t.Fatal("empty probes source list was written")
	}
	if err := WriteProbes(&buf, Probes{DeviceID: testDevice, SourceIDs: make([]string, MaxProbes+1)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized probes write err = %v, want ErrTooLarge", err)
	}
	rawTooMany := []byte{markerProbes, ProtocolVersion}
	rawTooMany = appendLen16(rawTooMany, testDevice)
	rawTooMany = append(rawTooMany, testDevice...)
	rawTooMany = binary.BigEndian.AppendUint16(rawTooMany, MaxProbes+1)
	if _, err := ReadFrame(bytes.NewReader(rawTooMany)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized probes read err = %v, want ErrTooLarge", err)
	}
}

func FuzzReadProbesReply(f *testing.F) {
	var buf bytes.Buffer
	if err := WriteProbesReply(&buf, ProbesReply{
		Entries: []store.SourceProbe{
			{Status: store.SourceProbeKnown, Cursor: 128},
			{Status: store.SourceProbeUnknown},
			{Status: store.SourceProbePurged, Message: "purged"},
		},
	}); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	buf.Reset()
	if err := WriteError(&buf, CodeRejected, "bad"); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		reply, perr, err := ReadProbesReply(bytes.NewReader(data))
		if err != nil {
			return
		}
		var out bytes.Buffer
		if perr != nil {
			err = WriteError(&out, perr.Code, perr.Message)
		} else {
			err = WriteProbesReply(&out, reply)
		}
		if err != nil {
			t.Fatalf("re-encoding decoded probes reply failed: %v", err)
		}
		again, perr2, err := ReadProbesReply(&out)
		if err != nil {
			t.Fatalf("re-decoding re-encoded probes reply failed: %v", err)
		}
		if (perr == nil) != (perr2 == nil) || len(reply.Entries) != len(again.Entries) {
			t.Fatal("probes reply shape changed across round trip")
		}
	})
}
