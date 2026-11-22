package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

const AnnotateToolName = "annotate"

var ErrAnnotateRejected = errors.New("annotate tool")

func boolPtr(b bool) *bool { return &b }

type annotateArgs struct {
	Operations []annotateArgsOperation `json:"operations" jsonschema:"1 to 100 annotation operations, applied atomically"`
}

type annotateArgsOperation struct {
	Op        string `json:"op" jsonschema:"upsert or delete"`
	EventID   int64  `json:"event_id" jsonschema:"id of an existing event"`
	Namespace string `json:"namespace" jsonschema:"1 to 128 ASCII characters from letters, digits, _, -, ., /"`
	Key       string `json:"key" jsonschema:"1 to 128 ASCII characters from letters, digits, _, -, ., /"`
	Value     any    `json:"value,omitempty" jsonschema:"JSON value; required for upsert, forbidden for delete; at most 64 KiB of UTF-8 JSON"`
}

type annotateRequestJSON struct {
	Operations []json.RawMessage `json:"operations"`
}

type annotateOperationJSON struct {
	Op        *string         `json:"op"`
	EventID   *int64          `json:"event_id"`
	Namespace *string         `json:"namespace"`
	Key       *string         `json:"key"`
	Value     json.RawMessage `json:"value,omitempty"`
}

func (e *Engine) Annotate(ctx context.Context, raw json.RawMessage) (store.AnnotateResult, error) {
	return e.AnnotateAs(ctx, e.options.WriterID, raw)
}

func (e *Engine) AnnotateAs(ctx context.Context, writerID string, raw json.RawMessage) (store.AnnotateResult, error) {
	if !e.traced {
		return e.annotateAs(ctx, writerID, raw, &observe.AnnotateOps{})
	}
	origin := OriginFromContext(ctx)
	if origin.Path == "" {
		origin.Path = observe.UsagePathLocal
	}
	if origin.DeviceID == "" && origin.Path == observe.UsagePathLocal {
		origin.DeviceID = writerID
	}
	trace := newQueryTrace()
	ops := &observe.AnnotateOps{}
	start := time.Now()
	result, err := e.annotateAs(ctx, writerID, raw, ops)
	trace.mu.Lock()
	trace.phases.TotalMS = elapsedMS(time.Since(start))
	trace.mu.Unlock()
	trace.finish()
	if e.options.Usage != nil {
		e.options.Usage.Record(trace.usageRecord(e, origin, Result{}, err, ops))
	}
	e.reportAnnotate(result, err)
	return result, err
}

func (e *Engine) annotateAs(ctx context.Context, writerID string, raw json.RawMessage, ops *observe.AnnotateOps) (store.AnnotateResult, error) {
	if len(raw) == 0 {
		return store.AnnotateResult{}, fmt.Errorf("%w: missing arguments", ErrAnnotateRejected)
	}
	if len(raw) > e.options.MaxAnnotateRequestBytes {
		return store.AnnotateResult{}, fmt.Errorf("%w: request holds %d bytes, above the %d byte limit",
			ErrAnnotateRejected, len(raw), e.options.MaxAnnotateRequestBytes)
	}
	if e.writer == nil {
		return store.AnnotateResult{}, fmt.Errorf("%w: no hub writer configured", ErrAnnotateRejected)
	}
	var request annotateRequestJSON
	if err := decodeStrict(raw, &request); err != nil {
		return store.AnnotateResult{}, fmt.Errorf("%w: arguments: %w", ErrAnnotateRejected, err)
	}
	if request.Operations == nil {
		return store.AnnotateResult{}, fmt.Errorf("%w: arguments: missing operations", ErrAnnotateRejected)
	}
	if len(request.Operations) > store.MaxAnnotationOperations {
		return store.AnnotateResult{}, fmt.Errorf("%w: %d operations exceed the limit of %d",
			ErrAnnotateRejected, len(request.Operations), store.MaxAnnotationOperations)
	}
	operations := make([]store.AnnotationOp, 0, len(request.Operations))
	for i, rawOp := range request.Operations {
		op, err := e.decodeOperation(i, rawOp)
		if err != nil {
			return store.AnnotateResult{}, err
		}
		switch op.Op {
		case store.AnnotationUpsert:
			ops.Upsert++
		case store.AnnotationDelete:
			ops.Delete++
		}
		operations = append(operations, op)
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return store.AnnotateResult{}, ErrBusy
	}
	runCtx, cancel := context.WithTimeout(ctx, e.options.Deadline)
	defer cancel()
	storeStart := time.Now()
	result, err := store.Annotate(runCtx, e.writer, writerID, operations)
	e.observer.Observe("annotate_duration", time.Since(storeStart))
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return store.AnnotateResult{}, fmt.Errorf("%w (%s per call): %w", ErrDeadline, e.options.Deadline, err)
	}
	return result, err
}

func (e *Engine) decodeOperation(index int, raw json.RawMessage) (store.AnnotationOp, error) {
	var op annotateOperationJSON
	if err := decodeStrict(raw, &op); err != nil {
		return store.AnnotationOp{}, fmt.Errorf("%w: operation %d: %w", ErrAnnotateRejected, index, err)
	}
	missing := func(field string) error {
		return fmt.Errorf("%w: operation %d: missing %s", ErrAnnotateRejected, index, field)
	}
	switch {
	case op.Op == nil:
		return store.AnnotationOp{}, missing("op")
	case op.EventID == nil:
		return store.AnnotationOp{}, missing("event_id")
	case op.Namespace == nil:
		return store.AnnotationOp{}, missing("namespace")
	case op.Key == nil:
		return store.AnnotationOp{}, missing("key")
	}
	result := store.AnnotationOp{
		Op:        store.AnnotationOpKind(*op.Op),
		EventID:   *op.EventID,
		Namespace: *op.Namespace,
		Key:       *op.Key,
	}
	switch result.Op {
	case store.AnnotationUpsert:
		if op.Value == nil {
			return store.AnnotationOp{}, fmt.Errorf("%w: operation %d: upsert requires a JSON value", ErrAnnotateRejected, index)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, op.Value); err != nil {
			return store.AnnotationOp{}, fmt.Errorf("%w: operation %d: value is not valid JSON: %w", ErrAnnotateRejected, index, err)
		}
		result.Value = compact.String()
	case store.AnnotationDelete:
		if op.Value != nil {
			return store.AnnotationOp{}, fmt.Errorf("%w: operation %d: delete takes no value; omit it", ErrAnnotateRejected, index)
		}
	default:
		return store.AnnotationOp{}, fmt.Errorf("%w: operation %d: unknown op %q, want %q or %q",
			ErrAnnotateRejected, index, *op.Op, store.AnnotationUpsert, store.AnnotationDelete)
	}
	return result, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("input holds trailing content after JSON value")
	}
	return nil
}

func AnnotateDescription(o EngineOptions) string {
	return fmt.Sprintf(`Atomically apply a batch of annotation operations in event_annotations without modifying original transcripts.

Operation fields: "op" ("upsert"|"delete"), "event_id" (existing events.id), "namespace" and "key" (1-128 ASCII chars: letters, digits, _ - . /), "value" (any JSON for upsert; omit for delete).
Rules: writer_id is hub-assigned as %s (never send writer_id; unknown fields are rejected). Identity is (event_id, namespace, key, writer_id). Upsert stores compact JSON, keeps created_at_ms on updates, and is a no-op on identical retries; absent delete counts as unchanged. Limits: %d ops, %d request bytes, %d bytes per value, no duplicate identities in one batch. Returns {"applied": n, "unchanged": m}. Query via sql.

Example:
{"operations":[{"op":"upsert","event_id":42,"namespace":"tags","key":"garbage","value":{"reason":"aborted start, single record","evidence":42}},{"op":"delete","event_id":42,"namespace":"tags","key":"needs-review"}]}
`, o.WriterID, store.MaxAnnotationOperations, o.MaxAnnotateRequestBytes, store.MaxAnnotationValueBytes)
}
