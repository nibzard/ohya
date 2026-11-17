package observe

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

const (
	SnapshotFormatVersion = 1
	LogFormatVersion      = 1
	UsageFormatVersion    = 1
)

const (
	ResourceHeapAllocBytes = "heap_alloc_bytes"
	ResourceHeapInuseBytes = "heap_inuse_bytes"
	ResourceHeapSysBytes   = "heap_sys_bytes"
	ResourceGoroutines     = "goroutines"
	ResourceGCCount        = "gc_count"
	ResourceGCCPausMS      = "gc_pause_ms"
	ResourceCPUSeconds     = "cpu_seconds"
	ResourceRSSBytes       = "rss_bytes"
)

type Role string

const (
	RoleServe  Role = "serve"
	RoleJoin   Role = "join"
	RoleIngest Role = "ingest"
	RoleMCP    Role = "mcp"
)

var roleSet = map[Role]bool{
	RoleServe:  true,
	RoleJoin:   true,
	RoleIngest: true,
	RoleMCP:    true,
}

func ParseRole(s string) (Role, bool) {
	r := Role(s)
	return r, roleSet[r]
}

type Instance struct {
	InstanceID    string   `json:"instance_id"`
	PID           int      `json:"pid"`
	Roles         []string `json:"roles"`
	StartWallMS   int64    `json:"start_wall_ms"`
	BuildRevision string   `json:"build_revision,omitempty"`
	BuildDirty    bool     `json:"build_dirty,omitempty"`
	Scope         string   `json:"scope,omitempty"`
}

func NewInstanceID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("observe: generate instance id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func NewInstance(scope string, revision string, dirty bool, clock Clock, roles ...Role) (Instance, error) {
	if clock == nil {
		return Instance{}, errors.New("observe: instance needs a clock")
	}
	if len(roles) == 0 {
		return Instance{}, errors.New("observe: instance needs at least one role")
	}
	known := make([]string, 0, len(roles))
	for _, r := range roles {
		if !roleSet[r] {
			return Instance{}, fmt.Errorf("observe: unknown role %q", string(r))
		}
		known = append(known, string(r))
	}
	id, err := NewInstanceID()
	if err != nil {
		return Instance{}, err
	}
	return Instance{
		InstanceID:    id,
		PID:           os.Getpid(),
		Roles:         known,
		StartWallMS:   clock.WallMS(),
		BuildRevision: revision,
		BuildDirty:    dirty,
		Scope:         scope,
	}, nil
}

func (i Instance) Validate() error {
	if len(i.InstanceID) != 32 {
		return errors.New("observe: instance id must be 32 hex characters")
	}
	for _, c := range i.InstanceID {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return errors.New("observe: instance id must be lowercase hex")
		}
	}
	if i.PID <= 0 {
		return errors.New("observe: instance needs a process id")
	}
	if len(i.Roles) == 0 {
		return errors.New("observe: instance needs at least one role")
	}
	for _, r := range i.Roles {
		if !roleSet[Role(r)] {
			return fmt.Errorf("observe: unknown role %q", r)
		}
	}
	if i.StartWallMS <= 0 {
		return errors.New("observe: instance needs a process start timestamp")
	}
	return nil
}
