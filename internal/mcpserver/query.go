package mcpserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/secrets"
)

const (
	DefaultMaxRows        = 500
	DefaultMaxOutputBytes = 256 << 10
	DefaultMaxQueryBytes  = 64 << 10
	DefaultDeadline       = 5 * time.Second
	DefaultMaxConcurrency = 4

	DefaultMaxAnnotateRequestBytes = 1 << 20

	minSQLiteLimitLength         = 4 << 20
	sqliteLimitExprDepth         = 64
	sqliteLimitCompoundSelect    = 16
	sqliteLimitLikePatternLength = 256
	sqliteLimitColumn            = 128
)

var (
	ErrBusy               = errors.New("server busy: too many concurrent queries, retry")
	ErrMultipleStatements = errors.New("exactly one SQL statement per call")
	ErrEmptyQuery         = errors.New("empty query")
	ErrQueryTooLarge      = errors.New("query text exceeds the input limit")
	ErrDenied             = errors.New("not authorized: the sql tool runs SELECT and read-only PRAGMA only")
	ErrDeadline           = errors.New("execution deadline exceeded")
)

type EngineOptions struct {
	WriterID                string
	MaxRows                 int
	MaxOutputBytes          int
	MaxQueryBytes           int
	Deadline                time.Duration
	MaxConcurrency          int
	MaxAnnotateRequestBytes int
	DenyTables              []string
	Usage                   UsageSink
	Observer                observe.Observer
	Events                  observe.EventSink
}

func (o EngineOptions) withDefaults() EngineOptions {
	if o.MaxRows <= 0 {
		o.MaxRows = DefaultMaxRows
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if o.MaxQueryBytes <= 0 {
		o.MaxQueryBytes = DefaultMaxQueryBytes
	}
	if o.Deadline <= 0 {
		o.Deadline = DefaultDeadline
	}
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = DefaultMaxConcurrency
	}
	if o.MaxAnnotateRequestBytes <= 0 {
		o.MaxAnnotateRequestBytes = DefaultMaxAnnotateRequestBytes
	}
	return o
}

type Result struct {
	Columns          []string `json:"columns"`
	Rows             [][]any  `json:"rows"`
	RowCount         int      `json:"row_count"`
	Truncated        bool     `json:"truncated"`
	TruncationReason string   `json:"truncation_reason,omitempty"`
	Redactions       int      `json:"redactions,omitempty"`
	InjectionFlags   int      `json:"injection_flags,omitempty"`
}

const (
	TruncationReasonRows  = "row_limit"
	TruncationReasonBytes = "byte_limit"
)

type Engine struct {
	readers    *sql.DB
	writer     *sql.DB
	options    EngineOptions
	observer   observe.Observer
	events     observe.EventSink
	traced     bool
	deniedTbls map[string]bool
	slots      chan struct{}
	mu         sync.Mutex
	active     int64
}

func NewEngine(readers, writer *sql.DB, o EngineOptions) *Engine {
	o = o.withDefaults()
	denied := make(map[string]bool, len(o.DenyTables)+1)
	denied["purged_sources"] = true
	for _, t := range o.DenyTables {
		denied[strings.ToLower(t)] = true
	}
	observer := o.Observer
	if observer == nil {
		observer = observe.Disabled
	}
	return &Engine{
		readers:    readers,
		writer:     writer,
		options:    o,
		observer:   observer,
		events:     o.Events,
		traced:     o.Usage != nil || o.Observer != nil || o.Events != nil,
		deniedTbls: denied,
		slots:      make(chan struct{}, o.MaxConcurrency),
	}
}

func (e *Engine) Options() EngineOptions { return e.options }

func (e *Engine) Query(ctx context.Context, query string) (Result, error) {
	origin := OriginFromContext(ctx)
	trace := newQueryTrace()
	start := time.Now()
	result, err := e.query(ctx, query, trace)
	trace.mu.Lock()
	trace.phases.TotalMS = elapsedMS(time.Since(start))
	trace.mu.Unlock()
	trace.finish()
	if e.traced {
		if e.options.Usage != nil {
			e.options.Usage.Record(trace.usageRecord(e, origin, result, err, nil))
		}
		e.reportQuery(trace, result, err)
	}
	return result, err
}

func (e *Engine) query(ctx context.Context, query string, trace *queryTrace) (Result, error) {
	if len(query) > e.options.MaxQueryBytes {
		return Result{}, fmt.Errorf("%w: %d bytes exceed the %d byte input limit",
			ErrQueryTooLarge, len(query), e.options.MaxQueryBytes)
	}
	if err := singleStatement(query); err != nil {
		return Result{}, err
	}
	waitStart := time.Now()
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		e.observer.Add("sql_busy_rejections", 1)
		return Result{}, ErrBusy
	}
	trace.phases.AdmissionWaitMS = elapsedMS(time.Since(waitStart))
	e.enterQuery()
	defer e.leaveQuery()
	ctx, cancel := context.WithTimeout(ctx, e.options.Deadline)
	defer cancel()

	readerStart := time.Now()
	conn, err := e.readers.Conn(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("acquire reader: %w", err)
	}
	defer conn.Close()
	trace.phases.ReaderWaitMS = elapsedMS(time.Since(readerStart))
	authorize := trace.authorizer(e.authorize)
	if err := conn.Raw(func(c any) error {
		sqliteConn, ok := c.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("reader connection is %T, want *sqlite3.SQLiteConn", c)
		}
		sqliteConn.RegisterAuthorizer(authorize)
		sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_LENGTH, max(e.options.MaxOutputBytes*2, minSQLiteLimitLength))
		sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_EXPR_DEPTH, sqliteLimitExprDepth)
		sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_COMPOUND_SELECT, sqliteLimitCompoundSelect)
		sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_LIKE_PATTERN_LENGTH, sqliteLimitLikePatternLength)
		sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_COLUMN, sqliteLimitColumn)
		return nil
	}); err != nil {
		return Result{}, err
	}

	execStart := time.Now()
	rows, err := conn.QueryContext(ctx, query)
	trace.phases.FirstRowMS = elapsedMS(time.Since(execStart))
	if err != nil {
		return Result{}, queryError(err, e.options.Deadline, trace)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return Result{}, fmt.Errorf("read columns: %w", err)
	}
	result := Result{Columns: columns, Rows: [][]any{}}
	bytes := 0
	iterStart := time.Now()
	for rows.Next() {
		if len(result.Rows) == e.options.MaxRows {
			result.Truncated, result.TruncationReason = true, TruncationReasonRows
			break
		}
		rawValues, err := scanRawRow(rows, len(result.Columns))
		if err != nil {
			return Result{}, err
		}
		if rawRowExceedsLimit(rawValues, e.options.MaxOutputBytes-bytes) {
			result.Truncated, result.TruncationReason = true, TruncationReasonBytes
			break
		}
		row := encodeRowValues(rawValues)
		encoded, err := json.Marshal(row)
		if err != nil {
			return Result{}, fmt.Errorf("encode row: %w", err)
		}
		if bytes+len(encoded) > e.options.MaxOutputBytes {
			result.Truncated, result.TruncationReason = true, TruncationReasonBytes
			break
		}
		bytes += len(encoded)
		result.Rows = append(result.Rows, row)
	}
	trace.phases.IterationMS = elapsedMS(time.Since(iterStart))
	trace.resultBytes = int64(bytes)
	if err := rows.Err(); err != nil {
		return Result{}, queryError(err, e.options.Deadline, trace)
	}
	result.RowCount = len(result.Rows)
	result.Redactions, result.InjectionFlags = screenCells(result.Rows)
	trace.rows = int64(result.RowCount)
	return result, nil
}

func screenCells(rows [][]any) (redactions, injectionFlags int) {
	for _, row := range rows {
		for i, v := range row {
			s, ok := v.(string)
			if !ok {
				continue
			}
			r, n := secrets.Redact(s)
			if n > 0 {
				row[i] = r
				redactions += n
			}
			injectionFlags += secrets.InjectionFlags(r)
		}
	}
	return redactions, injectionFlags
}

func scanRawRow(rows *sql.Rows, width int) ([]any, error) {
	values := make([]any, width)
	targets := make([]any, width)
	for i := range values {
		targets[i] = &values[i]
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, fmt.Errorf("read row: %w", err)
	}
	return values, nil
}

func rawRowExceedsLimit(values []any, remaining int) bool {
	if remaining < 2 {
		return true
	}
	est := 2
	for _, v := range values {
		switch t := v.(type) {
		case []byte:
			est += base64.StdEncoding.EncodedLen(len(t)) + 2
		case string:
			est += len(t) + 2
		}
		if est > remaining {
			return true
		}
	}
	return false
}

func encodeRowValues(values []any) []any {
	for i, v := range values {
		values[i] = jsonValue(v)
	}
	return values
}

func jsonValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return base64.StdEncoding.EncodeToString(t)
	case float64:
		switch {
		case math.IsNaN(t):
			return "NaN"
		case math.IsInf(t, 1):
			return "Infinity"
		case math.IsInf(t, -1):
			return "-Infinity"
		}
	}
	return v
}

const hintLike = "this query used LIKE and read events; LIKE on a large table reads every row: " +
	"filter session_id with = and search text with events_fts MATCH"

const hintRawJSON = "this query read events.raw_json; each raw_json value holds a full original record: " +
	"select the event ids first with events_fts MATCH or session_id =, then read raw_json for those ids only"

const hintCount = "this query used count() and read events; a count reads every matching row: " +
	"for the total read sum(event_count) FROM sessions, and for one session read sessions.event_count"

const hintNarrow = "narrow the query: filter on an indexed column (session_id =, or a time_ms range " +
	"inside one session) and select ids in a covering subquery before you read text or raw_json"

func (t *queryTrace) deadlineHint() string {
	switch {
	case t.usedFunction("like") && t.readTable("events"):
		return hintLike
	case t.readColumn("events", "raw_json"):
		return hintRawJSON
	case t.usedFunction("count") && t.readTable("events") && !t.readTable("events_fts"):
		return hintCount
	default:
		return hintNarrow
	}
}

func queryError(err error, deadline time.Duration, trace *queryTrace) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%s per call): %w; %s", ErrDeadline, deadline, err, trace.deadlineHint())
	}
	var serr sqlite3.Error
	if errors.As(err, &serr) && serr.Code == sqlite3.ErrAuth {
		return fmt.Errorf("%w: %w", ErrDenied, err)
	}
	if msg := err.Error(); strings.Contains(msg, "not authorized") ||
		strings.Contains(msg, "is prohibited") ||
		strings.Contains(msg, "authorization denied") {
		return fmt.Errorf("%w: %w", ErrDenied, err)
	}
	return fmt.Errorf("query failed: %w", err)
}

const sqliteOpRecursive = 33

func (e *Engine) authorize(op int, arg1, arg2, _ string) int {
	switch op {
	case sqlite3.SQLITE_SELECT, sqliteOpRecursive:
		return sqlite3.SQLITE_OK
	case sqlite3.SQLITE_READ:
		if e.deniedTbls[strings.ToLower(arg1)] {
			return sqlite3.SQLITE_DENY
		}
		return sqlite3.SQLITE_OK
	case sqlite3.SQLITE_PRAGMA:
		if allowedPragmas[strings.ToLower(arg1)] && !e.deniedTbls[strings.ToLower(arg2)] {
			return sqlite3.SQLITE_OK
		}
		return sqlite3.SQLITE_DENY
	case sqlite3.SQLITE_FUNCTION:
		if deniedFunctions[strings.ToLower(arg2)] {
			return sqlite3.SQLITE_DENY
		}
		return sqlite3.SQLITE_OK
	default:
		return sqlite3.SQLITE_DENY
	}
}

var allowedPragmas = map[string]bool{
	"collation_list":   true,
	"data_version":     true,
	"database_list":    true,
	"foreign_key_list": true,
	"function_list":    true,
	"index_info":       true,
	"index_list":       true,
	"index_xinfo":      true,
	"module_list":      true,
	"pragma_list":      true,
	"table_info":       true,
	"table_list":       true,
	"table_xinfo":      true,
}

var deniedFunctions = map[string]bool{
	"load_extension": true,
	"fts3_tokenizer": true,
}
