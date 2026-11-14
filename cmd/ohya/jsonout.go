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

const jsonSchemaVersion = 1

type jsonEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Command       string          `json:"command"`
	Data          json.RawMessage `json:"data"`
	Error         *jsonError      `json:"error"`
}

type jsonError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

type classifiedError struct {
	code string
	hint string
	err  error
}

func (e *classifiedError) Error() string { return e.err.Error() }
func (e *classifiedError) Unwrap() error { return e.err }

func classify(err error, code, hint string) error {
	return &classifiedError{code: code, hint: hint, err: err}
}

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
	enc.SetEscapeHTML(false)
	return enc.Encode(env)
}

func printJSON(w io.Writer, command string, data any) error {
	return writeJSONEnvelope(w, command, data, nil)
}

func printJSONError(w io.Writer, command string, err error) error {
	code, hint := jsonClassOf(err)
	_ = writeJSONEnvelope(w, command, nil, &jsonError{Code: code, Message: err.Error(), Hint: hint})
	return err
}

func jsonFailurePrint(asJSON bool, stdout io.Writer, command string, err error) error {
	if asJSON {
		return printJSONError(stdout, command, err)
	}
	return err
}

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
