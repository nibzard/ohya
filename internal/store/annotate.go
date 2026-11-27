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

	"github.com/nibzard/ohya/internal/secrets"
)

type AnnotationOpKind string

const (
	AnnotationUpsert AnnotationOpKind = "upsert"
	AnnotationDelete AnnotationOpKind = "delete"
)

var ErrAnnotationRejected = errors.New("store: annotate")

const (
	MaxAnnotationOperations = 100
	MaxAnnotationValueBytes = 64 << 10
)

var annotationNameRE = regexp.MustCompile(`^[A-Za-z0-9_\-./]{1,128}$`)

type AnnotationOp struct {
	Op        AnnotationOpKind
	EventID   int64
	Namespace string
	Key       string
	Value     string
}

type AnnotateResult struct {
	Applied   int `json:"applied"`
	Unchanged int `json:"unchanged"`

	AppliedUpsert   int `json:"-"`
	UnchangedUpsert int `json:"-"`
	AppliedDelete   int `json:"-"`
	UnchangedDelete int `json:"-"`
}

type annotationID struct {
	eventID   int64
	namespace string
	key       string
}

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
	defer tx.Rollback()

	if err := requireAnnotationEvents(ctx, tx, ops); err != nil {
		return AnnotateResult{}, err
	}

	result := AnnotateResult{}
	for i := range ops {
		changed, err := applyAnnotation(ctx, tx, &ops[i], writerID)
		if err != nil {
			return AnnotateResult{}, err
		}
		switch {
		case changed && ops[i].Op == AnnotationUpsert:
			result.Applied++
			result.AppliedUpsert++
		case changed:
			result.Applied++
			result.AppliedDelete++
		case ops[i].Op == AnnotationUpsert:
			result.Unchanged++
			result.UnchangedUpsert++
		default:
			result.Unchanged++
			result.UnchangedDelete++
		}
	}
	if err := tx.Commit(); err != nil {
		return AnnotateResult{}, fmt.Errorf("store: annotate: commit: %w", err)
	}
	return result, nil
}

func valueViolation(value string) string {
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return ""
	}
	return jsonViolation(decoded)
}

func jsonViolation(v any) string {
	switch x := v.(type) {
	case string:
		return secrets.Violation(x)
	case []any:
		for _, item := range x {
			if rule := jsonViolation(item); rule != "" {
				return rule
			}
		}
	case map[string]any:
		for k, item := range x {
			if rule := secrets.Violation(k); rule != "" {
				return rule
			}
			if rule := jsonViolation(item); rule != "" {
				return rule
			}
		}
	}
	return ""
}

func validateAnnotationBatch(writerID string, ops []AnnotationOp) error {
	if writerID == "" {
		return fmt.Errorf("%w: no writer ID; the hub assigns it from the device identity", ErrAnnotationRejected)
	}
	if len(ops) > MaxAnnotationOperations {
		return fmt.Errorf("%w: %d operations exceed the limit of %d", ErrAnnotationRejected, len(ops), MaxAnnotationOperations)
	}
	first := make(map[annotationID]int, len(ops))
	for i := range ops {
		op := &ops[i]
		switch op.Op {
		case AnnotationUpsert, AnnotationDelete:
		default:
			return fmt.Errorf("%w: operation %d: unknown op %q, want %q or %q",
				ErrAnnotationRejected, i, op.Op, AnnotationUpsert, AnnotationDelete)
		}
		if op.EventID < 1 {
			return fmt.Errorf("%w: operation %d: event_id %d is not a positive integer", ErrAnnotationRejected, i, op.EventID)
		}
		if !annotationNameRE.MatchString(op.Namespace) {
			return fmt.Errorf("%w: operation %d: namespace %q must hold 1 to 128 ASCII characters from letters, digits and _ - . /",
				ErrAnnotationRejected, i, op.Namespace)
		}
		if !annotationNameRE.MatchString(op.Key) {
			return fmt.Errorf("%w: operation %d: key %q must hold 1 to 128 ASCII characters from letters, digits and _ - . /",
				ErrAnnotationRejected, i, op.Key)
		}
		if op.Op == AnnotationUpsert {
			if op.Value == "" {
				return fmt.Errorf("%w: operation %d: upsert requires a JSON value", ErrAnnotationRejected, i)
			}
			if !json.Valid([]byte(op.Value)) {
				return fmt.Errorf("%w: operation %d: value is not valid JSON", ErrAnnotationRejected, i)
			}
			if rule := valueViolation(op.Value); rule != "" {
				return fmt.Errorf("%w: operation %d: value matches content rule %q; annotations hold your own interpretation, not copied instructions or hidden characters",
					ErrAnnotationRejected, i, rule)
			}
			if len(op.Value) > MaxAnnotationValueBytes {
				return fmt.Errorf("%w: operation %d: value holds %d bytes, above the %d byte limit",
					ErrAnnotationRejected, i, len(op.Value), MaxAnnotationValueBytes)
			}
		} else if op.Value != "" {
			return fmt.Errorf("%w: operation %d: delete takes no value; omit it", ErrAnnotationRejected, i)
		}
		id := annotationID{op.EventID, op.Namespace, op.Key}
		if j, dup := first[id]; dup {
			return fmt.Errorf("%w: operation %d: duplicate identity: operations %d and %d both annotate event %d, namespace %q, key %q",
				ErrAnnotationRejected, i, j, i, op.EventID, op.Namespace, op.Key)
		}
		first[id] = i
	}
	return nil
}

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
		return fmt.Errorf("%w: no event with id %s; annotations attach to existing events",
			ErrAnnotationRejected, strings.Join(missing, ", "))
	}
	return nil
}

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
