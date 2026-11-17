package observe

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestProbePIDReportsSelfAndDeadProcesses(t *testing.T) {
	if got := ProbePID(os.Getpid()); got != "running" {
		t.Errorf("own pid probes as %s, want running", got)
	}
	if got := ProbePID(0); got != "unknown" {
		t.Errorf("pid 0 probes as %s, want unknown", got)
	}
	cmd := exec.Command("sleep", "0.02")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a child process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for the child: %v", err)
	}
	if got := ProbePID(pid); got != "dead" {
		t.Errorf("exited pid probes as %s, want dead", got)
	}
}

func TestSampleRuntimeFillsSupportedResources(t *testing.T) {
	recorder := testRecorder(t)
	SampleRuntime(recorder)
	snapshot := recorder.Snapshot()
	heap := snapshot.Resources[ResourceHeapAllocBytes]
	if heap.Value == nil || *heap.Value <= 0 {
		t.Errorf("heap alloc must be a supported reading: %+v", heap)
	}
	goroutines := snapshot.Resources[ResourceGoroutines]
	if goroutines.Value == nil || *goroutines.Value < 1 {
		t.Errorf("goroutines must be a supported reading: %+v", goroutines)
	}
	if _, ok := snapshot.Durations["runtime_sample_duration"]; !ok {
		t.Error("runtime sampling must observe its own duration")
	}
}

func TestSampleRuntimeKeepsUnsupportedDistinctFromZero(t *testing.T) {
	recorder := testRecorder(t)
	SampleRuntime(recorder)
	snapshot := recorder.Snapshot()
	for _, name := range []string{ResourceCPUSeconds, ResourceRSSBytes} {
		reading := snapshot.Resources[name]
		if reading.Value == nil && reading.Reason == "" {
			t.Errorf("resource %s must be either supported or carry a reason", name)
		}
	}
}

func TestSelfStatsOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only self stats")
	}
	if cpu, ok := SelfCPUSeconds(); !ok || cpu < 0 {
		t.Errorf("cpu seconds = %v %t, want a supported non-negative value", cpu, ok)
	}
	if rss, ok := SelfRSSBytes(); !ok || rss <= 0 {
		t.Errorf("rss bytes = %v %t, want a supported positive value", rss, ok)
	}
}
