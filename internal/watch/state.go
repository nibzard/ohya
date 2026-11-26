package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/nibzard/ohya/internal/durablefile"
)

const stateVersion = 1

var agentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var ErrStateLost = errors.New("watch: local source state exists but cannot be read; recovery required")

type sourceCheckpoint struct {
	SourceID          string `json:"source_id"`
	Agent             string `json:"agent"`
	Path              string `json:"path"`
	FileIdentity      string `json:"file_identity"`
	CursorBytes       int64  `json:"cursor_bytes"`
	SizeBytes         int64  `json:"size_bytes"`
	FingerprintWindow int64  `json:"fingerprint_window,omitempty"`
	FingerprintSHA256 string `json:"fingerprint_sha256,omitempty"`
}

type checkpointFile struct {
	Version int                         `json:"version"`
	Sources map[string]sourceCheckpoint `json:"sources"`
}

func statePath(stateDir, agent string) string {
	return filepath.Join(stateDir, "sources", agent+".json")
}

type SourceSnapshot struct {
	Agent       string
	Path        string
	SourceID    string
	CursorBytes int64
}

func InspectSources(stateDir, agent string) ([]SourceSnapshot, error) {
	state, err := loadState(stateDir, agent)
	if err != nil {
		return nil, err
	}
	out := make([]SourceSnapshot, 0, len(state.Sources))
	for _, key := range slices.Sorted(maps.Keys(state.Sources)) {
		entry := state.Sources[key]
		out = append(out, SourceSnapshot{
			Agent:       entry.Agent,
			Path:        entry.Path,
			SourceID:    entry.SourceID,
			CursorBytes: entry.CursorBytes,
		})
	}
	return out, nil
}

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

func saveState(stateDir, agent string, state checkpointFile) (int64, error) {
	path := statePath(stateDir, agent)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, fmt.Errorf("watch: create state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return 0, fmt.Errorf("watch: encode state: %w", err)
	}
	payload := append(data, '\n')
	if err := durablefile.Write(path, payload); err != nil {
		return 0, err
	}
	return int64(len(payload)), nil
}
