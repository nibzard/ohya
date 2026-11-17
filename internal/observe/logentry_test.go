package observe

import (
	"strings"
	"testing"
	"time"
)

func TestEveryEventCodeHasCuratedMessage(t *testing.T) {
	codes := []EventCode{
		EventStartup, EventShutdown, EventConfigSummary,
		EventTransportOnline, EventTransportOffline,
		EventAuthSuccess, EventAuthFailure, EventDeviceRevoked,
		EventSourceStopped, EventSourceMissing, EventSourceReturned, EventSourceRecovered,
		EventCheckpointFailed, EventCommitFailed,
		EventQueryTimeout, EventQueryBusy, EventQueryTruncated,
		EventInstrumentationFailed, EventSummary, EventHeartbeat,
	}
	if len(codes) != len(curatedMessages) {
		t.Fatalf("%d event codes declared but %d curated messages", len(codes), len(curatedMessages))
	}
	for _, code := range codes {
		message, err := code.Curated()
		if err != nil || message == "" {
			t.Errorf("event code %s has no curated message", code)
		}
		if strings.Contains(message, "%") || strings.Contains(message, "'") {
			t.Errorf("curated message for %s must be fixed text, not a template: %s", code, message)
		}
	}
	if _, err := EventCode("something_happened").Curated(); err == nil {
		t.Error("unknown event code must not have a curated message")
	}
}

func TestNewLogEntry(t *testing.T) {
	clock := NewFakeClock(1794840423123)
	entry, err := NewLogEntry(clock, "3f9b2c1d8e4a7f60b5d3c2a19e8f7b6d", "ingest", LevelWarn, EventCheckpointFailed)
	if err != nil {
		t.Fatalf("NewLogEntry: %v", err)
	}
	if entry.FormatVersion != LogFormatVersion {
		t.Errorf("format version = %d", entry.FormatVersion)
	}
	if entry.TimeMS != 1794840423123 {
		t.Errorf("timestamp = %d, want the wall clock value", entry.TimeMS)
	}
	if err := entry.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestNewLogEntryRejectsUnknownCode(t *testing.T) {
	clock := NewFakeClock(1794840423123)
	if _, err := NewLogEntry(clock, "3f9b2c1d8e4a7f60b5d3c2a19e8f7b6d", "ingest", LevelInfo, EventCode("freeform")); err == nil {
		t.Fatal("unknown event code must be refused at construction")
	}
	if _, err := NewLogEntry(nil, "i", "ingest", LevelInfo, EventStartup); err == nil {
		t.Fatal("missing clock must be refused")
	}
}

func TestLogEntryValidateRejectsDegradedShapes(t *testing.T) {
	base := func() LogEntry {
		entry, err := NewLogEntry(NewFakeClock(1794840423123), "3f9b2c1d8e4a7f60b5d3c2a19e8f7b6d", "ingest", LevelWarn, EventQueryTimeout)
		if err != nil {
			t.Fatalf("NewLogEntry: %v", err)
		}
		return entry
	}
	cases := []struct {
		name   string
		mutate func(*LogEntry)
	}{
		{"wrong format version", func(e *LogEntry) { e.FormatVersion = 2 }},
		{"missing timestamp", func(e *LogEntry) { e.TimeMS = 0 }},
		{"missing instance", func(e *LogEntry) { e.InstanceID = "" }},
		{"missing component", func(e *LogEntry) { e.Component = "" }},
		{"oversized component", func(e *LogEntry) { e.Component = strings.Repeat("x", MaxCorrelationID+1) }},
		{"unknown level", func(e *LogEntry) { e.Level = Level("trace") }},
		{"unknown event code", func(e *LogEntry) { e.EventCode = "raw_error_text" }},
		{"oversized correlation id", func(e *LogEntry) { e.CorrelationID = strings.Repeat("c", MaxCorrelationID+1) }},
		{"negative duration", func(e *LogEntry) { d := -1.0; e.DurationMS = &d }},
	}
	for _, tc := range cases {
		entry := base()
		tc.mutate(&entry)
		if err := entry.Validate(); err == nil {
			t.Errorf("%s must fail validation", tc.name)
		}
	}
}

func TestLogEntryCarriesNoErrorText(t *testing.T) {
	entry := LogEntry{
		FormatVersion: LogFormatVersion,
		TimeMS:        1794840423123,
		InstanceID:    "3f9b2c1d8e4a7f60b5d3c2a19e8f7b6d",
		Component:     "ingest",
		Level:         LevelError,
		EventCode:     string(EventCommitFailed),
		Counts:        map[string]int64{"attempts": 1},
	}
	if err := entry.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	_, err := EventCommitFailed.Curated()
	if err != nil {
		t.Fatalf("curated: %v", err)
	}
}

func TestSystemClockIsUsable(t *testing.T) {
	clock := NewSystemClock()
	if clock.WallMS() < 1_700_000_000_000 {
		t.Errorf("wall milliseconds %d looks far in the past", clock.WallMS())
	}
	if clock.Monotonic() < 0 {
		t.Error("monotonic time must not be negative")
	}
	time.Sleep(2 * time.Millisecond)
	if clock.Monotonic() <= 0 {
		t.Error("monotonic time must advance")
	}
}

func TestFakeClockAdvancesIndependently(t *testing.T) {
	clock := NewFakeClock(1794837600000)
	if clock.Monotonic() != 0 || clock.WallMS() != 1794837600000 {
		t.Fatalf("fake clock must start at zero monotonic and the given wall time")
	}
	clock.Advance(1500 * time.Millisecond)
	if clock.Monotonic() != 1500*time.Millisecond {
		t.Errorf("monotonic = %v", clock.Monotonic())
	}
	if clock.WallMS() != 1794837601500 {
		t.Errorf("wall = %d", clock.WallMS())
	}
	clock.SetWallMS(1794830000000)
	if clock.Monotonic() != 1500*time.Millisecond {
		t.Errorf("a wall change must not move monotonic time: %v", clock.Monotonic())
	}
}
