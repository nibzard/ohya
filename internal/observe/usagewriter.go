package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	UsageMaxFileBytes int64 = 8 << 20
	UsageRotatedFiles       = 2
	UsageQueueDepth         = 256
	UsageFlushTimeout       = time.Second
	usageFileName           = "usage.jsonl"
)

type UsageWriterConfig struct {
	Dir          string
	Instance     Instance
	Clock        Clock
	MaxFileBytes int64
	RotatedFiles int
	QueueDepth   int
}

type UsageWriter struct {
	cfg     UsageWriterConfig
	queue   chan UsageRecord
	done    chan struct{}
	mu      sync.Mutex
	closed  bool
	err     error
	dropped atomic.Uint64
	failed  atomic.Uint64
}

func NewUsageWriter(cfg UsageWriterConfig) (*UsageWriter, error) {
	if cfg.Dir == "" {
		return nil, errors.New("observe: usage writer needs a directory")
	}
	if err := cfg.Instance.Validate(); err != nil {
		return nil, fmt.Errorf("observe: usage writer: %w", err)
	}
	if cfg.Clock == nil {
		return nil, errors.New("observe: usage writer needs a clock")
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = UsageMaxFileBytes
	}
	if cfg.RotatedFiles <= 0 {
		cfg.RotatedFiles = UsageRotatedFiles
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = UsageQueueDepth
	}
	if err := usageDir(cfg.Dir, cfg.Instance.InstanceID); err != nil {
		return nil, err
	}
	w := &UsageWriter{
		cfg:   cfg,
		queue: make(chan UsageRecord, cfg.QueueDepth),
		done:  make(chan struct{}),
	}
	go w.run()
	return w, nil
}

func usageDir(dir, instanceID string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("observe: create usage state directory: %w", err)
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("observe: usage state directory %s is not a directory", dir)
	}
	instance := filepath.Join(dir, instanceID)
	if err := os.Mkdir(instance, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("observe: create usage instance directory: %w", err)
		}
		if info, lerr := os.Lstat(instance); lerr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("observe: usage instance path %s exists and is not a usable directory", instance)
		}
	}
	if err := os.Chmod(instance, 0o700); err != nil {
		return fmt.Errorf("observe: restrict usage instance directory: %w", err)
	}
	return nil
}

func (w *UsageWriter) Record(r UsageRecord) bool {
	if r.TimeMS == 0 {
		r.TimeMS = w.cfg.Clock.WallMS()
	}
	if r.TimeBucket == "" {
		r.TimeBucket = HourBucket(r.TimeMS)
	}
	if r.InstanceID == "" {
		r.InstanceID = w.cfg.Instance.InstanceID
	}
	if err := r.Validate(); err != nil {
		w.dropped.Add(1)
		return false
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.dropped.Add(1)
		return false
	}
	select {
	case w.queue <- r:
		w.mu.Unlock()
		return true
	default:
		w.mu.Unlock()
		w.dropped.Add(1)
		return false
	}
}

func (w *UsageWriter) Dropped() uint64 { return w.dropped.Load() }

func (w *UsageWriter) Failed() uint64 { return w.failed.Load() }

func (w *UsageWriter) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return w.err
	}
	w.closed = true
	close(w.queue)
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-time.After(UsageFlushTimeout):
	}
	return w.err
}

func (w *UsageWriter) run() {
	defer close(w.done)
	path := filepath.Join(w.cfg.Dir, w.cfg.Instance.InstanceID, usageFileName)
	var file *os.File
	var size int64
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()
	for r := range w.queue {
		line, err := json.Marshal(r)
		if err != nil {
			w.fail(fmt.Errorf("observe: encode usage record: %w", err))
			continue
		}
		if file == nil {
			file, size, err = w.open(path)
			if err != nil {
				w.fail(err)
				continue
			}
		}
		if size > 0 && size+int64(len(line))+1 > w.cfg.MaxFileBytes {
			if err := w.rotate(file); err != nil {
				w.fail(err)
				file = nil
				continue
			}
			file, size, err = w.open(path)
			if err != nil {
				w.fail(err)
				continue
			}
		}
		n, err := file.Write(append(line, '\n'))
		size += int64(n)
		if err != nil {
			w.fail(fmt.Errorf("observe: write usage record: %w", err))
			_ = file.Close()
			file = nil
			continue
		}
	}
}

func (w *UsageWriter) open(path string) (*os.File, int64, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("observe: usage file %s is a symlink", path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("observe: open usage file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, 0, fmt.Errorf("observe: restrict usage file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, fmt.Errorf("observe: stat usage file: %w", err)
	}
	return file, info.Size(), nil
}

func (w *UsageWriter) rotate(file *os.File) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("observe: close usage file for rotation: %w", err)
	}
	dir := filepath.Join(w.cfg.Dir, w.cfg.Instance.InstanceID)
	oldest := filepath.Join(dir, usageFileName+"."+fmt.Sprint(w.cfg.RotatedFiles))
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("observe: drop oldest usage file: %w", err)
	}
	for i := w.cfg.RotatedFiles; i > 1; i-- {
		from := filepath.Join(dir, fmt.Sprintf("%s.%d", usageFileName, i-1))
		to := filepath.Join(dir, fmt.Sprintf("%s.%d", usageFileName, i))
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("observe: rotate usage files: %w", err)
		}
	}
	if err := os.Rename(filepath.Join(dir, usageFileName), filepath.Join(dir, usageFileName+".1")); err != nil {
		return fmt.Errorf("observe: rotate usage file: %w", err)
	}
	return nil
}

func (w *UsageWriter) fail(err error) {
	w.failed.Add(1)
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
}
