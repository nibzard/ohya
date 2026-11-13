// Package mcpserver exposes the hub database to coding agents through the
// read-only sql(query) MCP tool. The query engine keeps every safeguard in
// this package: one statement per call, a connection-level SQLite
// authorizer, an execution deadline, row and byte output limits, and bounded
// concurrency.
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
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// Default limits. They bound one tool call: an agent query must answer fast
// and must never flood the caller's context with output.
const (
	DefaultMaxRows        = 500             // rows returned per call
	DefaultMaxOutputBytes = 256 << 10       // JSON bytes of all returned rows
	DefaultMaxQueryBytes  = 64 << 10        // input query text
	DefaultDeadline       = 5 * time.Second // statement execution deadline
	DefaultMaxConcurrency = 4               // queries executing at once
)

// Sentinel errors reported to callers. Errors wrap them, so tests and
// clients can classify outcomes with errors.Is.
var (
	// ErrBusy reports that all query slots are taken; retrying the same call
	// later succeeds.
	ErrBusy = errors.New("server busy: too many concurrent queries, retry")
	// ErrMultipleStatements reports input holding more than one statement.
	ErrMultipleStatements = errors.New("exactly one SQL statement per call")
	// ErrEmptyQuery reports input holding no statement at all.
	ErrEmptyQuery = errors.New("empty query")
	// ErrDenied reports a statement rejected by the read-only policy.
	ErrDenied = errors.New("not authorized: the sql tool runs SELECT and read-only PRAGMA only")
	// ErrDeadline reports a statement interrupted at the execution deadline.
	// The statement is not a policy violation; it was too slow.
	ErrDeadline = errors.New("execution deadline exceeded")
)

// EngineOptions configures the query engine. The zero value is not usable;
// NewEngine fills unset limits with the package defaults.
type EngineOptions struct {
	// WriterID identifies this device to agents. It is published in the tool
	// description, because annotate (not sql) derives writer_id from it.
	WriterID string
	// MaxRows bounds the rows returned per call; further rows are cut with
	// TruncationReason "row_limit".
	MaxRows int
	// MaxOutputBytes bounds the JSON encoding of all returned rows; the row
	// that would cross the bound is dropped with TruncationReason
	// "byte_limit". A single row is never split.
	MaxOutputBytes int
	// MaxQueryBytes bounds the input query text.
	MaxQueryBytes int
	// Deadline bounds one statement's execution; the driver interrupts the
	// statement when it fires.
	Deadline time.Duration
	// MaxConcurrency bounds queries executing at once; further calls fail
	// with ErrBusy instead of queueing.
	MaxConcurrency int
	// DenyTables names tables the tool must not read. The transcript tables
	// are all exposed; the future device-credential registry (see
	// ARCHITECTURE.md) must be added here before it ships.
	DenyTables []string
}

// withDefaults returns o with every unset limit replaced by its default.
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
	return o
}

// Result is one query's answer. Truncation is explicit: Truncated is set
// with a TruncationReason whenever limits, never silence, cut the output.
type Result struct {
	Columns          []string `json:"columns"`
	Rows             [][]any  `json:"rows"`
	RowCount         int      `json:"row_count"`
	Truncated        bool     `json:"truncated"`
	TruncationReason string   `json:"truncation_reason,omitempty"`
}

// Truncation reasons, reported in Result.TruncationReason.
const (
	truncatedByRows  = "row_limit"
	truncatedByBytes = "byte_limit"
)

// Engine runs one read-only statement per call on a restricted reader pool.
// A pool handed to NewEngine belongs to the tool: the authorizer stays
// installed on every connection the pool opens.
type Engine struct {
	readers    *sql.DB
	options    EngineOptions
	deniedTbls map[string]bool
	slots      chan struct{}
}

// NewEngine returns an engine running statements on readers, a read-only
// pool from store.DB.Reader. Zero-valued limits in o take the defaults.
func NewEngine(readers *sql.DB, o EngineOptions) *Engine {
	o = o.withDefaults()
	denied := make(map[string]bool, len(o.DenyTables))
	for _, t := range o.DenyTables {
		denied[strings.ToLower(t)] = true
	}
	return &Engine{
		readers:    readers,
		options:    o,
		deniedTbls: denied,
		slots:      make(chan struct{}, o.MaxConcurrency),
	}
}

// Options returns the effective limits, defaults applied.
func (e *Engine) Options() EngineOptions { return e.options }

// Query runs exactly one read-only statement and returns its rows within the
// configured limits. Every error, except ctx errors the caller owns, is
// paired with a sentinel from this package.
func (e *Engine) Query(ctx context.Context, query string) (Result, error) {
	if len(query) > e.options.MaxQueryBytes {
		return Result{}, fmt.Errorf("query text exceeds the %d byte input limit", e.options.MaxQueryBytes)
	}
	if err := singleStatement(query); err != nil {
		return Result{}, err
	}
	// Fail fast instead of queueing: the caller can retry.
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return Result{}, ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, e.options.Deadline)
	defer cancel()

	conn, err := e.readers.Conn(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("acquire reader: %w", err)
	}
	defer conn.Close()
	if err := conn.Raw(func(c any) error {
		sqliteConn, ok := c.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("reader connection is %T, want *sqlite3.SQLiteConn", c)
		}
		sqliteConn.RegisterAuthorizer(e.authorize)
		return nil
	}); err != nil {
		return Result{}, err
	}

	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return Result{}, queryError(err, e.options.Deadline)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return Result{}, fmt.Errorf("read columns: %w", err)
	}
	result := Result{Columns: columns, Rows: [][]any{}} // Rows encodes as [] when empty
	bytes := 0
	for rows.Next() {
		if len(result.Rows) == e.options.MaxRows {
			result.Truncated, result.TruncationReason = true, truncatedByRows
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
			// The whole row is dropped, never split: included rows stay
			// complete and truthful.
			result.Truncated, result.TruncationReason = true, truncatedByBytes
			break
		}
		bytes += len(encoded)
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Result{}, queryError(err, e.options.Deadline)
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

// scanRow reads one row into JSON-encodable values.
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

// jsonValue converts a driver value to one json.Marshal accepts. BLOBs
// become base64 text; non-finite floats become their names, because JSON
// cannot carry them.
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

// queryError keeps deadline interruptions recognizable and marks SQLite
// policy rejections with ErrDenied. The authorizer's denials surface with
// different wording per operation: "not authorized" generally, "access to
// table.column is prohibited" for SQLITE_READ, and "authorization denied"
// for VACUUM.
func queryError(err error, deadline time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%s per call): %w", ErrDeadline, deadline, err)
	}
	if msg := err.Error(); strings.Contains(msg, "not authorized") ||
		strings.Contains(msg, "is prohibited") ||
		strings.Contains(msg, "authorization denied") {
		return fmt.Errorf("%w: %v", ErrDenied, err)
	}
	return fmt.Errorf("query failed: %w", err)
}

// sqliteOpRecursive is SQLITE_RECURSIVE, which the driver does not export
// (its constant list comments it out). The bundled header numbers it 33,
// directly after the exported SQLITE_SAVEPOINT (32); verified against the
// driver, and pinned by the recursive-CTE case in the query tests, which
// fails if a driver update renumbers it. A recursive CTE is a read like any
// other SELECT; the execution deadline bounds its runtime.
const sqliteOpRecursive = 33

// authorize is the connection-level SQLite authorizer: deny everything
// except plain reads. It is installed on every pooled connection before its
// first query, and SQLite also runs on a read-only connection, so a policy
// gap cannot become a write.
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
		// Covers writes, DDL, ATTACH and DETACH, transactions, savepoints,
		// virtual-table management, REINDEX, ANALYZE, and everything else
		// that is not a plain read.
		return sqlite3.SQLITE_DENY
	}
}

// allowedPragmas are the read-only pragmas agents may use to inspect the
// schema. data_version is also read by FTS5 itself while running a MATCH
// query, so it must stay allowed for search to work. Every other pragma —
// including connection-state changes such as query_only or busy_timeout —
// is denied.
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

// deniedFunctions cannot run: load_extension loads native code and
// fts3_tokenizer rebinds tokenizer pointers, which is an execution primitive
// even on a read-only connection.
var deniedFunctions = map[string]bool{
	"load_extension": true,
	"fts3_tokenizer": true,
}
