package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/store"
)

func engineOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{NoModifyPath: true, Clients: ClientOptions{Home: dir, State: filepath.Join(dir, "state"), Binary: "/bin/ohya", Args: []string{"mcp"}, Agents: "claude", Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "/bin/claude", nil }}, ServiceArgs: []string{"ingest"}, Platform: "linux", NoService: true, Verify: func(context.Context, Command) error { return nil }}
}

func TestSetupDryRunDoesNotCreateState(t *testing.T) {
	o := engineOptions(t)
	o.DryRun = true
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() || !report.DryRun {
		t.Fatalf("dry run = %+v %v", report, err)
	}
	if _, err := os.Stat(o.Clients.State); !os.IsNotExist(err) {
		t.Fatalf("created state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.Clients.Home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("changed config: %v", err)
	}
}

func TestSetupRepairsAndRemovesIntegration(t *testing.T) {
	o := engineOptions(t)
	for i := 0; i < 2; i++ {
		report, err := Run(context.Background(), o)
		if err != nil || !report.Complete() {
			t.Fatalf("setup = %+v %v", report, err)
		}
		last := report.Results[len(report.Results)-1]
		if last.Status != "configured" || last.Changed != (i == 0) {
			t.Fatal(last)
		}
	}
	report, err := Remove(context.Background(), o.Clients.State, nil)
	if err != nil || !report.Complete() {
		t.Fatalf("remove = %+v %v", report, err)
	}
	root, _, _, err := readEntry(JSONEntry{Path: filepath.Join(o.Clients.Home, ".claude.json"), Parent: "mcpServers"})
	if err != nil || root["mcpServers"] == nil {
		t.Fatalf("remaining config = %+v %v", root, err)
	}
}

func TestSetupReportsVerificationAndServiceFailures(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	o.RunService = func(context.Context, []string) error { return errors.New("user manager unavailable") }
	o.Verify = func(context.Context, Command) error { return errors.New("MCP unavailable") }
	report, err := Run(context.Background(), o)
	if err != nil || report.Complete() {
		t.Fatalf("partial = %+v %v", report, err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != "failed" {
		t.Fatal(report)
	}
}

func TestSetupPreservesUnownedLoadedService(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	calls := 0
	o.RunService = func(context.Context, []string) error { calls++; return nil }
	report, err := Run(context.Background(), o)
	if err != nil || report.Complete() {
		t.Fatalf("unowned = %+v %v", report, err)
	}
	if calls != 1 {
		t.Fatalf("modified unowned service: %d calls", calls)
	}
}

func TestSetupRecoveryPrecedesMemoryOpen(t *testing.T) {
	o := engineOptions(t)
	o.NoAgents = true
	o.Database = filepath.Join(o.Clients.State, "hub.db")
	if err := os.MkdirAll(o.Clients.State, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.Database, []byte("not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	verified := false
	o.Verify = func(context.Context, Command) error { verified = true; return nil }
	_, err := Run(context.Background(), o)
	if err == nil || verified {
		t.Fatalf("unsafe open: verified=%v error=%v", verified, err)
	}
	after, err := os.ReadFile(o.Database)
	if err != nil || string(after) != "not a SQLite database" {
		t.Fatal("changed original database")
	}
}

func recoveryOptions(t *testing.T) Options {
	t.Helper()
	o := engineOptions(t)
	o.NoAgents = true
	o.Database = filepath.Join(o.Clients.State, "hub.db")
	if err := os.MkdirAll(o.Clients.State, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), o.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().Exec(`CREATE TABLE recovery_size_probe (padding TEXT); INSERT INTO recovery_size_probe VALUES (hex(zeroblob(655360)))`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return o
}

func staleSchema(t *testing.T, o Options) {
	t.Helper()
	db, err := store.Open(context.Background(), o.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().Exec(`DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func recoveryPoints(t *testing.T, state string) []string {
	t.Helper()
	points, err := filepath.Glob(filepath.Join(state, "setup", "backups", "recovery-*"))
	if err != nil {
		t.Fatal(err)
	}
	return points
}

func backupResult(t *testing.T, report Report) Result {
	t.Helper()
	for _, r := range report.Results {
		if r.Component == "backup" {
			return r
		}
	}
	t.Fatalf("no backup result in %+v", report)
	return Result{}
}

func TestSetupSkipsRecoveryPointForCurrentSchema(t *testing.T) {
	o := recoveryOptions(t)
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() {
		t.Fatalf("setup = %+v %v", report, err)
	}
	if r := backupResult(t, report); r.Status != "not needed" || r.Changed {
		t.Fatalf("backup = %+v", r)
	}
	if points := recoveryPoints(t, o.Clients.State); len(points) != 0 {
		t.Fatalf("recovery points = %v", points)
	}
}

func TestSetupSavesRecoveryPointBeforeMigration(t *testing.T) {
	o := recoveryOptions(t)
	staleSchema(t, o)
	var progress []string
	o.Progress = func(line string) { progress = append(progress, line) }
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() {
		t.Fatalf("setup = %+v %v", report, err)
	}
	if r := backupResult(t, report); r.Status != "saved" || !r.Changed {
		t.Fatalf("backup = %+v", r)
	}
	if points := recoveryPoints(t, o.Clients.State); len(points) != 1 {
		t.Fatalf("recovery points = %v", points)
	}
	if len(progress) < 2 || !strings.HasPrefix(progress[0], "saving recovery point (about ") || !strings.Contains(progress[0], ") to ") {
		t.Fatalf("size progress = %q", progress)
	}
	for _, line := range progress[1:] {
		if !strings.HasPrefix(line, "recovery point ") || !strings.Contains(line, "% (") {
			t.Fatalf("copy progress = %q", progress)
		}
	}
}

func TestSetupKeepsNewestRecoveryPoints(t *testing.T) {
	o := recoveryOptions(t)
	dir := filepath.Join(o.Clients.State, "setup", "backups")
	old := time.Now().Add(-time.Hour)
	for i, name := range []string{"recovery-a", "recovery-b", "recovery-c"} {
		point := filepath.Join(dir, name)
		if err := os.MkdirAll(point, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(point, "hub.db"), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		stamp := old.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(point, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	staleSchema(t, o)
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() {
		t.Fatalf("setup = %+v %v", report, err)
	}
	saved := filepath.Dir(backupResult(t, report).Detail)
	points := recoveryPoints(t, o.Clients.State)
	if len(points) != 2 || !slices.Contains(points, saved) || !slices.Contains(points, filepath.Join(dir, "recovery-c")) {
		t.Fatalf("recovery points = %v, saved %s", points, saved)
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated")); err != nil {
		t.Fatalf("removed unrelated file: %v", err)
	}
}

func TestSetupRefusesRecoveryPointWithoutFreeSpace(t *testing.T) {
	o := recoveryOptions(t)
	staleSchema(t, o)
	if err := os.Truncate(o.Database, 5<<40); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), o)
	if err == nil || !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("refusal = %v", err)
	}
	r := backupResult(t, report)
	if r.Status != "failed" || r.Changed || !strings.Contains(r.Detail, "free") {
		t.Fatalf("backup = %+v", r)
	}
	if points := recoveryPoints(t, o.Clients.State); len(points) != 0 {
		t.Fatalf("recovery points = %v", points)
	}
	if _, err := os.Stat(filepath.Join(o.Clients.State, "setup", "backups")); !os.IsNotExist(err) {
		t.Fatalf("created the backups directory: %v", err)
	}
	if info, err := os.Stat(o.Database); err != nil || info.Size() != 5<<40 {
		t.Fatalf("database changed: %v %v", info, err)
	}
}

func TestSetupInterruptedRecoveryCopyLeavesNoPartialPoint(t *testing.T) {
	o := recoveryOptions(t)
	staleSchema(t, o)
	dir := filepath.Join(o.Clients.State, "setup", "backups")
	kept := filepath.Join(dir, "recovery-kept")
	if err := os.MkdirAll(kept, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, "hub.db"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Progress = func(line string) {
		if strings.HasPrefix(line, "recovery point ") {
			cancel()
		}
	}
	report, err := Run(ctx, o)
	if err == nil || !errors.Is(err, context.Canceled) || report.Complete() {
		t.Fatalf("interrupted copy = %+v %v", report, err)
	}
	if points := recoveryPoints(t, o.Clients.State); len(points) != 1 || points[0] != kept {
		t.Fatalf("recovery points = %v", points)
	}
	staging, err := filepath.Glob(filepath.Join(dir, ".recovery-*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("staging left behind: %v %v", staging, err)
	}
}

func TestSetupRemovesAbandonedRecoveryStaging(t *testing.T) {
	o := recoveryOptions(t)
	staleSchema(t, o)
	dir := filepath.Join(o.Clients.State, "setup", "backups")
	abandoned := filepath.Join(dir, ".recovery-abandoned")
	if err := os.MkdirAll(abandoned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "hub.db"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() {
		t.Fatalf("setup = %+v %v", report, err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned staging kept: %v", err)
	}
	if points := recoveryPoints(t, o.Clients.State); len(points) != 1 {
		t.Fatalf("recovery points = %v", points)
	}
}

func TestSetupReportsResultsAsTheyFinish(t *testing.T) {
	o := engineOptions(t)
	var seen []Result
	o.Notify = func(r Result) { seen = append(seen, r) }
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() {
		t.Fatalf("setup = %+v %v", report, err)
	}
	if !slices.Equal(seen, report.Results) {
		t.Fatalf("notified %+v, reported %+v", seen, report.Results)
	}
}
