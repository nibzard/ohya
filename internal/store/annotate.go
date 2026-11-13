// Bounded atomic annotation write path. One Annotate call commits one batch
// of operations on event_annotations through the hub writer: validation and
// the missing-event check precede every write statement, the whole batch
// commits as one transaction, and counts are reported only after the durable
// commit. Events, sessions, sources, cursors and FTS are never touched.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// AnnotationOpKind selects one annotation operation.
type AnnotationOpKind string

// The two operations annotate accepts.
const (
	AnnotationUpsert AnnotationOpKind = "upsert"
	AnnotationDelete AnnotationOpKind = "delete"
)

// Annotation limits, shared by this write path and the MCP tool.
const (
	// MaxAnnotationOperations bounds one batch.
	MaxAnnotationOperations = 100
	// MaxAnnotationValueBytes bounds the stored UTF-8 JSON text of one value.
	MaxAnnotationValueBytes = 64 << 10
)

// annotationNameRE bounds namespaces and keys: one to 128 ASCII characters
// from letters, digits, underscore, hyphen, dot and slash. The class holds
// ASCII only, so no non-ASCII name can match.
var annotationNameRE = regexp.MustCompile(`^[A-Za-z0-9_\-./]{1,128}$`)

// AnnotationOp is one operation of an annotate batch. Value holds compact
// JSON text for upserts and is empty for deletes. The hub writer ID is a
// parameter of the batch, never of an operation: one call writes as one writer.
type AnnotationOp struct {
	Op        AnnotationOpKind
	EventID   int64
	Namespace string
	Key       string
	Value     string
}

// AnnotateResult reports one committed batch. Applied counts the operations
// that changed a row (insert, value replacement, delete that removed a row);
// Unchanged counts the ones that matched stored state exactly (identical
// upsert, delete of an absent annotation).
type AnnotateResult struct {
	Applied   int `json:"applied"`
	Unchanged int `json:"unchanged"`
}

// annotationID is the batch-level identity of one operation. The writer ID is
// constant within a batch, so the duplicate check leaves it out.
type annotationID struct {
	eventID   int64
	namespace string
	key       string
}

// Annotate validates and commits one batch of annotation operations
// atomically. writer must be the single hub writer from Open; writerID is the
// authenticated device's ID, assigned by the hub and never taken from caller
// input. A rejected batch applies nothing: every rule below is checked before
// the first write statement, and any statement error rolls back the batch.
func Annotate(ctx context.Context, writer *sql.DB, writerID string, ops []AnnotationOp) (AnnotateResult, error) {
	if writer == nil {
		return AnnotateResult{}, fmt.Errorf("store: annotate: no writer configured")
	}
	if err := validateAnnotationBatch(writerID, ops); err != nil {
		return AnnotateResult{}, err
	}
	if len(ops) == 0 {
		return AnnotateResult{}, nil
	}

	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return AnnotateResult{}, fmt.Errorf("store: annotate: begin: %w", err)
	}
	defer tx.Rollback() // no-op after commit

	if err := requireAnnotationEvents(ctx, tx, ops); err != nil {
		return AnnotateResult{}, err
	}

	result := AnnotateResult{}
	for i := range ops {
		changed, err := applyAnnotation(ctx, tx, &ops[i], writerID)
		if err != nil {
			return AnnotateResult{}, err
		}
		if changed {
			result.Applied++
		} else {
			result.Unchanged++
		}
	}
	if err := tx.Commit(); err != nil {
		return AnnotateResult{}, fmt.Errorf("store: annotate: commit: %w", err)
	}
	return result, nil
}

// validateAnnotationBatch checks every batch rule before any database work:
// writer identity, batch size, operation shape, name syntax, value size and
// validity, and duplicate identities.
func validateAnnotationBatch(writerID string, ops []AnnotationOp) error {
	if writerID == "" {
		return fmt.Errorf("store: annotate: no writer ID; the hub assigns it from the device identity")
	}
	if len(ops) > MaxAnnotationOperations {
		return fmt.Errorf("store: annotate: %d operations exceed the limit of %d", len(ops), MaxAnnotationOperations)
	}
	first := make(map[annotationID]int, len(ops))
	for i := range ops {
		op := &ops[i]
		where := fmt.Sprintf("store: annotate: operation %d", i)
		switch op.Op {
		case AnnotationUpsert, AnnotationDelete:
		default:
			return fmt.Errorf("%s: unknown op %q, want %q or %q", where, op.Op, AnnotationUpsert, AnnotationDelete)
		}
		if op.EventID < 1 {
			return fmt.Errorf("%s: event_id %d is not a positive integer", where, op.EventID)
		}
		if !annotationNameRE.MatchString(op.Namespace) {
			return fmt.Errorf("%s: namespace %q must hold 1 to 128 ASCII characters from letters, digits and _ - . /",
				where, op.Namespace)
		}
		if !annotationNameRE.MatchString(op.Key) {
			return fmt.Errorf("%s: key %q must hold 1 to 128 ASCII characters from letters, digits and _ - . /",
				where, op.Key)
		}
		if op.Op == AnnotationUpsert {
			if op.Value == "" {
				return fmt.Errorf("%s: upsert requires a JSON value", where)
			}
			if !json.Valid([]byte(op.Value)) {
				return fmt.Errorf("%s: value is not valid JSON", where)
			}
			if len(op.Value) > MaxAnnotationValueBytes {
				return fmt.Errorf("%s: value holds %d bytes, above the %d byte limit",
					where, len(op.Value), MaxAnnotationValueBytes)
			}
		} else if op.Value != "" {
			return fmt.Errorf("%s: delete takes no value; omit it", where)
		}
		id := annotationID{op.EventID, op.Namespace, op.Key}
		if j, dup := first[id]; dup {
			return fmt.Errorf("%s: duplicate identity: operations %d and %d both annotate event %d, namespace %q, key %q",
				where, j, i, op.EventID, op.Namespace, op.Key)
		}
		first[id] = i
	}
	return nil
}

// requireAnnotationEvents proves every referenced event exists before any
// write runs, so a missing event rejects the batch with a precise error
// instead of failing on the foreign key mid-transaction.
func requireAnnotationEvents(ctx context.Context, tx *sql.Tx, ops []AnnotationOp) error {
	wanted := make(map[int64]bool, len(ops))
	for _, op := range ops {
		wanted[op.EventID] = true
	}
	query := `SELECT id FROM events WHERE id IN (?` +
		strings.Repeat(", ?", len(wanted)-1) + `)`
	args := make([]any, 0, len(wanted))
	for id := range wanted {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: annotate: read events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("store: annotate: read event: %w", err)
		}
		delete(wanted, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: annotate: read events: %w", err)
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for id := range wanted {
			missing = append(missing, fmt.Sprint(id))
		}
		slices.Sort(missing)
		return fmt.Errorf("store: annotate: no event with id %s; annotations attach to existing events",
			strings.Join(missing, ", "))
	}
	return nil
}

// applyAnnotation applies one operation inside the batch transaction and
// reports whether it changed a row. Timestamps come from the hub clock, not
// the caller; an update never moves updated_at_ms below created_at_ms, so a
// hub clock step backwards cannot trip the table's check.
func applyAnnotation(ctx context.Context, tx *sql.Tx, op *AnnotationOp, writerID string) (bool, error) {
	where := fmt.Sprintf("store: annotate: event %d, namespace %q, key %q: ", op.EventID, op.Namespace, op.Key)
	switch op.Op {
	case AnnotationUpsert:
		now := time.Now().UnixMilli()
		var stored string
		var created int64
		err := tx.QueryRowContext(ctx,
			`SELECT value, created_at_ms FROM event_annotations
			 WHERE event_id = ? AND namespace = ? AND key = ? AND writer_id = ?`,
			op.EventID, op.Namespace, op.Key, writerID).Scan(&stored, &created)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO event_annotations
				   (event_id, namespace, key, value, writer_id, created_at_ms, updated_at_ms)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				op.EventID, op.Namespace, op.Key, op.Value, writerID, now, now); err != nil {
				return false, fmt.Errorf("%sinsert: %w", where, err)
			}
			return true, nil
		case err != nil:
			return false, fmt.Errorf("%sread annotation: %w", where, err)
		case stored == op.Value:
			// Identical retry: no write, so even timestamps stay untouched.
			return false, nil
		default:
			updated := now
			if created > updated {
				updated = created
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE event_annotations SET value = ?, updated_at_ms = ?
				 WHERE event_id = ? AND namespace = ? AND key = ? AND writer_id = ?`,
				op.Value, updated, op.EventID, op.Namespace, op.Key, writerID); err != nil {
				return false, fmt.Errorf("%supdate: %w", where, err)
			}
			return true, nil
		}
	case AnnotationDelete:
		res, err := tx.ExecContext(ctx,
			`DELETE FROM event_annotations
			 WHERE event_id = ? AND namespace = ? AND key = ? AND writer_id = ?`,
			op.EventID, op.Namespace, op.Key, writerID)
		if err != nil {
			return false, fmt.Errorf("%sdelete: %w", where, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("%scount deleted: %w", where, err)
		}
		return affected > 0, nil
	default:
		return false, fmt.Errorf("%sunknown op %q", where, op.Op)
	}
}
