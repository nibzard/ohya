package hubsync

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/workspace"
)

func TestWorkspacesFrameRoundTrip(t *testing.T) {
	want := Workspaces{DeviceID: testDevice, Agent: "claude-code", Observations: []workspace.Observation{
		{Path: "/Users/n/dev/dioclet", Repo: "github.com/nibzard/dioclet"},
		{Path: "/tmp/scratch"},
	}}
	var buf bytes.Buffer
	if err := WriteWorkspaces(&buf, want); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Workspaces == nil || !reflect.DeepEqual(*frame.Workspaces, want) {
		t.Fatalf("frame = %+v, want %+v", frame.Workspaces, want)
	}

	empty := Workspaces{DeviceID: testDevice, Agent: "codex", Observations: []workspace.Observation{}}
	buf.Reset()
	if err := WriteWorkspaces(&buf, empty); err != nil {
		t.Fatal(err)
	}
	if frame, err = ReadFrame(&buf); err != nil || !reflect.DeepEqual(*frame.Workspaces, empty) {
		t.Fatalf("empty frame = %+v, %v", frame.Workspaces, err)
	}

	reply := WorkspacesReply{Result: store.ObserveResult{Applied: 2, Unchanged: 1, Ignored: 3},
		Pending: []string{"/a", "/b c"}}
	buf.Reset()
	if err := WriteWorkspacesReply(&buf, reply); err != nil {
		t.Fatal(err)
	}
	got, perr, err := ReadWorkspacesReply(&buf)
	if err != nil || perr != nil || !reflect.DeepEqual(got, reply) {
		t.Fatalf("reply = %+v, %v, %v; want %+v", got, perr, err, reply)
	}

	buf.Reset()
	if err := WriteError(&buf, CodeRejected, "no"); err != nil {
		t.Fatal(err)
	}
	if _, perr, err := ReadWorkspacesReply(&buf); err != nil || perr == nil || perr.Code != CodeRejected {
		t.Fatalf("error reply = %+v, %v", perr, err)
	}
}

func TestWorkspacesFrameBounds(t *testing.T) {
	many := make([]workspace.Observation, MaxWorkspaces+1)
	for i := range many {
		many[i] = workspace.Observation{Path: "/p"}
	}
	for name, ws := range map[string]Workspaces{
		"too many":     {DeviceID: testDevice, Agent: "claude-code", Observations: many},
		"empty path":   {DeviceID: testDevice, Agent: "claude-code", Observations: []workspace.Observation{{Repo: "github.com/x/y"}}},
		"long path":    {DeviceID: testDevice, Agent: "claude-code", Observations: []workspace.Observation{{Path: "/" + strings.Repeat("p", maxPathLen)}}},
		"long repo":    {DeviceID: testDevice, Agent: "claude-code", Observations: []workspace.Observation{{Path: "/p", Repo: strings.Repeat("r", maxRepoLen+1)}}},
		"empty agent":  {DeviceID: testDevice},
		"empty device": {Agent: "claude-code"},
	} {
		if err := WriteWorkspaces(&bytes.Buffer{}, ws); err == nil {
			t.Errorf("%s: writer accepted the frame", name)
		}
	}

	head := []byte{markerWorkspaces, ProtocolVersion}
	head = appendLen16(head, testDevice)
	head = append(head, testDevice...)
	head = appendLen16(head, "claude-code")
	head = append(head, "claude-code"...)
	head = binary.BigEndian.AppendUint16(head, MaxWorkspaces+1)
	if _, err := ReadFrame(bytes.NewReader(head)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized count: err = %v, want ErrTooLarge", err)
	}

	reply := []byte{markerPending, ProtocolVersion}
	reply = binary.BigEndian.AppendUint64(reply, 1)
	reply = binary.BigEndian.AppendUint64(reply, 0)
	reply = binary.BigEndian.AppendUint64(reply, 0)
	reply = binary.BigEndian.AppendUint16(reply, MaxWorkspaces+1)
	if _, _, err := ReadWorkspacesReply(bytes.NewReader(reply)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized reply: err = %v, want ErrTooLarge", err)
	}
}

func cwdLine(sessionID, cwd, content string) []byte {
	return []byte(`{"type":"user","sessionId":"` + sessionID + `","cwd":"` + cwd +
		`","message":{"role":"user","content":"` + content + `"}}`)
}

func (d *deviceConn) workspaces(ws Workspaces) (WorkspacesReply, *Error) {
	d.t.Helper()
	d.deadline()
	if err := WriteWorkspaces(d.conn, ws); err != nil {
		d.t.Fatal(err)
	}
	reply, perr, err := ReadWorkspacesReply(d.br)
	if err != nil {
		d.t.Fatalf("read workspaces reply: %v", err)
	}
	return reply, perr
}

func TestEndpointObservesWorkspaces(t *testing.T) {
	db := newTestDB(t)
	srv := startEndpoint(t, db, nil, Options{})
	d := dial(t, srv.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	first := Record{Offset: 0, JSON: cwdLine("s1", "/work/dioclet", "one")}
	second := Record{Offset: first.End(), JSON: cwdLine("s2", "/work/scratch", "two")}
	d.send(fixtureBatch(deviceA, "src-1", []Record{first, second}))
	d.mustAck(Ack{Cursor: second.End(), Inserted: 2})

	reply, perr := d.workspaces(Workspaces{DeviceID: deviceA, Agent: claudecode.AgentName})
	if perr != nil {
		t.Fatal(perr)
	}
	if !reflect.DeepEqual(reply.Pending, []string{"/work/dioclet", "/work/scratch"}) {
		t.Fatalf("pending = %q", reply.Pending)
	}
	reply, perr = d.workspaces(Workspaces{DeviceID: deviceA, Agent: claudecode.AgentName,
		Observations: []workspace.Observation{
			{Path: "/work/dioclet", Repo: "github.com/nibzard/dioclet"},
			{Path: "/work/scratch"},
		}})
	if perr != nil {
		t.Fatal(perr)
	}
	if reply.Result != (store.ObserveResult{Applied: 2}) || len(reply.Pending) != 0 {
		t.Fatalf("reply = %+v", reply)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM sessions WHERE repo = 'github.com/nibzard/dioclet'`); n != 1 {
		t.Fatalf("sessions with the observed repository = %d, want 1", n)
	}

	_, perr = d.workspaces(Workspaces{DeviceID: deviceA, Agent: claudecode.AgentName,
		Observations: []workspace.Observation{{Path: "/work/scratch", Repo: "https://github.com/x/y.git"}}})
	if perr == nil || perr.Code != CodeRejected {
		t.Fatalf("raw remote URL: error = %+v, want rejected", perr)
	}
	_, perr = d.workspaces(Workspaces{DeviceID: deviceA, Agent: "no-such-agent"})
	if perr == nil || perr.Code != CodeUnsupported {
		t.Fatalf("unknown agent: error = %+v, want unsupported", perr)
	}

	d.deadline()
	if err := WriteWorkspaces(d.conn, Workspaces{DeviceID: deviceB, Agent: claudecode.AgentName}); err != nil {
		t.Fatal(err)
	}
	d.wantError(CodeAuth, "belongs to")
	d.expectClosed()
}
