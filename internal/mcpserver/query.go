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
)

const (
	DefaultMaxRows        = 500
	DefaultMaxOutputBytes = 256 << 10
	DefaultMaxQueryBytes  = 64 << 10
	DefaultDeadline       = 5 * time.Second
	DefaultMaxConcurrency = 4

	DefaultMaxAnnotateRequestBytes = 1 << 20
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
	traced     bool
	deniedTbls map[string]bool
	slots      chan struct{}
	mu         sync.Mutex
	active     int64
}

func NewEngine(readers, writer *sql.DB, o EngineOptions) *Engine {
	o = o.withDefaults()
	denied := make(map[string]bool, len(o.DenyTables))
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
		traced:     o.Usage != nil || o.Observer != nil,
		deniedTbls: denied,
		slots:      make(chan struct{}, o.MaxConcurrency),
	}
}

func (e *Engine) Options() EngineOptions { return e.options }

func (e *Engine) Query(ctx context.Context, query string) (Result, error) {
	if !e.traced {
		return e.query(ctx, query, nil)
	}
	origin := OriginFromContext(ctx)
	trace := newQueryTrace()
	start := time.Now()
	result, err := e.query(ctx, query, trace)
	trace.mu.Lock()
	trace.phases.TotalMS = elapsedMS(time.Since(start))
	trace.mu.Unlock()
	trace.finish()
	if e.options.Usage != nil {
		e.options.Usage.Record(trace.usageRecord(e, origin, result, err, nil))
	}
	e.reportQuery(trace, result, err)
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
	if trace != nil {
		trace.phases.AdmissionWaitMS = elapsedMS(time.Since(waitStart))
	}
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
	if trace != nil {
		trace.phases.ReaderWaitMS = elapsedMS(time.Since(readerStart))
	}
	authorize := e.authorize
	if trace != nil {
		authorize = trace.authorizer(e.authorize)
	}
	if err := conn.Raw(func(c any) error {
		sqliteConn, ok := c.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("reader connection is %T, want *sqlite3.SQLiteConn", c)
		}
		sqliteConn.RegisterAuthorizer(authorize)
		return nil
	}); err != nil {
		return Result{}, err
	}

	execStart := time.Now()
	rows, err := conn.QueryContext(ctx, query)
	if trace != nil {
		trace.phases.FirstRowMS = elapsedMS(time.Since(execStart))
	}
	if err != nil {
		return Result{}, queryError(err, e.options.Deadline)
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
		row, err := scanRow(rows, len(result.Columns))
		if err != nil {
			return Result{}, err
		}
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
	if trace != nil {
		trace.phases.IterationMS = elapsedMS(time.Since(iterStart))
		trace.resultBytes = int64(bytes)
	}
	if err := rows.Err(); err != nil {
		return Result{}, queryError(err, e.options.Deadline)
	}
	result.RowCount = len(result.Rows)
	if trace != nil {
		trace.rows = int64(result.RowCount)
	}
	return result, nil
}

func scanRow(rows *sql.Rows, width int) ([]any, error) {
	values := make([]any, width)
	targets := make([]any, width)
	for i := range values {
		targets[i] = &values[i]
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, fmt.Errorf("read row: %w", err)
	}
	for i, v := range values {
		values[i] = jsonValue(v)
	}
	return values, nil
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

func queryError(err error, deadline time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%s per call): %w", ErrDeadline, deadline, err)
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
		if allowedPragmas[strings.ToLower(arg1)] {
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
