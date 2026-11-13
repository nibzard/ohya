package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/nibzard/ohya/internal/durablefile"
)

// stateVersion is the checkpoint file format version.
const stateVersion = 1

// agentRE bounds agent names. Agent names become state file names, so they
// are lowercase slugs: "claude-code", "codex", "pi".
var agentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ErrStateLost reports that the local source state exists but cannot be
// used. The watcher refuses to poll: without the saved source IDs and
// acknowledged offsets it cannot tell which bytes were already ingested, so
// it must not silently restart and claim exactly-once ingestion. Restore the
// state file, or delete it to re-import every source under new source IDs.
// Re-import is retry-safe through deduplication, but it duplicates history:
// deduplication keys cannot merge two incarnations.
var ErrStateLost = errors.New("watch: local source state exists but cannot be read; recovery required")

// sourceCheckpoint is the durable local view of one watched source: the
// identity of its file incarnation and the acknowledged byte offset.
type sourceCheckpoint struct {
	// SourceID is the persisted random ID of this file incarnation.
	SourceID string `json:"source_id"`
	// Agent names the agent whose session logs this source holds.
	Agent string `json:"agent"`
	// Path is the watched path (also the key in the checkpoint file).
	Path string `json:"path"`
	// FileIdentity is the platform identity of the incarnation, or "" when
	// the platform provides none.
	FileIdentity string `json:"file_identity"`
	// CursorBytes is the acknowledged byte offset: the hub durably
	// committed everything before it.
	CursorBytes int64 `json:"cursor_bytes"`
	// SizeBytes is the file size observed at the last checkpoint.
	SizeBytes int64 `json:"size_bytes"`
	// FingerprintWindow is the length of the content fingerprint window
	// ending at CursorBytes; 0 while the cursor is 0.
	FingerprintWindow int64 `json:"fingerprint_window"`
	// FingerprintSHA256 is the hex digest of the window bytes; "" while the
	// cursor is 0.
	FingerprintSHA256 string `json:"fingerprint_sha256"`
}

// checkpointFile is the on-disk shape of <stateDir>/sources/<agent>.json.
type checkpointFile struct {
	Version int                         `json:"version"`
	Sources map[string]sourceCheckpoint `json:"sources"`
}

// statePath returns the checkpoint file holding one agent's sources.
func statePath(stateDir, agent string) string {
	return filepath.Join(stateDir, "sources", agent+".json")
}

// loadState reads the checkpoint file. A missing file is a fresh start with
// no sources. Any existing-but-unusable file is ErrStateLost: the watcher
// cannot tell what was already ingested, so it must report the recovery
// need instead of restarting silently.
func loadState(stateDir, agent string) (checkpointFile, error) {
	path := statePath(stateDir, agent)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkpointFile{
				Version: stateVersion,
				Sources: map[string]sourceCheckpoint{},
			}, nil
		}
		return checkpointFile{}, fmt.Errorf("%w: read %s: %v", ErrStateLost, path, err)
	}
	var state checkpointFile
	if err := json.Unmarshal(data, &state); err != nil {
		return checkpointFile{}, fmt.Errorf("%w: parse %s: %v", ErrStateLost, path, err)
	}
	if state.Version != stateVersion {
		return checkpointFile{}, fmt.Errorf("%w: %s has version %d, want %d",
			ErrStateLost, path, state.Version, stateVersion)
	}
	if state.Sources == nil {
		state.Sources = map[string]sourceCheckpoint{}
	}
	for key, entry := range state.Sources {
		if entry.SourceID == "" || entry.Path != key || entry.Agent != agent ||
			entry.CursorBytes < 0 || entry.SizeBytes < 0 || entry.FingerprintWindow < 0 ||
			entry.FingerprintWindow > entry.CursorBytes ||
			(entry.CursorBytes > 0 && (entry.FingerprintWindow == 0 || entry.FingerprintSHA256 == "")) {
			return checkpointFile{}, fmt.Errorf("%w: %s holds an inconsistent entry for %s", ErrStateLost, path, key)
		}
	}
	return state, nil
}

// saveState durably replaces the checkpoint file.
func saveState(stateDir, agent string, state checkpointFile) error {
	path := statePath(stateDir, agent)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("watch: create state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("watch: encode state: %w", err)
	}
	if err := durablefile.Write(path, append(data, '\n')); err != nil {
		return err
	}
	return nil
}
