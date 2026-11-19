package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	LogMaxFileBytes         int64 = 8 << 20
	LogRotatedFiles               = 2
	LogQueueDepth                 = 256
	LogFlushTimeout               = time.Second
	LogLineMaxBytes               = 8 << 10
	LogStderrNoticeInterval       = time.Minute
	LogSummaryInterval            = time.Minute
	LogHeartbeatInterval          = 5 * time.Minute

	logsFileName = "events.jsonl"
)

var summaryCounters = []string{
	"discovery_scans",
	"ingest_batches{batch_result=committed}",
	"ingest_events{event_result=inserted}",
	"ingest_events{event_result=duplicate}",
	"upload_batches{batch_result=successful}",
	"upload_ack_bytes",
	"hub_connections{conn_result=admitted}",
	"sql_requests{query_class=sql,query_outcome=ok}",
	"sql_requests{query_class=annotate,query_outcome=ok}",
}

type EventSink interface {
	Event(component string, level Level, code EventCode, outcome string, counts map[string]int64) bool
}

type LogWriterConfig struct {
	Dir               string
	Instance          Instance
	Clock             Clock
	Recorder          *Recorder
	MaxFileBytes      int64
	RotatedFiles      int
	QueueDepth        int
	Stderr            io.Writer
	SummaryInterval   time.Duration
	HeartbeatInterval time.Duration

	writeLine func(w *LogWriter, entry LogEntry, line []byte) error
}

type LogWriter struct {
	cfg          LogWriterConfig
	dir          string
	queue        chan LogEntry
	stop         chan struct{}
	done         chan struct{}
	closeOnce    sync.Once
	mu           sync.Mutex
	closed       bool
	pending      map[string]LogEntry
	lastStderrMS int64
	everNoticed  bool
	droppedSince uint64
	lastEntryMS  int64
	lastSummary  map[string]uint64
	err          error
	dropped      atomic.Uint64
	coalesced    atomic.Uint64
	writeLine    func(w *LogWriter, entry LogEntry, line []byte) error
}

func NewLogWriter(cfg LogWriterConfig) (*LogWriter, error) {
	if cfg.Dir == "" {
		return nil, errors.New("observe: log writer needs a directory")
	}
	if err := cfg.Instance.Validate(); err != nil {
		return nil, fmt.Errorf("observe: log writer: %w", err)
	}
	if cfg.Clock == nil {
		return nil, errors.New("observe: log writer needs a clock")
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = LogMaxFileBytes
	}
	if cfg.RotatedFiles <= 0 {
		cfg.RotatedFiles = LogRotatedFiles
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = LogQueueDepth
	}
	if cfg.SummaryInterval <= 0 {
		cfg.SummaryInterval = LogSummaryInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = LogHeartbeatInterval
	}
	dir, err := EnsureInstanceDir(cfg.Dir, cfg.Instance.InstanceID)
	if err != nil {
		return nil, err
	}
	w := &LogWriter{
		cfg:     cfg,
		dir:     dir,
		queue:   make(chan LogEntry, cfg.QueueDepth),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		pending: make(map[string]LogEntry),
	}
	w.lastSummary = make(map[string]uint64, len(summaryCounters))
	w.writeLine = cfg.writeLine
	if w.writeLine == nil {
		w.writeLine = writeLogFiles
	}
	go w.run()
	return w, nil
}

func (w *LogWriter) Dir() string { return w.dir }

func (w *LogWriter) Event(component string, level Level, code EventCode, outcome string, counts map[string]int64) bool {
	entry := LogEntry{
		FormatVersion: LogFormatVersion,
		TimeMS:        w.cfg.Clock.WallMS(),
		InstanceID:    w.cfg.Instance.InstanceID,
		Component:     component,
		Level:         level,
		EventCode:     string(code),
		Outcome:       outcome,
	}
	if counts != nil {
		entry.Counts = make(map[string]int64, len(counts))
		for name, value := range counts {
			entry.Counts[name] = value
		}
	}
	if err := entry.Validate(); err != nil {
		w.drop()
		return false
	}
	key := coalesceKey(entry)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.drop()
		return false
	}
	if key != "" {
		if held, ok := w.pending[key]; ok {
			if held.Counts == nil {
				held.Counts = map[string]int64{}
			}
			held.Counts["coalesced"]++
			w.pending[key] = held
			w.mu.Unlock()
			w.coalesced.Add(1)
			if w.cfg.Recorder != nil {
				w.cfg.Recorder.Add("instrumentation_log_coalesced", 1)
			}
			return true
		}
	}
	select {
	case w.queue <- entry:
		if key != "" {
			w.pending[key] = entry
		}
		w.mu.Unlock()
		if w.cfg.Recorder != nil {
			w.cfg.Recorder.Set("instrumentation_queue_depth", float64(len(w.queue)))
		}
		return true
	default:
		w.mu.Unlock()
		w.drop()
		return false
	}
}

func coalesceKey(entry LogEntry) string {
	if entry.Level != LevelWarn && entry.Level != LevelError {
		return ""
	}
	return entry.Component + "\x00" + string(entry.Level) + "\x00" + entry.EventCode
}

func (w *LogWriter) drop() {
	w.dropped.Add(1)
	w.mu.Lock()
	w.droppedSince++
	w.mu.Unlock()
	if w.cfg.Recorder != nil {
		w.cfg.Recorder.Add("instrumentation_log_dropped", 1)
	}
}

func (w *LogWriter) Dropped() uint64   { return w.dropped.Load() }
func (w *LogWriter) Coalesced() uint64 { return w.coalesced.Load() }

func (w *LogWriter) Close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		close(w.stop)
		w.mu.Unlock()
		select {
		case <-w.done:
			w.mu.Lock()
			w.closed = true
			w.mu.Unlock()
			for {
				select {
				case <-w.queue:
					w.drop()
				default:
					return
				}
			}
		case <-time.After(LogFlushTimeout):
			w.mu.Lock()
			w.closed = true
			w.mu.Unlock()
		}
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *LogWriter) run() {
	defer close(w.done)
	summary := time.NewTicker(w.cfg.SummaryInterval)
	defer summary.Stop()
	for {
		select {
		case <-w.stop:
			w.emitSummary(true)
			w.drain()
			w.mu.Lock()
			w.closed = true
			w.mu.Unlock()
			return
		case entry := <-w.queue:
			entry = w.claim(entry)
			w.write(entry)
		case <-summary.C:
			w.emitSummary(false)
		}
	}
}

func (w *LogWriter) drain() {
	deadline := time.Now().Add(LogFlushTimeout)
	for {
		select {
		case entry := <-w.queue:
			entry = w.claim(entry)
			w.write(entry)
		default:
			return
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

func (w *LogWriter) claim(entry LogEntry) LogEntry {
	key := coalesceKey(entry)
	w.mu.Lock()
	if key != "" {
		if held, ok := w.pending[key]; ok {
			entry = held
			delete(w.pending, key)
		}
	}
	w.mu.Unlock()
	if w.cfg.Recorder != nil {
		w.cfg.Recorder.Set("instrumentation_queue_depth", float64(len(w.queue)))
	}
	return entry
}

func (w *LogWriter) write(entry LogEntry) {
	line, err := json.Marshal(entry)
	if err == nil && len(line)+1 > LogLineMaxBytes {
		w.drop()
		err = fmt.Errorf("observe: log line of %d bytes exceeds the %d byte bound", len(line)+1, LogLineMaxBytes)
	}
	if err == nil {
		err = w.writeLine(w, entry, line)
	}
	if err != nil {
		w.recordFailure(err)
		return
	}
	w.mu.Lock()
	w.lastEntryMS = entry.TimeMS
	w.mu.Unlock()
}

func (w *LogWriter) recordFailure(err error) {
	if w.cfg.Recorder != nil {
		w.cfg.Recorder.Add("instrumentation_write_errors", 1, "log")
	}
	w.mu.Lock()
	first := w.err == nil
	if first {
		w.err = err
	}
	now := w.cfg.Clock.WallMS()
	notify := !w.everNoticed || now-w.lastStderrMS >= LogStderrNoticeInterval.Milliseconds()
	var dropped uint64
	if notify && w.cfg.Stderr != nil {
		w.everNoticed = true
		w.lastStderrMS = now
		dropped = w.droppedSince
		w.droppedSince = 0
	}
	w.mu.Unlock()
	if notify && w.cfg.Stderr != nil {
		fmt.Fprintf(w.cfg.Stderr, "ohya: application log write failed; %d entries dropped; telemetry stays in memory\n", dropped)
	}
}

func (w *LogWriter) emitSummary(final bool) {
	if w.cfg.Recorder == nil {
		return
	}
	snapshot := w.cfg.Recorder.Snapshot()
	counts := make(map[string]int64, len(summaryCounters))
	active := false
	for _, key := range summaryCounters {
		delta := int64(snapshot.Counters[key]) - int64(w.lastSummary[key])
		if delta > 0 {
			counts[key] = delta
			active = true
		}
		w.lastSummary[key] = snapshot.Counters[key]
	}
	w.mu.Lock()
	now := w.cfg.Clock.WallMS()
	lastEntry := w.lastEntryMS
	w.mu.Unlock()
	switch {
	case active:
		w.Event("observe", LevelInfo, EventSummary, "", counts)
	case final:
		w.Event("observe", LevelInfo, EventSummary, "final", nil)
	case lastEntry > 0 && now-lastEntry >= w.cfg.HeartbeatInterval.Milliseconds():
		w.Event("observe", LevelInfo, EventHeartbeat, "", nil)
	}
}

func writeLogFiles(w *LogWriter, entry LogEntry, line []byte) error {
	path := filepath.Join(w.dir, logsFileName)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("observe: log file %s is a symlink", path)
		}
		if info.Mode().IsRegular() && info.Size() > 0 && info.Size()+int64(len(line))+1 > w.cfg.MaxFileBytes {
			if err := w.rotate(); err != nil {
				return err
			}
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("observe: open log file: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("observe: restrict log file: %w", err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("observe: write log entry: %w", err)
	}
	return nil
}

func (w *LogWriter) rotate() error {
	oldest := filepath.Join(w.dir, logsFileName+"."+fmt.Sprint(w.cfg.RotatedFiles))
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("observe: drop oldest log file: %w", err)
	}
	for i := w.cfg.RotatedFiles; i > 1; i-- {
		from := filepath.Join(w.dir, fmt.Sprintf("%s.%d", logsFileName, i-1))
		to := filepath.Join(w.dir, fmt.Sprintf("%s.%d", logsFileName, i))
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("observe: rotate log files: %w", err)
		}
	}
	if err := os.Rename(filepath.Join(w.dir, logsFileName), filepath.Join(w.dir, logsFileName+".1")); err != nil {
		return fmt.Errorf("observe: rotate log file: %w", err)
	}
	return nil
}
