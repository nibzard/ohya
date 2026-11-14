// The annotate(operations) MCP tool. Agents persist tags, classifications
// and security-review findings as JSON annotations on existing events,
// without touching any original transcript record. The tool decodes one
// request strictly, enforces the request limits here, and commits the batch
// atomically through the write path in internal/store.

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nibzard/ohya/internal/store"
)

// AnnotateToolName is the annotation tool's name.
const AnnotateToolName = "annotate"

// ErrAnnotateRejected reports an annotate request refused on its content
// before anything touched the database: strict decoding, request limits, or
// a rule an operation violates. Retrying the same bytes can never succeed.
var ErrAnnotateRejected = errors.New("annotate tool")

// boolPtr returns a pointer to b, for the tool annotation hints that are
// pointers so an explicit false reaches clients.
func boolPtr(b bool) *bool { return &b }

// annotateArgs shapes the tool's advertised input schema. The handler decodes
// the raw arguments itself, with strict unknown-field rejection and the
// upsert/delete value rule the schema cannot express; these types only
// describe the contract to clients.
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

// annotateRequestJSON is the strict wire shape the handler decodes.
type annotateRequestJSON struct {
	Operations []json.RawMessage `json:"operations"`
}

// annotateOperationJSON decodes one operation strictly. Every required field
// is a pointer, so absent and null stay distinguishable from empty values;
// Value is raw text, so absent, null and a JSON null value differ too.
type annotateOperationJSON struct {
	Op        *string         `json:"op"`
	EventID   *int64          `json:"event_id"`
	Namespace *string         `json:"namespace"`
	Key       *string         `json:"key"`
	Value     json.RawMessage `json:"value,omitempty"`
}

// Annotate applies one annotate request attributed to this engine's writer
// identity: the local Device ID of the stdio tool.
func (e *Engine) Annotate(ctx context.Context, raw json.RawMessage) (store.AnnotateResult, error) {
	return e.AnnotateAs(ctx, e.options.WriterID, raw)
}

// AnnotateAs applies one annotate request attributed to writerID, which the
// caller derives from an authenticated identity, never from request
// content. The sync endpoint passes the authenticated device; a request can
// never name a writer, because writer_id fails the strict decode. Every
// content rejection fires before the batch touches the database, so a
// rejected request applies nothing.
func (e *Engine) AnnotateAs(ctx context.Context, writerID string, raw json.RawMessage) (store.AnnotateResult, error) {
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
	ops := make([]store.AnnotationOp, 0, len(request.Operations))
	for i, rawOp := range request.Operations {
		op, err := e.decodeOperation(i, rawOp)
		if err != nil {
			return store.AnnotateResult{}, err
		}
		ops = append(ops, op)
	}
	return store.Annotate(ctx, e.writer, writerID, ops)
}

// decodeOperation turns one raw operation into a validated store operation.
// The writer identity is deliberately absent: the hub assigns it, so a caller
// supplying writer_id fails the unknown-field check below.
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

// decodeStrict unmarshals exactly one JSON value into v, rejecting unknown
// fields and trailing data.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("input holds more than one JSON value")
	}
	return nil
}

// AnnotateDescription publishes the annotate contract, so an agent can call
// the tool without any out-of-band context.
func AnnotateDescription(o EngineOptions) string {
	return fmt.Sprintf(`Apply a batch of annotation operations to existing events, atomically: every operation commits or none does. Annotations are your interpretations (tags, classifications, security-review findings) stored in event_annotations; original transcript records are never modified.

Each operation is an object with:
- "op": "upsert" or "delete"
- "event_id": id of an existing event (SELECT id FROM events)
- "namespace" and "key": each 1-128 ASCII characters from letters, digits and _ - . /
- "value": any JSON value — required for upsert, omit it for delete

Rules:
- writer_id is assigned by the hub; this device writes as %s. Never send it: unknown fields, including writer_id, are rejected.
- The annotation identity is (event_id, namespace, key, writer_id). Upsert replaces the complete value, stored as compact JSON. An identical retry changes nothing, not even timestamps. A changed value keeps created_at_ms and refreshes updated_at_ms.
- Delete of an absent annotation succeeds and counts as unchanged.
- Limits: at most %d operations and %d bytes per request; at most %d bytes of UTF-8 JSON per value; no duplicate identities in one batch; every event_id must exist.
- The result {"applied": n, "unchanged": m} reports the durable commit. Query annotations with the sql tool.

Example — record a completed security review:
{"operations":[{"op":"upsert","event_id":42,"namespace":"security","key":"review","value":{"status":"reviewed","analysis_version":"v1","findings":[]}}]}

Example — remove a tag and retry another annotation identically:
{"operations":[{"op":"delete","event_id":42,"namespace":"tags","key":"needs-review"}]}
`, o.WriterID, store.MaxAnnotationOperations, o.MaxAnnotateRequestBytes, store.MaxAnnotationValueBytes)
}
