// The --json output contract (ADR 0005): the commands that report facts —
// ingest --once, devices list, devices invite, devices revoke, rederive,
// version — print exactly one JSON envelope object on stdout when asked,
// while diagnostics stay on stderr and the T059/T060 exit codes stand.
// Text stays the default. Usage errors never produce an envelope: they
// report an unparseable command line before any command semantics exist.

package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/nibzard/ohya/internal/control"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/watch"
)

// jsonSchemaVersion is the envelope contract's version. It changes only on a
// breaking change to the envelope; additive fields do not bump it (ADR 0005).
const jsonSchemaVersion = 1

// jsonEnvelope is the one object a --json command prints. Data holds the
// command's report on success and stays null on failure; Error is null on
// success. Data is json.RawMessage so the same struct decodes every
// command's output.
type jsonEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Command       string          `json:"command"`
	Data          json.RawMessage `json:"data"`
	Error         *jsonError      `json:"error"`
}

// jsonError carries a failure's stable classification: a snake_case code a
// consumer can branch on, the message the text output's stderr line also
// carries, and a hint naming the remedy when one exists.
type jsonError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// classifiedError attaches the ADR 0005 code and hint to a failure at the
// call site that knows its class, where no sentinel exists to classify by.
// Error stays the underlying message, so the text output is unchanged.
type classifiedError struct {
	code string
	hint string
	err  error
}

func (e *classifiedError) Error() string { return e.err.Error() }
func (e *classifiedError) Unwrap() error { return e.err }

// classify wraps err with its ADR 0005 code and hint.
func classify(err error, code, hint string) error {
	return &classifiedError{code: code, hint: hint, err: err}
}

// writeJSONEnvelope prints exactly one envelope object on w: data marshaled
// with a null error on success, or data null with the classified error on
// failure. Only the write error is returned; command failures travel through
// each command's normal error return, so the exit-code mapping stands.
func writeJSONEnvelope(w io.Writer, command string, data any, jerr *jsonError) error {
	env := jsonEnvelope{SchemaVersion: jsonSchemaVersion, Command: command, Error: jerr}
	if jerr == nil {
		raw, err := json.Marshal(data)
		if err != nil {
			env.Error = &jsonError{Code: "internal_error", Message: err.Error()}
		} else {
			env.Data = raw
		}
	}
	enc := json.NewEncoder(w)
	// Messages carry paths and flag spellings; keep them readable instead of
	// HTML-escaped — the output is machine-parsed, not embedded in HTML.
	enc.SetEscapeHTML(false)
	return enc.Encode(env)
}

// printJSON prints the success envelope of one --json command.
func printJSON(w io.Writer, command string, data any) error {
	return writeJSONEnvelope(w, command, data, nil)
}

// printJSONError prints the failure envelope of one --json command with the
// error's classification and returns err unchanged, so the caller's return
// keeps the T059/T060 exit code and cliMain's stderr line.
func printJSONError(w io.Writer, command string, err error) error {
	code, hint := jsonClassOf(err)
	_ = writeJSONEnvelope(w, command, nil, &jsonError{Code: code, Message: err.Error(), Hint: hint})
	return err
}

// jsonFailurePrint reports one failure under the --json contract: the
// envelope on stdout when asJSON is set, and err either way.
func jsonFailurePrint(asJSON bool, stdout io.Writer, command string, err error) error {
	if asJSON {
		return printJSONError(stdout, command, err)
	}
	return err
}

// jsonClassOf maps a failure to its stable ADR 0005 code and hint. A failure
// nothing classifies is internal_error, which carries no hint.
func jsonClassOf(err error) (code, hint string) {
	var c *classifiedError
	if errors.As(err, &c) {
		return c.code, c.hint
	}
	switch {
	case errors.Is(err, watch.ErrStateLost), errors.Is(err, device.ErrUnusable):
		return "state_recovery_failed", "restore the state directory's identity and checkpoint files from a backup, then rerun"
	case errors.Is(err, control.ErrNotProvisioned):
		return "coordination_plane_absent", "run ohya serve once to provision the coordination plane, then retry"
	case errors.Is(err, registry.ErrDeviceUnknown):
		return "device_unknown", "run ohya devices list to see every paired device ID"
	case errors.Is(err, registry.ErrAlreadyRevoked):
		return "device_already_revoked", "revocation is final for the identity; re-admission needs a new device identity"
	}
	return "internal_error", ""
}
