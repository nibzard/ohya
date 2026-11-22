package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func inspected(component string, err error, ready bool) Result {
	result := Result{Component: component, Status: "ready"}
	if err != nil {
		result.Status = "failed"
		result.Detail = err.Error()
	} else if !ready {
		result.Status = "changed"
		result.Detail = "owned configuration is missing, edited, or awaiting repair"
	}
	return result
}

func inspectServiceReceipt(ctx context.Context, r fileReceipt, run func(context.Context, []string) error, component string, plan func(string, string, string, string, []string) (Service, error)) Result {
	raw, readErr := readRegular(r.Path)
	result := inspected(component, readErr, bytes.Equal(raw, r.Content))
	if result.Status == "ready" {
		platform := "linux"
		if filepath.Ext(r.Path) == ".plist" {
			platform = "darwin"
		}
		s, err := plan(platform, "/", "/", "/ohya", nil)
		if err == nil {
			err = run(ctx, s.Check)
		}
		if err != nil {
			result.Status = "failed"
			result.Detail = err.Error()
		}
	}
	return result
}

func Inspect(ctx context.Context, state string, run func(context.Context, []string) error) ([]Result, error) {
	if run == nil {
		run = RunServiceCommand
	}
	var results []Result
	servicePath := filepath.Join(state, "setup", "service.json")
	if r, err := loadFileReceipt(servicePath); err == nil {
		results = append(results, inspectServiceReceipt(ctx, r, run, "service", ServicePlan))
	} else if !os.IsNotExist(err) {
		results = append(results, inspected("service", err, false))
	}
	hubServicePath := filepath.Join(state, "setup", "hub-service.json")
	if r, err := loadFileReceipt(hubServicePath); err == nil {
		results = append(results, inspectServiceReceipt(ctx, r, run, "hub-service", HubServicePlan))
	} else if !os.IsNotExist(err) {
		results = append(results, inspected("hub-service", err, false))
	}
	for _, name := range []string{"claude", "codex", "pi", "agy"} {
		path := filepath.Join(state, "setup", "clients", name+".json")
		raw, err := readRegular(path)
		if os.IsNotExist(err) {
			continue
		}
		ready := false
		if err == nil && name == "codex" {
			var r tomlReceipt
			err = json.Unmarshal(raw, &r)
			if err == nil {
				var current []byte
				current, err = readRegular(r.Path)
				if err == nil {
					_, err = ownedTOML(current, r)
					ready = err == nil && bytes.Contains(current, []byte(r.Block))
				}
			}
		} else if err == nil {
			var r entryReceipt
			err = json.Unmarshal(raw, &r)
			if err == nil {
				var children map[string]json.RawMessage
				_, children, _, err = readEntry(r.Entry)
				ready = err == nil && sameJSON(children[r.Entry.Name], r.Entry.Value)
			}
		}
		results = append(results, inspected(name, err, ready))
	}
	for _, name := range []string{"pi-extension.json", "pi-bridge-receipt.json"} {
		path := filepath.Join(state, "setup", name)
		r, err := loadFileReceipt(path)
		if os.IsNotExist(err) {
			continue
		}
		var current []byte
		if err == nil {
			current, err = readRegular(r.Path)
		}
		results = append(results, inspected(name, err, bytes.Equal(current, r.Content)))
	}
	profiles, err := filepath.Glob(filepath.Join(state, "setup", "profiles", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range profiles {
		raw, err := readRegular(path)
		var r profileReceipt
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		var current []byte
		if err == nil {
			current, err = readRegular(r.Path)
		}
		ready := false
		if err == nil {
			if r.WholeFile {
				ready = bytes.Equal(current, r.Content)
			} else {
				ready = bytes.Contains(current, r.Content)
			}
		}
		results = append(results, inspected("path:"+filepath.Base(r.Path), err, ready))
	}
	if err := ctx.Err(); err != nil {
		return results, fmt.Errorf("inspect setup: %w", err)
	}
	return results, nil
}
