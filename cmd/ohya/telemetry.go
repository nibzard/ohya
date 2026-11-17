package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
)

type telemetry struct {
	recorder  *observe.Recorder
	publisher *observe.Publisher
	usage     *observe.UsageWriter
}

func startTelemetry(stateDir string, observeOn bool, sampleFiles map[string]string, roles ...observe.Role) *telemetry {
	_, revision, dirty, _ := buildIdentity()
	clock := observe.NewSystemClock()
	instance, err := observe.NewInstance(stateDir, revision, dirty == "true", clock, roles...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ohya: observability disabled: %v\n", err)
		return nil
	}
	t := &telemetry{}
	if usageEligible(roles...) {
		if usage, err := observe.NewUsageWriter(observe.UsageWriterConfig{
			Dir:      filepath.Join(stateDir, "observe"),
			Instance: instance,
			Clock:    clock,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "ohya: usage records disabled: %v\n", err)
		} else {
			t.usage = usage
		}
	}
	if !observeOn {
		return t
	}
	recorder, err := observe.NewRecorder(instance, clock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ohya: observability disabled: %v\n", err)
		return t
	}
	publisher, err := observe.NewPublisher(observe.PublisherConfig{
		Dir:         filepath.Join(stateDir, "observe"),
		Recorder:    recorder,
		Clock:       clock,
		SampleFiles: sampleFiles,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ohya: observability disabled: %v\n", err)
		return t
	}
	t.recorder = recorder
	t.publisher = publisher
	return t
}

func (t *telemetry) observer() observe.Observer {
	if t == nil || t.recorder == nil {
		return observe.Disabled
	}
	return t.recorder
}

func (t *telemetry) usageSink() mcpserver.UsageSink {
	if t == nil || t.usage == nil {
		return nil
	}
	return t.usage
}

func (t *telemetry) close() {
	if t == nil {
		return
	}
	if t.publisher != nil {
		if err := t.publisher.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "ohya: observability write failure: %v\n", err)
		}
	}
	if t.usage != nil {
		if err := t.usage.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "ohya: usage record write failure: %v\n", err)
		}
	}
}

func usageEligible(roles ...observe.Role) bool {
	for _, role := range roles {
		if role == observe.RoleServe || role == observe.RoleMCP {
			return true
		}
	}
	return false
}

func sqliteSampleFiles(dbPath string) map[string]string {
	return map[string]string{
		"db":  dbPath,
		"wal": dbPath + "-wal",
		"shm": dbPath + "-shm",
	}
}

func parseObserveFlag(value string) (bool, error) {
	switch value {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("--observe must be on or off, got %q", value)
	}
}
