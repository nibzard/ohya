package hubsync

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
)

type remoteUsageSink struct {
	mu      sync.Mutex
	records []observe.UsageRecord
}

func (s *remoteUsageSink) Record(r observe.UsageRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	return true
}

func (s *remoteUsageSink) all() []observe.UsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observe.UsageRecord(nil), s.records...)
}

func TestRemoteCallsRecordUsageOnTheHub(t *testing.T) {
	sink := &remoteUsageSink{}
	s, _, _ := remoteFixtureHub(t, mcpserver.EngineOptions{Usage: sink})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)
	ctx := context.Background()

	if _, err := b.Query(ctx, `SELECT count(*) FROM events`); err != nil {
		t.Fatal(err)
	}
	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(records))
	}
	record := records[0]
	if record.Tool != observe.UsageToolSQL || record.Outcome != observe.UsageOutcomeOK {
		t.Fatalf("tool/outcome = %s/%s", record.Tool, record.Outcome)
	}
	if record.Path != observe.UsagePathRemote {
		t.Fatalf("path = %s, want remote", record.Path)
	}
	if record.Client != observe.UsageClientUnknown {
		t.Fatalf("client = %s, want unknown: the query frame carries no client name", record.Client)
	}
	if record.DeviceID != deviceB {
		t.Fatalf("device_id = %s, want the authenticated calling device %s", record.DeviceID, deviceB)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}

	if _, err := b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"usage","key":"probe","value":1}]}`)); err != nil {
		t.Fatal(err)
	}
	records = sink.all()
	if len(records) != 2 {
		t.Fatalf("usage records = %d, want 2", len(records))
	}
	record = records[1]
	if record.Tool != observe.UsageToolAnnotate || record.Path != observe.UsagePathRemote {
		t.Fatalf("tool/path = %s/%s", record.Tool, record.Path)
	}
	if record.DeviceID != deviceB || record.Client != observe.UsageClientUnknown {
		t.Fatalf("device/client = %s/%s", record.DeviceID, record.Client)
	}
	if record.AnnotateOps == nil || record.AnnotateOps.Upsert != 1 {
		t.Fatalf("annotate_ops = %+v", record.AnnotateOps)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}
}

func TestRemoteUsageRecordsHoldNoQueryText(t *testing.T) {
	const secret = "OHYA-SECRET-remote-2c9f61"
	sink := &remoteUsageSink{}
	s, _, _ := remoteFixtureHub(t, mcpserver.EngineOptions{Usage: sink})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)

	if _, err := b.Query(context.Background(),
		`SELECT count(*) FROM events WHERE text != '`+secret+`'`); err != nil {
		t.Fatal(err)
	}
	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(records))
	}
	encoded, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("hub-side usage record leaks the SQL literal: %s", encoded)
	}
}
