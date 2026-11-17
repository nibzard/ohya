package setup

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubServicePlans(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		s, err := HubServicePlan(platform, "/home/a b", "/home/a b/ohya", []string{"serve", "--state", "/home/a b"})
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(s.Path) || len(s.Start) == 0 || len(s.Stop) == 0 || len(s.Check) == 0 {
			t.Fatalf("incomplete hub service plan: %+v", s)
		}
		if platform == "linux" {
			if filepath.Base(s.Path) != "ohya-serve.service" {
				t.Fatalf("hub unit = %s", s.Path)
			}
			if !strings.Contains(string(s.Content), "Description=ohya memory hub for devices\n") {
				t.Fatal(string(s.Content))
			}
			if !strings.Contains(string(s.Content), `"/home/a b/ohya" "serve" "--state" "/home/a b"`) {
				t.Fatal(string(s.Content))
			}
			joined := strings.Join(s.Start[2], " ")
			if !strings.Contains(joined, "ohya-serve.service") {
				t.Fatalf("restart command = %s", joined)
			}
		} else {
			if filepath.Base(s.Path) != "com.nibzard.ohya.serve.plist" {
				t.Fatalf("hub agent = %s", s.Path)
			}
			d := xml.NewDecoder(strings.NewReader(string(s.Content)))
			var label string
			for {
				token, err := d.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if start, ok := token.(xml.StartElement); ok && start.Name.Local == "string" && label == "" {
					if err := d.DecodeElement(&label, &start); err != nil {
						t.Fatal(err)
					}
				}
			}
			if label != "com.nibzard.ohya.serve" {
				t.Fatalf("hub label = %q", label)
			}
		}
		ingest, err := ServicePlan(platform, "/home/a b", "/home/a b/ohya", nil)
		if err != nil {
			t.Fatal(err)
		}
		if ingest.Path == s.Path {
			t.Fatalf("hub service collides with the ingestion service at %s", s.Path)
		}
	}
}

func TestHubServiceConfigureIsIdempotent(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	o.RunService = func(context.Context, []string) error { return nil }
	o.LoadedService = func(context.Context, []string) error { return errors.New("not loaded") }
	o.HubServiceArgs = []string{"serve", "--db", filepath.Join(o.Clients.State, "hub.db")}
	for i := 0; i < 2; i++ {
		report, err := Run(context.Background(), o)
		if err != nil || !report.Complete() {
			t.Fatalf("hub setup run %d = %+v %v", i, report, err)
		}
		var hub Result
		for _, r := range report.Results {
			if r.Component == "hub-service" {
				hub = r
			}
		}
		if hub.Status != "running" {
			t.Fatalf("hub service run %d = %+v", i, report.Results)
		}
		if hub.Changed != (i == 0) {
			t.Fatalf("rerun rewrote the hub service file: %+v", hub)
		}
	}
}

func TestHubServiceDryRunPlansWithoutState(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	o.DryRun = true
	o.HubServiceArgs = []string{"serve"}
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() {
		t.Fatalf("dry run = %+v %v", report, err)
	}
	found := false
	for _, r := range report.Results {
		if r.Component == "hub-service" {
			found = true
			if r.Status != "planned" {
				t.Fatalf("planned hub service = %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("dry run omitted the hub service: %+v", report.Results)
	}
	if _, err := os.Stat(o.Clients.State); !os.IsNotExist(err) {
		t.Fatalf("dry run created state: %v", err)
	}
}

func TestHubCheckFailureStopsSetup(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	o.NoAgents = true
	o.Verify = nil
	o.RunService = func(context.Context, []string) error { return nil }
	o.LoadedService = func(context.Context, []string) error { return errors.New("not loaded") }
	o.HubServiceArgs = []string{"serve"}
	o.CheckHub = func(context.Context) error {
		return errors.New("no relay is reachable: the hub needs outbound HTTPS (TCP 443)")
	}
	report, err := Run(context.Background(), o)
	if err != nil || report.Complete() {
		t.Fatalf("failed hub check = %+v %v", report, err)
	}
	var hub Result
	have := false
	for _, r := range report.Results {
		if r.Component == "hub" {
			hub, have = r, true
		}
	}
	if !have || hub.Status != "failed" || !strings.Contains(hub.Detail, "outbound HTTPS (TCP 443)") {
		t.Fatalf("hub result = %+v have=%v", hub, have)
	}
	for _, r := range report.Results {
		if r.Component == "memory" {
			t.Fatalf("setup continued past a failed hub check: %+v", report.Results)
		}
	}
}

func TestRemoveStopsHubService(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	o.RunService = func(context.Context, []string) error { return nil }
	o.LoadedService = func(context.Context, []string) error { return errors.New("not loaded") }
	o.HubServiceArgs = []string{"serve"}
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(o.Clients.State, "setup", "hub-service.json")
	if _, err := os.Stat(receipt); err != nil {
		t.Fatal(err)
	}
	report, err := Remove(context.Background(), o.Clients.State, func(_ context.Context, args []string) error {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "ohya-serve.service") && !strings.Contains(joined, "disable") {
			t.Errorf("unexpected hub service command: %s", joined)
		}
		return nil
	})
	if err != nil || !report.Complete() {
		t.Fatalf("remove = %+v %v", report, err)
	}
	found := false
	for _, r := range report.Results {
		if r.Component == "hub-service" {
			found = true
			if r.Status != "removed" || !r.Changed {
				t.Fatalf("hub service removal = %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("remove omitted the hub service: %+v", report.Results)
	}
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatal("hub service receipt survived removal")
	}
}

func TestInspectReportsHubService(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	o.RunService = func(context.Context, []string) error { return nil }
	o.LoadedService = func(context.Context, []string) error { return errors.New("not loaded") }
	o.HubServiceArgs = []string{"serve"}
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	results, err := Inspect(context.Background(), o.Clients.State, func(context.Context, []string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range results {
		if r.Component == "hub-service" {
			found = true
			if r.Status != "ready" {
				t.Fatalf("inspected hub service = %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("inspect omitted the hub service: %+v", results)
	}
}
