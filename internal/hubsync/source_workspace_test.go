package hubsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/store"
)

func (d *deviceConn) sourceWorkspace(sw SourceWorkspace) {
	d.t.Helper()
	d.deadline()
	if err := WriteSourceWorkspace(d.conn, sw); err != nil {
		d.t.Fatal(err)
	}
}

func TestSourceWorkspaceFrameRoundTrip(t *testing.T) {
	want := SourceWorkspace{DeviceID: testDevice, SourceID: testSource, Path: "/work/with space/antigravity"}
	var buf bytes.Buffer
	if err := WriteSourceWorkspace(&buf, want); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&buf)
	if err != nil || frame.SourceWorkspace == nil || !reflect.DeepEqual(*frame.SourceWorkspace, want) {
		t.Fatalf("frame = %+v, %v; want %+v", frame.SourceWorkspace, err, want)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes left after the frame", buf.Len())
	}
}

func TestSourceWorkspaceFrameBounds(t *testing.T) {
	for name, sw := range map[string]SourceWorkspace{
		"empty device": {SourceID: testSource, Path: "/work/agy"},
		"empty source": {DeviceID: testDevice, Path: "/work/agy"},
		"empty path":   {DeviceID: testDevice, SourceID: testSource},
		"long device":  {DeviceID: strings.Repeat("d", maxDeviceIDLen+1), SourceID: testSource, Path: "/work/agy"},
		"long source":  {DeviceID: testDevice, SourceID: strings.Repeat("s", maxSourceIDLen+1), Path: "/work/agy"},
		"long path":    {DeviceID: testDevice, SourceID: testSource, Path: "/" + strings.Repeat("p", maxPathLen)},
	} {
		if err := WriteSourceWorkspace(&bytes.Buffer{}, sw); err == nil {
			t.Errorf("%s: writer accepted the frame", name)
		}
	}
	longest := SourceWorkspace{DeviceID: testDevice, SourceID: testSource, Path: "/" + strings.Repeat("p", maxPathLen-1)}
	if err := WriteSourceWorkspace(&bytes.Buffer{}, longest); err != nil {
		t.Fatalf("a path of exactly %d bytes: %v", maxPathLen, err)
	}

	frame := []byte{markerSourceWorkspace, ProtocolVersion}
	frame = appendLen16(frame, testDevice)
	frame = append(frame, testDevice...)
	frame = appendLen16(frame, testSource)
	frame = append(frame, testSource...)
	head := frame
	frame = binary.BigEndian.AppendUint32(frame, maxPathLen+1)
	if _, err := ReadFrame(bytes.NewReader(frame)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized path: err = %v, want ErrTooLarge", err)
	}
	frame = binary.BigEndian.AppendUint32(head, 0)
	if _, err := ReadFrame(bytes.NewReader(frame)); err == nil || errors.Is(err, ErrTooLarge) {
		t.Fatalf("empty path: err = %v, want a decode error", err)
	}
}

func storedWorkspace(t *testing.T, db *store.DB, sourceID string) string {
	t.Helper()
	var path string
	if err := db.Writer().QueryRow(`SELECT workspace_path FROM sources WHERE id = ?`, sourceID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEndpointStoresSourceWorkspace(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	auth := newPeerAuth()
	s := startEndpoint(t, db, auth, Options{})

	a := dial(t, s.addr)
	auth.assign(a.conn, deviceA)
	mustRegisterVia(t, a, deviceA, "src-1", "/brain/c1/.system_generated/logs/transcript_full.jsonl")
	a.sourceWorkspace(SourceWorkspace{DeviceID: deviceA, SourceID: "src-1", Path: "/work/agy"})
	a.mustAck(Ack{})
	a.sourceWorkspace(SourceWorkspace{DeviceID: deviceA, SourceID: "src-1", Path: "/work/agy"})
	a.mustAck(Ack{})
	a.sourceWorkspace(SourceWorkspace{DeviceID: deviceA, SourceID: "src-1", Path: "/work/moved"})
	a.mustAck(Ack{})
	if got := storedWorkspace(t, db, "src-1"); got != "/work/agy" {
		t.Fatalf("workspace = %q; want the first path /work/agy", got)
	}

	late := Record{Offset: 0, JSON: rawLine("late", "after the workspace")}
	a.send(fixtureBatch(deviceA, "src-1", []Record{late}))
	a.mustAck(Ack{Cursor: late.End(), Inserted: 1})
	var cwd string
	sid := store.SessionID(deviceA, claudecode.AgentName, "late")
	if err := db.Writer().QueryRow(`SELECT cwd FROM sessions WHERE id = ?`, sid).Scan(&cwd); err != nil || cwd != "/work/agy" {
		t.Fatalf("session cwd = %q, %v; want the source workspace", cwd, err)
	}

	mustRegisterVia(t, a, deviceA, "src-2", "/brain/c2/.system_generated/logs/transcript_full.jsonl")
	doomed := Record{Offset: 0, JSON: rawLine("doomed", "purged")}
	a.send(fixtureBatch(deviceA, "src-2", []Record{doomed}))
	a.mustAck(Ack{Cursor: doomed.End(), Inserted: 1})
	if _, _, err := db.ApplyPurge(ctx, store.PurgeSelector{Session: "doomed"}); err != nil {
		t.Fatal(err)
	}

	a.sourceWorkspace(SourceWorkspace{DeviceID: deviceA, SourceID: "missing", Path: "/work/agy"})
	a.wantError(CodeUnknownSource, "is not registered")
	a.sourceWorkspace(SourceWorkspace{DeviceID: deviceA, SourceID: "src-2", Path: "/work/agy"})
	a.wantError(CodeRejected, "was purged by the owner")
	a.sourceWorkspace(SourceWorkspace{DeviceID: deviceA, SourceID: "src-1", Path: "/work/\x00agy"})
	a.wantError(CodeRejected, "not valid text")
	if got := storedWorkspace(t, db, "src-1"); got != "/work/agy" {
		t.Fatalf("workspace after refused frames = %q; want /work/agy", got)
	}

	b := dial(t, s.addr)
	auth.assign(b.conn, deviceB)
	mustRegisterVia(t, b, deviceB, "src-b", "/b.jsonl")
	b.sourceWorkspace(SourceWorkspace{DeviceID: deviceB, SourceID: "src-1", Path: "/work/agy"})
	b.wantError(CodeOwnership, "belongs to device")

	liar := dial(t, s.addr)
	auth.assign(liar.conn, deviceA)
	liar.sourceWorkspace(SourceWorkspace{DeviceID: deviceB, SourceID: "src-1", Path: "/work/agy"})
	liar.wantError(CodeAuth, "belongs to")
	liar.expectClosed()
	if got := storedWorkspace(t, db, "src-1"); got != "/work/agy" {
		t.Fatalf("workspace after foreign frames = %q; want /work/agy", got)
	}
}
