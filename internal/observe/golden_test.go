package observe

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestGoldenSnapshotDecodesAndIgnoresUnknownFields(t *testing.T) {
	var snapshot Snapshot
	if err := json.Unmarshal(loadFixture(t, "snapshot.json"), &snapshot); err != nil {
		t.Fatalf("snapshot fixture must decode despite unknown future fields: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot fixture must validate: %v", err)
	}
	if snapshot.FormatVersion != SnapshotFormatVersion {
		t.Errorf("format version = %d", snapshot.FormatVersion)
	}
	if snapshot.Counters["parse_records{agent=claude-code}"] != 12 {
		t.Errorf("labelled counter missing: %v", snapshot.Counters)
	}
	if snapshot.Durations["parse_duration{agent=claude-code}"].Count != 12 {
		t.Errorf("duration histogram missing: %v", snapshot.Durations)
	}
	rss := snapshot.Resources["rss_bytes"]
	if rss.Value != nil {
		t.Error("unsupported resource must decode a null value")
	}
	if rss.Reason == "" {
		t.Error("unsupported resource must carry a reason")
	}
	goroutines := snapshot.Resources["goroutines"]
	if goroutines.Value == nil || *goroutines.Value != 42 {
		t.Error("supported resource must decode its value")
	}
	if _, ok := snapshot.Durations["parse_duration{agent=claude-code}"].Quantile(0.5); ok {
		t.Error("histogram below the minimum sample count must not answer quantiles")
	}
	round, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again Snapshot
	if err := json.Unmarshal(round, &again); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if again.Counters["ingest_events{result=inserted}"] != 12 {
		t.Errorf("round trip lost data: %v", again.Counters)
	}
}

func TestGoldenHistorySampleDecodesAsSnapshot(t *testing.T) {
	var snapshot Snapshot
	if err := json.Unmarshal(loadFixture(t, "history-sample.json"), &snapshot); err != nil {
		t.Fatalf("history fixture must decode as a snapshot sample: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("history fixture must validate: %v", err)
	}
	window := Delta(&snapshot, &snapshot)
	if !window.SameInstance || len(window.UnavailableCounters) != 0 {
		t.Errorf("self-delta must be empty and available: %+v", window)
	}
}

func TestGoldenLogEntryDecodes(t *testing.T) {
	var entry LogEntry
	if err := json.Unmarshal(loadFixture(t, "log-entry.json"), &entry); err != nil {
		t.Fatalf("log fixture must decode despite unknown future fields: %v", err)
	}
	if err := entry.Validate(); err != nil {
		t.Fatalf("log fixture must validate: %v", err)
	}
	if _, err := EventCode(entry.EventCode).Curated(); err != nil {
		t.Fatalf("fixture event code must be in the allowlist: %v", err)
	}
	round, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(round, []byte("future_field")) {
		t.Error("unknown fields must not survive a round trip")
	}
}

func TestGoldenUsageRecordSQLDecodes(t *testing.T) {
	var record UsageRecord
	if err := json.Unmarshal(loadFixture(t, "usage-record-sql.json"), &record); err != nil {
		t.Fatalf("usage fixture must decode despite unknown future fields: %v", err)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("usage fixture must validate: %v", err)
	}
	if record.Tool != UsageToolSQL || record.Outcome != UsageOutcomeOK {
		t.Errorf("fixture tool/outcome wrong: %s/%s", record.Tool, record.Outcome)
	}
	if record.Reads[0] != "events.id" || record.FTS != true {
		t.Errorf("fixture reads or fts flag wrong: %v %v", record.Reads, record.FTS)
	}
	round, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again UsageRecord
	if err := json.Unmarshal(round, &again); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if err := again.Validate(); err != nil {
		t.Fatalf("round-tripped record must validate: %v", err)
	}
}

func TestGoldenUsageRecordAnnotateDecodes(t *testing.T) {
	var record UsageRecord
	if err := json.Unmarshal(loadFixture(t, "usage-record-annotate.json"), &record); err != nil {
		t.Fatalf("usage annotate fixture must decode: %v", err)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("usage annotate fixture must validate: %v", err)
	}
	if record.AnnotateOps == nil || record.AnnotateOps.Upsert != 3 || record.AnnotateOps.Delete != 1 {
		t.Errorf("annotate operations wrong: %+v", record.AnnotateOps)
	}
	if record.Client != UsageClientCodex || record.Path != UsagePathRemote {
		t.Errorf("remote annotate provenance wrong: %s/%s", record.Client, record.Path)
	}
}

func TestUnsupportedVersionFailsClearly(t *testing.T) {
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(`{"format_version": 99}`), &snapshot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := snapshot.Validate(); err == nil {
		t.Error("future snapshot versions must fail validation")
	}
	var entry LogEntry
	if err := json.Unmarshal([]byte(`{"format_version": 2, "time_ms": 1, "instance_id": "i", "component": "c", "level": "info", "event_code": "startup"}`), &entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := entry.Validate(); err == nil {
		t.Error("future log versions must fail validation")
	}
	var record UsageRecord
	if err := json.Unmarshal([]byte(`{"schema_version": 2}`), &record); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := record.Validate(); err == nil {
		t.Error("future usage schema versions must fail validation")
	}
}
