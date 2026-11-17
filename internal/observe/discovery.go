package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type InstanceView struct {
	InstanceID string
	Dir        string
	Snapshot   *Snapshot
	DecodeErr  string
	ModMS      int64
	HasUsage   bool
}

func DiscoverInstances(dir string) ([]InstanceView, bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("observe: read observe directory: %w", err)
	}
	views := make([]InstanceView, 0, len(entries))
	truncated := false
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !validInstanceName(name) {
			continue
		}
		view, err := LoadInstanceView(dir, name)
		if err != nil {
			return nil, false, err
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].ModMS != views[j].ModMS {
			return views[i].ModMS > views[j].ModMS
		}
		return views[i].InstanceID < views[j].InstanceID
	})
	if len(views) > MaxLiveInstances {
		views = views[:MaxLiveInstances]
		truncated = true
	}
	return views, truncated, nil
}

func LoadInstanceView(dir, instanceID string) (InstanceView, error) {
	if !validInstanceName(instanceID) {
		return InstanceView{}, fmt.Errorf("observe: instance id %q is not a valid directory name", instanceID)
	}
	instanceDir := filepath.Join(dir, instanceID)
	view := InstanceView{InstanceID: instanceID, Dir: instanceDir}
	if _, err := os.Lstat(filepath.Join(instanceDir, usageFileName)); err == nil {
		view.HasUsage = true
	}
	snapshotPath := filepath.Join(instanceDir, snapshotFileName)
	info, err := os.Lstat(snapshotPath)
	if errors.Is(err, os.ErrNotExist) {
		if dirInfo, derr := os.Lstat(instanceDir); derr == nil {
			view.ModMS = dirInfo.ModTime().UnixMilli()
		}
		return view, nil
	}
	if err != nil {
		return view, fmt.Errorf("observe: stat snapshot: %w", err)
	}
	view.ModMS = info.ModTime().UnixMilli()
	if info.Mode()&os.ModeSymlink != 0 {
		view.DecodeErr = "snapshot.json is a symlink"
		return view, nil
	}
	if info.Size() > MaxSnapshotFileBytes {
		view.DecodeErr = fmt.Sprintf("snapshot.json of %d bytes exceeds the %d byte bound", info.Size(), MaxSnapshotFileBytes)
		return view, nil
	}
	raw, err := readBounded(snapshotPath, MaxSnapshotFileBytes+1)
	if err != nil {
		view.DecodeErr = "snapshot.json cannot be read"
		return view, nil
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		view.DecodeErr = "snapshot.json is not valid JSON"
		return view, nil
	}
	if err := snapshot.Validate(); err != nil {
		view.DecodeErr = err.Error()
		return view, nil
	}
	if snapshot.Instance.InstanceID != instanceID {
		view.DecodeErr = "snapshot instance id does not match its directory"
		return view, nil
	}
	view.Snapshot = &snapshot
	return view, nil
}

func (v InstanceView) Roles() []string {
	if v.Snapshot == nil {
		return nil
	}
	return v.Snapshot.Instance.Roles
}

func (v InstanceView) HasRole(role string) bool {
	for _, r := range v.Roles() {
		if r == role {
			return true
		}
	}
	return false
}
