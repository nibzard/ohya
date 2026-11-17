package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/observe"
)

func readUsageRecords(t *testing.T, stateDir string) []observe.UsageRecord {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stateDir, "observe", "*", "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var records []observe.UsageRecord
	for _, path := range matches {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var record observe.UsageRecord
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
			records = append(records, record)
		}
		file.Close()
	}
	return records
}

func waitUsageRecords(t *testing.T, stateDir string, want int) []observe.UsageRecord {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		records := readUsageRecords(t, stateDir)
		if len(records) >= want {
			return records[:want]
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records = %d after wait, want %d", len(records), want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestMCPStdioWritesUsageRecords(t *testing.T) {
	dbPath, stateDir := seedMcpDatabase(t)
	session := startMcpProcess(t, dbPath, stateDir)

	result := callSql(t, session, `SELECT id FROM events`)
	if result.IsError {
		t.Fatalf("query failed: %+v", result)
	}
	if result := callSql(t, session, `DELETE FROM events`); !result.IsError {
		t.Fatal("adversarial call accepted")
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close client session: %v", err)
	}

	records := waitUsageRecords(t, stateDir, 2)
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	first := records[0]
	if first.Tool != observe.UsageToolSQL || first.Outcome != observe.UsageOutcomeOK {
		t.Fatalf("first record tool/outcome = %s/%s", first.Tool, first.Outcome)
	}
	if first.Path != observe.UsagePathLocal || first.DeviceID != deviceID {
		t.Fatalf("first record path/device = %s/%s, want local and %s", first.Path, first.DeviceID, deviceID)
	}
	if first.Client != observe.UsageClientOther {
		t.Fatalf("first record client = %s, want other for the stdio test client", first.Client)
	}
	if len(first.Reads) != 1 || first.Reads[0] != "events.id" {
		t.Fatalf("first record reads = %v, want events.id", first.Reads)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("first record invalid: %v", err)
	}

	second := records[1]
	if second.Outcome != observe.UsageOutcomeRejected {
		t.Fatalf("second record outcome = %s, want rejected for the denied write", second.Outcome)
	}
	if second.SQLiteCode != 23 {
		t.Fatalf("second record sqlite_code = %d, want SQLITE_AUTH (23)", second.SQLiteCode)
	}
	if err := second.Validate(); err != nil {
		t.Fatalf("second record invalid: %v", err)
	}
}

func TestMCPStdioServesWithoutUsageRecords(t *testing.T) {
	dbPath, stateDir := seedMcpDatabase(t)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "observe"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := startMcpProcess(t, dbPath, stateDir)
	result := callSql(t, session, `SELECT id FROM events`)
	if result.IsError {
		t.Fatalf("query failed with the usage writer disabled: %+v", result)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close client session: %v", err)
	}
	if records := readUsageRecords(t, stateDir); len(records) != 0 {
		t.Fatalf("usage records = %d, want none with the writer disabled", len(records))
	}
}
