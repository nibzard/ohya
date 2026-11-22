package observe

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type FieldClass string

const (
	ClassExportSafe     FieldClass = "export-safe"
	ClassLocalOnly      FieldClass = "local-only"
	ClassNeverCollected FieldClass = "never-collected"
)

const (
	UsageToolSQL      = "sql"
	UsageToolAnnotate = "annotate"

	UsagePathLocal  = "local"
	UsagePathRemote = "remote"

	UsageClientClaudeCode  = "claude-code"
	UsageClientCodex       = "codex"
	UsageClientPi          = "pi"
	UsageClientAntigravity = "antigravity"
	UsageClientOther       = "other"
	UsageClientUnknown     = "unknown"

	UsageOutcomeOK             = "ok"
	UsageOutcomeTimeout        = "timeout"
	UsageOutcomeBusy           = "busy"
	UsageOutcomeSQLError       = "sql_error"
	UsageOutcomeRejected       = "rejected"
	UsageOutcomeTruncatedRows  = "truncated_rows"
	UsageOutcomeTruncatedBytes = "truncated_bytes"
	UsageOutcomeCancelled      = "cancelled"
)

type UsagePhases struct {
	AdmissionWaitMS float64 `json:"admission_wait_ms"`
	ReaderWaitMS    float64 `json:"reader_wait_ms"`
	FirstRowMS      float64 `json:"first_row_ms"`
	IterationMS     float64 `json:"iteration_ms"`
	TotalMS         float64 `json:"total_ms"`
}

type AnnotateOps struct {
	Upsert int64 `json:"upsert"`
	Delete int64 `json:"delete"`
}

type UsageRecord struct {
	SchemaVersion int          `json:"schema_version"`
	TimeBucket    string       `json:"time_bucket"`
	TimeMS        int64        `json:"time_ms"`
	InstanceID    string       `json:"instance_id"`
	Tool          string       `json:"tool"`
	Client        string       `json:"client"`
	ClientVersion string       `json:"client_version"`
	Path          string       `json:"path"`
	DeviceID      string       `json:"device_id"`
	SessionHint   string       `json:"session_hint"`
	Outcome       string       `json:"outcome"`
	SQLiteCode    int          `json:"sqlite_code"`
	PhaseMS       UsagePhases  `json:"phase_ms"`
	Rows          int64        `json:"rows"`
	ResultBytes   int64        `json:"result_bytes"`
	Reads         []string     `json:"reads"`
	Functions     []string     `json:"functions"`
	FTS           bool         `json:"fts"`
	AnnotateOps   *AnnotateOps `json:"annotate_ops,omitempty"`
}

var UsageFieldClasses = map[string]FieldClass{
	"schema_version": ClassExportSafe,
	"time_bucket":    ClassExportSafe,
	"time_ms":        ClassLocalOnly,
	"instance_id":    ClassLocalOnly,
	"tool":           ClassExportSafe,
	"client":         ClassExportSafe,
	"client_version": ClassExportSafe,
	"path":           ClassExportSafe,
	"device_id":      ClassLocalOnly,
	"session_hint":   ClassLocalOnly,
	"outcome":        ClassExportSafe,
	"sqlite_code":    ClassExportSafe,
	"phase_ms":       ClassExportSafe,
	"rows":           ClassExportSafe,
	"result_bytes":   ClassExportSafe,
	"reads":          ClassExportSafe,
	"functions":      ClassExportSafe,
	"fts":            ClassExportSafe,
	"annotate_ops":   ClassExportSafe,
}

var NeverCollectedFields = []string{
	"sql",
	"sql_text",
	"query",
	"query_text",
	"statement",
	"literal",
	"literals",
	"params",
	"parameters",
	"file_path",
	"source_path",
	"transcript",
	"transcript_content",
	"content",
	"text",
	"raw",
	"raw_json",
	"annotation_value",
	"value",
	"error",
	"error_message",
	"err",
	"message",
	"credential",
	"credentials",
	"secret",
	"secrets",
	"token",
	"password",
	"env",
	"environment",
}

var usageTools = stringSet(UsageToolSQL, UsageToolAnnotate)
var usagePaths = stringSet(UsagePathLocal, UsagePathRemote)
var usageClients = stringSet(UsageClientClaudeCode, UsageClientCodex, UsageClientPi, UsageClientAntigravity, UsageClientOther, UsageClientUnknown)
var usageOutcomes = stringSet(
	UsageOutcomeOK, UsageOutcomeTimeout, UsageOutcomeBusy, UsageOutcomeSQLError,
	UsageOutcomeRejected, UsageOutcomeTruncatedRows, UsageOutcomeTruncatedBytes, UsageOutcomeCancelled,
)

func stringSet(values ...string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

func MapClient(name string) string {
	if name == "" {
		return UsageClientUnknown
	}
	if usageClients[name] {
		return name
	}
	return UsageClientOther
}

func BucketVersion(version string) string {
	version = strings.TrimPrefix(version, "v")
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return UsageClientUnknown
	}
	for _, part := range parts[:2] {
		if _, err := strconv.Atoi(part); err != nil {
			return UsageClientUnknown
		}
	}
	return parts[0] + "." + parts[1]
}

func HourBucket(ms int64) string {
	return time.UnixMilli(ms).UTC().Truncate(time.Hour).Format("2006-01-02T15:00Z")
}

var ReadCatalogue = []string{
	"sessions.agent",
	"sessions.branch",
	"sessions.cwd",
	"sessions.device_id",
	"sessions.event_count",
	"sessions.first_prompt",
	"sessions.id",
	"sessions.last_event_at_ms",
	"sessions.metadata",
	"sessions.model",
	"sessions.native_id",
	"sessions.repo",
	"sessions.started_at_ms",
	"session_token_usage.cache_creation_input_tokens",
	"session_token_usage.cache_read_input_tokens",
	"session_token_usage.input_tokens",
	"session_token_usage.message_id",
	"session_token_usage.output_tokens",
	"session_token_usage.session_id",
	"sources.agent",
	"sources.cursor_bytes",
	"sources.device_id",
	"sources.file_identity",
	"sources.id",
	"sources.path",
	"sources.updated_at_ms",
	"events.duplicate_of",
	"events.id",
	"events.kind",
	"events.metadata",
	"events.ordinal",
	"events.parser_version",
	"events.raw_json",
	"events.role",
	"events.session_id",
	"events.source_id",
	"events.source_offset",
	"events.text",
	"events.time_ms",
	"events.tool_name",
	"events_settled.duplicate_of",
	"events_settled.id",
	"events_settled.kind",
	"events_settled.metadata",
	"events_settled.ordinal",
	"events_settled.raw_json",
	"events_settled.role",
	"events_settled.session_id",
	"events_settled.source_id",
	"events_settled.source_offset",
	"events_settled.text",
	"events_settled.time_ms",
	"events_settled.tool_name",
	"events_fts.text",
	"event_annotations.created_at_ms",
	"event_annotations.event_id",
	"event_annotations.key",
	"event_annotations.namespace",
	"event_annotations.updated_at_ms",
	"event_annotations.value",
	"event_annotations.writer_id",
	"workspaces.device_id",
	"workspaces.observed_at_ms",
	"workspaces.path",
	"workspaces.repo",
}

var FunctionCatalogue = []string{
	"abs",
	"avg",
	"bm25",
	"coalesce",
	"count",
	"group_concat",
	"hex",
	"highlight",
	"ifnull",
	"instr",
	"json_extract",
	"json_valid",
	"length",
	"lower",
	"ltrim",
	"match",
	"max",
	"min",
	"nullif",
	"replace",
	"round",
	"rtrim",
	"snippet",
	"substr",
	"sum",
	"total",
	"trim",
	"typeof",
	"upper",
}

var readSet = stringSet(ReadCatalogue...)
var functionSet = stringSet(FunctionCatalogue...)

func NormalizeReads(reads []string) []string {
	return normalizeCatalogued(readSet, reads)
}

func NormalizeFunctions(functions []string) []string {
	return normalizeCatalogued(functionSet, functions)
}

func normalizeCatalogued(catalogue map[string]bool, values []string) []string {
	seen := make(map[string]bool, len(values))
	other := false
	for _, value := range values {
		if catalogue[value] {
			seen[value] = true
		} else {
			other = true
		}
	}
	if other {
		seen["other"] = true
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (u UsageRecord) Validate() error {
	if u.SchemaVersion != UsageFormatVersion {
		return fmt.Errorf("observe: usage record schema version %d is unsupported, want %d", u.SchemaVersion, UsageFormatVersion)
	}
	if !usageTools[u.Tool] {
		return fmt.Errorf("observe: usage record tool %q is not sql or annotate", u.Tool)
	}
	if !usagePaths[u.Path] {
		return fmt.Errorf("observe: usage record path %q is not local or remote", u.Path)
	}
	if !usageClients[u.Client] {
		return fmt.Errorf("observe: usage record client %q is outside the fixed client set", u.Client)
	}
	if !usageOutcomes[u.Outcome] {
		return fmt.Errorf("observe: usage record outcome %q is outside the fixed outcome set", u.Outcome)
	}
	if u.TimeMS <= 0 {
		return errors.New("observe: usage record needs a timestamp")
	}
	if u.TimeBucket != HourBucket(u.TimeMS) {
		return fmt.Errorf("observe: usage record time bucket %q does not match its timestamp", u.TimeBucket)
	}
	if u.Rows < 0 || u.ResultBytes < 0 {
		return errors.New("observe: usage record rows and bytes must not be negative")
	}
	if u.PhaseMS.TotalMS < 0 || u.PhaseMS.AdmissionWaitMS < 0 || u.PhaseMS.ReaderWaitMS < 0 || u.PhaseMS.FirstRowMS < 0 || u.PhaseMS.IterationMS < 0 {
		return errors.New("observe: usage record phase durations must not be negative")
	}
	if u.Tool == UsageToolAnnotate && u.AnnotateOps == nil {
		return errors.New("observe: annotate usage record needs annotate_ops")
	}
	if u.Tool == UsageToolSQL && u.AnnotateOps != nil {
		return errors.New("observe: sql usage record must not carry annotate_ops")
	}
	if u.AnnotateOps != nil && (u.AnnotateOps.Upsert < 0 || u.AnnotateOps.Delete < 0) {
		return errors.New("observe: annotate operation counts must not be negative")
	}
	for _, value := range u.Reads {
		if value != "other" && !readSet[value] {
			return fmt.Errorf("observe: usage record read %q is outside the catalogue", value)
		}
	}
	for _, value := range u.Functions {
		if value != "other" && !functionSet[value] {
			return fmt.Errorf("observe: usage record function %q is outside the catalogue", value)
		}
	}
	return nil
}
