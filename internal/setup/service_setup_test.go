package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
