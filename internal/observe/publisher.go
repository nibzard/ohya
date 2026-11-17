package observe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SnapshotInterval     = 10 * time.Second
	HistoryInterval      = time.Minute
	MaxSnapshotFileBytes = 256 << 10
	MaxHistoryFileBytes  = 16 << 20
	MaxHistorySampleAge  = 24 * time.Hour
	RetainedInstances    = 8
	RetentionAge         = 7 * 24 * time.Hour
	MaxLiveInstances     = 64
	SnapshotStaleAge     = 30 * time.Second

	snapshotFileName = "snapshot.json"
	historyFileName  = "history.jsonl"
)

type PublisherConfig struct {
	Dir              string
	Recorder         *Recorder
	Clock            Clock
	Interval         time.Duration
	HistoryInterval  time.Duration
	MaxSnapshotBytes int64
	MaxHistoryBytes  int64
	MaxHistoryAge    time.Duration
	SampleFiles      map[string]string

	snapshotWriter func(path string, data []byte) error
}

type Publisher struct {
	cfg               PublisherConfig
	dir               string
	stop              chan struct{}
	done              chan struct{}
	closeOnce         sync.Once
	mu                sync.Mutex
	writeErrs         map[string]uint64
	lastErr           error
	lastHistoryWallMS int64
	writeSnapshot     func(path string, data []byte) error
	appendHistory     func(path string, data []byte) error
}

func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if cfg.Dir == "" {
		return nil, errors.New("observe: publisher needs a directory")
	}
	if cfg.Recorder == nil {
		return nil, errors.New("observe: publisher needs a recorder")
	}
	if cfg.Clock == nil {
		return nil, errors.New("observe: publisher needs a clock")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = SnapshotInterval
	}
	if cfg.HistoryInterval <= 0 {
		cfg.HistoryInterval = HistoryInterval
	}
	if cfg.MaxSnapshotBytes <= 0 {
		cfg.MaxSnapshotBytes = MaxSnapshotFileBytes
	}
	if cfg.MaxHistoryBytes <= 0 {
		cfg.MaxHistoryBytes = MaxHistoryFileBytes
	}
	if cfg.MaxHistoryAge <= 0 {
		cfg.MaxHistoryAge = MaxHistorySampleAge
	}
	instance := cfg.Recorder.Instance()
	dir, err := EnsureInstanceDir(cfg.Dir, instance.InstanceID)
	if err != nil {
		return nil, err
	}
	writeSnapshot := cfg.snapshotWriter
	if writeSnapshot == nil {
		writeSnapshot = writeInstanceFile
	}
	p := &Publisher{
		cfg:           cfg,
		dir:           dir,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		writeErrs:     make(map[string]uint64),
		writeSnapshot: writeSnapshot,
		appendHistory: appendInstanceFile,
	}
	if err := CleanStaleInstances(cfg.Dir, cfg.Clock); err != nil {
		p.recordWriteError("other", err)
	}
	p.publish()
	p.recordHistory()
	go p.run()
	return p, nil
}

func (p *Publisher) Dir() string { return p.dir }

func (p *Publisher) run() {
	defer close(p.done)
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

func (p *Publisher) tick() {
	SampleRuntime(p.cfg.Recorder)
	p.sampleFiles()
	p.publish()
	p.recordHistory()
}

func (p *Publisher) Close() error {
	p.closeOnce.Do(func() {
		close(p.stop)
		<-p.done
		p.publish()
		p.recordHistory()
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

func (p *Publisher) WriteErrors() map[string]uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]uint64, len(p.writeErrs))
	for target, count := range p.writeErrs {
		out[target] = count
	}
	return out
}

func (p *Publisher) publish() {
	start := time.Now()
	snapshot := p.cfg.Recorder.Snapshot()
	data, err := json.Marshal(snapshot)
	if err == nil && int64(len(data)) > p.cfg.MaxSnapshotBytes {
		reduced := snapshot
		reduced.Gauges = nil
		reduced.Durations = nil
		reduced.Resources = nil
		reduced.Truncated = true
		data, err = json.Marshal(reduced)
	}
	if err == nil && int64(len(data)) > p.cfg.MaxSnapshotBytes {
		err = fmt.Errorf("observe: snapshot of %d bytes exceeds the %d byte bound", len(data), p.cfg.MaxSnapshotBytes)
	}
	if err == nil {
		err = p.writeSnapshot(filepath.Join(p.dir, snapshotFileName), data)
	}
	p.cfg.Recorder.Observe("instrumentation_publish_duration", time.Since(start))
	if err != nil {
		p.recordWriteError("snapshot", err)
		return
	}
	p.cfg.Recorder.Set("instrumentation_last_publish_ms", float64(p.cfg.Clock.Monotonic().Milliseconds()))
}

func (p *Publisher) recordHistory() {
	now := p.cfg.Clock.WallMS()
	interval := p.cfg.HistoryInterval.Milliseconds()
	p.mu.Lock()
	last := p.lastHistoryWallMS
	p.mu.Unlock()
	if last != 0 && now-last < interval {
		return
	}
	snapshot := p.cfg.Recorder.Snapshot()
	data, err := json.Marshal(snapshot)
	if err == nil {
		err = p.appendHistory(filepath.Join(p.dir, historyFileName), append(data, '\n'))
	}
	if err != nil {
		p.recordWriteError("history", err)
		return
	}
	p.mu.Lock()
	p.lastHistoryWallMS = now
	p.mu.Unlock()
	p.pruneHistory(now)
}

func (p *Publisher) pruneHistory(nowMS int64) {
	path := filepath.Join(p.dir, historyFileName)
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.recordWriteError("history", fmt.Errorf("observe: stat history: %w", err))
		}
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		p.recordWriteError("history", fmt.Errorf("observe: history file %s is a symlink", path))
		return
	}
	raw, err := readBounded(path, p.cfg.MaxHistoryBytes+MaxSnapshotFileBytes+1)
	if err != nil {
		p.recordWriteError("history", fmt.Errorf("observe: read history: %w", err))
		return
	}
	cutoffAge := nowMS - p.cfg.MaxHistoryAge.Milliseconds()
	var kept [][]byte
	var keptBytes int64
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if len(line) == 0 {
			continue
		}
		var head struct {
			SampledWallMS int64 `json:"sampled_wall_ms"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			head.SampledWallMS = 0
		}
		if head.SampledWallMS > 0 && head.SampledWallMS < cutoffAge {
			continue
		}
		if keptBytes+int64(len(line))+1 > p.cfg.MaxHistoryBytes {
			break
		}
		kept = append(kept, line)
		keptBytes += int64(len(line)) + 1
	}
	if keptBytes == info.Size() {
		return
	}
	var out bytes.Buffer
	for i := len(kept) - 1; i >= 0; i-- {
		out.Write(kept[i])
		out.WriteByte('\n')
	}
	if err := writeInstanceFile(path, out.Bytes()); err != nil {
		p.recordWriteError("history", err)
	}
}

func (p *Publisher) sampleFiles() {
	for label, path := range p.cfg.SampleFiles {
		info, err := os.Stat(path)
		switch {
		case err != nil:
			continue
		case !info.Mode().IsRegular():
			continue
		default:
			p.cfg.Recorder.Set("sqlite_file_bytes", float64(info.Size()), label)
		}
	}
}

func (p *Publisher) recordWriteError(target string, err error) {
	p.cfg.Recorder.Add("instrumentation_write_errors", 1, target)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeErrs[target]++
	if p.lastErr == nil {
		p.lastErr = err
	}
}

func writeInstanceFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+strings.TrimSuffix(base, filepath.Ext(base))+"-*")
	if err != nil {
		return fmt.Errorf("observe: create temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("observe: write temporary file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("observe: restrict temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("observe: close temporary file: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(name)
		return fmt.Errorf("observe: instance file %s is a symlink", path)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("observe: replace instance file: %w", err)
	}
	return nil
}

func appendInstanceFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("observe: instance file %s is a symlink", path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("observe: open history file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("observe: write history file: %w", err)
	}
	return nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}

type staleCandidate struct {
	name       string
	path       string
	modMS      int64
	role       string
	terminated bool
	live       bool
}

func CleanStaleInstances(dir string, clock Clock) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("observe: read observe directory: %w", err)
	}
	nowMS := clock.WallMS()
	var candidates []staleCandidate
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !validInstanceName(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		candidate := staleCandidate{name: name, path: filepath.Join(dir, name), modMS: info.ModTime().UnixMilli()}
		if raw, err := readBounded(filepath.Join(dir, name, snapshotFileName), MaxSnapshotFileBytes); err == nil {
			var snapshot Snapshot
			if json.Unmarshal(raw, &snapshot) == nil && snapshot.Validate() == nil {
				candidate.role = snapshot.Instance.firstRole()
				switch ProbePID(snapshot.Instance.PID) {
				case "dead":
					candidate.terminated = true
				case "running":
					candidate.live = true
				}
			}
		}
		candidates = append(candidates, candidate)
	}
	var cleanupErr error
	byRole := make(map[string][]staleCandidate)
	for _, candidate := range candidates {
		switch {
		case candidate.live:
			continue
		case nowMS-candidate.modMS > RetentionAge.Milliseconds():
			cleanupErr = errors.Join(cleanupErr, removeInstanceDir(candidate.path))
		case candidate.terminated:
			byRole[candidate.role] = append(byRole[candidate.role], candidate)
		}
	}
	for _, group := range byRole {
		if len(group) <= RetainedInstances {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].modMS > group[j].modMS })
		for _, candidate := range group[RetainedInstances:] {
			cleanupErr = errors.Join(cleanupErr, removeInstanceDir(candidate.path))
		}
	}
	return cleanupErr
}

func removeInstanceDir(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("observe: remove stale instance %s: %w", path, err)
	}
	return nil
}

func (i Instance) firstRole() string {
	if len(i.Roles) == 0 {
		return "unknown"
	}
	return i.Roles[0]
}
