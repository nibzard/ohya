package mcpserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

type UsageSink interface {
	Record(r observe.UsageRecord) bool
}

type Origin struct {
	Client        string
	ClientVersion string
	Path          string
	DeviceID      string
	SessionHint   string
}

type originContextKey struct{}

func WithOrigin(ctx context.Context, o Origin) context.Context {
	return context.WithValue(ctx, originContextKey{}, o)
}

func OriginFromContext(ctx context.Context) Origin {
	origin, _ := ctx.Value(originContextKey{}).(Origin)
	return origin
}

type queryTrace struct {
	mu          sync.Mutex
	closed      bool
	reads       map[string]bool
	tables      map[string]bool
	functions   map[string]bool
	fts         bool
	phases      observe.UsagePhases
	rows        int64
	resultBytes int64
}

func newQueryTrace() *queryTrace {
	return &queryTrace{
		reads:     map[string]bool{},
		tables:    map[string]bool{},
		functions: map[string]bool{},
	}
}

func (t *queryTrace) collect(op int, arg1, arg2 string) {
	switch op {
	case sqlite3.SQLITE_READ:
		table := strings.ToLower(arg1)
		t.tables[table] = true
		if table == "events_fts" {
			t.fts = true
		}
		if column := strings.ToLower(arg2); column != "" {
			t.reads[table+"."+column] = true
		}
	case sqlite3.SQLITE_FUNCTION:
		if name := strings.ToLower(arg2); name != "" {
			t.functions[name] = true
		}
	}
}

func (t *queryTrace) finish() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

func (t *queryTrace) authorizer(next func(int, string, string, string) int) func(int, string, string, string) int {
	return func(op int, arg1, arg2, arg3 string) int {
		t.mu.Lock()
		if !t.closed {
			t.collect(op, arg1, arg2)
		}
		t.mu.Unlock()
		return next(op, arg1, arg2, arg3)
	}
}

func elapsedMS(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

func (t *queryTrace) usedFunction(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.functions[name]
}

func (t *queryTrace) readTable(table string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tables[table]
}

func (t *queryTrace) usageRecord(engine *Engine, origin Origin, result Result, err error, annotateOps *observe.AnnotateOps) observe.UsageRecord {
	t.mu.Lock()
	reads := make([]string, 0, len(t.reads))
	for read := range t.reads {
		reads = append(reads, read)
	}
	functions := make([]string, 0, len(t.functions))
	for function := range t.functions {
		functions = append(functions, function)
	}
	phases, rows, resultBytes, fts := t.phases, t.rows, t.resultBytes, t.fts
	t.mu.Unlock()

	path := origin.Path
	if path == "" {
		path = observe.UsagePathLocal
	}
	deviceID := origin.DeviceID
	if deviceID == "" && path == observe.UsagePathLocal {
		deviceID = engine.options.WriterID
	}
	tool := observe.UsageToolSQL
	if annotateOps != nil {
		tool = observe.UsageToolAnnotate
		rows, resultBytes = 0, 0
	}
	timeMS := time.Now().UnixMilli()
	return observe.UsageRecord{
		SchemaVersion: observe.UsageFormatVersion,
		TimeMS:        timeMS,
		TimeBucket:    observe.HourBucket(timeMS),
		Tool:          tool,
		Client:        observe.MapClient(origin.Client),
		ClientVersion: observe.BucketVersion(origin.ClientVersion),
		Path:          path,
		DeviceID:      deviceID,
		SessionHint:   origin.SessionHint,
		Outcome:       usageOutcome(err, result),
		SQLiteCode:    usageSQLiteCode(err),
		PhaseMS:       phases,
		Rows:          rows,
		ResultBytes:   resultBytes,
		Reads:         observe.NormalizeReads(reads),
		Functions:     observe.NormalizeFunctions(functions),
		FTS:           fts,
		AnnotateOps:   annotateOps,
	}
}

func usageOutcome(err error, result Result) string {
	switch {
	case err == nil:
		switch {
		case result.Truncated && result.TruncationReason == TruncationReasonBytes:
			return observe.UsageOutcomeTruncatedBytes
		case result.Truncated:
			return observe.UsageOutcomeTruncatedRows
		default:
			return observe.UsageOutcomeOK
		}
	case errors.Is(err, ErrBusy):
		return observe.UsageOutcomeBusy
	case errors.Is(err, context.Canceled):
		return observe.UsageOutcomeCancelled
	case errors.Is(err, ErrDeadline), errors.Is(err, context.DeadlineExceeded):
		return observe.UsageOutcomeTimeout
	case errors.Is(err, ErrDenied),
		errors.Is(err, ErrMultipleStatements),
		errors.Is(err, ErrEmptyQuery),
		errors.Is(err, ErrQueryTooLarge),
		errors.Is(err, ErrAnnotateRejected),
		errors.Is(err, store.ErrAnnotationRejected):
		return observe.UsageOutcomeRejected
	default:
		return observe.UsageOutcomeSQLError
	}
}

func usageSQLiteCode(err error) int {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return int(sqliteErr.Code) & 0xff
	}
	return 0
}
