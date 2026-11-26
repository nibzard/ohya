package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/store"
)

func TestSetupRestoresServiceAfterFailedUpgrade(t *testing.T) {
	for _, failure := range []string{"backup", "start"} {
		t.Run(failure, func(t *testing.T) {
			o := engineOptions(t)
			o.NoService = false
			o.NoAgents = true
			old, err := ServicePlan("linux", o.Clients.Home, o.Clients.State, o.Clients.Binary, []string{"ingest", "--state", "/previous"})
			if err != nil {
				t.Fatal(err)
			}
			receipt := filepath.Join(o.Clients.State, "setup", "service.json")
			if _, err := ApplyFile(old.Path, receipt, old.Content); err != nil {
				t.Fatal(err)
			}
			if failure == "backup" {
				o.Database = filepath.Join(o.Clients.State, "hub.db")
				if err := os.WriteFile(o.Database, []byte("invalid database"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			o.LoadedService = func(context.Context, []string) error { return nil }
			starts := 0
			o.RunService = func(_ context.Context, args []string) error {
				if strings.Contains(strings.Join(args, " "), "restart ohya.service") {
					starts++
					if failure == "start" && starts == 1 {
						return errors.New("new service failed")
					}
				}
				return nil
			}
			o.Verify = func(context.Context, Command) error { t.Fatal("verified failed setup"); return nil }
			report, err := Run(context.Background(), o)
			if err != nil || report.Complete() {
				t.Fatalf("report %+v %v", report, err)
			}
			current, err := os.ReadFile(old.Path)
			if err != nil || !bytes.Equal(current, old.Content) {
				t.Fatal("lost previous service configuration")
			}
			last := report.Results[len(report.Results)-1]
			if !strings.Contains(last.Detail, "previous service restored") || starts == 0 {
				t.Fatalf("recovery %+v starts=%d", last, starts)
			}
		})
	}
}

func TestSetupCleansUpInitialInstallAfterStartFailure(t *testing.T) {
	for _, loadedOnStart := range []bool{false, true} {
		name := "unloaded"
		if loadedOnStart {
			name = "loaded"
		}
		t.Run(name, func(t *testing.T) {
			o := engineOptions(t)
			o.NoService = false
			o.NoAgents = true
			service, err := ServicePlan("linux", o.Clients.Home, o.Clients.State, o.Clients.Binary, o.ServiceArgs)
			if err != nil {
				t.Fatal(err)
			}
			receipt := filepath.Join(o.Clients.State, "setup", "service.json")
			loaded := false
			stopped := false
			o.LoadedService = func(context.Context, []string) error {
				if loaded {
					return nil
				}
				return errors.New("not loaded")
			}
			o.RunService = func(_ context.Context, args []string) error {
				joined := strings.Join(args, " ")
				if loadedOnStart && strings.Contains(joined, "enable ohya.service") {
					loaded = true
				}
				if strings.Contains(joined, "restart ohya.service") {
					return errors.New("start failed")
				}
				if strings.Contains(joined, "disable --now ohya.service") {
					stopped = true
					loaded = false
				}
				return nil
			}
			o.Verify = func(context.Context, Command) error { t.Fatal("verified failed setup"); return nil }
			report, err := Run(context.Background(), o)
			if err != nil || report.Complete() {
				t.Fatalf("report %+v %v", report, err)
			}
			if _, err := os.Stat(service.Path); !os.IsNotExist(err) {
				t.Fatalf("half-installed unit survived: %v", err)
			}
			if _, err := os.Stat(receipt); !os.IsNotExist(err) {
				t.Fatalf("half-installed receipt survived: %v", err)
			}
			if stopped != loadedOnStart {
				t.Fatalf("stopped = %v, want %v", stopped, loadedOnStart)
			}
			last := report.Results[len(report.Results)-1]
			if last.Status != "failed" || last.Changed || !strings.Contains(last.Detail, "newly installed service removed") {
				t.Fatalf("cleanup result = %+v", last)
			}
		})
	}
}

func TestSetupUpgradeFailureNotesMigratedSchemaAndRecoveryPoint(t *testing.T) {
	o := serviceOptionsWithDatabase(t)
	createSchema9Database(t, o.Database)
	old, err := ServicePlan("linux", o.Clients.Home, o.Clients.State, o.Clients.Binary, []string{"ingest", "--state", "/previous"})
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(o.Clients.State, "setup", "service.json")
	if _, err := ApplyFile(old.Path, receipt, old.Content); err != nil {
		t.Fatal(err)
	}
	o.LoadedService = func(context.Context, []string) error { return nil }
	starts := 0
	o.RunService = func(_ context.Context, args []string) error {
		if strings.Contains(strings.Join(args, " "), "restart ohya.service") {
			starts++
			if starts == 1 {
				return errors.New("new service failed")
			}
		}
		return nil
	}
	o.Verify = func(context.Context, Command) error { t.Fatal("verified failed setup"); return nil }
	report, err := Run(context.Background(), o)
	if err != nil || report.Complete() {
		t.Fatalf("report %+v %v", report, err)
	}
	current, err := store.SchemaCurrent(context.Background(), o.Database)
	if err != nil || !current {
		t.Fatalf("schema current = %v, %v", current, err)
	}
	backup := backupResult(t, report)
	if backup.Status != "saved" || backup.Detail == "" {
		t.Fatalf("backup = %+v", backup)
	}
	last := report.Results[len(report.Results)-1]
	for _, want := range []string{"previous service restored", "database schema was already migrated", "see the backup result for the recovery point"} {
		if !strings.Contains(last.Detail, want) {
			t.Fatalf("detail %q missing %q", last.Detail, want)
		}
	}
}
