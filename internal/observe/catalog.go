package observe

import "strings"

type MetricKind uint8

const (
	Counter MetricKind = iota
	Gauge
	Duration
)

type LabelValues struct {
	Values   []string
	Fallback string
}

type Metric struct {
	Name   string
	Kind   MetricKind
	Labels []string
	Help   string
}

var labelSets = map[string]LabelValues{
	"agent":           {Values: []string{"claude-code", "codex", "pi", "antigravity", "unknown"}, Fallback: "unknown"},
	"state":           {Values: []string{"active", "stopped", "blocked", "missing", "unknown"}, Fallback: "unknown"},
	"watch_event":     {Values: []string{"replacement", "truncation", "missing", "returned", "other"}, Fallback: "other"},
	"parse_result":    {Values: []string{"attempted", "normalized", "stopped", "other"}, Fallback: "other"},
	"stop_code":       {Values: []string{"oversize", "invalid", "other"}, Fallback: "other"},
	"batch_result":    {Values: []string{"attempted", "committed", "rolled_back", "successful", "rejected", "other"}, Fallback: "other"},
	"event_result":    {Values: []string{"inserted", "duplicate", "other"}, Fallback: "other"},
	"conn_result":     {Values: []string{"active", "admitted", "rejected", "other"}, Fallback: "other"},
	"auth_outcome":    {Values: []string{"ok", "rejected", "revoked", "other"}, Fallback: "other"},
	"failure_kind":    {Values: []string{"connect", "auth", "retry", "checkpoint", "commit", "write", "read", "other"}, Fallback: "other"},
	"sqlite_outcome":  {Values: []string{"ok", "busy", "locked", "error", "other"}, Fallback: "other"},
	"file":            {Values: []string{"db", "wal", "shm", "other"}, Fallback: "other"},
	"query_class":     {Values: []string{"sql", "annotate", "other"}, Fallback: "other"},
	"query_outcome":   {Values: []string{"ok", "timeout", "busy", "sql_error", "rejected", "truncated_rows", "truncated_bytes", "cancelled", "other"}, Fallback: "other"},
	"truncation_kind": {Values: []string{"row_limit", "byte_limit", "other"}, Fallback: "other"},
	"op":              {Values: []string{"upsert", "delete", "other"}, Fallback: "other"},
	"op_result":       {Values: []string{"applied", "unchanged", "invalid", "other"}, Fallback: "other"},
	"write_target":    {Values: []string{"snapshot", "history", "log", "usage", "other"}, Fallback: "other"},
}

var Catalog = []Metric{
	{Name: "discovery_scans", Kind: Counter, Help: "completed source discovery scans"},
	{Name: "discovery_candidates", Kind: Counter, Help: "candidate files found by discovery scans; not a valid-source count"},
	{Name: "discovery_errors", Kind: Counter, Labels: []string{"failure_kind"}, Help: "discovery stat and read failures"},
	{Name: "discovery_duration", Kind: Duration, Help: "discovery scan wall time"},
	{Name: "discovery_last_scan_ms", Kind: Gauge, Help: "monotonic milliseconds of the last completed scan"},
	{Name: "discovery_excluded", Kind: Gauge, Labels: []string{"agent"}, Help: "candidate files dropped by --exclude globs in the last discovery scan; published only while exclusions are configured"},
	{Name: "watch_bytes_observed", Kind: Counter, Help: "source bytes observed by the watcher"},
	{Name: "watch_read_bytes", Kind: Counter, Help: "bytes read from watched files for verification, checkpoints, and pending records"},
	{Name: "watch_checkpoint_saves", Kind: Counter, Help: "checkpoint state files durably saved"},
	{Name: "watch_checkpoint_bytes_written", Kind: Counter, Help: "bytes written by checkpoint state saves"},
	{Name: "watch_pending_complete_bytes", Kind: Gauge, Help: "bytes of complete records past the acknowledged cursor"},
	{Name: "watch_partial_tail_bytes", Kind: Gauge, Help: "bytes of the incomplete record tail past the cursor"},
	{Name: "watch_source_events", Kind: Counter, Labels: []string{"watch_event"}, Help: "watched-source replacement, truncation, missing, and returned observations"},
	{Name: "watch_sources", Kind: Gauge, Labels: []string{"state"}, Help: "watched sources by state"},
	{Name: "parse_bytes", Kind: Counter, Help: "bytes handed to parsers"},
	{Name: "parse_records", Kind: Counter, Labels: []string{"parse_result"}, Help: "records by parse result"},
	{Name: "parse_stops", Kind: Counter, Labels: []string{"stop_code"}, Help: "parser stops by reason code"},
	{Name: "parse_duration", Kind: Duration, Labels: []string{"agent"}, Help: "parser wall time by agent"},
	{Name: "ingest_batches", Kind: Counter, Labels: []string{"batch_result"}, Help: "local ingestion batches by result"},
	{Name: "ingest_events", Kind: Counter, Labels: []string{"event_result"}, Help: "events by ingestion result; inserted counts only durable commits"},
	{Name: "ingest_bytes", Kind: Counter, Help: "accepted record bytes committed locally"},
	{Name: "ingest_parse_duration", Kind: Duration, Help: "parse stage wall time inside a local pass"},
	{Name: "ingest_checkpoint_duration", Kind: Duration, Help: "checkpoint persistence wall time inside a local pass"},
	{Name: "ingest_passes", Kind: Counter, Help: "local ingest passes completed"},
	{Name: "ingest_pass_duration", Kind: Duration, Help: "local ingest pass wall time, discovery through checkpoint flush"},
	{Name: "ingest_failures", Kind: Counter, Labels: []string{"failure_kind"}, Help: "local ingest registration, read, and checkpoint failures by kind"},
	{Name: "upload_batches", Kind: Counter, Labels: []string{"batch_result"}, Help: "upload batches by result"},
	{Name: "upload_payload_bytes", Kind: Counter, Help: "attempted upload payload bytes; resubmissions counted again"},
	{Name: "upload_wire_bytes", Kind: Counter, Help: "frame bytes written for upload batches; below upload_payload_bytes while compression is negotiated"},
	{Name: "upload_ack_bytes", Kind: Counter, Help: "acknowledged byte advancement within one source incarnation"},
	{Name: "upload_checkpoint_advances", Kind: Counter, Help: "locally persisted checkpoint advancements after acknowledgment"},
	{Name: "upload_events_inserted", Kind: Counter, Help: "events the hub reported as newly inserted"},
	{Name: "upload_round_trip_duration", Kind: Duration, Help: "upload exchange round-trip wall time, hub work included"},
	{Name: "upload_failures", Kind: Counter, Labels: []string{"failure_kind"}, Help: "upload reconnect, authentication, and retry failures"},
	{Name: "upload_backoff_duration", Kind: Duration, Help: "retry backoff wall time"},
	{Name: "upload_backlog_bytes", Kind: Gauge, Labels: []string{"agent"}, Help: "bytes left to upload across the watcher's sources while a backlog exists"},
	{Name: "upload_backlog_sent_bytes", Kind: Gauge, Labels: []string{"agent"}, Help: "bytes the hub acknowledged since the current backlog drain began; set only while a backlog exists"},
	{Name: "upload_backlog_rate_bytes", Kind: Gauge, Labels: []string{"agent"}, Help: "acknowledged bytes per second over the recent drain window; set only once a rate is measurable"},
	{Name: "upload_backlog_eta_ms", Kind: Gauge, Labels: []string{"agent"}, Help: "estimated milliseconds to drain the backlog at the current rate; set only once a rate is measurable"},
	{Name: "hub_connections", Kind: Counter, Labels: []string{"conn_result"}, Help: "hub connection attempts by result"},
	{Name: "hub_active_connections", Kind: Gauge, Help: "currently open hub connections"},
	{Name: "hub_authorization", Kind: Counter, Labels: []string{"auth_outcome"}, Help: "authorization checks by outcome"},
	{Name: "hub_gate_wait_duration", Kind: Duration, Help: "per-source gate wait before batch handling"},
	{Name: "hub_normalize_duration", Kind: Duration, Help: "hub-side record normalization wall time"},
	{Name: "hub_store_duration", Kind: Duration, Help: "hub store transaction wall time"},
	{Name: "hub_ack_write_duration", Kind: Duration, Help: "acknowledgment write wall time"},
	{Name: "hub_failures", Kind: Counter, Labels: []string{"failure_kind"}, Help: "hub-side failures by kind"},
	{Name: "sqlite_open_connections", Kind: Gauge, Help: "pool open connections from DBStats"},
	{Name: "sqlite_in_use_connections", Kind: Gauge, Help: "pool in-use connections from DBStats"},
	{Name: "sqlite_idle_connections", Kind: Gauge, Help: "pool idle connections from DBStats"},
	{Name: "sqlite_wait_count", Kind: Counter, Help: "connection wait events from DBStats deltas"},
	{Name: "sqlite_wait_duration", Kind: Duration, Help: "connection acquisition wall time"},
	{Name: "sqlite_transaction_duration", Kind: Duration, Help: "store transaction wall time, commit included"},
	{Name: "sqlite_outcomes", Kind: Counter, Labels: []string{"sqlite_outcome"}, Help: "store operations by SQLite outcome"},
	{Name: "sqlite_file_bytes", Kind: Gauge, Labels: []string{"file"}, Help: "database, WAL, and SHM file sizes in bytes"},
	{Name: "sql_requests", Kind: Counter, Labels: []string{"query_class", "query_outcome"}, Help: "sql and annotate requests by outcome"},
	{Name: "sql_active", Kind: Gauge, Help: "queries currently executing"},
	{Name: "sql_busy_rejections", Kind: Counter, Help: "requests refused at the admission limit"},
	{Name: "sql_reader_wait_duration", Kind: Duration, Help: "read-only reader acquisition wall time"},
	{Name: "sql_first_row_duration", Kind: Duration, Help: "execute-to-first-row wall time"},
	{Name: "sql_iteration_duration", Kind: Duration, Help: "row iteration and materialization wall time"},
	{Name: "sql_total_duration", Kind: Duration, Help: "total engine wall time per request"},
	{Name: "sql_rows", Kind: Counter, Help: "rows returned by sql requests"},
	{Name: "sql_result_bytes", Kind: Counter, Help: "encoded row JSON bytes returned by sql requests"},
	{Name: "sql_truncations", Kind: Counter, Labels: []string{"truncation_kind"}, Help: "output truncations by limit"},
	{Name: "annotate_operations", Kind: Counter, Labels: []string{"op", "op_result"}, Help: "annotate operations by type and result"},
	{Name: "annotate_validation_failures", Kind: Counter, Help: "annotate batches rejected by validation"},
	{Name: "annotate_duration", Kind: Duration, Help: "annotate transaction wall time"},
	{Name: "runtime_sample_duration", Kind: Duration, Help: "resource sampling wall time"},
	{Name: "runtime_sample_errors", Kind: Counter, Help: "resource sampling failures"},
	{Name: "instrumentation_log_dropped", Kind: Counter, Help: "log entries dropped by the bounded queue"},
	{Name: "instrumentation_log_coalesced", Kind: Counter, Help: "repeated log entries coalesced by event code"},
	{Name: "instrumentation_write_errors", Kind: Counter, Labels: []string{"write_target"}, Help: "telemetry write failures by target"},
	{Name: "instrumentation_queue_depth", Kind: Gauge, Help: "pending entries in the bounded log queue"},
	{Name: "instrumentation_last_publish_ms", Kind: Gauge, Help: "monotonic milliseconds of the last successful snapshot publication"},
	{Name: "instrumentation_publish_duration", Kind: Duration, Help: "snapshot publication wall time"},
	{Name: "instrumentation_metrics_dropped", Kind: Counter, Help: "recorder calls dropped for unknown metrics, kinds, or labels"},
}

var catalogIndex = func() map[string]Metric {
	index := make(map[string]Metric, len(Catalog))
	for _, m := range Catalog {
		index[m.Name] = m
	}
	return index
}()

func MetricByName(name string) (Metric, bool) {
	m, ok := catalogIndex[name]
	return m, ok
}

func SeriesKey(name string, labelValues ...string) string {
	metric, ok := catalogIndex[name]
	if !ok || len(metric.Labels) == 0 || len(labelValues) != len(metric.Labels) {
		return name
	}
	rendered := make([]string, len(metric.Labels))
	for i, labelName := range metric.Labels {
		rendered[i] = labelName + "=" + labelValues[i]
	}
	return name + "{" + strings.Join(rendered, ",") + "}"
}

func LabelSet(name string) (LabelValues, bool) {
	set, ok := labelSets[name]
	return set, ok
}

func SeriesBound() int {
	total := 0
	for _, m := range Catalog {
		series := 1
		for _, name := range m.Labels {
			set, ok := labelSets[name]
			if !ok {
				continue
			}
			series *= len(set.Values)
		}
		total += series
	}
	return total
}
