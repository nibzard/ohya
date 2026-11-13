// Package watch tracks session-log files by polling and owns the local
// checkpoint state that gives each file incarnation a stable source ID and
// records acknowledged byte offsets.
//
// A changed incarnation is detected with three signals: the platform file
// identity (device and inode numbers on Unix), the file size against the
// acknowledged cursor, and a content fingerprint of the bytes ending at the
// checkpoint. A detected replacement, truncation or in-place rewrite starts
// a new source ID. A rename that keeps the identity keeps the source.
// Filesystem notifications are deferred (ARCHITECTURE.md); the caller drives
// every poll with the paths it discovered.
package watch

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultFingerprintWindow is the number of bytes ending at each checkpoint
// covered by the saved content fingerprint when Options leaves it unset.
const DefaultFingerprintWindow int64 = 4096

// Options configures a Watcher.
type Options struct {
	// FingerprintWindow bounds the bytes hashed for the checkpoint
	// fingerprint. Values <= 0 select DefaultFingerprintWindow.
	FingerprintWindow int64
}

// Watcher polls one agent's session files and keeps that agent's local
// checkpoint state in <stateDir>/sources/<agent>.json. It is not safe for
// concurrent use; one goroutine drives it, like the database writer.
type Watcher struct {
	stateDir string
	agent    string
	window   int64
	state    checkpointFile
}

// Outcome describes one watched path after a poll.
type Outcome struct {
	// Path is the watched path after this poll: the file's new location
	// when it was renamed.
	Path string
	// SourceID identifies the file incarnation. Replacement or truncation
	// starts a new one.
	SourceID string
	// Cursor is the acknowledged byte offset: the hub durably committed
	// everything before it.
	Cursor int64
	// End is the file size observed this poll. Bytes [Cursor, End) await
	// ingestion; ReadPending returns them.
	End int64
	// Fresh reports that this poll registered the source. Its ID is
	// durably persisted before Poll returns, so before any ingestion.
	Fresh bool
	// Reason explains why a fresh source started; empty otherwise.
	Reason string
	// RenamedFrom is the path this source was watched under before the
	// file moved; empty when it did not move this poll.
	RenamedFrom string
	// Missing reports that the file was not observable this poll. The
	// checkpoint is retained, so the source resumes when the file returns.
	Missing bool
}

// New loads, or starts, the local checkpoint state for agent in stateDir.
// An existing but unusable state file makes New fail with an error that
// wraps ErrStateLost: the watcher must report the recovery need instead of
// silently restarting and claiming exactly-once ingestion.
func New(stateDir, agent string, o Options) (*Watcher, error) {
	if !agentRE.MatchString(agent) {
		return nil, fmt.Errorf("watch: agent name %q is not a lowercase slug", agent)
	}
	window := o.FingerprintWindow
	if window <= 0 {
		window = DefaultFingerprintWindow
	}
	state, err := loadState(stateDir, agent)
	if err != nil {
		return nil, err
	}
	return &Watcher{stateDir: stateDir, agent: agent, window: window, state: state}, nil
}

// probe holds what one poll observed for a candidate path.
type probe struct {
	identity string
	size     int64
}

// Poll observes the candidate paths, which the caller discovers — for
// example by walking the agent's session directory. Paths that do not exist
// or are not regular files are unobservable: tracked ones surface as Missing
// outcomes, untracked ones are ignored.
//
// Poll returns one Outcome per live source and one per missing tracked
// path, sorted by path. Sources with Fresh set, including new files, are
// durably persisted before Poll returns, so a crash right after Poll cannot
// lose a new source ID.
func (w *Watcher) Poll(candidates []string) ([]Outcome, error) {
	probes := make(map[string]probe, len(candidates))
	for _, p := range candidates {
		if p == "" {
			continue
		}
		norm := normalizePath(p)
		if _, seen := probes[norm]; seen {
			continue
		}
		info, err := os.Stat(norm)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		probes[norm] = probe{identity: platformFileIdentity(info), size: info.Size()}
	}

	// Live incarnations by identity, for rename detection. Two live paths
	// cannot share one identity.
	byIdentity := make(map[string]string, len(probes))
	for p, pr := range probes {
		if pr.identity != "" {
			byIdentity[pr.identity] = p
		}
	}

	var outcomes []Outcome
	changed := false

	// A tracked incarnation that left its watched path but is live under
	// another candidate path keeps its source: rebind it to the new path.
	// All moves are computed from the unchanged map and applied together, so
	// two files that swap paths both survive.
	renamedFrom := map[string]string{}
	moves := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(w.state.Sources)) {
		entry := w.state.Sources[key]
		if entry.FileIdentity == "" {
			continue
		}
		if pr, ok := probes[key]; ok && pr.identity == entry.FileIdentity {
			continue // the incarnation still sits at its watched path
		}
		if newPath, ok := byIdentity[entry.FileIdentity]; ok && newPath != key {
			moves[key] = newPath
		}
	}
	if len(moves) > 0 {
		moved := make(map[string]sourceCheckpoint, len(w.state.Sources))
		for key, entry := range w.state.Sources {
			if target, ok := moves[key]; ok {
				entry.Path = target
				renamedFrom[target] = key
				moved[target] = entry
			} else {
				moved[key] = entry
			}
		}
		w.state.Sources = moved
		changed = true
	}

	// Classify every tracked entry at its (possibly rebound) path.
	for _, key := range slices.Sorted(maps.Keys(w.state.Sources)) {
		entry := w.state.Sources[key]
		pr, live := probes[key]
		if !live {
			outcomes = append(outcomes, Outcome{
				Path: key, SourceID: entry.SourceID,
				Cursor: entry.CursorBytes, Missing: true,
			})
			continue
		}
		reason := ""
		switch {
		case pr.identity != entry.FileIdentity:
			reason = "platform file identity changed: the path holds a different file incarnation"
		case pr.size < entry.CursorBytes:
			reason = fmt.Sprintf("file size %d is below the acknowledged cursor %d: the file was truncated",
				pr.size, entry.CursorBytes)
		default:
			reason = w.verifyFingerprint(key, entry)
		}
		if reason == "" {
			outcomes = append(outcomes, Outcome{
				Path: key, SourceID: entry.SourceID,
				Cursor: entry.CursorBytes, End: pr.size,
				RenamedFrom: renamedFrom[key],
			})
			continue
		}
		outcome, err := w.register(key, pr, reason)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
		changed = true
	}

	// Candidate paths no entry claims are new files.
	for _, key := range slices.Sorted(maps.Keys(probes)) {
		if _, tracked := w.state.Sources[key]; tracked {
			continue
		}
		outcome, err := w.register(key, probes[key], "new file")
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
		changed = true
	}

	if changed {
		if err := saveState(w.stateDir, w.agent, w.state); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(outcomes, func(a, b Outcome) int { return strings.Compare(a.Path, b.Path) })
	return outcomes, nil
}

// verifyFingerprint compares the saved checkpoint fingerprint with the
// current file content. It returns the fresh-source reason on a detected
// change, or "" when the checkpoint still matches. A window that cannot be
// read right now also returns "": the source keeps its checkpoint, and
// ReadPending surfaces any real read failure.
func (w *Watcher) verifyFingerprint(path string, entry sourceCheckpoint) string {
	if entry.CursorBytes == 0 {
		return "" // nothing ingested yet; identity and size are the only signals
	}
	sum, err := fingerprintFile(path, entry.CursorBytes, entry.FingerprintWindow)
	switch {
	case err == nil && sum != entry.FingerprintSHA256:
		return "content before the acknowledged cursor changed: the file was rewritten in place"
	case errors.Is(err, errShortRead):
		return "the acknowledged prefix became unreadable during verification: the file shrank"
	default:
		return "" // matches, or unobservable now; a later poll re-verifies
	}
}

// register starts a new source for a path and returns its outcome.
func (w *Watcher) register(path string, pr probe, reason string) (Outcome, error) {
	id, err := newSourceID()
	if err != nil {
		return Outcome{}, err
	}
	w.state.Sources[path] = sourceCheckpoint{
		SourceID:     id,
		Agent:        w.agent,
		Path:         path,
		FileIdentity: pr.identity,
		SizeBytes:    pr.size,
	}
	return Outcome{Path: path, SourceID: id, End: pr.size, Fresh: true, Reason: reason}, nil
}

// ReadPending returns the unacknowledged bytes [o.Cursor, o.End) of the
// source described by outcome o. It re-verifies the file incarnation first,
// so a replaced or truncated file never feeds wrong bytes to ingestion. Call
// Acknowledge once ingestion durably committed an offset; until then every
// poll reports the same pending bytes again.
func (w *Watcher) ReadPending(o Outcome) ([]byte, error) {
	entry, ok := w.state.Sources[o.Path]
	if !ok || entry.SourceID != o.SourceID {
		return nil, fmt.Errorf("watch: %s is not watched as source %s; poll again", o.Path, o.SourceID)
	}
	if o.Cursor < entry.CursorBytes {
		return nil, fmt.Errorf("watch: outcome for %s is already acknowledged through %d; poll again",
			o.Path, entry.CursorBytes)
	}
	if o.End < o.Cursor {
		return nil, fmt.Errorf("watch: outcome for %s has end %d before cursor %d", o.Path, o.End, o.Cursor)
	}
	f, info, err := observe(o.Path, entry.FileIdentity)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info.Size() < o.End {
		return nil, fmt.Errorf("watch: %s shrank since the poll; poll again", o.Path)
	}
	buf := make([]byte, o.End-o.Cursor)
	if _, err := f.ReadAt(buf, o.Cursor); err != nil {
		return nil, fmt.Errorf("watch: read %s at %d: %w", o.Path, o.Cursor, err)
	}
	return buf, nil
}

// Acknowledge records that the hub durably committed the source's bytes
// through offset end. It advances the local checkpoint and refreshes its
// content fingerprint. Acknowledge only after a durable commit: a lost or
// older checkpoint causes safe replay, never silent data loss.
func (w *Watcher) Acknowledge(path, sourceID string, end int64) error {
	p := normalizePath(path)
	entry, ok := w.state.Sources[p]
	if !ok {
		return fmt.Errorf("watch: %s is not a watched path", p)
	}
	if entry.SourceID != sourceID {
		return fmt.Errorf("watch: %s is watched as source %s, not %s", p, entry.SourceID, sourceID)
	}
	if end < entry.CursorBytes {
		return fmt.Errorf("watch: acknowledgment at %d is before the checkpoint cursor %d for %s",
			end, entry.CursorBytes, p)
	}
	f, info, err := observe(p, entry.FileIdentity)
	if err != nil {
		return err
	}
	defer f.Close()
	if info.Size() < end {
		return fmt.Errorf("watch: %s has size %d below the acknowledged offset %d; poll again", p, info.Size(), end)
	}
	window := min(end, w.window)
	sum := ""
	if window > 0 {
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, end-window); err != nil {
			return fmt.Errorf("watch: read %s at %d: %w", p, end-window, err)
		}
		digest := sha256.Sum256(buf)
		sum = hex.EncodeToString(digest[:])
	}
	entry.CursorBytes = end
	entry.SizeBytes = info.Size()
	entry.FingerprintWindow = window
	entry.FingerprintSHA256 = sum
	w.state.Sources[p] = entry
	return saveState(w.stateDir, w.agent, w.state)
}

// observe opens path and verifies it still holds the expected incarnation.
// The caller closes the returned file. An empty identity skips the check;
// only size and fingerprint then guard the content.
func observe(path, identity string) (*os.File, fs.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("watch: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("watch: stat %s: %w", path, err)
	}
	if identity != "" && platformFileIdentity(info) != identity {
		f.Close()
		return nil, nil, fmt.Errorf("watch: %s holds a different file incarnation since the poll; poll again", path)
	}
	return f, info, nil
}

// errShortRead reports a fingerprint window that could not be read whole.
var errShortRead = errors.New("watch: short read")

// fingerprintFile returns the SHA-256 hex digest of the window bytes ending
// at cursor. The stored window never exceeds the cursor; a shrunk file
// surfaces as errShortRead.
func fingerprintFile(path string, cursor, window int64) (string, error) {
	window = min(window, cursor)
	if window <= 0 {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("watch: open %s: %w", path, err)
	}
	defer f.Close()
	buf := make([]byte, window)
	n, err := f.ReadAt(buf, cursor-window)
	if n < int(window) {
		return "", errShortRead
	}
	if err != nil {
		return "", fmt.Errorf("watch: read %s: %w", path, err)
	}
	digest := sha256.Sum256(buf)
	return hex.EncodeToString(digest[:]), nil
}

// newSourceID returns a fresh random source ID: 32 lowercase hex characters
// from crypto/rand.
func newSourceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("watch: generate source ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// normalizePath canonicalizes a watched path for use as a state key. It
// resolves symlinks in the parent directories, because temporary
// directories are symlinked on macOS (for example /tmp → /private/tmp), so
// one file keeps one key whether or not it currently exists.
func normalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	dir, base := filepath.Split(abs)
	if resolved, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Join(resolved, base)
	}
	return filepath.Clean(abs)
}
