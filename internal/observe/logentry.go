package observe

import (
	"errors"
	"fmt"
)

type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

type EventCode string

const (
	EventStartup               EventCode = "startup"
	EventShutdown              EventCode = "shutdown"
	EventConfigSummary         EventCode = "config_summary"
	EventTransportOnline       EventCode = "transport_online"
	EventTransportOffline      EventCode = "transport_offline"
	EventAuthSuccess           EventCode = "auth_success"
	EventAuthFailure           EventCode = "auth_failure"
	EventDeviceRevoked         EventCode = "device_revoked"
	EventSourceStopped         EventCode = "source_stopped"
	EventSourceMissing         EventCode = "source_missing"
	EventSourceReturned        EventCode = "source_returned"
	EventSourceRecovered       EventCode = "source_recovered"
	EventCheckpointFailed      EventCode = "checkpoint_failed"
	EventCommitFailed          EventCode = "commit_failed"
	EventQueryTimeout          EventCode = "query_timeout"
	EventQueryBusy             EventCode = "query_busy"
	EventQueryTruncated        EventCode = "query_truncated"
	EventInstrumentationFailed EventCode = "instrumentation_failed"
	EventSummary               EventCode = "summary"
	EventHeartbeat             EventCode = "heartbeat"
)

var curatedMessages = map[EventCode]string{
	EventStartup:               "process started",
	EventShutdown:              "process stopped",
	EventConfigSummary:         "configuration and capability summary",
	EventTransportOnline:       "transport connection established",
	EventTransportOffline:      "transport connection lost",
	EventAuthSuccess:           "peer authenticated",
	EventAuthFailure:           "peer authentication refused",
	EventDeviceRevoked:         "device revocation applied",
	EventSourceStopped:         "source stopped at an offending record",
	EventSourceMissing:         "source path no longer observable",
	EventSourceReturned:        "source path observable again",
	EventSourceRecovered:       "source resumed after repair",
	EventCheckpointFailed:      "checkpoint persistence failed",
	EventCommitFailed:          "store transaction failed",
	EventQueryTimeout:          "query exceeded its execution deadline",
	EventQueryBusy:             "query refused at the admission limit",
	EventQueryTruncated:        "query output truncated at a row or byte limit",
	EventInstrumentationFailed: "telemetry write failed",
	EventSummary:               "periodic activity summary",
	EventHeartbeat:             "idle heartbeat",
}

func (c EventCode) Curated() (string, error) {
	message, ok := curatedMessages[c]
	if !ok {
		return "", fmt.Errorf("observe: unknown event code %q", string(c))
	}
	return message, nil
}

const MaxCorrelationID = 64

type LogEntry struct {
	FormatVersion int              `json:"format_version"`
	TimeMS        int64            `json:"time_ms"`
	InstanceID    string           `json:"instance_id"`
	Component     string           `json:"component"`
	Level         Level            `json:"level"`
	EventCode     string           `json:"event_code"`
	Outcome       string           `json:"outcome,omitempty"`
	DurationMS    *float64         `json:"duration_ms,omitempty"`
	Counts        map[string]int64 `json:"counts,omitempty"`
	CorrelationID string           `json:"correlation_id,omitempty"`
}

func NewLogEntry(clock Clock, instanceID, component string, level Level, code EventCode) (LogEntry, error) {
	if clock == nil {
		return LogEntry{}, errors.New("observe: log entry needs a clock")
	}
	if _, err := code.Curated(); err != nil {
		return LogEntry{}, err
	}
	entry := LogEntry{
		FormatVersion: LogFormatVersion,
		TimeMS:        clock.WallMS(),
		InstanceID:    instanceID,
		Component:     component,
		Level:         level,
		EventCode:     string(code),
	}
	if err := entry.Validate(); err != nil {
		return LogEntry{}, err
	}
	return entry, nil
}

func (e LogEntry) Validate() error {
	if e.FormatVersion != LogFormatVersion {
		return fmt.Errorf("observe: log format version %d is unsupported, want %d", e.FormatVersion, LogFormatVersion)
	}
	if e.TimeMS <= 0 {
		return errors.New("observe: log entry needs a timestamp")
	}
	if e.InstanceID == "" {
		return errors.New("observe: log entry needs an instance id")
	}
	if e.Component == "" || len(e.Component) > MaxCorrelationID {
		return errors.New("observe: log entry needs a component of at most 64 characters")
	}
	switch e.Level {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
	default:
		return fmt.Errorf("observe: unknown log level %q", string(e.Level))
	}
	if _, err := EventCode(e.EventCode).Curated(); err != nil {
		return err
	}
	if len(e.CorrelationID) > MaxCorrelationID {
		return errors.New("observe: correlation id exceeds 64 characters")
	}
	if e.DurationMS != nil && *e.DurationMS < 0 {
		return errors.New("observe: log entry duration must not be negative")
	}
	return nil
}
