package observe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeViewSnapshot(t *testing.T, observeDir, id string, mutate func(*Snapshot)) {
	t.Helper()
	instanceDir := filepath.Join(observeDir, id)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{
		FormatVersion: SnapshotFormatVersion,
		Instance:      Instance{InstanceID: id, PID: os.Getpid(), Roles: []string{"serve"}, StartWallMS: time.Now().UnixMilli()},
		UptimeMS:      1,
		SampledWallMS: time.Now().UnixMilli(),
		Counters:      map[string]uint64{},
	}
	if mutate != nil {
		mutate(&snapshot)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, snapshotFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverInstancesOrdersNewestFirstAndReportsUsage(t *testing.T) {
	observeDir := filepath.Join(t.TempDir(), "observe")
	writeViewSnapshot(t, observeDir, instanceName(1), nil)
	writeViewSnapshot(t, observeDir, instanceName(2), func(s *Snapshot) { s.Counters["discovery_scans"] = 1 })
	if err := os.Chtimes(filepath.Join(observeDir, instanceName(2), snapshotFileName), time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(observeDir, instanceName(2), usageFileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	views, truncated, err := DiscoverInstances(observeDir)
	if err != nil {
		t.Fatalf("DiscoverInstances: %v", err)
	}
	if truncated {
		t.Error("two instances must not report truncation")
	}
	if len(views) != 2 {
		t.Fatalf("instances = %d, want 2", len(views))
	}
	if views[0].InstanceID != instanceName(2) || !views[0].HasUsage {
		t.Errorf("newest instance must sort first with usage noted: %+v", views[0])
	}
	if views[1].HasUsage {
		t.Errorf("usage flag must follow the files present: %+v", views[1])
	}
}

func instanceName(n int) string { return fmt.Sprintf("%032d", n) }

func TestDiscoverInstancesReportsBrokenSnapshots(t *testing.T) {
	observeDir := filepath.Join(t.TempDir(), "observe")
	writeViewSnapshot(t, observeDir, instanceName(1), func(s *Snapshot) { s.FormatVersion = 99 })
	writeViewSnapshot(t, observeDir, instanceName(2), func(s *Snapshot) { s.Instance.InstanceID = instanceName(9) })
	corruptDir := filepath.Join(observeDir, instanceName(3))
	if err := os.MkdirAll(corruptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, snapshotFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkDir := filepath.Join(observeDir, instanceName(4))
	if err := os.MkdirAll(symlinkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), snapshotFileName), filepath.Join(symlinkDir, snapshotFileName)); err != nil {
		t.Fatal(err)
	}
	emptyDir := filepath.Join(observeDir, instanceName(5))
	if err := os.MkdirAll(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	views, _, err := DiscoverInstances(observeDir)
	if err != nil {
		t.Fatalf("DiscoverInstances: %v", err)
	}
	byID := map[string]InstanceView{}
	for _, view := range views {
		byID[view.InstanceID] = view
	}
	if byID[instanceName(1)].Snapshot != nil || byID[instanceName(1)].DecodeErr == "" {
		t.Errorf("unsupported version must surface a decode error: %+v", byID[instanceName(1)])
	}
	if byID[instanceName(3)].DecodeErr == "" {
		t.Errorf("corrupt JSON must surface a decode error: %+v", byID[instanceName(3)])
	}
	if byID[instanceName(4)].DecodeErr == "" {
		t.Errorf("symlinked snapshot must surface a decode error: %+v", byID[instanceName(4)])
	}
	if byID[instanceName(2)].Snapshot != nil || byID[instanceName(2)].DecodeErr == "" {
		t.Errorf("instance id mismatch must surface a decode error: %+v", byID[instanceName(2)])
	}
	if view, ok := byID[instanceName(5)]; !ok || view.Snapshot != nil || view.DecodeErr != "" {
		t.Errorf("a snapshotless instance must be reported without error: %+v", view)
	}
}

func TestDiscoverInstancesSkipsForeignEntries(t *testing.T) {
	observeDir := filepath.Join(t.TempDir(), "observe")
	if err := os.MkdirAll(observeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(observeDir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(observeDir, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	views, truncated, err := DiscoverInstances(observeDir)
	if err != nil {
		t.Fatalf("DiscoverInstances: %v", err)
	}
	if len(views) != 0 || truncated {
		t.Errorf("foreign entries must be ignored: %v %t", views, truncated)
	}
}

func TestDiscoverInstancesBoundsLiveInstances(t *testing.T) {
	observeDir := filepath.Join(t.TempDir(), "observe")
	for i := 0; i < MaxLiveInstances+6; i++ {
		writeViewSnapshot(t, observeDir, instanceName(i), nil)
	}
	views, truncated, err := DiscoverInstances(observeDir)
	if err != nil {
		t.Fatalf("DiscoverInstances: %v", err)
	}
	if !truncated {
		t.Error("more instances than the bound must report truncation")
	}
	if len(views) != MaxLiveInstances {
		t.Errorf("instances = %d, want %d", len(views), MaxLiveInstances)
	}
}

func TestDiscoverMissingDirectoryIsEmpty(t *testing.T) {
	views, truncated, err := DiscoverInstances(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("missing observe directory must not be an error: %v", err)
	}
	if len(views) != 0 || truncated {
		t.Errorf("missing observe directory must be empty: %v %t", views, truncated)
	}
}

func TestLoadInstanceViewRejectsInvalidName(t *testing.T) {
	if _, err := LoadInstanceView(t.TempDir(), "../escape"); err == nil {
		t.Error("a traversal-style name must be rejected")
	}
}
