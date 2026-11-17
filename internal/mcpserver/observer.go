package mcpserver

import (
	"errors"
	"time"

	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

func (e *Engine) enterQuery() {
	e.mu.Lock()
	e.active++
	e.observer.Set("sql_active", float64(e.active))
	e.mu.Unlock()
}

func (e *Engine) leaveQuery() {
	e.mu.Lock()
	e.active--
	e.observer.Set("sql_active", float64(e.active))
	e.mu.Unlock()
}

func (e *Engine) reportQuery(trace *queryTrace, result Result, err error) {
	trace.mu.Lock()
	phases := trace.phases
	rows, resultBytes := trace.rows, trace.resultBytes
	trace.mu.Unlock()
	e.observer.Add("sql_requests", 1, "sql", usageOutcome(err, result))
	e.observer.Observe("sql_total_duration", durationFromMS(phases.TotalMS))
	observePhase(e.observer, "sql_reader_wait_duration", phases.ReaderWaitMS)
	observePhase(e.observer, "sql_first_row_duration", phases.FirstRowMS)
	observePhase(e.observer, "sql_iteration_duration", phases.IterationMS)
	if err != nil {
		return
	}
	e.observer.Add("sql_rows", uint64(rows))
	e.observer.Add("sql_result_bytes", uint64(resultBytes))
	if result.Truncated {
		e.observer.Add("sql_truncations", 1, result.TruncationReason)
	}
}

func (e *Engine) reportAnnotate(result store.AnnotateResult, err error) {
	e.observer.Add("sql_requests", 1, "annotate", usageOutcome(err, Result{}))
	if err != nil {
		if errors.Is(err, ErrAnnotateRejected) || errors.Is(err, store.ErrAnnotationRejected) {
			e.observer.Add("annotate_validation_failures", 1)
		}
		return
	}
	e.observer.Add("annotate_operations", uint64(result.AppliedUpsert), "upsert", "applied")
	e.observer.Add("annotate_operations", uint64(result.UnchangedUpsert), "upsert", "unchanged")
	e.observer.Add("annotate_operations", uint64(result.AppliedDelete), "delete", "applied")
	e.observer.Add("annotate_operations", uint64(result.UnchangedDelete), "delete", "unchanged")
}

func observePhase(o observe.Observer, name string, ms float64) {
	if ms > 0 {
		o.Observe(name, durationFromMS(ms))
	}
}

func durationFromMS(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}
