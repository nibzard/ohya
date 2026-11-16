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

const DefaultFingerprintWindow int64 = 4096

type Options struct {
	FingerprintWindow int64
}

type Watcher struct {
	stateDir string
	agent    string
	window   int64
	state    checkpointFile
	dirty    bool
}

type Outcome struct {
	Path             string
	SourceID         string
	Cursor           int64
	End              int64
	Fresh            bool
	Reason           string
	ReplacedSourceID string
	ReplacedCursor   int64
	RenamedFrom      string
	Missing          bool
}

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

type probe struct {
	identity string
	size     int64
}

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

	byIdentity := make(map[string]string, len(probes))
	for _, p := range slices.Sorted(maps.Keys(probes)) {
		if id := probes[p].identity; id != "" {
			if _, taken := byIdentity[id]; !taken {
				byIdentity[id] = p
			}
		}
	}

	var outcomes []Outcome
	changed := false

	renamedFrom := map[string]string{}
	moves := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(w.state.Sources)) {
		entry := w.state.Sources[key]
		if entry.FileIdentity == "" {
			continue
		}
		if pr, ok := probes[key]; ok && pr.identity == entry.FileIdentity {
			continue
		}
		if newPath, ok := byIdentity[entry.FileIdentity]; ok && newPath != key {
			moves[key] = newPath
		}
	}
	if len(moves) > 0 {
		for {
			claims := make(map[string]int, len(w.state.Sources))
			for key := range w.state.Sources {
				if target, ok := moves[key]; ok {
					claims[target]++
				} else {
					claims[key]++
				}
			}
			refused := false
			for key, target := range moves {
				if claims[target] > 1 {
					delete(moves, key)
					refused = true
				}
			}
			if !refused {
				break
			}
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
		outcome.ReplacedSourceID = entry.SourceID
		outcome.ReplacedCursor = entry.CursorBytes
		outcomes = append(outcomes, outcome)
		changed = true
	}

	claimed := make(map[string]bool, len(w.state.Sources))
	for key, entry := range w.state.Sources {
		if pr, ok := probes[key]; ok && pr.identity != "" && pr.identity == entry.FileIdentity {
			claimed[pr.identity] = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(probes)) {
		if _, tracked := w.state.Sources[key]; tracked {
			continue
		}
		id := probes[key].identity
		if id != "" && claimed[id] {
			continue
		}
		outcome, err := w.register(key, probes[key], "new file")
		if err != nil {
			return nil, err
		}
		if id != "" {
			claimed[id] = true
		}
		outcomes = append(outcomes, outcome)
		changed = true
	}

	if changed {
		w.dirty = true
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	slices.SortFunc(outcomes, func(a, b Outcome) int { return strings.Compare(a.Path, b.Path) })
	return outcomes, nil
}

func (w *Watcher) Flush() error {
	if !w.dirty {
		return nil
	}
	if err := saveState(w.stateDir, w.agent, w.state); err != nil {
		return err
	}
	w.dirty = false
	return nil
}

func (w *Watcher) verifyFingerprint(path string, entry sourceCheckpoint) string {
	if entry.CursorBytes == 0 {
		return ""
	}
	sum, err := fingerprintFile(path, entry.CursorBytes, entry.FingerprintWindow)
	switch {
	case err == nil && sum != entry.FingerprintSHA256:
		return "content before the acknowledged cursor changed: the file was rewritten in place"
	case errors.Is(err, errShortRead):
		return "the acknowledged prefix became unreadable during verification: the file shrank"
	default:
		return ""
	}
}

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

func (w *Watcher) Acknowledge(path, sourceID string, end int64) error {
	p, entry, err := w.entry(path, sourceID)
	if err != nil {
		return err
	}
	if end < entry.CursorBytes {
		return fmt.Errorf("watch: acknowledgment at %d is before the checkpoint cursor %d for %s",
			end, entry.CursorBytes, p)
	}
	if end == entry.CursorBytes {
		return nil
	}
	return w.checkpoint(p, entry, end)
}

func (w *Watcher) Rewind(path, sourceID string, cursor int64) error {
	p, entry, err := w.entry(path, sourceID)
	if err != nil {
		return err
	}
	if cursor < 0 || cursor > entry.CursorBytes {
		return fmt.Errorf("watch: rewind to %d is outside the acknowledged range [0, %d] of %s",
			cursor, entry.CursorBytes, p)
	}
	if entry.CursorBytes > 0 {
		sum, err := fingerprintFile(p, entry.CursorBytes, entry.FingerprintWindow)
		if err != nil || sum != entry.FingerprintSHA256 {
			return fmt.Errorf("watch: %s no longer holds the acknowledged bytes; they cannot be replayed", p)
		}
	}
	if cursor == entry.CursorBytes {
		return nil
	}
	return w.checkpoint(p, entry, cursor)
}

func (w *Watcher) entry(path, sourceID string) (string, sourceCheckpoint, error) {
	p := normalizePath(path)
	entry, ok := w.state.Sources[p]
	if !ok {
		return p, entry, fmt.Errorf("watch: %s is not a watched path", p)
	}
	if entry.SourceID != sourceID {
		return p, entry, fmt.Errorf("watch: %s is watched as source %s, not %s", p, entry.SourceID, sourceID)
	}
	return p, entry, nil
}

func (w *Watcher) checkpoint(p string, entry sourceCheckpoint, end int64) error {
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
	w.dirty = true
	return nil
}

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

var errShortRead = errors.New("watch: short read")

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

func newSourceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("watch: generate source ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

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
